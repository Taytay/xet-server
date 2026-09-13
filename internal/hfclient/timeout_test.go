package hfclient

// Tests for defaultHTTPClient's bounded-execution guarantee: a hung or
// unresponsive upstream must fail the request within a bounded time,
// not hold the calling goroutine open indefinitely. This class of
// failure was never exercised by this package's other tests (all driven
// against a real, responsive httptest.Server with zero artificial
// latency) - see docs/PROTOCOL.md or the v0.9.0 CHANGELOG entry on why
// this needed its own dedicated coverage.
//
// These tests use a short custom timeout (not the real 10s default) so
// the suite stays fast; they exercise the identical code path
// (http.Transport's ResponseHeaderTimeout) defaultHTTPClient relies on.

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// hangingListener accepts a connection and then never writes a response
// - simulating a completely unresponsive upstream (a hung real
// huggingface.co, a network partition that drops responses but not the
// TCP handshake, etc.) more realistically than closing the connection
// outright, which most HTTP clients handle very differently (an
// immediate connection-reset error, not a hang).
func newHangingServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept the connection (completing the TCP handshake) but
			// never read or write anything - the request's headers are
			// never even fully answered, let alone a response body sent.
			go func() {
				<-done
				conn.Close()
			}()
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		ln.Close()
	}
}

func TestDefaultHTTPClient_ResponseHeaderTimeoutBoundsHungUpstream(t *testing.T) {
	addr, cleanup := newHangingServer(t)
	defer cleanup()

	// A short timeout standing in for defaultHTTPClient's real 10s
	// bound - same Transport field, same mechanism, just fast enough for
	// a unit test.
	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 200 * time.Millisecond,
		},
	}

	start := time.Now()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/v1/xorbs/default/deadbeef", nil)
	_, err := client.Do(req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a hung upstream, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("request took %v to fail, want well under 2s (ResponseHeaderTimeout must bound this)", elapsed)
	}
}

func TestNew_UsesDefaultHTTPClientWithBoundedTransport(t *testing.T) {
	c := New("http://example.invalid")
	if c.HTTP != defaultHTTPClient {
		t.Error("New() did not wire the bounded defaultHTTPClient")
	}
	transport, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatal("defaultHTTPClient.Transport is not *http.Transport")
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Error("defaultHTTPClient.Transport.ResponseHeaderTimeout is unset - a hung upstream would block forever")
	}
}

func TestNewCASClient_UsesDefaultHTTPClientWithBoundedTransport(t *testing.T) {
	c := NewCASClient("http://example.invalid")
	if c.HTTP != defaultHTTPClient {
		t.Error("NewCASClient() did not wire the bounded defaultHTTPClient")
	}
}
