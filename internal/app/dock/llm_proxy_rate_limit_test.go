package dock

import "testing"

func TestLLMProxyRateLimiterAllowsUnderLimit(t *testing.T) {
	l := newLLMProxyRateLimiter()
	for i := 0; i < 5; i++ {
		ok, retry := l.allow(42, 10)
		if !ok {
			t.Fatalf("call %d: expected allow under limit, got reject (retry=%d)", i, retry)
		}
	}
}

func TestLLMProxyRateLimiterRejectsOverLimit(t *testing.T) {
	l := newLLMProxyRateLimiter()
	// Fill the window
	for i := 0; i < 3; i++ {
		ok, _ := l.allow(7, 3)
		if !ok {
			t.Fatalf("call %d: should fit", i)
		}
	}
	// 4th call must reject with retry-after in [1, 60]
	ok, retry := l.allow(7, 3)
	if ok {
		t.Fatalf("4th call exceeded rpm=3 but was allowed")
	}
	if retry < 1 || retry > 60 {
		t.Fatalf("retry-after out of [1,60]: %d", retry)
	}
}

func TestLLMProxyRateLimiterPerToken(t *testing.T) {
	l := newLLMProxyRateLimiter()
	// Token A maxes out
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow(1, 2); !ok {
			t.Fatalf("token 1 call %d should fit", i)
		}
	}
	if ok, _ := l.allow(1, 2); ok {
		t.Fatalf("token 1 should now be over its limit")
	}
	// Token B is independent and starts fresh
	if ok, _ := l.allow(2, 2); !ok {
		t.Fatalf("token 2 should not be affected by token 1's window")
	}
}

func TestLLMProxyRateLimiterZeroIsUnlimited(t *testing.T) {
	l := newLLMProxyRateLimiter()
	// rpm=0 (admin opt-out) must not gate ANY call
	for i := 0; i < 200; i++ {
		if ok, _ := l.allow(9, 0); !ok {
			t.Fatalf("rpm=0 must never reject; rejected at i=%d", i)
		}
	}
}

func TestLLMProxyRateLimiterNegativeIsUnlimited(t *testing.T) {
	l := newLLMProxyRateLimiter()
	if ok, _ := l.allow(9, -1); !ok {
		t.Fatalf("negative rpm should be treated as unlimited")
	}
}
