# `github.com/guilt/xet-server/internal/ratelimit`

```
package ratelimit // import "github.com/guilt/xet-server/internal/ratelimit"

Package ratelimit implements a hand-rolled per-source-IP token-bucket rate
limiter, used to blunt a single client hammering an expensive endpoint without
needing a new dependency - a token bucket is simple enough to write directly and
keeps the rest of this project's zero-external-dependency posture for the main
module.

Scope varies by caller: casserver.Server.SetUploadRateLimiter gates only
its upload endpoints (uploads are the expensive local operation there:
chunk decompression + hashing), while proxycas.Server. SetRateLimiter and
proxyhub.Server.SetRateLimiter both gate EVERY route - for a caching proxy,
a read that misses cache costs a real outbound call to the real upstream,
not just a write.

TYPES

type Limiter struct {
	Burst           float64
	RefillPerSecond float64

	// Has unexported fields.
}
    Limiter enforces a per-source-IP token bucket. The zero value is not usable;
    construct with New.

func New(burst, refillPerSecond float64) *Limiter
    New creates a Limiter allowing burst requests immediately per source IP,
    refilling at refillPerSecond tokens/second thereafter (fractional rates are
    fine, e.g. 0.5 for one request every two seconds).

func (l *Limiter) Allow(key string) bool
    Allow reports whether a request from key (typically a source IP) may proceed
    right now, consuming one token if so.

func (l *Limiter) AllowRequest(r *http.Request) bool
    AllowRequest reports whether r's source IP may proceed right now (consuming
    one token if so) - the same check Middleware applies inline, exposed for a
    caller (proxyhub.Server.gate) that needs to gate a request without wrapping
    it in an http.Handler.

func (l *Limiter) Middleware(next http.Handler) http.Handler
    Middleware wraps next so that requests exceeding the per-source-IP rate get
    a 429 Too Many Requests with a Retry-After header instead of reaching next.
    Rejections log at Debug, not Warn: a client retrying after backing off is
    expected, well-behaved behavior under this limiter - not a server-side fault
    worth surfacing louder (matches the 4xx-is-Debug convention used elsewhere
    in this codebase for client-caused responses).

func (l *Limiter) RetryAfterSeconds(key string) int
    RetryAfterSeconds estimates how many seconds until key's bucket has at least
    one token again, for a 429 response's Retry-After header. Not exact under
    concurrent access (another request could consume the next token first),
    but close enough to give a well-behaved client a reasonable backoff hint.

func (l *Limiter) RetryAfterSecondsForRequest(r *http.Request) int
    RetryAfterSecondsForRequest is RetryAfterSeconds keyed by r's own source IP
    - see AllowRequest's doc comment for why this exists alongside Middleware.
```
