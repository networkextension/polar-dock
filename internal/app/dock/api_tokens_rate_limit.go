package dock

import (
	"sync"
	"time"
)

// apiTokenRateLimiter is an in-memory sliding-window limiter per
// api_tokens.id — each open-API token gets at most `rpm` requests in any
// rolling 60-second window. Mirrors llmProxyRateLimiter: process-local,
// resets on dock restart (the limit is a runaway-client safety net, not a
// billing meter — api_token_calls is the authoritative usage record).
type apiTokenRateLimiter struct {
	mu      sync.Mutex
	windows map[int64][]int64 // tokenID -> unix-seconds of recent allowed calls
}

func newAPITokenRateLimiter() *apiTokenRateLimiter {
	return &apiTokenRateLimiter{windows: map[int64][]int64{}}
}

// allow returns (true, 0) when the call fits under the per-minute budget,
// else (false, retryAfterSec) clamped to [1,60]. rpm <= 0 = no limit.
func (l *apiTokenRateLimiter) allow(tokenID int64, rpm int) (bool, int) {
	if rpm <= 0 {
		return true, 0
	}
	now := time.Now().Unix()
	cutoff := now - 60

	l.mu.Lock()
	defer l.mu.Unlock()
	ring := l.windows[tokenID]

	i := 0
	for i < len(ring) && ring[i] < cutoff {
		i++
	}
	if i > 0 {
		ring = ring[i:]
	}

	if len(ring) >= rpm {
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
