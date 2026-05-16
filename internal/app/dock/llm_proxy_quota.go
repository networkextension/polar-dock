package dock

import (
	"sync"
	"time"
)

// Monthly quota enforcement — P4. Uses the cost_usd column landed
// in P6.
//
// Strategy: in-memory per-token cache of "USD spent so far this UTC
// month", refreshed from the DB every 60 seconds. Worst case we
// over-spend by up to ~60s of traffic past the cap; for the v0
// safety-net usage that's acceptable. A future Phase could bump the
// cache in-line after each successful insert to tighten the window
// to zero drift, but it'd be a write-after-write race that's not
// worth the complexity right now.
//
// Month boundary: bucketed by UTC year-month. When the month rolls
// over, the previous bucket is implicitly discarded — next allow()
// call sees the new bucket key and queries fresh.

const llmProxyQuotaTTL = 60 * time.Second

type llmProxyQuotaTracker struct {
	server *Server

	mu    sync.Mutex
	state map[int64]*llmProxyQuotaEntry
}

type llmProxyQuotaEntry struct {
	spendUSD    float64
	refreshedAt time.Time
	monthBucket string // "2006-01"
}

func newLLMProxyQuotaTracker(s *Server) *llmProxyQuotaTracker {
	return &llmProxyQuotaTracker{server: s, state: map[int64]*llmProxyQuotaEntry{}}
}

// allow returns (ok, currentSpend). ok=false means the call would
// exceed the cap (caller must reject). quotaUSD <= 0 means
// "unlimited" — admin opt-out — and always returns (true, 0)
// without touching the cache (don't query DB for tokens with no
// cap).
//
// Fails OPEN on DB error — we don't want a degraded DB to block
// customer traffic. The operator already sees DB pool exhaustion
// via polar_db_pool_* and the LLM proxy upstream-up gauge.
func (t *llmProxyQuotaTracker) allow(tokenID int64, quotaUSD float64) (bool, float64) {
	if quotaUSD <= 0 {
		return true, 0
	}
	spend := t.currentSpend(tokenID)
	return spend < quotaUSD, spend
}

func (t *llmProxyQuotaTracker) currentSpend(tokenID int64) float64 {
	bucket := time.Now().UTC().Format("2006-01")
	t.mu.Lock()
	if entry, ok := t.state[tokenID]; ok && entry.monthBucket == bucket && time.Since(entry.refreshedAt) < llmProxyQuotaTTL {
		s := entry.spendUSD
		t.mu.Unlock()
		return s
	}
	t.mu.Unlock()

	spend, err := t.queryMonthSpend(tokenID)
	if err != nil {
		// Fail-open: log the read miss but don't block.
		return 0
	}
	t.mu.Lock()
	t.state[tokenID] = &llmProxyQuotaEntry{
		spendUSD:    spend,
		refreshedAt: time.Now(),
		monthBucket: bucket,
	}
	t.mu.Unlock()
	return spend
}

func (t *llmProxyQuotaTracker) queryMonthSpend(tokenID int64) (float64, error) {
	if t.server == nil || t.server.db == nil {
		return 0, nil
	}
	var v float64
	err := t.server.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0)::FLOAT8
		   FROM llm_proxy_calls
		  WHERE token_id   = $1
		    AND created_at >= date_trunc('month', NOW())`,
		tokenID,
	).Scan(&v)
	return v, err
}
