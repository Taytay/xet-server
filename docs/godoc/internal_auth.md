# `xet-server/internal/auth`

```
package auth // import "xet-server/internal/auth"

Package auth defines this project's authentication/authorization interfaces,
kept deliberately small so a third party can implement a custom backend (JWT,
OAuth, mTLS, an internal SSO integration, whatever a real deployment needs)
without touching casserver, hubserver, or the cmd/xet client at all — implement
Authenticator (server side) and/or CredentialHelper (client side), and both
sides of this project work with it automatically.

This mirrors the real xet-core/hf_xet protocol's actual auth model (confirmed
against xet-core's own openapi/cas.openapi.yaml and its client-side
xet_client::common::auth module): Authorization: Bearer <token>, with tokens
carrying scopes (read, write) — 401 for a missing or invalid token, 403 for a
valid token lacking the scope a given endpoint requires. See docs/PROTOCOL.md
for the full writeup.

The zero-configuration default on both sides (NoAuth, NoopCredentialHelper) is
byte-for-byte today's pre-v0.8.0 behavior: no token is required, and none is
sent. Passing -auth-token to xetd or xet is what opts into enforcement/sending
a credential — existing deployments and scripts that never pass that flag see no
behavior change at all.

VARIABLES

var ErrForbidden = errors.New("auth: principal lacks required scope")
    ErrForbidden is returned by Authenticate (via errors.Is) when the request's
    credential is valid but the resulting Principal lacks a scope the caller is
    about to check for. In practice this project's Authenticator implementations
    return a Principal from Authenticate and let the caller check HasScope
    itself (see casserver/hubserver's requireScope helper) — ErrForbidden
    exists for a custom Authenticator that prefers to reject at authentication
    time instead, and for callers that want one consistent sentinel for
    "authenticated but not allowed" regardless of which layer decided that.

var ErrUnauthenticated = errors.New("auth: request is not authenticated")
    ErrUnauthenticated is returned by Authenticate (via errors.Is) when the
    request carries no credential, or one the Authenticator cannot validate at
    all (wrong token, malformed header, expired token, etc.). Callers map this
    to HTTP 401.


FUNCTIONS

func BearerToken(r *http.Request) string
    BearerToken extracts the token from an incoming request's "Authorization:
    Bearer <token>" header (case-insensitive scheme name per RFC 6750), or "" if
    the header is absent or not in that exact form. Exported (StaticTokenAuth
    uses the same logic internally) so a relay/proxy Authenticator can pull a
    caller's credential back out of a request it just authenticated, in order
    to forward that exact credential upstream via a CredentialHelper — the
    chaining primitive a passthrough proxy needs, as opposed to resolving its
    own independent, unrelated upstream secret.

func ResolveToken(flagValue string, envVars ...string) string
    ResolveToken returns flagValue if it was explicitly set to something other
    than empty or the sentinel "None" (this project's convention, used by every
    -auth-token flag, for "not passed on the command line"), otherwise returns
    the first non-empty value found among envVars, checked in the order given,
    otherwise returns flagValue unchanged (i.e. "None" or "").

    An explicit flag always wins over the environment, so an operator can still
    override a machine-wide/session-wide environment variable per invocation;
    the environment is checked before falling back to the flag's own zero
    value because a flag value is visible to any other local user via `ps
    -ef`/`/proc/<pid>/cmdline` and gets written to shell history, while an
    environment variable set through a secrets manager, a `.env` file kept out
    of history, or `read -s` is not.

    This is the one call shape every binary in this project uses to resolve
    its token: xetd calls auth.ResolveToken(*authToken, "XETD_AUTH_TOKEN",
    "HF_TOKEN") so its shared secret can fall back to the same $HF_TOKEN a user
    already has exported for the real `hf` CLI — a local xetd then "just works"
    as a drop-in replacement without configuring a second, separate secret.
    xet calls auth.ResolveToken(authToken, "XET_AUTH_TOKEN", "HF_TOKEN") for
    the same reason on the client side. A future relay/proxy (e.g. one that
    authenticates local callers and forwards their credential to the real
    huggingface.co Xet backend) resolves its own upstream credential with the
    identical call, just a different env var chain — no new primitive needed to
    support that case.


TYPES

type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}
    Authenticator validates an incoming HTTP request's credential and returns
    the Principal it maps to. Implementations should extract whatever credential
    their scheme uses (a bearer token, a client certificate, a signed cookie,
    ...) from r and validate it — r must not be mutated.

    A request with no credential at all is not a special case an Authenticator
    needs to detect separately: reading a missing Authorization header is
    indistinguishable from reading an empty one, and both should simply fail
    validation and return ErrUnauthenticated like any other invalid credential
    would.

type BearerCredentialHelper struct {
	// Has unexported fields.
}
    BearerCredentialHelper attaches "Authorization: Bearer <token>" to every
    request — the client-side counterpart to StaticTokenAuth.

func NewBearerCredentialHelper(token string) *BearerCredentialHelper
    NewBearerCredentialHelper constructs a BearerCredentialHelper sending token
    on every request it fills.

func (c *BearerCredentialHelper) FillCredential(req *http.Request) error

func (c *BearerCredentialHelper) WhoAmI() string

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
    CredentialHelper is this project's client-side mirror of Authenticator:
    something that attaches whatever credential a server-side Authenticator
    expects to an outgoing request. Kept to exactly two methods, mirroring real
    xet-core's own xet_client::common::auth::CredentialHelper trait (confirmed
    against its source — see docs/PROTOCOL.md), so implementing a custom one
    (e.g. to fetch a short-lived token from an internal service before each
    request) is a small, self-contained task.

type NoAuth struct{}
    NoAuth is the default Authenticator: every request succeeds, and the
    returned Principal holds every scope unconditionally. This is exactly
    this project's pre-v0.8.0 behavior (no auth enforcement at all) —
    both casserver.Server and hubserver.Server use this by default until
    SetAuthenticator is called with something else, so existing code that never
    touches the new auth APIs is completely unaffected.

func (NoAuth) Authenticate(*http.Request) (Principal, error)

type NoopCredentialHelper struct{}
    NoopCredentialHelper attaches nothing. This is the default on the client
    side (cmd/xet), matching NoAuth on the server side — this project's
    pre-v0.8.0 behavior, unaffected unless -auth-token is explicitly passed.

func (NoopCredentialHelper) FillCredential(*http.Request) error

func (NoopCredentialHelper) WhoAmI() string

type Principal interface {
	// Subject returns an identifier for logging/auditing — a username,
	// token ID, service account name, whatever the Authenticator
	// considers meaningful. Not used for any authorization decision
	// itself; HasScope is.
	Subject() string

	// HasScope reports whether this Principal is authorized for scope.
	HasScope(scope Scope) bool
}
    Principal is the authenticated identity/permission set an Authenticator
    hands back for a request that passed authentication. Kept to exactly two
    methods so a minimal custom implementation (e.g. "this token maps to this
    one hardcoded scope set") costs almost nothing to write.

type Scope string
    Scope identifies a capability a Principal may or may not hold. Kept as
    a plain string type (not a closed enum) so a custom Authenticator can
    introduce its own scopes beyond ScopeRead/ScopeWrite if its endpoints need
    finer-grained authorization than this project's built-in servers require.

const (
	// ScopeRead gates every read-only CAS/Hub endpoint (reconstruction,
	// xorb fetch, chunk-dedup lookup, resolve) — mirrors the real
	// protocol's `read` scope.
	ScopeRead Scope = "read"
	// ScopeWrite gates every mutating CAS/Hub endpoint (xorb/shard
	// upload, commit, repo create) — mirrors the real protocol's `write`
	// scope.
	ScopeWrite Scope = "write"
)
type StaticTokenAuth struct {
	// Has unexported fields.
}
    StaticTokenAuth is a single shared-secret Authenticator: exactly one bearer
    token, set at construction, grants full access (every scope) to any request
    presenting it; any other token, or none, fails with ErrUnauthenticated.
    This deliberately does not distinguish per-user identity or partial scopes
    — it's a single gate for the whole server, matching how this project's
    -auth-token flag is meant to be used (an operator-chosen shared secret,
    not a multi-tenant credential system). A deployment that needs real per-user
    scopes should implement its own Authenticator against whatever identity
    provider it already has; this type is intentionally the simplest possible
    non-trivial option, not the only one.

func NewStaticTokenAuth(token string) *StaticTokenAuth
    NewStaticTokenAuth constructs a StaticTokenAuth requiring exactly token.
    token must be non-empty — constructing this with an empty string would make
    every bearer-auth request (even one presenting an empty token, which some
    malformed clients do) succeed, defeating the point; use NoAuth{} directly if
    no enforcement is wanted.

func (a *StaticTokenAuth) Authenticate(r *http.Request) (Principal, error)
```
