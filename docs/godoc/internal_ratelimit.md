# `xet-server/internal/ratelimit`

```
package ratelimit // import "xet-server/internal/ratelimit"

Package ratelimit implements a hand-rolled per-source-IP token-bucket rate
limiter, used to blunt a single client hammering the expensive upload endpoints
(chunk decompression + hashing) without needing a new dependency — a token
bucket is simple enough to write directly and keeps the rest of this project's
zero-external-dependency posture for the main module.

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

func (l *Limiter) Middleware(next http.Handler) http.Handler
    Middleware wraps next so that requests exceeding the per-source-IP rate get
    a 429 Too Many Requests with a Retry-After header instead of reaching next.
    Rejections log at Debug, not Warn: a client retrying after backing off is
    expected, well-behaved behavior under this limiter — not a server-side fault
    worth surfacing louder (matches the 4xx-is-Debug convention used elsewhere
    in this codebase for client-caused responses).

func (l *Limiter) RetryAfterSeconds(key string) int
    RetryAfterSeconds estimates how many seconds until key's bucket has at least
    one token again, for a 429 response's Retry-After header. Not exact under
    concurrent access (another request could consume the next token first),
    but close enough to give a well-behaved client a reasonable backoff hint.
```
