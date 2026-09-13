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
	"net/http"
	"strconv"
	"time"

	"github.com/guilt/xet-server/internal/auth"
	"github.com/guilt/xet-server/internal/hfclient"
)

func (s *Server) handleXetToken(w http.ResponseWriter, r *http.Request, repoType, repoID, revision string, kind hfclient.XetTokenKind) {
	cred := auth.CredentialFromRequest(r)
	key := cacheKey(repoType, repoID, revision, strconv.Itoa(int(kind)))

	tok, err := fetchOrServeCache(s.NoCache, s.xetTokenCache, s.CacheTTL, key, func() (*hfclient.XetToken, error) {
		callCtx, cancel := s.callContext(r)
		defer cancel()
		return s.Hub.GetXetToken(callCtx, cred, repoType, repoID, revision, kind)
	})
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
func fetchOrServeCache[V any](noCache bool, cache *ttlCache[V], ttl time.Duration, key string, fetch func() (V, error)) (V, error) {
	if noCache {
		return fetch()
	}
	if value, present, fresh := cache.Get(key, ttl); present && fresh {
		return value, nil
	}
	value, err := fetch()
	if err != nil {
		if stale, present, _ := cache.Get(key, ttl); present {
			return stale, nil
		}
		return value, err
	}
	cache.Set(key, value)
	return value, nil
}
