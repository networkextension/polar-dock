package dock

import "testing"

func TestAssetRootQuotaConfig_RootCapBytes(t *testing.T) {
	cases := []struct {
		mode  string
		value int64
		want  int64
	}{
		{"absolute", 429496729600, 429496729600},
		{"absolute", 0, 0},
		{"partition_pct", 80, -1}, // not yet implemented; -1 = no cap
		{"unknown_mode", 999, -1}, // unknown → no cap (safe default)
	}
	for _, tc := range cases {
		cfg := &AssetRootQuotaConfig{Mode: tc.mode, Value: tc.value}
		got := cfg.rootCapBytes()
		if got != tc.want {
			t.Errorf("rootCapBytes(mode=%q value=%d) = %d want %d", tc.mode, tc.value, got, tc.want)
		}
	}
	if (*AssetRootQuotaConfig)(nil).rootCapBytes() != -1 {
		t.Error("nil config should return -1 (no cap)")
	}
}
