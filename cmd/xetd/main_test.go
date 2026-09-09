package main

// Unit test for resolveAuthToken's exact call-site precedence:
// -auth-token, then $XETD_AUTH_TOKEN, then $HF_TOKEN. The generic
// precedence rule (auth.ResolveToken) is exhaustively tested in
// internal/auth; this just locks in that xetd calls it with the right
// env var order, including the $HF_TOKEN fallback that lets a local xetd
// act as a drop-in replacement for the real huggingface.co Xet backend.
//
// "test-fixture-token-not-a-real-secret" and "hf-fixture-token-not-a-real-secret"
// below are hardcoded test fixtures with no relation to any real
// credential — flagged explicitly so static-analysis secret scanners
// don't need to guess.

import "testing"

const xetdEnvFixtureToken = "test-fixture-token-not-a-real-secret"

func TestResolveAuthToken_FlagWinsOverBothEnvVars(t *testing.T) {
	t.Setenv("XETD_AUTH_TOKEN", xetdEnvFixtureToken)
	t.Setenv("HF_TOKEN", "hf-fixture-token-not-a-real-secret")

	got := resolveAuthToken("flag-fixture-token")
	if got != "flag-fixture-token" {
		t.Errorf("resolveAuthToken() = %q, want the explicit flag to win", got)
	}
}

func TestResolveAuthToken_FallsBackToXETDAuthTokenOverHFToken(t *testing.T) {
	t.Setenv("XETD_AUTH_TOKEN", xetdEnvFixtureToken)
	t.Setenv("HF_TOKEN", "hf-fixture-token-not-a-real-secret")

	got := resolveAuthToken("None")
	if got != xetdEnvFixtureToken {
		t.Errorf("resolveAuthToken() = %q, want $XETD_AUTH_TOKEN to win over $HF_TOKEN", got)
	}
}

func TestResolveAuthToken_FallsBackToHFTokenWhenXETDAuthTokenUnset(t *testing.T) {
	t.Setenv("XETD_AUTH_TOKEN", "")
	t.Setenv("HF_TOKEN", "hf-fixture-token-not-a-real-secret")

	got := resolveAuthToken("None")
	if got != "hf-fixture-token-not-a-real-secret" {
		t.Errorf("resolveAuthToken() = %q, want fallback to $HF_TOKEN", got)
	}
}

func TestResolveAuthToken_NoneWhenNeitherFlagNorEnvSet(t *testing.T) {
	t.Setenv("XETD_AUTH_TOKEN", "")
	t.Setenv("HF_TOKEN", "")

	got := resolveAuthToken("None")
	if got != "None" {
		t.Errorf("resolveAuthToken() = %q, want %q (no auth enforced) when nothing is set", got, "None")
	}
}
