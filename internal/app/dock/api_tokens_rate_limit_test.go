package dock

import "testing"

func TestAPITokenRateLimiter(t *testing.T) {
	l := newAPITokenRateLimiter()

	// rpm=3: first 3 allowed, 4th rejected with a Retry-After in [1,60].
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow(1, 3); !ok {
			t.Fatalf("call %d should be allowed under rpm=3", i+1)
		}
	}
	ok, retry := l.allow(1, 3)
	if ok {
		t.Fatal("4th call should be rejected under rpm=3")
	}
	if retry < 1 || retry > 60 {
		t.Fatalf("retryAfter = %d, want [1,60]", retry)
	}

	// rpm<=0 means unlimited.
	for i := 0; i < 100; i++ {
		if ok, _ := l.allow(2, 0); !ok {
			t.Fatal("rpm=0 must never reject")
		}
	}

	// Limits are per-token: token 3 unaffected by token 1's exhausted window.
	if ok, _ := l.allow(3, 3); !ok {
		t.Fatal("a different token should have its own budget")
	}
}
