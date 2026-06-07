package dock

// WG↔Hosts UI cross-link (doc/arch/wg-host-crosslink.md, P2).
//
// When a polar-agent `hello` frame carries host_info.wg_pubkeys (the agent
// reads its own local WireGuard public keys), dock forwards the
// (host_id, pubkeys) pairing to wg-svc so it can stamp wg_devices.host_id
// on the matching device rows. The agent is the authoritative source of the
// pairing — it knows both its host_id and its wg pubkey locally — so this
// replaces the unreliable hostname-matching backfill.
//
// Loopback-only on the wg-svc side (no HMAC), same deploy convention as the
// polar-hosts hello forward (sister file polar_hosts_hello.go). Fail-soft:
// any error is logged and dropped; never blocks the hello path.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type wgLinkPayload struct {
	HostID  string   `json:"host_id"`
	Pubkeys []string `json:"pubkeys"`
}

// wgPubkeysFromHostInfo extracts the wg_pubkeys map (iface → pubkey) the
// agent ships in host_info and returns the distinct pubkey values. Returns
// nil when the blob has no wg_pubkeys (the common case: hosts not on a mesh).
func wgPubkeysFromHostInfo(hostInfo json.RawMessage) []string {
	if len(hostInfo) == 0 {
		return nil
	}
	var hi struct {
		WGPubkeys map[string]string `json:"wg_pubkeys"`
	}
	if err := json.Unmarshal(hostInfo, &hi); err != nil || len(hi.WGPubkeys) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(hi.WGPubkeys))
	out := make([]string, 0, len(hi.WGPubkeys))
	for _, pk := range hi.WGPubkeys {
		if pk == "" {
			continue
		}
		if _, dup := seen[pk]; dup {
			continue
		}
		seen[pk] = struct{}{}
		out = append(out, pk)
	}
	return out
}

// forwardWGLinkToWGSvc posts {host_id, pubkeys} to wg-svc's loopback
// /internal/v1/wg-devices/link so it can stamp wg_devices.host_id. No-op when
// the feature is unconfigured (polarWGLinkURL empty), the host has no host_id,
// or host_info carries no wg pubkeys.
func (s *Server) forwardWGLinkToWGSvc(hostID string, hostInfo json.RawMessage) {
	if s.polarWGLinkURL == "" || hostID == "" {
		return
	}
	pubkeys := wgPubkeysFromHostInfo(hostInfo)
	if len(pubkeys) == 0 {
		return
	}
	body, err := json.Marshal(wgLinkPayload{HostID: hostID, Pubkeys: pubkeys})
	if err != nil {
		log.Printf("[agent ws] wg-link marshal host=%s: %v", hostID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.polarWGLinkURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[agent ws] wg-link build host=%s: %v", hostID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[agent ws] wg-link send host=%s: %v", hostID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[agent ws] wg-link reject host=%s status=%d", hostID, resp.StatusCode)
	}
}
