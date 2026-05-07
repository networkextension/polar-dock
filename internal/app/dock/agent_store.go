package dock

// Storage layer for agent_tokens (long-lived credentials given to
// polar-agent CLI instances) and the in-memory agentHub that
// tracks live WebSocket connections. Schema lives in store.go.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const agentTokenPrefix = "polar_agent_"

// AgentToken is the public-facing row. Raw token is only returned
// at creation; subsequent reads only see the hash.
type AgentToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func generateAgentToken() (raw, hashHex string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw = agentTokenPrefix + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:]), nil
}

func hashAgentToken(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// createAgentToken inserts a new token row and returns the raw
// token string (caller shows it to the user once). Hash is what's
// persisted; raw can never be retrieved again.
func (s *Server) createAgentToken(userID, name string) (string, *AgentToken, error) {
	if userID == "" {
		return "", nil, errors.New("userID is empty")
	}
	raw, hashHex, err := generateAgentToken()
	if err != nil {
		return "", nil, err
	}
	tok := &AgentToken{
		ID:        generateResourceID(),
		UserID:    userID,
		Name:      strings.TrimSpace(name),
		CreatedAt: time.Now().UTC(),
	}
	if _, err := s.db.Exec(
		`INSERT INTO agent_tokens (id, user_id, name, token_hash, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		tok.ID, tok.UserID, tok.Name, hashHex, tok.CreatedAt,
	); err != nil {
		return "", nil, err
	}
	return raw, tok, nil
}

func (s *Server) listAgentTokensForUser(userID string) ([]AgentToken, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name, last_used_at, revoked_at, created_at
		 FROM agent_tokens WHERE user_id = $1 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentToken{}
	for rows.Next() {
		var t AgentToken
		var lastUsedAt, revokedAt sql.NullTime
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &lastUsedAt, &revokedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		if lastUsedAt.Valid {
			v := lastUsedAt.Time
			t.LastUsedAt = &v
		}
		if revokedAt.Valid {
			v := revokedAt.Time
			t.RevokedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// resolveAgentToken returns the (id, user_id) of the live token
// matching the provided raw bearer string, or empty if not found /
// revoked.
func (s *Server) resolveAgentToken(raw string) (string, string, error) {
	if raw == "" {
		return "", "", nil
	}
	h := hashAgentToken(raw)
	var id, userID string
	err := s.db.QueryRow(
		`SELECT id, user_id FROM agent_tokens
		 WHERE token_hash = $1 AND revoked_at IS NULL LIMIT 1`,
		h,
	).Scan(&id, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, userID, err
}

func (s *Server) touchAgentTokenUsed(tokenID string) error {
	_, err := s.db.Exec(`UPDATE agent_tokens SET last_used_at = NOW() WHERE id = $1`, tokenID)
	return err
}

func (s *Server) revokeAgentToken(tokenID, userID string) error {
	res, err := s.db.Exec(
		`UPDATE agent_tokens SET revoked_at = NOW()
		 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		tokenID, userID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
