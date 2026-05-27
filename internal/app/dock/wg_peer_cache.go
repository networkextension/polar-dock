package dock

// wg_peer_cache.go — in-memory cache of `skill.event` frames where
// `event_kind == "metric"` and `data.kind == "wg_peer_status"`.
//
// One slot per (host_id, iface) — the agent only runs one wireguard
// sample loop per iface, so this is the natural key. The cache is
// fed from `runAgentReadPump` in agent_handlers.go and read by
// `/internal/v1/wg-peer-status` (for plugins like polar-wg) and the
// future host detail SSE in dock-ui.
//
// No DB persistence — peers churn every 30s, history goes to
// Prometheus if anyone wants it. See doc/arch/issue-344.

import (
	"sync"
	"time"
)

// wgPeerSampleMaxAge — drop entries older than this on read. Samples
// flow every ~30s (configurable agent-side, range 5–300s). 10 min
// is generous so a temporarily-slow agent doesn't blink out, but the
// table still self-cleans when the wireguard skill stops running.
const wgPeerSampleMaxAge = 10 * time.Minute

// wgPeerSample is one snapshot the agent reported. We hold the
// already-decoded `data` map verbatim (the agent owns the shape) plus
// a few extracted fields used by indexing and TTL.
type wgPeerSample struct {
	HostID         string         // dock-side host id
	Iface          string         // e.g. "wg0"
	IfacePublicKey string         // plugins use this to join to wg_hubs
	RunID          int64          // for exit-driven eviction
	RecordedAt     time.Time      // dock-side wall clock
	Data           map[string]any // raw `data` payload from the agent
}

// wgPeerCache — thread-safe map of (host_id, iface) → latest sample.
type wgPeerCache struct {
	mu      sync.RWMutex
	entries map[wgPeerKey]wgPeerSample
}

type wgPeerKey struct {
	HostID string
	Iface  string
}

func newWGPeerCache() *wgPeerCache {
	return &wgPeerCache{entries: map[wgPeerKey]wgPeerSample{}}
}

// upsert stores the latest sample for (host_id, iface). Silently
// no-ops if hostID or iface is empty — defensive against malformed
// agent frames so a missing field never wipes a good entry.
func (c *wgPeerCache) upsert(s wgPeerSample) {
	if s.HostID == "" || s.Iface == "" {
		return
	}
	if s.RecordedAt.IsZero() {
		s.RecordedAt = time.Now().UTC()
	}
	c.mu.Lock()
	c.entries[wgPeerKey{HostID: s.HostID, Iface: s.Iface}] = s
	c.mu.Unlock()
}

// evictByRun removes every entry whose RunID matches — called on
// `skill.event{exit}` so a stopped wireguard run doesn't leave a
// stale sample lingering until TTL.
func (c *wgPeerCache) evictByRun(runID int64) {
	if runID == 0 {
		return
	}
	c.mu.Lock()
	for k, v := range c.entries {
		if v.RunID == runID {
			delete(c.entries, k)
		}
	}
	c.mu.Unlock()
}

// list returns every non-expired sample, optionally filtered by
// host_id or iface_public_key. Empty filters match everything.
func (c *wgPeerCache) list(hostID, ifacePublicKey string) []wgPeerSample {
	cutoff := time.Now().Add(-wgPeerSampleMaxAge)
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]wgPeerSample, 0, len(c.entries))
	for _, v := range c.entries {
		if v.RecordedAt.Before(cutoff) {
			continue
		}
		if hostID != "" && v.HostID != hostID {
			continue
		}
		if ifacePublicKey != "" && v.IfacePublicKey != ifacePublicKey {
			continue
		}
		out = append(out, v)
	}
	return out
}
