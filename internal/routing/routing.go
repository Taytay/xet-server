// Package routing provides two small helpers — Mount/MountWithVersion —
// so an HTTP server's route table can be built as one declarative list
// (each entry produced by Mount or MountWithVersion) and registered onto
// its *http.ServeMux in a single Apply call, instead of a long sequence
// of individual mux.Handle/HandleFunc statements repeating the same
// version prefix at every call site.
package routing

import "net/http"

// Route describes one registration: an HTTP method (empty means "any
// method", matching plain mux.Handle(pattern, handler) semantics — no
// leading "METHOD " in the pattern) plus a URL pattern and the handler to
// serve it.
type Route struct {
	Method  string
	Path    string
	Handler http.Handler
}

// Mount describes a route to be registered on some mux via Apply,
// equivalent to mux.Handle(method+" "+path, handler) when method is
// non-empty, or mux.Handle(path, handler) (matching any method) when it
// is empty.
func Mount(method, path string, handler http.Handler) Route {
	return Route{Method: method, Path: path, Handler: handler}
}

// MountWithVersion is Mount with path prefixed by version — e.g.
// MountWithVersion("/v1", "GET", "/stats", h) registers "GET /v1/stats".
// version is a plain string (not a named type) so a package can share
// one constant across every route it registers, e.g.:
//
//	const v1 = "/v1"
//	routing.MountWithVersion(v1, "GET", "/stats", h)
//	routing.MountWithVersion(v1, "POST", "/upload", h)
//
// instead of repeating the concatenation "/v1"+"/stats" at each call
// site, or a version bump requiring an edit at every route instead of one
// constant.
func MountWithVersion(version, method, path string, handler http.Handler) Route {
	return Mount(method, version+path, handler)
}

// Apply registers every route in routes onto mux, in order. Panics if two
// routes collide on the same method+path — the same failure mode as
// calling mux.Handle twice with an identical pattern, just surfaced here
// instead of at an individual call site.
func Apply(mux *http.ServeMux, routes []Route) {
	for _, r := range routes {
		pattern := r.Path
		if r.Method != "" {
			pattern = r.Method + " " + pattern
		}
		mux.Handle(pattern, r.Handler)
	}
}
