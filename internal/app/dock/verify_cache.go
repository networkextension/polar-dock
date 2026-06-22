package dock

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Shared token-verification cache — Phase 1 of the control-plane /
// data-plane split (see doc/arch/control-plane-data-plane-split.md).
//
// Session tokens are already redis-native (session_store.go). The other
// credential types (agent / llm_proxy / plugin-HMAC / api tokens) are a
// Postgres QueryRow per request. This helper puts a short-TTL redis
// cache in front of those lookups so:
//   1. the auth hot path stops hitting Postgres on every request, and
//   2. (combined with /internal/v1/auth/verify-agent) credential
//      verification is no longer bound to the dock process — the
//      prerequisite for extracting the LLM proxy and other services.
//
// Keys are namespaced `{prefix}:verify:{kind}:{key}` and the `key` is
// always a TOKEN HASH (never a raw secret), so the cache never holds a
// usable credential. A one-byte "-" payload is a negative entry
// (known-invalid) with a shorter TTL to blunt brute-force scans.
//
// Invalidation is best-effort `Del` on revoke/rotate; combined with the
// short positive TTL this bounds the stale-credential window to
// verifyCachePosTTL. The whole layer is gated by POLAR_VERIFY_CACHE so
// it can be killed instantly if a revocation-latency issue surfaces.

const (
	verifyCachePosTTL = 30 * time.Second // positive entries
	verifyCacheNegTTL = 5 * time.Second  // negative (invalid/not-found) entries
)

var verifyCacheNegative = []byte("-")

func (s *Server) verifyKey(kind, key string) string {
	prefix := DefaultRedisPrefix
	if s.redisPrefix != "" {
		prefix = s.redisPrefix
	}
	return fmt.Sprintf("%s:verify:%s:%s", prefix, kind, key)
}

// verifyCacheGet reports the cached state for (kind, key):
//   - hit=false           → cache miss / disabled / redis error → caller hits Postgres
//   - hit=true, neg=true  → cached negative (known-invalid); caller returns "not found"
//   - hit=true, neg=false → positive; `out` is populated from the cached JSON
func (s *Server) verifyCacheGet(kind, key string, out any) (neg bool, hit bool) {
	if !s.verifyCache || s.redis == nil || key == "" {
		return false, false
	}
	b, err := s.redis.Get(context.Background(), s.verifyKey(kind, key)).Bytes()
	if err != nil {
		return false, false // miss or transient redis error → fall back to PG
	}
	if len(b) == 1 && b[0] == '-' {
		return true, true
	}
	if err := json.Unmarshal(b, out); err != nil {
		return false, false
	}
	return false, true
}

// verifyCacheSet stores a positive entry. Best-effort: redis errors are ignored.
func (s *Server) verifyCacheSet(kind, key string, val any) {
	if !s.verifyCache || s.redis == nil || key == "" {
		return
	}
	b, err := json.Marshal(val)
	if err != nil {
		return
	}
	_ = s.redis.Set(context.Background(), s.verifyKey(kind, key), b, verifyCachePosTTL).Err()
}

// verifyCacheSetNegative caches a known-invalid lookup for a short window.
func (s *Server) verifyCacheSetNegative(kind, key string) {
	if !s.verifyCache || s.redis == nil || key == "" {
		return
	}
	_ = s.redis.Set(context.Background(), s.verifyKey(kind, key), verifyCacheNegative, verifyCacheNegTTL).Err()
}

// verifyCacheDel removes an entry on revoke/rotate. Runs regardless of the
// POLAR_VERIFY_CACHE flag so flipping the flag off never strands a stale
// positive entry that a later flip-on would serve.
func (s *Server) verifyCacheDel(kind, key string) {
	if s.redis == nil || key == "" {
		return
	}
	_ = s.redis.Del(context.Background(), s.verifyKey(kind, key)).Err()
}
