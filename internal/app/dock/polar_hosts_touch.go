package dock

// Forward skill.advertise from the dock WS agent_hub to polar-hosts-svc's
// /internal/v1/hosts/touch endpoint. Post-H9 the hosts table lives in
// polar_hosts DB which dock can't reach directly; this keeps the row the
// UI reads from in sync with the WS heartbeats.
//
// Loopback-only on the other side (no HMAC) because dock + polar-hosts
// run on the same host by deployment convention.

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

type polarHostsTouchPayload struct {
	HostID   string             `json:"host_id"`
	Skills   []AdvertisedSkill  `json:"skills"`
	RemoteIP string             `json:"remote_ip"`
	SeenAt   time.Time          `json:"seen_at"`
}

func (s *Server) forwardSkillAdvertiseToPolarHosts(hostID string, skills []AdvertisedSkill, remoteIP string, seenAt time.Time) {
	if s.polarHostsTouchURL == "" {
		return
	}
	body, err := json.Marshal(polarHostsTouchPayload{
		HostID:   hostID,
		Skills:   skills,
		RemoteIP: remoteIP,
		SeenAt:   seenAt,
	})
	if err != nil {
		log.Printf("[agent ws] forward touch marshal host=%s: %v", hostID, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.polarHostsTouchURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[agent ws] forward touch build host=%s: %v", hostID, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[agent ws] forward touch send host=%s: %v", hostID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[agent ws] forward touch reject host=%s status=%d", hostID, resp.StatusCode)
		return
	}
}
