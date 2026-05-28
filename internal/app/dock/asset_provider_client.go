package dock

// asset_provider_client.go — dock → polar-assets-svc HTTP client
// for the upload path. POST /v1/receive with multipart (file + sha256).
//
// Auth: the provider verifies the same HMAC plugin sig dock plugins
// already use (X-Polar-Plugin-Name/Timestamp/Sig over canonical
// METHOD\nPATH\nTS\nhex(sha256(body))). We reuse the per-provider
// hmac_token as the plugin token. Provider name in the header is
// the slug.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// providerHTTPClient is the shared client used for dock→provider
// calls. 5-minute timeout for the upload streaming case.
var providerHTTPClient = &http.Client{
	Timeout: 5 * time.Minute,
}

// uploadAssetToProvider POSTs the staged file to the provider's
// /v1/receive endpoint with a multipart body. Returns nil on 2xx;
// error wrapping the upstream body on non-2xx.
//
// The HMAC sig is computed over canonical
// METHOD\nPATH\nTS\nhex(sha256(body)). For streaming uploads of
// huge files we'd want to skip body hashing — but for v1 (≤5 GB
// per upload) buffering the body fingerprint is acceptable. P4 can
// switch to a chunked content-md5 if perf demands.
func (s *Server) uploadAssetToProvider(provider *AssetProvider, sha, stagePath, filename string) error {
	src, err := os.Open(stagePath)
	if err != nil {
		return fmt.Errorf("open staged file: %w", err)
	}
	defer src.Close()
	stat, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat staged file: %w", err)
	}

	// Build multipart body in memory. For 5 GB files this is a lot
	// — but operationally the boxes we deploy on (.57 prod, .69 dev)
	// have 32-128 GB RAM, so 5 GB body buffering is fine for v1.
	// True streaming with a chunked sig comes in P4.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("expected_sha256", sha); err != nil {
		return fmt.Errorf("write sha field: %w", err)
	}
	if err := mw.WriteField("size_bytes", strconv.FormatInt(stat.Size(), 10)); err != nil {
		return fmt.Errorf("write size field: %w", err)
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, src); err != nil {
		return fmt.Errorf("copy file body: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}

	url := strings.TrimRight(provider.BaseURL, "/") + "/v1/receive"
	bodyBytes := body.Bytes()
	bodySum := sha256.Sum256(bodyBytes)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	canonical := fmt.Sprintf("%s\n%s\n%s\n%s",
		"POST", "/v1/receive", ts, hex.EncodeToString(bodySum[:]))
	mac := hmac.New(sha256.New, []byte(provider.HMACToken))
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Polar-Plugin-Name", provider.Slug)
	req.Header.Set("X-Polar-Plugin-Timestamp", ts)
	req.Header.Set("X-Polar-Plugin-Sig", sig)

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("provider POST failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
