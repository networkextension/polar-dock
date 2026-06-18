package dock

import "testing"

func TestNormalizeTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"trim+drop empty", []string{" 职业 ", "", "  "}, []string{"职业"}},
		{"dedup preserves order", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeTags(tc.in)
			if got == nil {
				t.Fatalf("normalizeTags returned nil, want non-nil")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestBotListCacheKeyVariesByParams(t *testing.T) {
	base := botListCacheKey("ws1", 0, "", "", 30, 0)
	// Each varying field must produce a distinct key.
	diffs := map[string]string{
		"version": botListCacheKey("ws1", 1, "", "", 30, 0),
		"q":       botListCacheKey("ws1", 0, "x", "", 30, 0),
		"group":   botListCacheKey("ws1", 0, "", "职业", 30, 0),
		"limit":   botListCacheKey("ws1", 0, "", "", 50, 0),
		"offset":  botListCacheKey("ws1", 0, "", "", 30, 30),
		"ws":      botListCacheKey("ws2", 0, "", "", 30, 0),
	}
	for field, k := range diffs {
		if k == base {
			t.Errorf("key did not change when %s changed", field)
		}
	}
}

func TestBotListCacheGetSet(t *testing.T) {
	c := newBotListCache()
	key := botListCacheKey("ws1", c.version("ws1"), "", "", 30, 0)
	if _, ok := c.get(key); ok {
		t.Fatal("empty cache should miss")
	}
	c.set(key, botListCacheValue{total: 7, groups: []GroupCount{{Name: "职业", Count: 3}}})
	got, ok := c.get(key)
	if !ok {
		t.Fatal("expected hit after set")
	}
	if got.total != 7 || len(got.groups) != 1 || got.groups[0].Name != "职业" {
		t.Fatalf("cached value mismatch: %+v", got)
	}
}

func TestBotListCacheBumpInvalidates(t *testing.T) {
	c := newBotListCache()
	ver0 := c.version("ws1")
	key0 := botListCacheKey("ws1", ver0, "", "", 30, 0)
	c.set(key0, botListCacheValue{total: 1})

	c.bump("ws1") // simulates a bot create/update/delete

	// The same logical request now computes a new key (new version) and misses.
	ver1 := c.version("ws1")
	if ver1 == ver0 {
		t.Fatal("bump did not advance version")
	}
	key1 := botListCacheKey("ws1", ver1, "", "", 30, 0)
	if _, ok := c.get(key1); ok {
		t.Fatal("post-bump key should miss")
	}
	// A bump on another workspace must not touch ws1's version.
	c.bump("ws2")
	if c.version("ws1") != ver1 {
		t.Fatal("bump on ws2 changed ws1 version")
	}
}

func TestBotListCacheEvictsAtCap(t *testing.T) {
	c := newBotListCache()
	for i := 0; i < botListCacheMaxEntries+10; i++ {
		c.set(botListCacheKey("ws", 0, "", "", 30, i), botListCacheValue{total: i})
	}
	if len(c.entries) > botListCacheMaxEntries {
		t.Fatalf("cache exceeded cap: %d > %d", len(c.entries), botListCacheMaxEntries)
	}
}
