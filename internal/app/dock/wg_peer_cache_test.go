package dock

import (
	"testing"
	"time"
)

func TestWGPeerCache_UpsertAndList(t *testing.T) {
	c := newWGPeerCache()

	now := time.Now().UTC()
	c.upsert(wgPeerSample{
		HostID:         "host-a",
		Iface:          "wg0",
		IfacePublicKey: "PUBKEY-1",
		RunID:          11,
		RecordedAt:     now,
		Data:           map[string]any{"peer_count": float64(3)},
	})
	c.upsert(wgPeerSample{
		HostID:         "host-b",
		Iface:          "wg0",
		IfacePublicKey: "PUBKEY-2",
		RunID:          22,
		RecordedAt:     now,
		Data:           map[string]any{"peer_count": float64(1)},
	})

	if got := c.list("", ""); len(got) != 2 {
		t.Fatalf("list() = %d entries, want 2", len(got))
	}
	if got := c.list("host-a", ""); len(got) != 1 || got[0].HostID != "host-a" {
		t.Fatalf("list(host-a) = %+v, want host-a only", got)
	}
	if got := c.list("", "PUBKEY-2"); len(got) != 1 || got[0].IfacePublicKey != "PUBKEY-2" {
		t.Fatalf("list(pubkey-2) = %+v, want pubkey-2 only", got)
	}
}

func TestWGPeerCache_UpsertReplacesSameKey(t *testing.T) {
	c := newWGPeerCache()
	c.upsert(wgPeerSample{HostID: "h", Iface: "wg0", Data: map[string]any{"peer_count": float64(1)}})
	c.upsert(wgPeerSample{HostID: "h", Iface: "wg0", Data: map[string]any{"peer_count": float64(5)}})
	got := c.list("", "")
	if len(got) != 1 {
		t.Fatalf("len(list) = %d, want 1 — same key should replace", len(got))
	}
	if pc, _ := got[0].Data["peer_count"].(float64); pc != 5 {
		t.Fatalf("peer_count = %v, want 5", pc)
	}
}

func TestWGPeerCache_EvictByRun(t *testing.T) {
	c := newWGPeerCache()
	c.upsert(wgPeerSample{HostID: "h", Iface: "wg0", RunID: 100})
	c.upsert(wgPeerSample{HostID: "h", Iface: "wg1", RunID: 200})
	c.evictByRun(100)
	got := c.list("", "")
	if len(got) != 1 || got[0].Iface != "wg1" {
		t.Fatalf("after evictByRun(100): %+v, want only wg1", got)
	}
}

func TestWGPeerCache_TTLDrop(t *testing.T) {
	c := newWGPeerCache()
	c.upsert(wgPeerSample{
		HostID:     "h",
		Iface:      "wg0",
		RecordedAt: time.Now().Add(-(wgPeerSampleMaxAge + time.Minute)),
	})
	if got := c.list("", ""); len(got) != 0 {
		t.Fatalf("expired entry should be filtered, got %d entries", len(got))
	}
}

func TestWGPeerCache_EmptyKeysIgnored(t *testing.T) {
	c := newWGPeerCache()
	c.upsert(wgPeerSample{HostID: "", Iface: "wg0"})
	c.upsert(wgPeerSample{HostID: "h", Iface: ""})
	if got := c.list("", ""); len(got) != 0 {
		t.Fatalf("samples with empty host or iface should be dropped, got %d", len(got))
	}
}
