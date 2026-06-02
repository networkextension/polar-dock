package dock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"
)

// reDeriveProviderPutSig mirrors polar-assets' verifyDockPutURL: it
// rebuilds the canonical from the URL's path sha + query (exp/max/ct)
// and HMACs with the shared secret. The dock-minted sig must equal it.
func reDeriveProviderPutSig(sha, exp, max, ct string, secret []byte) string {
	canonical := "PUT\n" + sha + "\n" + exp + "\n" + max + "\n" + ct
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestBuildSignedPutURL_ProviderWouldAccept(t *testing.T) {
	secret := []byte("shared-provider-secret")
	sha := strings.Repeat("ab", 32)
	raw, err := buildSignedPutURL("https://prov.example.com/", sha, time.Minute, 4096, "image/png", secret)
	if err != nil {
		t.Fatalf("buildSignedPutURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Path != "/v1/blob/"+sha {
		t.Errorf("path: got %q", u.Path)
	}
	q := u.Query()
	if q.Get("max") != "4096" {
		t.Errorf("max: got %q", q.Get("max"))
	}
	if q.Get("ct") != "image/png" {
		t.Errorf("ct: got %q", q.Get("ct"))
	}
	// The provider re-derives over the DECODED query values; must match.
	want := reDeriveProviderPutSig(sha, q.Get("exp"), q.Get("max"), q.Get("ct"), secret)
	if q.Get("sig") != want {
		t.Errorf("sig mismatch: provider would reject\n got=%s\nwant=%s", q.Get("sig"), want)
	}
}

func TestBuildSignedPutURL_DefaultsContentType(t *testing.T) {
	raw, err := buildSignedPutURL("https://p/", strings.Repeat("c", 64), 0, 10, "", []byte("k"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(raw, "ct=application%2Foctet-stream") {
		t.Errorf("expected default octet-stream ct, got %q", raw)
	}
}

func TestBuildSignedPutURL_Rejects(t *testing.T) {
	if _, err := buildSignedPutURL("https://p", "", time.Minute, 1, "x", []byte("k")); err == nil {
		t.Error("empty sha should error")
	}
	if _, err := buildSignedPutURL("https://p", "ab", time.Minute, 0, "x", []byte("k")); err == nil {
		t.Error("non-positive max should error")
	}
	if _, err := buildSignedPutURL("https://p", "ab", time.Minute, 1, "x", nil); err == nil {
		t.Error("empty secret should error")
	}
}

// A download token must NOT validate as a PUT grant — the canonicals
// (newline/verb-prefixed PUT vs "<sha>:<exp>") can't collide.
func TestPutGrant_NotConfusableWithGetToken(t *testing.T) {
	secret := []byte("k")
	sha := strings.Repeat("ab", 32)
	getTok, _ := signAssetURL(sha, time.Minute, secret) // <exp>:<sig> over <sha>:<exp>
	colon := strings.IndexByte(getTok, ':')
	exp, getSig := getTok[:colon], getTok[colon+1:]
	putSig := reDeriveProviderPutSig(sha, exp, "100", "application/octet-stream", secret)
	if getSig == putSig {
		t.Error("GET token sig equals PUT grant sig for same exp — canonicals collided")
	}
}
