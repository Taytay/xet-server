package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// withClock swaps in a controllable clock for deterministic tests, since
// Allow's refill math is time-based and real wall-clock sleeps would make
// this test slow and flaky.
func withClock(l *Limiter, start time.Time) *fakeClock {
	fc := &fakeClock{t: start}
	l.now = fc.Now
	return fc
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func TestAllow_BurstThenBlocked(t *testing.T) {
	l := New(3, 1) // burst 3, refill 1/sec
	withClock(l, time.Now())

	for i := 0; i < 3; i++ {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("Allow() call %d = false, want true within burst", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("Allow() = true after burst exhausted, want false")
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	l := New(1, 1) // burst 1, refill 1/sec
	clock := withClock(l, time.Now())

	if !l.Allow("1.2.3.4") {
		t.Fatal("first Allow() = false, want true")
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("second Allow() immediately after = true, want false (no refill yet)")
	}

	clock.Advance(1100 * time.Millisecond)
	if !l.Allow("1.2.3.4") {
		t.Fatal("Allow() after refill interval = false, want true")
	}
}

func TestAllow_PerKeyIndependent(t *testing.T) {
	l := New(1, 1)
	withClock(l, time.Now())

	if !l.Allow("1.1.1.1") {
		t.Fatal("first key's Allow() = false, want true")
	}
	if !l.Allow("2.2.2.2") {
		t.Fatal("second key's Allow() = false, want true (independent bucket)")
	}
	if l.Allow("1.1.1.1") {
		t.Fatal("first key's second Allow() = true, want false (exhausted)")
	}
}

func TestAllow_NeverExceedsBurstCap(t *testing.T) {
	l := New(2, 100) // fast refill
	clock := withClock(l, time.Now())

	l.Allow("k")
	l.Allow("k")
	clock.Advance(time.Hour) // plenty of time to overflow tokens if uncapped

	allowed := 0
	for i := 0; i < 10; i++ {
		if l.Allow("k") {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("allowed %d requests after long idle, want exactly burst (2), got token accumulation beyond cap", allowed)
	}
}

func TestMiddleware_Returns429WithRetryAfter(t *testing.T) {
	l := New(1, 1)
	withClock(l, time.Now())

	called := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++ })
	handler := l.Middleware(next)

	req := httptest.NewRequest("POST", "/v1/xorbs/default/abc", nil)
	req.RemoteAddr = "5.6.7.8:12345"

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
	if called != 1 {
		t.Errorf("next handler called %d times, want exactly 1 (second request must not reach it)", called)
	}
}

func TestMiddleware_DifferentSourceIPsIndependent(t *testing.T) {
	l := New(1, 1)
	withClock(l, time.Now())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := l.Middleware(next)

	for _, addr := range []string{"1.1.1.1:1", "2.2.2.2:2", "3.3.3.3:3"} {
		req := httptest.NewRequest("POST", "/v1/xorbs/default/abc", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("first request from %s status = %d, want 200", addr, rec.Code)
		}
	}
}

func TestSourceIP_FallsBackToRawRemoteAddrWithoutPort(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "not-a-host-port"
	if got := sourceIP(req); got != "not-a-host-port" {
		t.Errorf("sourceIP() = %q, want raw RemoteAddr as fallback", got)
	}
}
