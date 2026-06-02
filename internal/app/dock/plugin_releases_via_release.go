package dock

// plugin_releases_via_release.go — flag-gated OTA cutover to polar-release.
//
// When POLAR_RELEASE_BASE is set (e.g. http://127.0.0.1:8101, internal),
// the heartbeat update-resolver consults the polar-release publish system
// instead of dock's local plugin_releases table + through-dock binary
// serving. The returned directive URL is the signed PROVIDER GET — so the
// module pulls the binary straight from the assets provider and the bytes
// never traverse dock (doc/arch/release-system.md §5.2).
//
// Off by default (empty env). Any error / miss falls back to the legacy
// plugin_releases path in resolvePluginUpdate, so a release outage can
// never stop a working module from updating.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var releaseResolveClient = &http.Client{Timeout: 3 * time.Second}

// resolvePluginUpdateViaRelease asks polar-release for the newest stable
// release of `name` on this platform (`<os>-<arch>`). Returns a directive
// with the absolute signed provider URL, or (nil,nil) when the feature is
// off / no release exists / the module is already current. Non-2xx (other
// than 404) and transport errors are returned so the caller logs + falls
// back to the legacy path.
func (s *Server) resolvePluginUpdateViaRelease(name, runningVer, osName, arch string) (*PluginUpdateDirective, error) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("POLAR_RELEASE_BASE")), "/")
	if base == "" {
		return nil, nil // feature off
	}
	platform := osName + "-" + arch
	reqURL := fmt.Sprintf("%s/release/resolve?module=%s&channel=stable&platform=%s",
		base, url.QueryEscape(name), url.QueryEscape(platform))
	resp, err := releaseResolveClient.Get(reqURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no release registered for this module/platform
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("release resolve HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r struct {
		Version    string          `json:"version"`
		SHA256     string          `json:"sha256"`
		DLURL      string          `json:"dl_url"`
		Ed25519Sig string          `json:"ed25519_sig"`
		Manifest   json.RawMessage `json:"manifest"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Version == "" || r.DLURL == "" || r.Version == strings.TrimSpace(runningVer) {
		return nil, nil // nothing to do
	}
	// Pass the signature + manifest through verbatim so a module with
	// POLAR_RELEASE_PUBKEY pinned can verify authenticity before swapping.
	return &PluginUpdateDirective{
		Version:    r.Version,
		URL:        r.DLURL,
		SHA256:     r.SHA256,
		Ed25519Sig: r.Ed25519Sig,
		Manifest:   r.Manifest,
	}, nil
}
