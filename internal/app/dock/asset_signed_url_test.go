package dock

import (
	"strings"
	"testing"
	"time"
)

func TestSignVerifyAssetURL_Roundtrip(t *testing.T) {
	secret := []byte("test-secret-bytes-please-rotate")
	sha := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	token, err := signAssetURL(sha, time.Minute, secret)
	if err != nil {
		t.Fatalf("signAssetURL: %v", err)
	}
	if err := verifyAssetURL(token, sha, secret); err != nil {
		t.Errorf("verifyAssetURL failed for fresh token: %v", err)
	}
}

func TestVerifyAssetURL_RejectsTamperedSha(t *testing.T) {
	secret := []byte("test-secret")
	good := "aa" + strings.Repeat("0", 62)
	bad := "bb" + strings.Repeat("0", 62)
	token, err := signAssetURL(good, time.Minute, secret)
	if err != nil {
		t.Fatalf("signAssetURL: %v", err)
	}
	if err := verifyAssetURL(token, bad, secret); err == nil {
		t.Error("expected error for tampered sha; got nil")
	}
}

func TestVerifyAssetURL_RejectsExpired(t *testing.T) {
	secret := []byte("test-secret")
	sha := strings.Repeat("a", 64)
	// Negative TTL → expired immediately.
	token, err := signAssetURL(sha, -time.Hour, secret)
	if err != nil {
		t.Fatalf("signAssetURL: %v", err)
	}
	err = verifyAssetURL(token, sha, secret)
	if err == nil {
		t.Fatal("expected expired error; got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected error mentioning expired, got %q", err)
	}
}

func TestVerifyAssetURL_RejectsBadSecret(t *testing.T) {
	good := []byte("good-secret")
	bad := []byte("bad-secret")
	sha := strings.Repeat("a", 64)
	token, _ := signAssetURL(sha, time.Minute, good)
	if err := verifyAssetURL(token, sha, bad); err == nil {
		t.Error("expected error with wrong secret; got nil")
	}
}

func TestVerifyAssetURL_RejectsMalformed(t *testing.T) {
	secret := []byte("test-secret")
	sha := strings.Repeat("a", 64)
	cases := []string{
		"",
		"no-colon-here",
		":",
		"123:",
		":abc",
		"not_a_number:abc",
		"123:not_hex_zz",
	}
	for _, tc := range cases {
		if err := verifyAssetURL(tc, sha, secret); err == nil {
			t.Errorf("token %q should have failed but didn't", tc)
		}
	}
}

func TestBuildSignedBlobURL_Shape(t *testing.T) {
	url, err := buildSignedBlobURL(
		"https://assets-zen.4950.store/", // trailing slash should be stripped
		strings.Repeat("a", 64),
		time.Minute,
		[]byte("s"),
	)
	if err != nil {
		t.Fatalf("buildSignedBlobURL: %v", err)
	}
	want := "https://assets-zen.4950.store/v1/blob/"
	if !strings.HasPrefix(url, want) {
		t.Errorf("URL doesn't start with %q: got %q", want, url)
	}
	if !strings.Contains(url, "?token=") {
		t.Errorf("URL missing ?token= query: %q", url)
	}
}

// The download variant appends ?ct= and ?name= (last path element of the
// catalog name) after the signed token; octet-stream / empty name add nothing.
func TestBuildSignedDownloadURL_Hints(t *testing.T) {
	a := &Asset{
		SHA256: strings.Repeat("a", 64),
		Name:   "releases/android/polar-datacollector-v1.0.0-1.apk",
		Mime:   "application/vnd.android.package-archive",
	}
	u, err := buildSignedDownloadURL("https://assets.example/", a, time.Minute, []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"?token=", "&ct=application%2Fvnd.android.package-archive", "&name=polar-datacollector-v1.0.0-1.apk"} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q in %q", want, u)
		}
	}
	plain := &Asset{SHA256: strings.Repeat("b", 64), Mime: "application/octet-stream"}
	u, _ = buildSignedDownloadURL("https://assets.example", plain, time.Minute, []byte("s"))
	if strings.Contains(u, "&") {
		t.Errorf("no hints expected for octet-stream/unnamed asset: %q", u)
	}
}
