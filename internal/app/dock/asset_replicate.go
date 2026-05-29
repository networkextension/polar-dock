package dock

// asset_replicate.go — P4 warm-pull orchestration.
//
// Given an (asset, target_provider) pair, ask the target provider
// to fetch the blob from an origin provider that already holds it.
// Dock mediates: it picks the origin, mints a short-lived signed
// URL to the origin's /v1/blob/<sha>?token=…, and signs the JSON
// body of POST /v1/pull on the target with the target's HMAC token.
//
// Why dock mediates the signed URL instead of the target deriving
// it: providers don't share secrets pairwise. Each provider has
// its own asset_providers.hmac_token; dock has all of them in the
// registry. The target just fetches the URL like any HTTPS source.
//
// State transitions on asset_locations(asset_id, provider_id):
//
//   nothing → fetching   (before the call)
//   fetching → ready     (on 200 from target)
//   fetching → (deleted) (on error; admin can retry)

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// replicateAssetToProvider orchestrates a warm-pull of asset.SHA256
// onto targetProviderID. Returns nil + the byte count on success.
//
// Steps:
//  1. Find the asset and a healthy origin provider that already has
//     it (uses the same picker the download 302 uses).
//  2. Mint a short-lived signed URL to the origin's /v1/blob.
//  3. INSERT (or UPDATE) asset_locations(target, fetching).
//  4. POST /v1/pull on the target with HMAC + the source_url.
//  5. UPDATE asset_locations to 'ready' on 200; DELETE on failure
//     so the next attempt starts clean.
//
// Caller is expected to be admin (audit log + permission check
// happen at the handler layer).
func (s *Server) replicateAssetToProvider(assetID, targetProviderID int64) (int64, error) {
	if assetID <= 0 || targetProviderID <= 0 {
		return 0, errors.New("replicate: invalid IDs")
	}
	asset, err := s.getAssetByID(assetID)
	if err != nil {
		return 0, fmt.Errorf("load asset: %w", err)
	}
	if asset == nil {
		return 0, errors.New("asset not found")
	}
	target, err := s.getAssetProviderByID(targetProviderID)
	if err != nil {
		return 0, fmt.Errorf("load target: %w", err)
	}
	if target == nil {
		return 0, errors.New("target provider not found")
	}
	// Replicate orchestrator picks an origin to copy FROM; the
	// needsWarmPull signal is for cache-miss downloads, not this
	// admin-driven path — discard it.
	origin, _, err := s.pickProviderForAsset(assetID, "")
	if err != nil {
		return 0, fmt.Errorf("pick origin: %w", err)
	}
	if origin == nil {
		return 0, errors.New("no origin provider holds this asset yet")
	}
	if origin.ID == target.ID {
		// Already on target — fast no-op; ensure the locations row is
		// in 'ready' state so the picker is consistent.
		if err := s.markAssetLocationReady(assetID, target.ID); err != nil {
			return 0, fmt.Errorf("mark ready (self): %w", err)
		}
		return asset.SizeBytes, nil
	}
	sourceURL, err := buildSignedBlobURL(
		origin.BaseURL, asset.SHA256, defaultAssetSignedURLTTL,
		[]byte(origin.HMACToken),
	)
	if err != nil {
		return 0, fmt.Errorf("sign url: %w", err)
	}
	if err := s.markAssetLocationFetching(assetID, target.ID); err != nil {
		return 0, fmt.Errorf("mark fetching: %w", err)
	}
	gotSize, err := s.requestPullOnProvider(target, asset.SHA256, sourceURL, asset.SizeBytes)
	if err != nil {
		// Roll back to no row so the next attempt picks a fresh
		// 'fetching' state. Best-effort; ignore the rollback error.
		_ = s.deleteAssetLocation(assetID, target.ID)
		return 0, err
	}
	if err := s.markAssetLocationReady(assetID, target.ID); err != nil {
		// The blob is on the target; we just couldn't update the
		// catalog row. Next picker pass will rediscover via a fresh
		// upload, OR an admin can retry. Surface the error so the
		// caller knows the row isn't 'ready' yet.
		return gotSize, fmt.Errorf("mark ready: %w", err)
	}
	return gotSize, nil
}

// requestPullOnProvider POSTs the warm-pull request to the target.
// Mirrors uploadAssetToProvider in HMAC shape: canonical signed is
// METHOD\nPATH\nTS\nhex(sha256(body)). Body is JSON.
func (s *Server) requestPullOnProvider(target *AssetProvider, sha, sourceURL string, expectedSize int64) (int64, error) {
	payload := struct {
		SHA256    string `json:"sha256"`
		SourceURL string `json:"source_url"`
		SizeBytes int64  `json:"size_bytes,omitempty"`
	}{
		SHA256:    sha,
		SourceURL: sourceURL,
		SizeBytes: expectedSize,
	}
	body, err := json.Marshal(&payload)
	if err != nil {
		return 0, fmt.Errorf("marshal pull req: %w", err)
	}
	url := strings.TrimRight(target.BaseURL, "/") + "/v1/pull"
	bodySum := sha256.Sum256(body)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	canonical := fmt.Sprintf("%s\n%s\n%s\n%s",
		"POST", "/v1/pull", ts, hex.EncodeToString(bodySum[:]))
	mac := hmac.New(sha256.New, []byte(target.HMACToken))
	mac.Write([]byte(canonical))
	sig := hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Polar-Plugin-Name", target.Slug)
	req.Header.Set("X-Polar-Plugin-Timestamp", ts)
	req.Header.Set("X-Polar-Plugin-Sig", sig)

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("provider POST failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("provider HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var ok struct {
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(respBody, &ok); err != nil {
		return 0, fmt.Errorf("decode pull response: %w", err)
	}
	return ok.SizeBytes, nil
}
