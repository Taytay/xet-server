package hfclient

// Error-path tests: credential-fill failures, malformed JSON responses,
// and StatusError.Error()'s two message formats. Closes the coverage
// gaps left by the happy-path tests in hfclient_test.go/cas_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// failingCredentialHelper always errors, to exercise the "fill credential"
// error path every do()/casDo() call site has.
type failingCredentialHelper struct{}

func (failingCredentialHelper) FillCredential(*http.Request) error {
	return errors.New("simulated credential failure")
}
func (failingCredentialHelper) WhoAmI() string { return "failing" }

func TestRepoInfo_CredentialFailurePropagates(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when FillCredential fails")
	})
	_, err := c.RepoInfo(context.Background(), failingCredentialHelper{}, "model", "a/b", "main")
	if err == nil || !strings.Contains(err.Error(), "simulated credential failure") {
		t.Errorf("error = %v, want it to wrap the credential failure", err)
	}
}

func TestFetchXorb_CredentialFailurePropagates(t *testing.T) {
	c := newTestCASClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when FillCredential fails")
	})
	_, err := c.FetchXorb(context.Background(), failingCredentialHelper{}, "default", "abc", "")
	if err == nil || !strings.Contains(err.Error(), "simulated credential failure") {
		t.Errorf("error = %v, want it to wrap the credential failure", err)
	}
}

func TestRepoInfo_MalformedJSONReturnsDecodeError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	_, err := c.RepoInfo(context.Background(), nil, "model", "a/b", "main")
	if err == nil || !strings.Contains(err.Error(), "decode repo info") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func TestListTree_MalformedJSONReturnsDecodeError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	_, err := c.ListTree(context.Background(), nil, "model", "a/b", "main")
	if err == nil || !strings.Contains(err.Error(), "decode tree page") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func TestGetXetToken_MalformedJSONReturnsDecodeError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	_, err := c.GetXetToken(context.Background(), nil, "model", "a/b", "main", XetTokenRead)
	if err == nil || !strings.Contains(err.Error(), "decode xet token") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func TestCommit_MalformedJSONReturnsDecodeError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	_, err := c.Commit(context.Background(), nil, "model", "a/b", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "decode commit result") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func TestPreupload_MalformedJSONReturnsDecodeError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	})
	_, err := c.Preupload(context.Background(), nil, "model", "a/b", "main", nil)
	if err == nil || !strings.Contains(err.Error(), "decode preupload result") {
		t.Errorf("error = %v, want a decode error", err)
	}
}

func TestCreateRepo_NonOKStatusReturnsStatusError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	})
	err := c.CreateRepo(context.Background(), nil, "my-model", "alice", "model")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusInternalServerError {
		t.Errorf("error = %v, want a *StatusError with status 500", err)
	}
}

func TestCreateBranch_NonOKStatusReturnsStatusError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	err := c.CreateBranch(context.Background(), nil, "model", "a/b", "dev")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusForbidden {
		t.Errorf("error = %v, want a *StatusError with status 403", err)
	}
}

func TestStatusError_ErrorMessageIncludesErrorCodeWhenPresent(t *testing.T) {
	err := &StatusError{Method: "GET", URL: "https://example.invalid/x", Status: 404, ErrorCode: "RevisionNotFound"}
	got := err.Error()
	if !strings.Contains(got, "404") || !strings.Contains(got, "RevisionNotFound") || !strings.Contains(got, "GET") {
		t.Errorf("Error() = %q, want it to mention method, status, and error code", got)
	}
}

func TestStatusError_ErrorMessageOmitsErrorCodeWhenAbsent(t *testing.T) {
	err := &StatusError{Method: "POST", URL: "https://example.invalid/y", Status: 500}
	got := err.Error()
	if strings.Contains(got, "X-Error-Code") {
		t.Errorf("Error() = %q, want no X-Error-Code mention when ErrorCode is empty", got)
	}
	if !strings.Contains(got, "500") || !strings.Contains(got, "POST") {
		t.Errorf("Error() = %q, want it to mention method and status", got)
	}
}

func TestNew_ZeroValueClientUsesDefaultHTTPClient(t *testing.T) {
	// A Client built directly (not via New) with HTTP left nil must still
	// work — httpClient() falls back to http.DefaultClient.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RepoInfo{ID: "a/b", SHA: "main"})
	}))
	defer ts.Close()

	c := &Client{HubBaseURL: ts.URL}
	if _, err := c.RepoInfo(context.Background(), nil, "model", "a/b", "main"); err != nil {
		t.Fatalf("RepoInfo() with zero-value HTTP field error = %v", err)
	}
}

func TestNewCASClient_ZeroValueClientUsesDefaultHTTPClient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	c := &CASClient{BaseURL: ts.URL}
	resp, err := c.FetchXorb(context.Background(), nil, "default", "abc", "")
	if err != nil {
		t.Fatalf("FetchXorb() with zero-value HTTP field error = %v", err)
	}
	resp.Body.Close()
}
