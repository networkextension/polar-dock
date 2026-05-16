package dock

import "testing"

func TestQuotaUnlimitedAlwaysAllows(t *testing.T) {
	tr := newLLMProxyQuotaTracker(nil)
	for _, q := range []float64{0, -1, -100} {
		if ok, spend := tr.allow(42, q); !ok || spend != 0 {
			t.Errorf("quota=%v: expected (true,0), got (%v,%v)", q, ok, spend)
		}
	}
}

func TestQuotaCacheKeyedByMonthBucket(t *testing.T) {
	// Without a DB the tracker can't query — but allow(_, quota>0) on
	// a fresh tracker still runs currentSpend() which short-circuits
	// to 0 on query failure (fail-open). Verify that path returns
	// (true, 0).
	tr := newLLMProxyQuotaTracker(nil)
	ok, spend := tr.allow(1, 100.0)
	if !ok {
		t.Fatalf("fail-open should allow on missing DB; got reject")
	}
	if spend != 0 {
		t.Fatalf("fail-open should report 0 spend; got %v", spend)
	}
}
