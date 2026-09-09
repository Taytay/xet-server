package main

// Unit tests for newClient's -auth-token/$XET_AUTH_TOKEN/$HF_TOKEN
// precedence rule — mirrors cmd/xetd's resolveSecretFlag tests, but on the
// client side, and with the additional $HF_TOKEN fallback (the same
// variable the real `hf` CLI reads) so pointing xet at an HF_TOKEN already
// exported for `hf` "just works" without a separate credential.
//
// Every token string below (e.g. "test-fixture-token-not-a-real-secret") is
// a hardcoded test fixture with no relation to any real credential —
// flagged explicitly so static-analysis secret scanners don't need to
// guess.

import (
	"net/http/httptest"
	"testing"
)

const envFixtureToken = "test-fixture-token-not-a-real-secret"

func TestNewClient_ExplicitFlagWinsOverEnv(t *testing.T) {
	t.Setenv("XET_AUTH_TOKEN", envFixtureToken)
	t.Setenv("HF_TOKEN", "hf-fixture-token-not-a-real-secret")

	c := newClient("http://example.invalid", "flag-fixture-token")
	req := httptest.NewRequest("GET", "/", nil)
	if err := c.Cred.FillCredential(req); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	want := "Bearer flag-fixture-token"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want %q (explicit flag must win)", got, want)
	}
}

func TestNewClient_FallsBackToXetAuthTokenEnv(t *testing.T) {
	t.Setenv("XET_AUTH_TOKEN", envFixtureToken)
	t.Setenv("HF_TOKEN", "hf-fixture-token-not-a-real-secret")

	c := newClient("http://example.invalid", "None")
	req := httptest.NewRequest("GET", "/", nil)
	if err := c.Cred.FillCredential(req); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	want := "Bearer " + envFixtureToken
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want %q ($XET_AUTH_TOKEN must win over $HF_TOKEN)", got, want)
	}
}

func TestNewClient_FallsBackToHFTokenWhenXetAuthTokenUnset(t *testing.T) {
	t.Setenv("XET_AUTH_TOKEN", "")
	t.Setenv("HF_TOKEN", "hf-fixture-token-not-a-real-secret")

	c := newClient("http://example.invalid", "None")
	req := httptest.NewRequest("GET", "/", nil)
	if err := c.Cred.FillCredential(req); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	want := "Bearer hf-fixture-token-not-a-real-secret"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization = %q, want %q (fallback to $HF_TOKEN)", got, want)
	}
}

func TestNewClient_NoFlagNoEnvSendsNoCredential(t *testing.T) {
	t.Setenv("XET_AUTH_TOKEN", "")
	t.Setenv("HF_TOKEN", "")

	c := newClient("http://example.invalid", "None")
	req := httptest.NewRequest("GET", "/", nil)
	if err := c.Cred.FillCredential(req); err != nil {
		t.Fatalf("FillCredential() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty when no flag/env token is set", got)
	}
}

// resolveServerFixtureURL and resolveServerEnvFixtureURL are hardcoded
// test values with no relation to any real server — plain example.invalid
// URLs, not credentials, so no "not a real secret" labeling is needed
// here, unlike the token fixtures above.
const (
	resolveServerFixtureURL    = "http://flag.example.invalid:1"
	resolveServerEnvFixtureURL = "http://env.example.invalid:2"
)

func TestResolveServer_ExplicitFlagWinsOverEnv(t *testing.T) {
	t.Setenv("XET_SERVER", resolveServerEnvFixtureURL)

	got := resolveServer(resolveServerFixtureURL)
	if got != resolveServerFixtureURL {
		t.Errorf("resolveServer() = %q, want the explicit flag to win", got)
	}
}

func TestResolveServer_FallsBackToXetServerEnvWhenFlagEmpty(t *testing.T) {
	t.Setenv("XET_SERVER", resolveServerEnvFixtureURL)

	got := resolveServer("")
	if got != resolveServerEnvFixtureURL {
		t.Errorf("resolveServer() = %q, want fallback to $XET_SERVER", got)
	}
}

func TestResolveServer_FallsBackToDefaultWhenNeitherSet(t *testing.T) {
	t.Setenv("XET_SERVER", "")

	got := resolveServer("")
	if got != defaultServer {
		t.Errorf("resolveServer() = %q, want %q when neither flag nor $XET_SERVER is set", got, defaultServer)
	}
}
