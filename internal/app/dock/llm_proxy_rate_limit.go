package dock

import (
	"sync"
	"time"
)

// llmProxyRateLimiter is an in-memory sliding-window limiter per
// llm_proxy_tokens.id. Each token gets at most `rpm` chat-completion
// requests in any rolling 60-second window. Survives across calls
// inside one dock process; resets on dock restart.
//
// Persisting the window is intentionally avoided — the limit is a
// safety net against runaway clients, not a billing meter. A restart
// briefly resets it; that's fine. The cost ledger (P6) will be the
// authoritative usage record.
//
// Concurrency: single mutex; chat dispatch is dominated by upstream
// HTTP latency (hundreds of ms), so the contention here is noise.
// If that ever changes, shard by tokenID%N.
type llmProxyRateLimiter struct {
	mu      sync.Mutex
	windows map[int64][]int64 // tokenID -> unix-seconds of recent allowed calls
}

func newLLMProxyRateLimiter() *llmProxyRateLimiter {
	return &llmProxyRateLimiter{windows: map[int64][]int64{}}
}

// allow returns (true, 0) when the call fits under the per-minute
// budget. When rejected returns (false, retryAfterSec) — the
// seconds-until-the-oldest-call-falls-out-of-the-window, clamped to
// [1, 60].
//
// rpm <= 0 means "no limit" (admin opted out by setting 0 explicitly).
func (l *llmProxyRateLimiter) allow(tokenID int64, rpm int) (bool, int) {
	if rpm <= 0 {
		return true, 0
	}
	now := time.Now().Unix()
	cutoff := now - 60

	l.mu.Lock()
	defer l.mu.Unlock()
	ring := l.windows[tokenID]

	// Prune entries that fell out of the 60-second window.
	i := 0
	for i < len(ring) && ring[i] < cutoff {
		i++
	}
	if i > 0 {
		ring = ring[i:]
	}

	if len(ring) >= rpm {
		// Reject. Retry-After = seconds until ring[0] ages out.
		retry := int(60 - (now - ring[0]))
		if retry < 1 {
			retry = 1
		}
		if retry > 60 {
			retry = 60
		}
		l.windows[tokenID] = ring
		return false, retry
	}

	ring = append(ring, now)
	l.windows[tokenID] = ring
	return true, 0
}
