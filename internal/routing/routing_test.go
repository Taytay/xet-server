package routing

// Tests for Mount/MountWithVersion/Apply: the version-prefix declarative
// route table helpers shared by casserver, hubserver, internal/api, and
// cmd/xetd's own top-level mux.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func handlerNamed(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(name)) })
}

func TestMount_WithMethodProducesMethodPrefixedPattern(t *testing.T) {
	mux := http.NewServeMux()
	Apply(mux, []Route{Mount("GET", "/stats", handlerNamed("stats"))})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/stats", nil))
	if got := w.Body.String(); got != "stats" {
		t.Errorf("GET /stats -> %q, want %q", got, "stats")
	}

	// A different method on the same path must NOT match — Mount with an
	// explicit method must actually constrain by method, not just path.
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest("POST", "/stats", nil))
	if w2.Code == http.StatusOK {
		t.Errorf("POST /stats matched a GET-only route, want no match")
	}
}

func TestMount_EmptyMethodMatchesAnyMethod(t *testing.T) {
	mux := http.NewServeMux()
	Apply(mux, []Route{Mount("", "/anything", handlerNamed("any"))})

	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, "/anything", nil))
		if got := w.Body.String(); got != "any" {
			t.Errorf("%s /anything -> %q, want %q", method, got, "any")
		}
	}
}

func TestMountWithVersion_PrefixesPath(t *testing.T) {
	mux := http.NewServeMux()
	Apply(mux, []Route{MountWithVersion("/v1", "GET", "/stats", handlerNamed("v1-stats"))})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/stats", nil))
	if got := w.Body.String(); got != "v1-stats" {
		t.Errorf("GET /v1/stats -> %q, want %q", got, "v1-stats")
	}

	// The un-prefixed path must not match at all.
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest("GET", "/stats", nil))
	if w2.Code == http.StatusOK {
		t.Error("GET /stats matched a /v1-prefixed-only route, want no match")
	}
}

func TestApply_MultipleRoutesRegisterIndependently(t *testing.T) {
	mux := http.NewServeMux()
	Apply(mux, []Route{
		MountWithVersion("/v1", "POST", "/upload", handlerNamed("upload")),
		MountWithVersion("/v1", "GET", "/files/{id}", handlerNamed("files")),
		MountWithVersion("/v1", "GET", "/stats", handlerNamed("stats")),
		Mount("", "/v1/", handlerNamed("catchall")),
	})

	cases := []struct{ method, path, want string }{
		{"POST", "/v1/upload", "upload"},
		{"GET", "/v1/files/abc", "files"},
		{"GET", "/v1/stats", "stats"},
		{"GET", "/v1/something-else", "catchall"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if got := w.Body.String(); got != tc.want {
			t.Errorf("%s %s -> %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestApply_ColliderPatternPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("Apply() with two identical patterns did not panic, want a panic (matching mux.Handle's own behavior)")
		}
	}()
	mux := http.NewServeMux()
	Apply(mux, []Route{
		Mount("GET", "/dup", handlerNamed("first")),
		Mount("GET", "/dup", handlerNamed("second")),
	})
}
