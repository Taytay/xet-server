package main

// Regression test for withLandingPage: it must intercept ONLY an exact
// "GET /" request and forward every other method/path unchanged.
// http.ServeMux's own behavior of routing an unmatched HEAD to a
// registered GET handler is exactly the trap this avoids - a mux-based
// version of this wiring was tried and reverted after this test caught it
// swallowing "HEAD /{repo}/resolve/{revision}/{filename}", hubserver's own
// real traffic, into the landing page instead.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithLandingPage_OnlyInterceptsExactGETRoot(t *testing.T) {
	var landingCalled, nextCalled bool
	landing := func(w http.ResponseWriter, r *http.Request) {
		landingCalled = true
		w.Write([]byte("landing"))
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.Write([]byte("next"))
	})
	h := withLandingPage(next, landing)

	cases := []struct {
		name         string
		method, path string
		wantLanding  bool
	}{
		{"GET root goes to landing", http.MethodGet, "/", true},
		{"HEAD root must NOT fall back to landing", http.MethodHead, "/", false},
		{"HEAD resolve path (real hubserver traffic)", http.MethodHead, "/alice/model/resolve/main/f.bin", false},
		{"GET api path", http.MethodGet, "/api/repos/create", false},
		{"POST root", http.MethodPost, "/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			landingCalled, nextCalled = false, false
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
			if landingCalled != tc.wantLanding || nextCalled == tc.wantLanding {
				t.Errorf("landingCalled=%v nextCalled=%v, want landingCalled=%v", landingCalled, nextCalled, tc.wantLanding)
			}
		})
	}
}
