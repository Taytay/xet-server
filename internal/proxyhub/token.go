package proxyhub

// token.go: GET /api/{repo_type}s/{repo_id}/xet-{read,write}-token/{revision}.
// The one read endpoint that CANNOT delegate to Embedded - hubserver's
// own xet-token handler mints a fake random token, worthless against
// the real upstream CAS - so this package caches the real XetToken
// value itself, independent of Embedded, under the same freshness+
// fallback policy every other endpoint uses. See the package doc
// comment for why CasURL is rewritten while AccessToken passes through
// unchanged, and why the real upstream CAS URL is ALSO tracked
// separately in casURLCache regardless of this endpoint's own per-repo
// cache entry (cmd/xet-proxyd's CAS-facing routing needs one shared
// "most recently known" value - see UpstreamCASBaseURL).

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/hfclient"
)

// tokenSafetyMargin is how far in the future a cached xet token's exp must
// still be for it to be served on the stale-fallback path (fetchOrServeCache
// below). A token any closer to expiry than this - or already past it - is
// worthless: the first CAS request presenting it would be rejected with a 401
// the client can't explain. This mirrors huggingface_hub's own 60s
// XET_CONNECTION_INFO_SAFETY_PERIOD; xet-core's docs recommend at least 30s.
const tokenSafetyMargin = 60 * time.Second

func (s *Server) handleXetToken(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string, kind hfclient.XetTokenKind) {
	cred := auth.CredentialFromRequest(r)
	key := cacheKey(repoType, repoID, revision, strconv.Itoa(int(kind)))

	tok, err := fetchOrServeCache(s.NoCache, s.xetTokenCache, s.CacheTTL, key,
		func() (*hfclient.XetToken, error) {
			callCtx, cancel := s.callContext(r)
			defer cancel()
			return s.Hub.GetXetToken(callCtx, cred, repoType, repoID, revision, kind)
		},
		// Stale-fallback guard: only serve a cached token that is still
		// actually usable. Serving one whose exp has already passed hands
		// the client a token the real CAS will reject with a 401 it can't
		// explain, when a clean refresh error would at least fail loudly
		// (and be retryable). See tokenSafetyMargin.
		func(t *hfclient.XetToken) bool { return t != nil && t.Exp > time.Now().Add(tokenSafetyMargin).Unix() },
	)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}

	if tok.CasURL != "" {
		// Recorded regardless of NoCache: this is routing state (which
		// real CAS base URL to point the CAS-facing proxy at), not cached
		// user data - withholding it under -no-cache would leave
		// UpstreamCASBaseURL permanently unanswered, 503ing every CAS
		// request even though every Hub call is still relayed live.
		s.casURLCache.Set(casURLCacheKey, tok.CasURL)
	}

	// Remember which credential minted this access token so a later
	// CAS-port 401 presenting it can be healed with a fresh replacement
	// (FreshXetTokenFor). Recording the token actually served - fresh or
	// stale - is deliberate: a client handed a stale token and presenting
	// it to the CAS is exactly the case the heal must be able to fix.
	s.recordAccessToken(tok.AccessToken, tokenRecord{
		cred: cred, repoType: repoType, repoID: repoID, revision: revision, kind: kind,
	})

	rewritten := *tok
	rewritten.CasURL = s.CASBaseURL

	w.Header().Set("X-Xet-Cas-Url", s.CASBaseURL)
	w.Header().Set("X-Xet-Access-Token", rewritten.AccessToken)
	w.Header().Set("X-Xet-Token-Expiration", strconv.FormatInt(rewritten.Exp, 10))
	writeJSON(w, rewritten)
}

// UpstreamCASBaseURL returns the most recently observed real upstream
// CAS base URL, or "" if none has been seen yet. Only ever reads
// present, never fresh - this cache has no periodic re-check to
// perform (the value only ever changes as a side effect of a real
// xet-token relay above, not on any TTL-driven schedule), so the ttl
// argument to Get is inert here; -1 makes that explicit rather than
// implying s.CacheTTL is meaningfully in play.
func (s *Server) UpstreamCASBaseURL() (string, bool) {
	value, present, _ := s.casURLCache.Get(casURLCacheKey, -1)
	return value, present
}

