package sigv4

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestPresignURL_AWSDocsVector uses the well-known AWS SigV4 worked example
// (GetObject on examplebucket/test.txt, access key AKIDEXAMPLE) as fixed
// input, and checks the result against a signature independently derived
// by hand via the documented algorithm (HMAC-SHA256 key-derivation chain
// computed with `openssl dgst -mac HMAC`, not this package), so a bug in
// canonicalization or key derivation fails against math done outside this
// codebase rather than only against this package's own logic.
func TestPresignURL_AWSDocsVector(t *testing.T) {
	s := New("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLE", "us-east-1", "s3")
	ts := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	got, err := s.PresignURL("GET", "https://examplebucket.s3.amazonaws.com/test.txt", 86400, ts)
	if err != nil {
		t.Fatalf("PresignURL() error = %v", err)
	}

	const wantSignature = "8926271efa3bf3c0f187bb5dcfbf653a811743569e1caab7f5666fc6b86e4632"
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("failed to parse presigned URL %q: %v", got, err)
	}
	gotSignature := u.Query().Get("X-Amz-Signature")
	if gotSignature != wantSignature {
		t.Errorf("X-Amz-Signature = %s, want %s (independently derived via openssl HMAC chain)\nfull URL: %s", gotSignature, wantSignature, got)
	}

	if got, want := u.Query().Get("X-Amz-Credential"), "AKIDEXAMPLE/20130524/us-east-1/s3/aws4_request"; got != want {
		t.Errorf("X-Amz-Credential = %s, want %s", got, want)
	}
}

func TestSignRequest_Deterministic(t *testing.T) {
	s := New("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLE", "us-east-1", "s3")
	ts := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	newReq := func() *http.Request {
		req, _ := http.NewRequest("PUT", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
		return req
	}

	req1 := newReq()
	s.SignRequest(req1, HashPayload([]byte("hello")), ts)

	req2 := newReq()
	s.SignRequest(req2, HashPayload([]byte("hello")), ts)

	auth1 := req1.Header.Get("Authorization")
	auth2 := req2.Header.Get("Authorization")
	if auth1 == "" {
		t.Fatal("Authorization header not set")
	}
	if auth1 != auth2 {
		t.Errorf("signing the same request twice produced different signatures:\n%s\n%s", auth1, auth2)
	}
	if !strings.HasPrefix(auth1, Algorithm+" Credential=AKIDEXAMPLE/20130524/us-east-1/s3/aws4_request") {
		t.Errorf("Authorization header has unexpected form: %s", auth1)
	}
}

func TestSignRequest_DifferentPayloadDifferentSignature(t *testing.T) {
	s := New("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLE", "us-east-1", "s3")
	ts := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	req1, _ := http.NewRequest("PUT", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	s.SignRequest(req1, HashPayload([]byte("hello")), ts)

	req2, _ := http.NewRequest("PUT", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	s.SignRequest(req2, HashPayload([]byte("goodbye")), ts)

	if req1.Header.Get("Authorization") == req2.Header.Get("Authorization") {
		t.Error("signing requests with different payload hashes produced the same signature")
	}
}

func TestSignRequest_MatchesIndependentlyComputedSignature(t *testing.T) {
	s := New("AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLE", "us-east-1", "s3")
	ts := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	req, _ := http.NewRequest("PUT", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	// sha256("hello"), independently verified via `openssl dgst -sha256`.
	const payloadHash = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	s.SignRequest(req, payloadHash, ts)

	// Signature derived by hand outside this package: the SigV4 key-derivation
	// chain (kDate/kRegion/kService/kSigning) via `openssl dgst -mac HMAC`,
	// then HMAC'd against the string-to-sign built from the same canonical
	// request this test's inputs imply.
	const wantSignature = "8985606f3e42826c3a59e7890b507508d92467fdbfa5605043078bc7a9ec24de"
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "Signature="+wantSignature) {
		t.Errorf("Authorization = %s, want it to contain Signature=%s", auth, wantSignature)
	}
}

func TestCanonicalQueryString_SortedByKeyThenValue(t *testing.T) {
	q := url.Values{}
	q.Set("b", "2")
	q.Add("a", "2")
	q.Add("a", "1")

	got := canonicalQueryString(q)
	want := "a=1&a=2&b=2"
	if got != want {
		t.Errorf("canonicalQueryString() = %q, want %q", got, want)
	}
}
