package dock

// api_tokens_prune.go — periodic retention sweep for api_token_calls (P2 audit
// rows). Open-API tokens can be high-volume, so the audit table would grow
// unbounded; this deletes rows older than the retention window once a day.
//
// Retention is POLAR_API_TOKEN_CALLS_RETENTION_DAYS (default 90). Set it to 0
// (or negative) to disable pruning entirely (keep everything).

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"
)

const (
	apiTokenCallsPruneInterval      = 24 * time.Hour
	apiTokenCallsRetentionDaysDeflt = 90
)

// apiTokenCallsRetentionDays resolves the configured retention window. Returns
// 0 when pruning is disabled.
func apiTokenCallsRetentionDays() int {
	v := os.Getenv("POLAR_API_TOKEN_CALLS_RETENTION_DAYS")
	if v == "" {
		return apiTokenCallsRetentionDaysDeflt
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("api_token_calls prune: bad POLAR_API_TOKEN_CALLS_RETENTION_DAYS=%q, using default %d", v, apiTokenCallsRetentionDaysDeflt)
		return apiTokenCallsRetentionDaysDeflt
	}
	if n < 0 {
		return 0
	}
	return n
}

func (s *Server) runAPITokenCallsPrune(ctx context.Context) {
	days := apiTokenCallsRetentionDays()
	if days <= 0 {
		log.Printf("api_token_calls prune: disabled (retention=0)")
		return
	}
	t := time.NewTicker(apiTokenCallsPruneInterval)
	defer t.Stop()
	s.apiTokenCallsPruneOnce(days)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.apiTokenCallsPruneOnce(days)
		}
	}
}

func (s *Server) apiTokenCallsPruneOnce(days int) {
	res, err := s.db.Exec(
		`DELETE FROM api_token_calls WHERE created_at < now() - ($1 || ' days')::interval`,
		days,
	)
	if err != nil {
		log.Printf("api_token_calls prune: delete failed: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("api_token_calls prune: deleted %d rows older than %d days", n, days)
	}
}
