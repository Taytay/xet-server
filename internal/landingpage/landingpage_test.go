package landingpage

// Tests for the two landing page handlers: correct rendering, correct
// root-only scoping (anything other than "/" must 404, not render the
// page, since these are mounted with mux.HandleFunc("/", ...) and would
// otherwise swallow every unmatched path).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCASHandler_RendersExpectedContent(t *testing.T) {
	h := CASHandler(":8420", ":8421")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := w.Body.String()
	for _, want := range []string{"/v1/xorbs/{prefix}/{hash}", "/v1/upload", "/api-docs/", ":8421"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing expected content %q", want)
		}
	}
}

func TestCASHandler_OmitsHubNoteWhenHubAddrEmpty(t *testing.T) {
	h := CASHandler(":8420", "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if strings.Contains(w.Body.String(), "Hub API shim is also running") {
		t.Error("body mentions a Hub shim even though hubAddr was empty")
	}
}

func TestCASHandler_NonRootPath404s(t *testing.T) {
	h := CASHandler(":8420", "")
	req := httptest.NewRequest(http.MethodGet, "/something-else", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a non-root path", w.Code)
	}
}

func TestHubHandler_RendersExpectedContent(t *testing.T) {
	h := HubHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"/api/repos/create", "revision/{revision}", "tree/{revision}", "branch/{branch}", "HF_ENDPOINT", "resolve/{revision}/{filename}"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing expected content %q", want)
		}
	}
	// Swagger UI isn't mounted on the Hub shim's own port.
	if strings.Contains(body, "/api-docs/") {
		t.Error("Hub landing page links to /api-docs/, which isn't mounted on this port")
	}
}

func TestHubHandler_NonRootPath404s(t *testing.T) {
	h := HubHandler()
	req := httptest.NewRequest(http.MethodGet, "/alice/model/resolve/main/f.bin", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a non-root path", w.Code)
	}
}
