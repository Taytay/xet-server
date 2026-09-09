package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// StaticTokenAuth is a single shared-secret Authenticator: exactly one
// bearer token, set at construction, grants full access (every scope) to
// any request presenting it; any other token, or none, fails with
// ErrUnauthenticated. This deliberately does not distinguish per-user
// identity or partial scopes — it's a single gate for the whole server,
// matching how this project's -auth-token flag is meant to be used (an
// operator-chosen shared secret, not a multi-tenant credential system).
// A deployment that needs real per-user scopes should implement its own
// Authenticator against whatever identity provider it already has; this
// type is intentionally the simplest possible non-trivial option, not
// the only one.
type StaticTokenAuth struct {
	token string
}

// NewStaticTokenAuth constructs a StaticTokenAuth requiring exactly
// token. token must be non-empty — constructing this with an empty
// string would make every bearer-auth request (even one presenting an
// empty token, which some malformed clients do) succeed, defeating the
// point; use NoAuth{} directly if no enforcement is wanted.
func NewStaticTokenAuth(token string) *StaticTokenAuth {
	return &StaticTokenAuth{token: token}
}

func (a *StaticTokenAuth) Authenticate(r *http.Request) (Principal, error) {
	got := bearerToken(r)
	if got == "" || a.token == "" {
		return nil, ErrUnauthenticated
	}
	// Constant-time comparison: this is a shared secret gating server
	// access, so a timing side-channel that lets an attacker recover it
	// byte-by-byte is exactly the class of bug worth avoiding here, even
	// though the token is compared against a single fixed value rather
	// than looked up in a keyed store.
	if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
		return nil, ErrUnauthenticated
	}
	return allScopesPrincipal{}, nil
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, or "" if the header is absent or not in that exact form (case-
// insensitive on the "Bearer" scheme name per RFC 6750, but nothing else
// about the header is normalized — a token with leading/trailing
// whitespace inside the header value is passed through as-is, matching
// how a real HTTP client would send it).
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}
