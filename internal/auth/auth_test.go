package auth

// Core correctness tests for NoAuth and StaticTokenAuth. Wiring-level
// tests (casserver/hubserver enforcing scopes over real HTTP, backward
// compatibility, adversarial headers) live alongside those packages —
// see internal/casserver/auth_test.go and internal/hubserver/auth_test.go.
//
// Every token string below (testFixtureToken, "wrong-fixture-token",
// "shared-fixture-token") is a hardcoded test fixture with no relation
// to any real credential — flagged explicitly so static-analysis secret
// scanners don't need to guess.

import (
	"errors"
	"net/http/httptest"
	"testing"
)

// testFixtureToken is the value most tests in this file configure
// StaticTokenAuth/BearerCredentialHelper with. Not a real credential.
const testFixtureToken = "test-fixture-token-not-a-real-secret"

func TestNoAuth_AlwaysSucceedsWithAllScopes(t *testing.T) {
	var a Authenticator = NoAuth{}
	req := httptest.NewRequest("GET", "/", nil)

	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate() error = %v, want nil", err)
	}
	if !p.HasScope(ScopeRead) || !p.HasScope(ScopeWrite) {
		t.Error("NoAuth principal missing a scope, want all scopes granted")
	}
	if p.Subject() == "" {
		t.Error("NoAuth principal has an empty Subject(), want a non-empty placeholder identity")
	}
}

func TestNoAuth_SucceedsWithNoAuthorizationHeaderAtAll(t *testing.T) {
	a := NoAuth{}
	req := httptest.NewRequest("GET", "/", nil) // no Authorization header set
	if _, err := a.Authenticate(req); err != nil {
		t.Errorf("Authenticate() error = %v, want nil (NoAuth must not require any header)", err)
	}
}

func TestStaticTokenAuth_CorrectTokenSucceeds(t *testing.T) {
	a := NewStaticTokenAuth(testFixtureToken)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+testFixtureToken)

	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate() error = %v, want nil", err)
	}
	if !p.HasScope(ScopeRead) || !p.HasScope(ScopeWrite) {
		t.Error("correct-token principal missing a scope, want all scopes granted")
	}
}

func TestStaticTokenAuth_WrongTokenFails(t *testing.T) {
	a := NewStaticTokenAuth(testFixtureToken)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer wrong-fixture-token") // deliberately incorrect, not a real credential

	_, err := a.Authenticate(req)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Authenticate() error = %v, want errors.Is(err, ErrUnauthenticated)", err)
	}
}

func TestStaticTokenAuth_MissingHeaderFails(t *testing.T) {
	a := NewStaticTokenAuth(testFixtureToken)
	req := httptest.NewRequest("GET", "/", nil)

	_, err := a.Authenticate(req)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Authenticate() error = %v, want errors.Is(err, ErrUnauthenticated)", err)
	}
}

func TestStaticTokenAuth_MalformedHeadersFailCleanly(t *testing.T) {
	a := NewStaticTokenAuth(testFixtureToken)
	cases := []string{
		"",
		"Bearer",                           // no trailing space, no token
		"Bearer ",                          // empty token
		testFixtureToken,                   // missing "Bearer " prefix entirely
		"Basic " + testFixtureToken,        // wrong scheme
		"Bearer  " + testFixtureToken,      // double space (the extra space becomes part of the token, so it won't match)
		"Bearer " + testFixtureToken + " ", // trailing space in token — must NOT match (not the same secret)
	}
	for _, h := range cases {
		t.Run(h, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if h != "" {
				req.Header.Set("Authorization", h)
			}
			_, err := a.Authenticate(req)
			if !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("Authenticate() with header %q error = %v, want errors.Is(err, ErrUnauthenticated)", h, err)
			}
		})
	}
}

func TestStaticTokenAuth_CaseInsensitiveSchemeStillMatchesToken(t *testing.T) {
	a := NewStaticTokenAuth(testFixtureToken)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "bearer "+testFixtureToken) // lowercase scheme, per RFC 6750 case-insensitivity
	if _, err := a.Authenticate(req); err != nil {
		t.Errorf("Authenticate() error = %v, want nil (scheme name is case-insensitive per RFC 6750)", err)
	}
}

func TestNewStaticTokenAuth_EmptyTokenNeverAuthenticates(t *testing.T) {
	// Constructing with an empty token must not become "accept anything,
	// including an empty bearer token" — that would defeat the point of
	// explicitly configuring a token at all.
	a := NewStaticTokenAuth("")
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer ")
	if _, err := a.Authenticate(req); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("Authenticate() error = %v, want errors.Is(err, ErrUnauthenticated) for an empty configured token", err)
	}
}

func TestNoopCredentialHelper_DoesNotModifyRequest(t *testing.T) {
	var c CredentialHelper = NoopCredentialHelper{}
	req := httptest.NewRequest("GET", "/", nil)
	if err := c.FillCredential(req); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header = %q, want empty (Noop must not set it)", got)
	}
	if c.WhoAmI() == "" {
		t.Error("NoopCredentialHelper.WhoAmI() is empty, want a non-empty identifier for logging")
	}
}

func TestBearerCredentialHelper_SetsAuthorizationHeader(t *testing.T) {
	c := NewBearerCredentialHelper(testFixtureToken)
	req := httptest.NewRequest("GET", "/", nil)
	if err := c.FillCredential(req); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	want := "Bearer " + testFixtureToken
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}
	if c.WhoAmI() == "" {
		t.Error("BearerCredentialHelper.WhoAmI() is empty, want a non-empty identifier for logging")
	}
}

func TestBearerCredentialHelper_RoundTripsWithStaticTokenAuth(t *testing.T) {
	// The two halves of this package must actually interoperate: a
	// request filled by BearerCredentialHelper must authenticate
	// successfully against a StaticTokenAuth configured with the same
	// token — this is the whole point of having a matched client/server
	// pair of interfaces. "shared-fixture-token" is a hardcoded test
	// value, not a real credential.
	const sharedFixtureToken = "shared-fixture-token"
	serverAuth := NewStaticTokenAuth(sharedFixtureToken)
	clientCred := NewBearerCredentialHelper(sharedFixtureToken)

	req := httptest.NewRequest("POST", "/v1/xorbs/default/abc", nil)
	if err := clientCred.FillCredential(req); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	if _, err := serverAuth.Authenticate(req); err != nil {
		t.Errorf("Authenticate() error = %v, want nil — client and server must interoperate on the same token", err)
	}
}
