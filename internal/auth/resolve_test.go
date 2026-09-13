package auth

// Unit tests for ResolveToken and BearerToken: the shared token-resolution
// and extraction primitives every binary in this project (xetd, xet, and
// any future relay/proxy) builds its -auth-token/environment-variable
// convention on top of.
//
// Every token string below (e.g. "test-fixture-token-not-a-real-secret") is
// a hardcoded test fixture with no relation to any real credential -
// flagged explicitly so static-analysis secret scanners don't need to
// guess.

import (
	"net/http/httptest"
	"testing"
)

const resolveFixtureToken = "test-fixture-token-not-a-real-secret"

func TestResolveToken_ExplicitFlagWinsOverEveryEnvVar(t *testing.T) {
	t.Setenv("FIRST_ENV", resolveFixtureToken)
	t.Setenv("SECOND_ENV", "second-fixture-token")

	got := ResolveToken("flag-fixture-token", "FIRST_ENV", "SECOND_ENV")
	if got != "flag-fixture-token" {
		t.Errorf("ResolveToken() = %q, want the explicit flag value to win", got)
	}
}

func TestResolveToken_FallsBackToFirstEnvVarWhenFlagIsNone(t *testing.T) {
	t.Setenv("FIRST_ENV", resolveFixtureToken)
	t.Setenv("SECOND_ENV", "second-fixture-token")

	got := ResolveToken("None", "FIRST_ENV", "SECOND_ENV")
	if got != resolveFixtureToken {
		t.Errorf("ResolveToken() = %q, want the first env var to win over the second", got)
	}
}

func TestResolveToken_FallsBackToSecondEnvVarWhenFirstUnset(t *testing.T) {
	t.Setenv("FIRST_ENV", "")
	t.Setenv("SECOND_ENV", "second-fixture-token")

	got := ResolveToken("", "FIRST_ENV", "SECOND_ENV")
	if got != "second-fixture-token" {
		t.Errorf("ResolveToken() = %q, want fallback to the second env var", got)
	}
}

func TestResolveToken_NoFlagNoEnvReturnsFlagValueUnchanged(t *testing.T) {
	t.Setenv("FIRST_ENV", "")
	t.Setenv("SECOND_ENV", "")

	got := ResolveToken("None", "FIRST_ENV", "SECOND_ENV")
	if got != "None" {
		t.Errorf("ResolveToken() = %q, want %q when no flag or env var is set", got, "None")
	}
}

func TestResolveToken_NoEnvVarsGivenReturnsFlagValue(t *testing.T) {
	got := ResolveToken("flag-fixture-token")
	if got != "flag-fixture-token" {
		t.Errorf("ResolveToken() = %q, want the flag value when no env vars are given at all", got)
	}
}

func TestBearerToken_ExtractsTokenFromValidHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+resolveFixtureToken)
	if got := BearerToken(req); got != resolveFixtureToken {
		t.Errorf("BearerToken() = %q, want %q", got, resolveFixtureToken)
	}
}

func TestBearerToken_EmptyWhenHeaderAbsent(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if got := BearerToken(req); got != "" {
		t.Errorf("BearerToken() = %q, want empty when no Authorization header is set", got)
	}
}

func TestBearerToken_RoundTripsForAProxyForwardingScenario(t *testing.T) {
	// Simulates the shape a relay/proxy needs: extract a caller's
	// credential from the request it just authenticated, then hand that
	// exact value to a CredentialHelper to forward upstream unchanged.
	incoming := httptest.NewRequest("GET", "/v1/reconstructions/abc", nil)
	incoming.Header.Set("Authorization", "Bearer "+resolveFixtureToken)

	extracted := BearerToken(incoming)
	if extracted == "" {
		t.Fatal("BearerToken() returned empty, want the caller's token")
	}

	forwarded := httptest.NewRequest("GET", "/v1/reconstructions/abc", nil)
	helper := NewBearerCredentialHelper(extracted)
	if err := helper.FillCredential(forwarded); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	if got := forwarded.Header.Get("Authorization"); got != "Bearer "+resolveFixtureToken {
		t.Errorf("forwarded Authorization = %q, want the caller's exact credential chained through", got)
	}
}
