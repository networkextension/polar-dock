package dock

import (
	"strings"
	"testing"
)

// Pure-logic tests for the plugin_modules token helpers. DB-touching
// coverage is in the manual verification path documented in the PR
// description (curl + sql checks on .57 post-deploy).

func TestNewPluginPlaintextFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		plaintext, hash := newPluginPlaintext()
		if !strings.HasPrefix(plaintext, pluginTokenPlaintextPrefix) {
			t.Fatalf("plaintext missing prefix: %q", plaintext)
		}
		// Length: 13-char prefix + 16 random bytes * 2 hex chars = 45.
		wantLen := len(pluginTokenPlaintextPrefix) + pluginTokenRandBytes*2
		if got := len(plaintext); got != wantLen {
			t.Fatalf("plaintext length got %d want %d (%q)", got, wantLen, plaintext)
		}
		if len(hash) != 64 {
			t.Fatalf("sha256 hex should be 64 chars, got %d", len(hash))
		}
		if seen[plaintext] {
			t.Fatalf("plaintext collision after %d iters: %q", i, plaintext)
		}
		seen[plaintext] = true
	}
}

func TestHashPluginTokenDeterministic(t *testing.T) {
	a := hashPluginToken("polar_plugin_deadbeef")
	b := hashPluginToken("polar_plugin_deadbeef")
	if a != b {
		t.Fatalf("hash should be deterministic; got %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("sha256 hex length: %d", len(a))
	}
	if hashPluginToken("polar_plugin_deadbeef") == hashPluginToken("polar_plugin_xxxxxxxx") {
		t.Fatalf("different inputs must produce different hashes")
	}
}
