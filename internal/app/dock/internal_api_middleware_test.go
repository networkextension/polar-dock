package dock

// Pure-logic tests for the HMAC signature computation. The full
// gin-context middleware test would need a DB or a mock; that lives
// in the manual smoke procedure documented in the PR description.
// What we test here is the canonical-string + HMAC behavior which is
// the security-critical core.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestComputePluginSig_StableForSameInput(t *testing.T) {
	key := "abcdef0123456789"
	a := computePluginSig(key, "GET", "/internal/v1/ping", "1716297600", nil)
	b := computePluginSig(key, "GET", "/internal/v1/ping", "1716297600", nil)
	if !hmac.Equal(a, b) {
		t.Fatalf("same input should produce same HMAC")
	}
}

func TestComputePluginSig_DifferentMethodChangesSig(t *testing.T) {
	key := "abcdef0123456789"
	a := computePluginSig(key, "GET", "/x", "1", nil)
	b := computePluginSig(key, "POST", "/x", "1", nil)
	if hmac.Equal(a, b) {
		t.Fatalf("different method should produce different HMAC")
	}
}

func TestComputePluginSig_DifferentPathChangesSig(t *testing.T) {
	key := "abcdef0123456789"
	a := computePluginSig(key, "GET", "/x", "1", nil)
	b := computePluginSig(key, "GET", "/y", "1", nil)
	if hmac.Equal(a, b) {
		t.Fatalf("different path should produce different HMAC")
	}
}

func TestComputePluginSig_DifferentTimestampChangesSig(t *testing.T) {
	key := "abcdef0123456789"
	a := computePluginSig(key, "GET", "/x", "1", nil)
	b := computePluginSig(key, "GET", "/x", "2", nil)
	if hmac.Equal(a, b) {
		t.Fatalf("different timestamp should produce different HMAC")
	}
}

func TestComputePluginSig_BodyContentChangesSig(t *testing.T) {
	key := "abcdef0123456789"
	a := computePluginSig(key, "POST", "/x", "1", []byte(`{"a":1}`))
	b := computePluginSig(key, "POST", "/x", "1", []byte(`{"a":2}`))
	if hmac.Equal(a, b) {
		t.Fatalf("different body should produce different HMAC")
	}
}

func TestComputePluginSig_EmptyBodyAndNilBodyMatch(t *testing.T) {
	key := "abcdef0123456789"
	a := computePluginSig(key, "GET", "/x", "1", nil)
	b := computePluginSig(key, "GET", "/x", "1", []byte{})
	if !hmac.Equal(a, b) {
		t.Fatalf("nil and empty body should produce same HMAC (both hash to sha256(\"\"))")
	}
}

func TestComputePluginSig_KeyChangesSig(t *testing.T) {
	a := computePluginSig("keyA", "GET", "/x", "1", nil)
	b := computePluginSig("keyB", "GET", "/x", "1", nil)
	if hmac.Equal(a, b) {
		t.Fatalf("different key should produce different HMAC")
	}
}

func TestComputePluginSig_MethodCaseInsensitive(t *testing.T) {
	key := "abcdef0123456789"
	a := computePluginSig(key, "get", "/x", "1", nil)
	b := computePluginSig(key, "GET", "/x", "1", nil)
	if !hmac.Equal(a, b) {
		t.Fatalf("method case should be normalized")
	}
}

func TestComputePluginSig_CanonicalStringShape(t *testing.T) {
	// Spot-check the exact canonical string format clients must replicate.
	// (Indirect: produce the HMAC we expect to match a hand-built one.)
	key := "k"
	path := "/internal/v1/ping"
	ts := "1716297600"
	emptyHash := sha256.Sum256(nil)
	canonical := "GET\n" + path + "\n" + ts + "\n" + hex.EncodeToString(emptyHash[:])
	expected := hmacHex(key, canonical)
	got := hex.EncodeToString(computePluginSig(key, "GET", path, ts, nil))
	if !strings.EqualFold(got, expected) {
		t.Fatalf("canonical-string HMAC: got %q want %q", got, expected)
	}
}

func hmacHex(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}
