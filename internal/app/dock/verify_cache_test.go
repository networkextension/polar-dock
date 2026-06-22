package dock

import "testing"

func TestVerifyKey(t *testing.T) {
	// Default prefix when redisPrefix is unset.
	s := &Server{}
	if got, want := s.verifyKey("agent", "abc"), "polar:verify:agent:abc"; got != want {
		t.Fatalf("default prefix: got %q want %q", got, want)
	}
	// Custom prefix is honored.
	s.redisPrefix = "zen"
	if got, want := s.verifyKey("plugin", "wg"), "zen:verify:plugin:wg"; got != want {
		t.Fatalf("custom prefix: got %q want %q", got, want)
	}
}

// TestVerifyCacheDisabledSafe is the load-bearing test: when the cache is off
// or redis is absent, the helpers must be inert no-ops (never panic on a nil
// redis client, always report a miss) so behavior is identical to the
// pre-cache code path.
func TestVerifyCacheDisabledSafe(t *testing.T) {
	cases := []struct {
		name        string
		verifyCache bool
	}{
		{"flag off, nil redis", false},
		{"flag on, nil redis", true}, // s.redis == nil must still short-circuit
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{verifyCache: tc.verifyCache} // redis is nil
			var out agentTokenIdentity
			if neg, hit := s.verifyCacheGet("agent", "h", &out); neg || hit {
				t.Fatalf("disabled/nil-redis Get must miss: neg=%v hit=%v", neg, hit)
			}
			// These must not panic with a nil redis client.
			s.verifyCacheSet("agent", "h", agentTokenIdentity{ID: "1", UserID: "u"})
			s.verifyCacheSetNegative("agent", "h")
			s.verifyCacheDel("agent", "h")
		})
	}
}

// TestVerifyCacheEmptyKey ensures an empty key never touches redis.
func TestVerifyCacheEmptyKey(t *testing.T) {
	s := &Server{verifyCache: true}
	var out agentTokenIdentity
	if neg, hit := s.verifyCacheGet("agent", "", &out); neg || hit {
		t.Fatalf("empty key must miss: neg=%v hit=%v", neg, hit)
	}
}
