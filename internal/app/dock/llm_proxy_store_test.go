package dock

import (
	"strings"
	"testing"
)

// Pure-logic tests for llm_proxy_store helpers. End-to-end DB
// + HTTP integration is covered by the manual smoke described in
// the P0 PR (see doc/llm/llm-proxy-design.md §8).

func TestNewLLMProxyTokenPlaintext(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		plaintext, prefix := newLLMProxyTokenPlaintext()
		if !strings.HasPrefix(plaintext, llmProxyTokenPlaintextPrefix) {
			t.Fatalf("plaintext missing required prefix: %q", plaintext)
		}
		// Format is "polar_proxy_" + 32 hex chars = 12 + 32 = 44.
		if got, want := len(plaintext), len(llmProxyTokenPlaintextPrefix)+32; got != want {
			t.Fatalf("plaintext length: got %d, want %d (%q)", got, want, plaintext)
		}
		// Prefix is the first 20 chars (12 prefix + 8 hex) — enough to
		// disambiguate in the UI without leaking the secret.
		wantPrefixLen := len(llmProxyTokenPlaintextPrefix) + 8
		if got := len(prefix); got != wantPrefixLen {
			t.Fatalf("prefix length: got %d, want %d", got, wantPrefixLen)
		}
		if !strings.HasPrefix(plaintext, prefix) {
			t.Fatalf("prefix is not a prefix of plaintext: prefix=%q plaintext=%q", prefix, plaintext)
		}
		if seen[plaintext] {
			t.Fatalf("plaintext collision after %d iters: %q", i, plaintext)
		}
		seen[plaintext] = true
	}
}

func TestHashLLMProxyTokenDeterministic(t *testing.T) {
	a := hashLLMProxyToken("polar_proxy_deadbeef")
	b := hashLLMProxyToken("polar_proxy_deadbeef")
	if a != b {
		t.Fatalf("hash should be deterministic; got %q vs %q", a, b)
	}
	// sha256 hex = 64 chars
	if got := len(a); got != 64 {
		t.Fatalf("sha256 hex length: got %d, want 64", got)
	}
	// Different inputs differ
	if hashLLMProxyToken("polar_proxy_deadbeef") == hashLLMProxyToken("polar_proxy_xxxxxxxx") {
		t.Fatalf("hash collided across distinct inputs")
	}
}

func TestHashLLMProxyTokenEmpty(t *testing.T) {
	// Empty plaintext still produces a valid sha256 — the lookup path
	// won't find a matching row because we never store an empty
	// plaintext, but the helper must not panic.
	got := hashLLMProxyToken("")
	if len(got) != 64 {
		t.Fatalf("empty-input hash: got len %d, want 64", len(got))
	}
}