// fetchOrServeCache implements the shared TTL-freshness-then-
// stale-fallback policy for a VALUE cache (xetTokenCache/casURLCache -
// the two caches in this package that still hold actual data, unlike
// the struct{}-valued freshness trackers repo.go/tree.go/resolve.go
// use against Embedded directly).
//
// staleUsable gates the stale-fallback path: when a live fetch fails but a
// cached value exists, that value is only served if staleUsable says it's
// still usable (nil means always usable - appropriate for the CAS URL, which
// never expires). This lets a caller refuse to serve a cached value that is
// technically present but has lost its reason to be served - an expired xet
// token, for example - and surface the fetch error instead.
func fetchOrServeCache[V any](noCache bool, cache *ttlCache[V], ttl time.Duration, key string, fetch func() (V, error), staleUsable func(V) bool) (V, error) {
	if noCache {
		return fetch()
	}
	if value, present, fresh := cache.Get(key, ttl); present && fresh {
		return value, nil
	}
	value, err := fetch()
	if err != nil {
		if stale, present, _ := cache.Get(key, ttl); present && (staleUsable == nil || staleUsable(stale)) {
			return stale, nil
		}
		return value, err
	}
	cache.Set(key, value)
	return value, nil
}

// tokenRecord remembers the exact Hub call parameters (and the caller's
// credential) that minted a given xet access token, so a later CAS-port 401
// presenting that token can be healed with a fresh replacement scoped to the
// same repo/ref (see FreshXetTokenFor).
type tokenRecord struct {
	cred     auth.CredentialHelper
	repoType string
	repoID   string
	revision string
	kind     hfclient.XetTokenKind
}

// maxIndexedAccessTokens bounds accessTokenIndex, which otherwise grows with
// every distinct token the proxy relays. Generous for any realistic mix of
// concurrent repos/refs; beyond it the oldest-recorded entry is evicted.
const maxIndexedAccessTokens = 4096

// recordAccessToken indexes presentedToken -> the tokenRecord that minted it,
// evicting an arbitrary entry once the index is full.
func (s *Server) recordAccessToken(presentedToken string, rec tokenRecord) {
	if presentedToken == "" {
		return
	}
	s.accessTokenMu.Lock()
	defer s.accessTokenMu.Unlock()
	if len(s.accessTokenIndex) >= maxIndexedAccessTokens {
		for k := range s.accessTokenIndex {
			delete(s.accessTokenIndex, k)
			break
		}
	}
	s.accessTokenIndex[presentedToken] = rec
}

// FreshXetTokenFor mints a freshly-issued replacement xet access token for
// the repo/ref that presentedToken was originally issued to, authenticating
// to the real Hub with the SAME credential that client used to obtain the
// original token (stored by recordAccessToken when the token was relayed).
// ok=false - and the caller should relay the upstream 401 as-is - when
// presentedToken isn't a token this proxy relayed, or the refresh call to the
// real Hub fails (including a revoked/expired client credential). Because the
// replacement is minted with the client's own credential for the same
// repo/ref/scope, this can never widen anyone's access - it only restores
// access the client already held.
func (s *Server) FreshXetTokenFor(ctx context.Context, presentedToken string) (string, bool) {
	s.accessTokenMu.Lock()
	rec, ok := s.accessTokenIndex[presentedToken]
	s.accessTokenMu.Unlock()
	if !ok {
		return "", false
	}
	callCtx := ctx
	cancel := func() {}
	if s.MetadataCallTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, s.MetadataCallTimeout)
	}
	defer cancel()
	fresh, err := s.Hub.GetXetToken(callCtx, rec.cred, rec.repoType, rec.repoID, rec.revision, rec.kind)
	if err != nil {
		slog.Warn("proxyhub: token-heal refresh failed", "repo", rec.repoID, "error", err)
		return "", false
	}
	// Keep the caches the rest of this proxy reads consistent with the
	// replacement token: the xet-token cache entry (so a future refresh
	// serves the fresh value) and the upstream CAS URL routing state.
	s.xetTokenCache.Set(cacheKey(rec.repoType, rec.repoID, rec.revision, strconv.Itoa(int(rec.kind))), fresh)
	if fresh.CasURL != "" {
		s.casURLCache.Set(casURLCacheKey, fresh.CasURL)
	}
	s.recordAccessToken(fresh.AccessToken, rec)
	return fresh.AccessToken, true
}
