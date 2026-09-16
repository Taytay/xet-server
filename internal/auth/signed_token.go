package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TokenMinter issues short-lived bearer tokens carrying a scope and a
// subject. hubserver uses it to answer the xet-{read,write}-token routes
// and to put a token into every git-lfs batch action it hands out, so
// the CAS side can authenticate those requests without the client ever
// seeing the operator's shared secret.
type TokenMinter interface {
	// MintToken returns a token granting scope to subject until exp.
	MintToken(scope Scope, subject string, ttl time.Duration) (token string, exp time.Time, err error)
}

// URLTokenVerifier is implemented by an Authenticator that accepts a
// minted token carried in a URL's query string instead of a header: the
// credential of a presigned-style download URL. xet-core fetches the
// xorb URLs in a reconstruction response with no Authorization header
// (on the real Hub they are presigned CDN URLs), so a CAS whose storage
// backend cannot presign signs its own byte-serving URLs this way. Only
// casserver's xorb GET/HEAD routes consult it; nothing else accepts a
// credential from a URL.
type URLTokenVerifier interface {
	VerifyURLToken(token string) (Principal, error)
}

// SignedTokenAuth is the Authenticator a deployment with one shared
// secret should use when the Hub shim and the CAS run as a pair. It
// accepts three credential shapes:
//
//   - Authorization: Bearer <secret>          - the operator's shared
//     secret, every scope (what StaticTokenAuth accepts).
//   - Authorization: Basic <user>:<secret>     - the same secret sent the
//     only way git-lfs can send a credential (HTTP Basic, via git's
//     credential helper). The user part becomes the Principal's Subject,
//     which is what names the owner of a git-lfs lock.
//   - Authorization: Bearer <minted token>     - a token this same secret
//     minted via MintToken: HMAC-signed, scoped (read or write), bound to
//     a subject, and expiring. This is what the Hub shim hands to
//     hf_xet / git-xet for the CAS, and what it puts on git-lfs download
//     hrefs. Also accepted as the password of a Basic credential, since
//     some clients can only send Basic.
//
// Minted tokens are stateless: any process holding the secret can verify
// one, so a CAS and a Hub shim in separate processes (or a restarted one)
// agree without sharing state. StaticTokenAuth stays as the plain
// single-secret Authenticator for deployments that never mint anything.
type SignedTokenAuth struct {
	secret []byte
}

// NewSignedTokenAuth constructs a SignedTokenAuth around secret, which
// must be non-empty (see NewStaticTokenAuth for why an empty secret is
// refused rather than silently accepted).
func NewSignedTokenAuth(secret string) *SignedTokenAuth {
	return &SignedTokenAuth{secret: []byte(secret)}
}

// mintedTokenPrefix versions the token format so a future change to the
// layout can coexist with tokens minted before it.
const mintedTokenPrefix = "xst1"

// MintToken implements TokenMinter. Layout (dot-separated, every field
// free of dots by construction):
//
//	xst1.<exp unix seconds>.<scope>.<hex(subject)>.<hex(HMAC-SHA256)>
//
// The MAC covers the prefix, expiry, scope, and subject, so none of them
// can be altered without the secret.
func (a *SignedTokenAuth) MintToken(scope Scope, subject string, ttl time.Duration) (string, time.Time, error) {
	if len(a.secret) == 0 {
		return "", time.Time{}, errors.New("auth: cannot mint a token without a secret")
	}
	if scope != ScopeRead && scope != ScopeWrite {
		return "", time.Time{}, fmt.Errorf("auth: cannot mint a token for scope %q", scope)
	}
	if ttl <= 0 {
		return "", time.Time{}, errors.New("auth: token ttl must be positive")
	}
	exp := time.Now().Add(ttl).Truncate(time.Second)
	payload := mintedPayload(exp.Unix(), scope, subject)
	mac := a.mac(payload)
	return payload + "." + hex.EncodeToString(mac), exp, nil
}

func mintedPayload(exp int64, scope Scope, subject string) string {
	return mintedTokenPrefix + "." + strconv.FormatInt(exp, 10) + "." + string(scope) + "." + hex.EncodeToString([]byte(subject))
}

func (a *SignedTokenAuth) mac(payload string) []byte {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// VerifyURLToken implements URLTokenVerifier: a minted token is as good
// in a URL as in a header, since it already carries its scope, subject
// and expiry.
func (a *SignedTokenAuth) VerifyURLToken(token string) (Principal, error) {
	return a.verifyMinted(token)
}

// verifyMinted parses and checks a token produced by MintToken. Returns
// ErrUnauthenticated for anything that is not a currently valid minted
// token (wrong shape, bad MAC, expired).
func (a *SignedTokenAuth) verifyMinted(token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != mintedTokenPrefix {
		return nil, ErrUnauthenticated
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	scope := Scope(parts[2])
	if scope != ScopeRead && scope != ScopeWrite {
		return nil, ErrUnauthenticated
	}
	subjectBytes, err := hex.DecodeString(parts[3])
	if err != nil {
		return nil, ErrUnauthenticated
	}
	gotMAC, err := hex.DecodeString(parts[4])
	if err != nil {
		return nil, ErrUnauthenticated
	}
	payload := mintedPayload(exp, scope, string(subjectBytes))
	if !hmac.Equal(gotMAC, a.mac(payload)) {
		return nil, ErrUnauthenticated
	}
	if time.Now().Unix() > exp {
		return nil, ErrUnauthenticated
	}
	return &scopedPrincipal{subject: string(subjectBytes), scope: scope}, nil
}

// Authenticate implements Authenticator; see the type's doc comment for
// the three accepted credential shapes.
func (a *SignedTokenAuth) Authenticate(r *http.Request) (Principal, error) {
	if len(a.secret) == 0 {
		return nil, ErrUnauthenticated
	}
	if token := bearerToken(r); token != "" {
		if subtle.ConstantTimeCompare([]byte(token), a.secret) == 1 {
			return &scopedPrincipal{subject: SharedSecretSubject, scope: ScopeWrite}, nil
		}
		return a.verifyMinted(token)
	}
	if user, pass, ok := r.BasicAuth(); ok {
		if subtle.ConstantTimeCompare([]byte(pass), a.secret) == 1 {
			if user == "" {
				user = SharedSecretSubject
			}
			return &scopedPrincipal{subject: user, scope: ScopeWrite}, nil
		}
		return a.verifyMinted(pass)
	}
	return nil, ErrUnauthenticated
}

// SharedSecretSubject is the Subject reported for a request that
// authenticated with the raw shared secret and no user name.
const SharedSecretSubject = "shared-secret"

// scopedPrincipal holds one scope; write implies read, matching how the
// real CAS's scopes nest.
type scopedPrincipal struct {
	subject string
	scope   Scope
}

func (p *scopedPrincipal) Subject() string { return p.subject }

func (p *scopedPrincipal) HasScope(scope Scope) bool {
	switch scope {
	case ScopeRead:
		return p.scope == ScopeRead || p.scope == ScopeWrite
	case ScopeWrite:
		return p.scope == ScopeWrite
	default:
		return false
	}
}
