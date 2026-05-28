package dock

import "testing"

func TestGenerateAssetShareCode_FormatAndLength(t *testing.T) {
	for _, n := range []int{4, 8, 10, 12, 32, 64} {
		for i := 0; i < 200; i++ {
			code, err := generateAssetShareCode(n)
			if err != nil {
				t.Fatalf("generateAssetShareCode(%d): %v", n, err)
			}
			if len(code) != n {
				t.Errorf("generateAssetShareCode(%d) returned %d-char %q", n, len(code), code)
			}
			if !shortCodeRE.MatchString(code) && n >= 8 && n <= 12 {
				// shortCodeRE accepts 8-12 by spec; longer lengths skip
				// the regex check (no defined regex).
				t.Errorf("code %q doesn't match %s", code, shortCodeRE)
			}
		}
	}
}

func TestGenerateAssetShareCode_RejectsBadN(t *testing.T) {
	if _, err := generateAssetShareCode(3); err == nil {
		t.Error("n=3 should error (below floor)")
	}
	if _, err := generateAssetShareCode(65); err == nil {
		t.Error("n=65 should error (above ceiling)")
	}
}

func TestGenerateAssetShareCode_NoCollisionsAt10kSamples(t *testing.T) {
	const samples = 10000
	seen := make(map[string]struct{}, samples)
	for i := 0; i < samples; i++ {
		code, err := generateAssetShareCode(8)
		if err != nil {
			t.Fatalf("generateAssetShareCode(8): %v", err)
		}
		if _, dup := seen[code]; dup {
			t.Fatalf("collision after %d samples on code %q (62^8=2.2e14; probability of any collision at 10k samples is <1 in 2e6 — flaky test bug, not entropy bug)", i, code)
		}
		seen[code] = struct{}{}
	}
}

func TestGenerateAssetShareCode_AlphabetIsBase62(t *testing.T) {
	allowed := map[rune]bool{}
	for _, r := range shareCodeAlphabet {
		allowed[r] = true
	}
	if len(allowed) != 62 {
		t.Fatalf("shareCodeAlphabet should be 62 distinct chars, got %d", len(allowed))
	}
	for i := 0; i < 1000; i++ {
		code, _ := generateAssetShareCode(10)
		for _, r := range code {
			if !allowed[r] {
				t.Errorf("char %q in code %q is outside the base62 alphabet", r, code)
			}
		}
	}
}
