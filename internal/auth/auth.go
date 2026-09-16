// Package auth defines this project's authentication/authorization
// interfaces, kept deliberately small so a third party can implement a
// custom backend (JWT, OAuth, mTLS, an internal SSO integration, whatever
// a real deployment needs) without touching casserver, hubserver, or the
// cmd/xet client at all - implement Authenticator (server side) and/or
// CredentialHelper (client side), and both sides of this project work
// with it automatically.
//
// This mirrors the real xet-core/hf_xet protocol's actual auth model
// (confirmed against xet-core's own openapi/cas.openapi.yaml and its
// client-side xet_client::common::auth module): Authorization: Bearer
// <token>, with tokens carrying scopes (read, write) - 401 for a missing
// or invalid token, 403 for a valid token lacking the scope a given
// endpoint requires. See docs/PROTOCOL.md for the full writeup.
//
// The zero-configuration default on both sides (NoAuth, NoopCredentialHelper)
// is byte-for-byte today's pre-v0.8.0 behavior: no token is required, and
// none is sent. Passing -auth-token to xetd or xet is what opts into
// enforcement/sending a credential - existing deployments and scripts
// that never pass that flag see no behavior change at all.
package auth

import (
	"errors"
	"net/http"
)

// Scope identifies a capability a Principal may or may not hold. Kept as
// a plain string type (not a closed enum) so a custom Authenticator can
// introduce its own scopes beyond ScopeRead/ScopeWrite if its endpoints
// need finer-grained authorization than this project's built-in servers
// require.
type Scope string

const (
	// ScopeRead gates every read-only CAS/Hub endpoint (reconstruction,
	// xorb fetch, chunk-dedup lookup, resolve) - mirrors the real
	// protocol's `read` scope.
	ScopeRead Scope = "read"
	// ScopeWrite gates every mutating CAS/Hub endpoint (xorb/shard
	// upload, commit, repo create) - mirrors the real protocol's `write`
	// scope.
	ScopeWrite Scope = "write"
	// ScopeAdmin gates this project's own operator routes that destroy
	// data (garbage collection). Not part of the real protocol: no
	// minted token ever carries it, only the operator's shared secret
	// itself (SignedTokenAuth) or a deployment with no auth at all
	// (NoAuth, where every scope is granted).
	ScopeAdmin Scope = "admin"
)

// Principal is the authenticated identity/permission set an Authenticator
// hands back for a request that passed authentication. Kept to exactly
// two methods so a minimal custom implementation (e.g. "this token maps
// to this one hardcoded scope set") costs almost nothing to write.
type Principal interface {
	// Subject returns an identifier for logging/auditing - a username,
	// token ID, service account name, whatever the Authenticator
	// considers meaningful. Not used for any authorization decision
	// itself; HasScope is.
	Subject() string

	// HasScope reports whether this Principal is authorized for scope.
	HasScope(scope Scope) bool
}

// ErrUnauthenticated is returned by Authenticate (via errors.Is) when the
// request carries no credential, or one the Authenticator cannot
// validate at all (wrong token, malformed header, expired token, etc.).
// Callers map this to HTTP 401.
var ErrUnauthenticated = errors.New("auth: request is not authenticated")

// ErrForbidden is returned by Authenticate (via errors.Is) when the
// request's credential is valid but the resulting Principal lacks a
// scope the caller is about to check for. In practice this project's
// Authenticator implementations return a Principal from Authenticate and
// let the caller check HasScope itself (see casserver/hubserver's
// requireScope helper) - ErrForbidden exists for a custom Authenticator
// that prefers to reject at authentication time instead, and for callers
// that want one consistent sentinel for "authenticated but not allowed"
// regardless of which layer decided that.
var ErrForbidden = errors.New("auth: principal lacks required scope")

// Authenticator validates an incoming HTTP request's credential and
// returns the Principal it maps to. Implementations should extract
// whatever credential their scheme uses (a bearer token, a client
// certificate, a signed cookie, ...) from r and validate it - r must not
// be mutated.
//
// A request with no credential at all is not a special case an
// Authenticator needs to detect separately: reading a missing
// Authorization header is indistinguishable from reading an empty one,
// and both should simply fail validation and return ErrUnauthenticated
// like any other invalid credential would.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// NoAuth is the default Authenticator: every request succeeds, and the
// returned Principal holds every scope unconditionally. This is exactly
// this project's pre-v0.8.0 behavior (no auth enforcement at all) - both
// casserver.Server and hubserver.Server use this by default until
// SetAuthenticator is called with something else, so existing code that
// never touches the new auth APIs is completely unaffected.
type NoAuth struct{}

func (NoAuth) Authenticate(*http.Request) (Principal, error) {
	return allScopesPrincipal{}, nil
}

type allScopesPrincipal struct{}

func (allScopesPrincipal) Subject() string     { return "anonymous" }
func (allScopesPrincipal) HasScope(Scope) bool { return true }
