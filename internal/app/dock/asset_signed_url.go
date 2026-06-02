package dock

// asset_signed_url.go — HMAC-signed URL tokens that let dock issue
// short-lived 302 redirects to polar-assets-svc providers without
// exposing the provider's bearer secret to the client.
//
// Token format: <exp_unix>:<hex_hmac_sha256>
//
//	canonical = <sha256>:<exp_unix>
//	sig       = HMAC-SHA256(secret, canonical) as hex
//	token     = <exp_unix>:<sig>
//
// The token travels as a query parameter:
//
//	GET /v1/blob/<sha>?token=<token>
//
// The provider re-derives sig from the same canonical (it knows
// sha256 from the URL path and exp from the token), then constant-
// time compares.
//
// Secret: the matching asset_providers.hmac_token bytes (TEXT in DB,
// used as []byte). Operators set the same value as
// POLAR_ASSETS_DOCK_HMAC_SECRET on the provider box.
//
// Skew tolerance: none. If a clock drifts > 30 min and you stamp
// exp=now+30min, the provider will see exp < its now and 403.
// 30-min default expiry gives a comfortable headroom for normal
// operation.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAssetSignedURLTTL = 30 * time.Minute
)

// signAssetURL builds a signed-URL token for the given sha256, valid
// for ttl from now. Empty sha256 or empty secret returns an error.
func signAssetURL(sha256Hex string, ttl time.Duration, secret []byte) (string, error) {
	if sha256Hex == "" {
		return "", errors.New("signAssetURL: sha256 required")
	}
	if len(secret) == 0 {
		return "", errors.New("signAssetURL: secret required")
	}
	if ttl == 0 {
		ttl = defaultAssetSignedURLTTL
	}
	exp := time.Now().Add(ttl).Unix()
	canonical := sha256Hex + ":" + strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))
	return strconv.FormatInt(exp, 10) + ":" + sig, nil
}

// verifyAssetURL validates a token against the expected sha256 + the
// secret. Returns nil on success; an error describing the failure
// (expired, bad format, bad sig) otherwise.
//
// Constant-time HMAC compare to avoid timing-leak; expiry is just an
// integer compare. We surface the failure category in the error so
// the provider can log a useful diagnostic — but never return the
// specific reason to the HTTP caller (use a generic 403 message).
func verifyAssetURL(token, sha256Hex string, secret []byte) error {
	if token == "" || sha256Hex == "" {
		return errors.New("missing token or sha")
	}
	if len(secret) == 0 {
		return errors.New("missing secret")
	}
	colon := strings.IndexByte(token, ':')
	if colon <= 0 || colon == len(token)-1 {
		return errors.New("malformed token")
	}
	expStr := token[:colon]
	sigHex := token[colon+1:]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return fmt.Errorf("bad exp: %w", err)
	}
	if time.Now().Unix() > exp {
		return errors.New("expired")
	}
	canonical := sha256Hex + ":" + expStr
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonical))
	wantSigBytes := mac.Sum(nil)
	gotSigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("bad sig hex: %w", err)
	}
	if !hmac.Equal(wantSigBytes, gotSigBytes) {
		return errors.New("bad sig")
	}
	return nil
}

// buildSignedBlobURL is a convenience that combines signAssetURL +
// path templating. Returns the full URL the client gets 302'd to.
func buildSignedBlobURL(providerBaseURL, sha256Hex string, ttl time.Duration, secret []byte) (string, error) {
	token, err := signAssetURL(sha256Hex, ttl, secret)
	if err != nil {
		return "", err
	}
	base := strings.TrimRight(providerBaseURL, "/")
	return base + "/v1/blob/" + sha256Hex + "?token=" + token, nil
}

// buildSignedPutURL mints a scoped **upload** grant — the symmetric of
// buildSignedBlobURL. The provider verifies it in verifyDockPutURL
// (polar-assets internal/assets/signed_url.go). The grant binds the
// exact object key (sha256), a max body size, a content-type, and an
// expiry, so a leaked grant can only write that one blob, capped, for
// a short window:
//
//	PUT <base>/v1/blob/<sha256>?exp=<unix>&max=<bytes>&ct=<mime>&sig=<hex>
//	sig = HMAC-SHA256(secret, "PUT\n<sha256>\n<exp>\n<max>\n<ct>")
//
// The "PUT\n…"-prefixed, newline-delimited canonical can never collide
// with the download token's "<sha>:<exp>" canonical, so a download
// token cannot be replayed as an upload grant. maxBytes must be > 0;
// ct defaults to application/octet-stream.
func buildSignedPutURL(providerBaseURL, sha256Hex string, ttl time.Duration, maxBytes int64, ct string, secret []byte) (string, error) {
	if sha256Hex == "" {
		return "", errors.New("buildSignedPutURL: sha256 required")
	}
	if len(secret) == 0 {
		return "", errors.New("buildSignedPutURL: secret required")
	}
	if maxBytes <= 0 {
		return "", errors.New("buildSignedPutURL: maxBytes must be > 0")
	}
	if ttl == 0 {
		ttl = defaultAssetSignedURLTTL
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	expStr := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	maxStr := strconv.FormatInt(maxBytes, 10)
	canonical := "PUT\n" + sha256Hex + "\n" + expStr + "\n" + maxStr + "\n" + ct
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	base := strings.TrimRight(providerBaseURL, "/")
	q := url.Values{}
	q.Set("exp", expStr)
	q.Set("max", maxStr)
	q.Set("ct", ct)
	q.Set("sig", sig)
	return base + "/v1/blob/" + sha256Hex + "?" + q.Encode(), nil
}
