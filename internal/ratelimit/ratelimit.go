// Package ratelimit implements a hand-rolled per-source-IP token-bucket
// rate limiter, used to blunt a single client hammering the expensive
// upload endpoints (chunk decompression + hashing) without needing a new
// dependency — a token bucket is simple enough to write directly and
// keeps the rest of this project's zero-external-dependency posture for
// the main module.
package ratelimit

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// bucket is one source IP's token bucket: it holds up to Limiter.Burst
// tokens, refilling at Limiter.RefillPerSecond tokens/second, capped at
// Burst. Refill is computed lazily on each Allow call from elapsed wall
// time rather than a background goroutine per bucket, so an idle bucket
// costs nothing between requests.
type bucket struct {
	mu       sync.Mutex
	tokens   float64
	lastSeen time.Time
}

// Limiter enforces a per-source-IP token bucket. The zero value is not
// usable; construct with New.
type Limiter struct {
	Burst           float64
	RefillPerSecond float64

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // overridable for tests; defaults to time.Now
}

// New creates a Limiter allowing burst requests immediately per source
// IP, refilling at refillPerSecond tokens/second thereafter (fractional
// rates are fine, e.g. 0.5 for one request every two seconds).
func New(burst, refillPerSecond float64) *Limiter {
	return &Limiter{
		Burst:           burst,
		RefillPerSecond: refillPerSecond,
		buckets:         make(map[string]*bucket),
		now:             time.Now,
	}
}

// Allow reports whether a request from key (typically a source IP) may
// proceed right now, consuming one token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.Burst, lastSeen: l.now()}
		l.buckets[key] = b
	}
	l.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()

	now := l.now()
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now
	b.tokens += elapsed * l.RefillPerSecond
	if b.tokens > l.Burst {
		b.tokens = l.Burst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RetryAfterSeconds estimates how many seconds until key's bucket has at
// least one token again, for a 429 response's Retry-After header. Not
// exact under concurrent access (another request could consume the next
// token first), but close enough to give a well-behaved client a
// reasonable backoff hint.
func (l *Limiter) RetryAfterSeconds(key string) int {
	l.mu.Lock()
	b, ok := l.buckets[key]
	l.mu.Unlock()
	if !ok || l.RefillPerSecond <= 0 {
		return 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	deficit := 1 - b.tokens
	if deficit <= 0 {
		return 0
	}
	seconds := deficit / l.RefillPerSecond
	if seconds < 1 {
		return 1
	}
	return int(seconds) + 1
}

// Middleware wraps next so that requests exceeding the per-source-IP rate
// get a 429 Too Many Requests with a Retry-After header instead of
// reaching next. Rejections log at Debug, not Warn: a client retrying
// after backing off is expected, well-behaved behavior under this limiter
// — not a server-side fault worth surfacing louder (matches the 4xx-is-Debug
// convention used elsewhere in this codebase for client-caused responses).
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := sourceIP(r)
		if !l.Allow(key) {
			w.Header().Set("Retry-After", strconv.Itoa(l.RetryAfterSeconds(key)))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sourceIP extracts the request's source IP, stripping the port
// RemoteAddr normally carries. Falls back to the raw RemoteAddr string if
// it isn't in host:port form (e.g. a unit test's httptest transport).
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
