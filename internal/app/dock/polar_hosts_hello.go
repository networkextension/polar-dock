package dock

// Forward the static host_info blob from a polar-agent `hello` frame
// to polar-hosts-svc's /internal/v1/hosts/hello endpoint. Sister to
// polar_hosts_touch.go — they live as separate functions (not flags
// on one shared poster) because the channels evolve independently:
// touch fires per skill.advertise tick, hello fires once per agent
// (re)connect, and a future dynamic-metrics channel will run on its
// own cadence again.
//
// Loopback-only on the other side (no HMAC) because dock + polar-hosts
// run on the same host by deployment convention.
//
// polar-dock#351.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

type polarHostsHelloPayload struct {
	HostID      string          `json:"host_id"`
	HostInfo    json.RawMessage `json:"host_info"`
	HelloSeenAt time.Time       `json:"hello_seen_at"`
}

// polarHostsHelloURL returns the /internal/v1/hosts/hello target derived
// from the configured touch URL (POLAR_HOSTS_TOUCH_URL). We don't have
// — or want — a second env var for the same plugin-svc origin; the
// touch URL is conventionally http://127.0.0.1:8095/internal/v1/hosts/touch
// so swap the trailing path. Same trick fetchCatalogEntry uses.
func (s *Server) polarHostsHelloURL() string {
	base := strings.TrimSpace(s.polarHostsTouchURL)
	if base == "" {
		return ""
	}
	idx := strings.Index(base, "/internal/")
	if idx <= 0 {
		return ""
	}
	return base[:idx] + "/internal/v1/hosts/hello"
}

// forwardHostInfoToPolarHosts posts the host_info blob the agent sent
// in its hello frame to polar-hosts-svc so the row the UI reads in
// polar_hosts DB picks up virt / memory / cpu / kernel details.
//
// Dedupe: per-conn sha256 cached on agentConn.lastHostInfoSHA — when
// the agent sends a hello on a reconnect with the same machine facts
// (the common case) we skip the POST. The cache lives only as long as
// the agentConn, so a fresh attach always re-sends one (we can't trust
// polar-hosts to have remembered through its own restart).
//
// Fail-soft: any failure (network, non-200, bad sha) is logged and
// dropped. Never blocks the hello path.
func (s *Server) forwardHostInfoToPolarHosts(ac *agentConn, hostID string, hostInfo json.RawMessage, seenAt time.Time) {
	url := s.polarHostsHelloURL()
	if url == "" || hostID == "" || len(hostInfo) == 0 {
		return
	}
	sum := sha256.Sum256(hostInfo)
	if ac != nil && bytes.Equal(ac.lastHostInfoSHA, sum[:]) {
		// Already forwarded this exact payload on this conn; skip.
		return
	}
	body, err := json.Marshal(polarHostsHelloPayload{
		HostID:      hostID,
		HostInfo:    hostInfo,
		HelloSeenAt: seenAt,
	})
	if err != nil {
		log.Printf("[agent ws] forward hello marshal host=%s: %v", hostID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("[agent ws] forward hello build host=%s: %v", hostID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[agent ws] forward hello send host=%s: %v", hostID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[agent ws] forward hello reject host=%s status=%d", hostID, resp.StatusCode)
		return
	}
	// Success — stamp the sha so identical reposts skip out. There's a
	// tiny race against a concurrent forwarder for the same conn, but
	// the worst-case is "we POST twice on a doubled hello", which the
	// endpoint handles idempotently anyway.
	if ac != nil {
		ac.lastHostInfoSHA = append(ac.lastHostInfoSHA[:0], sum[:]...)
	}
}
