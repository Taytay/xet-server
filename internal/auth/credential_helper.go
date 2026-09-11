package auth

import "net/http"

// CredentialHelper is this project's client-side mirror of Authenticator:
// something that attaches whatever credential a server-side Authenticator
// expects to an outgoing request. Kept to exactly two methods, mirroring
// real xet-core's own xet_client::common::auth::CredentialHelper trait
// (confirmed against its source — see docs/PROTOCOL.md), so implementing
// a custom one (e.g. to fetch a short-lived token from an internal
// service before each request) is a small, self-contained task.
type CredentialHelper interface {
	// FillCredential attaches a credential to req in place (e.g. setting
	// an Authorization header) before it's sent. Returning an error
	// aborts the request — a helper that needs to fetch a token from
	// elsewhere first should report a failure to do so this way, rather
	// than silently sending an unauthenticated request.
	FillCredential(req *http.Request) error

	// WhoAmI returns a short identifier for logging (which credential
	// helper is active, not the identity it authenticates as — a bearer
	// helper doesn't necessarily know who its token belongs to).
	WhoAmI() string
}

// NoopCredentialHelper attaches nothing. This is the default on the
// client side (cmd/xet), matching NoAuth on the server side — this
// project's pre-v0.8.0 behavior, unaffected unless -auth-token is
// explicitly passed.
type NoopCredentialHelper struct{}

func (NoopCredentialHelper) FillCredential(*http.Request) error { return nil }
func (NoopCredentialHelper) WhoAmI() string                     { return "noop" }

// BearerCredentialHelper attaches "Authorization: Bearer <token>" to
// every request — the client-side counterpart to StaticTokenAuth.
type BearerCredentialHelper struct {
	token string
}

// NewBearerCredentialHelper constructs a BearerCredentialHelper sending
// token on every request it fills.
func NewBearerCredentialHelper(token string) *BearerCredentialHelper {
	return &BearerCredentialHelper{token: token}
}

func (c *BearerCredentialHelper) FillCredential(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	return nil
}

func (c *BearerCredentialHelper) WhoAmI() string { return "bearer" }

// CredentialFromRequest returns a CredentialHelper that forwards r's own
// "Authorization: Bearer <token>" header unchanged on an outgoing
// request (NoopCredentialHelper if r carries none) — the shared building
// block a relay/proxy (internal/proxycas, internal/proxyhub) uses to
// implement pure credential passthrough: whatever bearer token the
// calling client sent, forwarded upstream exactly as received, never
// replaced by a secret the proxy layer itself holds.
func CredentialFromRequest(r *http.Request) CredentialHelper {
	token := bearerToken(r)
	if token == "" {
		return NoopCredentialHelper{}
	}
	return NewBearerCredentialHelper(token)
}
