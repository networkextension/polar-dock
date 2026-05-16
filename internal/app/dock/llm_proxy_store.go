package dock

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LLMProxyToken is one row of llm_proxy_tokens. Plaintext is NEVER
// stored — it's surfaced once at create-time on the response struct
// (`Plaintext` field) and immediately discarded server-side.
type LLMProxyToken struct {
	ID                 int64      `json:"id"`
	WorkspaceID        string     `json:"workspace_id"`
	OwnerUserID        string     `json:"owner_user_id"`
	Name               string     `json:"name"`
	Prefix             string     `json:"prefix"`
	DefaultLLMConfigID *int64     `json:"default_llm_config_id,omitempty"`
	RateLimitRPM       int        `json:"rate_limit_rpm"`
	MonthlyQuotaUSD    float64    `json:"monthly_quota_usd"`
	Scopes             string     `json:"scopes"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	// Plaintext is set ONLY on create. Never read back from DB.
	Plaintext string `json:"plaintext,omitempty"`
}

// LLMProxyCall is one row of llm_proxy_calls.
type LLMProxyCall struct {
	ID               int64     `json:"id"`
	TokenID          int64     `json:"token_id"`
	WorkspaceID      string    `json:"workspace_id"`
	ModelRequested   string    `json:"model_requested"`
	ModelResolved    string    `json:"model_resolved"`
	LLMConfigID      *int64    `json:"llm_config_id,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	LatencyMS        int       `json:"latency_ms"`
	StatusCode       int       `json:"status_code"`
	ErrorText        string    `json:"error_text,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

const llmProxyTokenPlaintextPrefix = "polar_proxy_"

// newLLMProxyTokenPlaintext returns "polar_proxy_" + 32 random hex
// chars (≈128 bits of entropy). The first 20 chars of the full
// string (prefix + 8 hex) are stored as `prefix` for UI display.
func newLLMProxyTokenPlaintext() (plaintext, prefix string) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	plaintext = llmProxyTokenPlaintextPrefix + hex.EncodeToString(b)
	prefix = plaintext[:len(llmProxyTokenPlaintextPrefix)+8]
	return
}

func hashLLMProxyToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func (s *Server) createLLMProxyToken(workspaceID, ownerUserID, name string, defaultLLMConfigID *int64, rateLimitRPM int, monthlyQuotaUSD float64, scopes string, now time.Time) (*LLMProxyToken, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name required")
	}
	if rateLimitRPM <= 0 {
		rateLimitRPM = 60
	}
	if monthlyQuotaUSD < 0 {
		monthlyQuotaUSD = 0
	}
	scopes = strings.TrimSpace(scopes)
	if scopes == "" {
		scopes = "chat.completions"
	}
	plaintext, prefix := newLLMProxyTokenPlaintext()
	hash := hashLLMProxyToken(plaintext)

	out := &LLMProxyToken{
		WorkspaceID:        workspaceID,
		OwnerUserID:        ownerUserID,
		Name:               name,
		Prefix:             prefix,
		DefaultLLMConfigID: defaultLLMConfigID,
		RateLimitRPM:       rateLimitRPM,
		MonthlyQuotaUSD:    monthlyQuotaUSD,
		Scopes:             scopes,
		CreatedAt:          now,
		Plaintext:          plaintext,
	}
	err := s.db.QueryRow(
		`INSERT INTO llm_proxy_tokens (workspace_id, owner_user_id, name, prefix, token_hash, default_llm_config_id, rate_limit_rpm, monthly_quota_usd, scopes, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		workspaceID, ownerUserID, name, prefix, hash, defaultLLMConfigID, rateLimitRPM, monthlyQuotaUSD, scopes, now,
	).Scan(&out.ID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) listLLMProxyTokens(workspaceID string) ([]LLMProxyToken, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_id, owner_user_id, name, prefix, default_llm_config_id, rate_limit_rpm, monthly_quota_usd, scopes, revoked_at, created_at, last_used_at
		   FROM llm_proxy_tokens
		  WHERE workspace_id = $1
		  ORDER BY created_at DESC, id DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LLMProxyToken, 0)
	for rows.Next() {
		var t LLMProxyToken
		var defaultLLMConfigID sql.NullInt64
		var revokedAt, lastUsedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.OwnerUserID, &t.Name, &t.Prefix, &defaultLLMConfigID, &t.RateLimitRPM, &t.MonthlyQuotaUSD, &t.Scopes, &revokedAt, &t.CreatedAt, &lastUsedAt); err != nil {
			return nil, err
		}
		if defaultLLMConfigID.Valid {
			v := defaultLLMConfigID.Int64
			t.DefaultLLMConfigID = &v
		}
		if revokedAt.Valid {
			rv := revokedAt.Time
			t.RevokedAt = &rv
		}
		if lastUsedAt.Valid {
			lv := lastUsedAt.Time
			t.LastUsedAt = &lv
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// revokeLLMProxyToken sets revoked_at to now. Idempotent: a row that's
// already revoked just keeps its original revoked_at. Returns the new
// row state or (nil, nil) when the id doesn't belong to the workspace.
func (s *Server) revokeLLMProxyToken(workspaceID string, id int64, now time.Time) (*LLMProxyToken, error) {
	res, err := s.db.Exec(
		`UPDATE llm_proxy_tokens
		    SET revoked_at = COALESCE(revoked_at, $3)
		  WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID, now,
	)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, nil
	}
	return s.getLLMProxyToken(workspaceID, id)
}

func (s *Server) getLLMProxyToken(workspaceID string, id int64) (*LLMProxyToken, error) {
	var t LLMProxyToken
	var defaultLLMConfigID sql.NullInt64
	var revokedAt, lastUsedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, workspace_id, owner_user_id, name, prefix, default_llm_config_id, rate_limit_rpm, monthly_quota_usd, scopes, revoked_at, created_at, last_used_at
		   FROM llm_proxy_tokens
		  WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID,
	).Scan(&t.ID, &t.WorkspaceID, &t.OwnerUserID, &t.Name, &t.Prefix, &defaultLLMConfigID, &t.RateLimitRPM, &t.MonthlyQuotaUSD, &t.Scopes, &revokedAt, &t.CreatedAt, &lastUsedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if defaultLLMConfigID.Valid {
		v := defaultLLMConfigID.Int64
		t.DefaultLLMConfigID = &v
	}
	if revokedAt.Valid {
		rv := revokedAt.Time
		t.RevokedAt = &rv
	}
	if lastUsedAt.Valid {
		lv := lastUsedAt.Time
		t.LastUsedAt = &lv
	}
	return &t, nil
}

// lookupLLMProxyTokenByHash is the auth side of the proxy: take the
// plaintext bearer sent by the client, hash it, find the row. Returns
// nil when the hash doesn't match anything OR the row is revoked.
// Token caller should also touch_last_used after a successful auth.
func (s *Server) lookupLLMProxyTokenByHash(hash string) (*LLMProxyToken, error) {
	var t LLMProxyToken
	var defaultLLMConfigID sql.NullInt64
	var revokedAt, lastUsedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, workspace_id, owner_user_id, name, prefix, default_llm_config_id, rate_limit_rpm, monthly_quota_usd, scopes, revoked_at, created_at, last_used_at
		   FROM llm_proxy_tokens
		  WHERE token_hash = $1 AND revoked_at IS NULL`,
		hash,
	).Scan(&t.ID, &t.WorkspaceID, &t.OwnerUserID, &t.Name, &t.Prefix, &defaultLLMConfigID, &t.RateLimitRPM, &t.MonthlyQuotaUSD, &t.Scopes, &revokedAt, &t.CreatedAt, &lastUsedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if defaultLLMConfigID.Valid {
		v := defaultLLMConfigID.Int64
		t.DefaultLLMConfigID = &v
	}
	if lastUsedAt.Valid {
		lv := lastUsedAt.Time
		t.LastUsedAt = &lv
	}
	return &t, nil
}

func (s *Server) touchLLMProxyTokenLastUsed(id int64, now time.Time) {
	_, _ = s.db.Exec(`UPDATE llm_proxy_tokens SET last_used_at = $2 WHERE id = $1`, id, now)
}

func (s *Server) insertLLMProxyCall(call *LLMProxyCall) error {
	if call == nil {
		return fmt.Errorf("nil call")
	}
	var llmConfigID sql.NullInt64
	if call.LLMConfigID != nil {
		llmConfigID = sql.NullInt64{Int64: *call.LLMConfigID, Valid: true}
	}
	return s.db.QueryRow(
		`INSERT INTO llm_proxy_calls (token_id, workspace_id, model_requested, model_resolved, llm_config_id, prompt_tokens, completion_tokens, total_tokens, latency_ms, status_code, error_text, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 RETURNING id`,
		call.TokenID, call.WorkspaceID, call.ModelRequested, call.ModelResolved, llmConfigID,
		call.PromptTokens, call.CompletionTokens, call.TotalTokens,
		call.LatencyMS, call.StatusCode, call.ErrorText, call.CreatedAt,
	).Scan(&call.ID)
}
