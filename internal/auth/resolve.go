package auth

// resolve.go: the token-resolution primitive shared by every binary in
// this project (xetd, xet, and any future relay/proxy) that needs to turn
// a -auth-token-style flag plus a prioritized list of environment
// variables into one token — and the primitive a relay/proxy needs to
// pull a caller's credential back out of an incoming request in order to
// forward it upstream unchanged (credential chaining, not just two
// independently-configured secrets).

import (
	"net/http"
	"os"
)

// ResolveToken returns flagValue if it was explicitly set to something
// other than empty or the sentinel "None" (this project's convention,
// used by every -auth-token flag, for "not passed on the command line"),
// otherwise returns the first non-empty value found among envVars, checked
// in the order given, otherwise returns flagValue unchanged (i.e. "None"
// or "").
//
// An explicit flag always wins over the environment, so an operator can
// still override a machine-wide/session-wide environment variable per
// invocation; the environment is checked before falling back to the
// flag's own zero value because a flag value is visible to any other
// local user via `ps -ef`/`/proc/<pid>/cmdline` and gets written to shell
// history, while an environment variable set through a secrets manager,
// a `.env` file kept out of history, or `read -s` is not.
//
// This is the one call shape every binary in this project uses to resolve
// its token: xetd calls auth.ResolveToken(*authToken, "XETD_AUTH_TOKEN",
// "HF_TOKEN") so its shared secret can fall back to the same $HF_TOKEN a
// user already has exported for the real `hf` CLI — a local xetd then
// "just works" as a drop-in replacement without configuring a second,
// separate secret. xet calls auth.ResolveToken(authToken, "XET_AUTH_TOKEN",
// "HF_TOKEN") for the same reason on the client side. A future relay/proxy
// (e.g. one that authenticates local callers and forwards their credential
// to the real huggingface.co Xet backend) resolves its own upstream
// credential with the identical call, just a different env var chain —
// no new primitive needed to support that case.
func ResolveToken(flagValue string, envVars ...string) string {
	if flagValue != "" && flagValue != "None" {
		return flagValue
	}
	for _, envVar := range envVars {
		if v := os.Getenv(envVar); v != "" {
			return v
		}
	}
	return flagValue
}

// BearerToken extracts the token from an incoming request's
// "Authorization: Bearer <token>" header (case-insensitive scheme name per
// RFC 6750), or "" if the header is absent or not in that exact form.
// Exported (StaticTokenAuth uses the same logic internally) so a
// relay/proxy Authenticator can pull a caller's credential back out of a
// request it just authenticated, in order to forward that exact credential
// upstream via a CredentialHelper — the chaining primitive a passthrough
// proxy needs, as opposed to resolving its own independent, unrelated
// upstream secret.
func BearerToken(r *http.Request) string {
	return bearerToken(r)
}
