package dock

import "testing"

func TestPctZeroTotal(t *testing.T) {
	if v := pct(100, 0); v != 0 {
		t.Fatalf("pct(_, 0): got %v, want 0", v)
	}
}

func TestPctHalf(t *testing.T) {
	if v := pct(50, 100); v != 50 {
		t.Fatalf("pct(50,100): got %v, want 50", v)
	}
}

func TestParseLinuxMeminfo(t *testing.T) {
	sample := `MemTotal:       16384000 kB
MemFree:         8192000 kB
MemAvailable:   12288000 kB
Buffers:          128000 kB
`
	total, avail := parseLinuxMeminfo(sample)
	if total != 16384000*1024 {
		t.Fatalf("total: got %d, want %d", total, 16384000*1024)
	}
	if avail != 12288000*1024 {
		t.Fatalf("avail: got %d, want %d", avail, 12288000*1024)
	}
}

func TestParseDarwinVMStatAvailable(t *testing.T) {
	// Snippet of real `vm_stat` output. Page size omitted (we fall back
	// to default 16384 on Apple Silicon, which is what the sample
	// reflects).
	sample := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                1000.
Pages active:                              5000.
Pages inactive:                            2000.
Pages speculative:                          500.
Pages throttled:                              0.
`
	// available = (1000 + 2000 + 500) * 16384 = 3500 * 16384 = 57344000
	got := parseDarwinVMStatAvailable(sample)
	want := uint64(3500 * 16384)
	if got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestVMStatNumberHandlesCommas(t *testing.T) {
	if n := vmStatNumber("Pages free:                          1,234,567."); n != 1234567 {
		t.Fatalf("got %d, want 1234567", n)
	}
}

func TestVMStatNumberHandlesPlain(t *testing.T) {
	if n := vmStatNumber("Pages free:                          42."); n != 42 {
		t.Fatalf("got %d, want 42", n)
	}
}

func TestGatherSystemInfoBasicFields(t *testing.T) {
	// Smoke: structure populates without panicking on the test host.
	// Doesn't assert exact values — those vary per machine — but does
	// assert the shape is reasonable.
	si := gatherSystemInfo()
	if si.Hostname == "" {
		t.Errorf("hostname empty")
	}
	if si.OS == "" {
		t.Errorf("os empty")
	}
	if si.Arch == "" {
		t.Errorf("arch empty")
	}
	if si.CPU.Count <= 0 {
		t.Errorf("cpu count: got %d", si.CPU.Count)
	}
	if si.Version.Go == "" {
		t.Errorf("go version empty")
	}
}
