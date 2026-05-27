package dock

// Storage layer for agent_tokens (long-lived credentials given to
// polar-agent CLI instances) and the in-memory agentHub that
// tracks live WebSocket connections. Schema lives in store.go.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const agentTokenPrefix = "polar_agent_"

// AgentCoderConfig describes per-coder install/auth choices made
// at agent-create time. Used by the install script renderer to
// know which CLIs to brew/uv/install and which auth flow to ask
// the user to complete.
//
// AuthMode values:
//
//	"api_key" — operator will set <TOOL>_API_KEY env (or write to
//	            tool's config file); install script doesn't OAuth
//	"login"   — install script reminds the operator to run the
//	            tool's own browser-OAuth login command after install
type AgentCoderEntry struct {
	Enabled  bool   `json:"enabled"`
	AuthMode string `json:"auth_mode"`
}

type AgentCoderConfig struct {
	Kimi   AgentCoderEntry `json:"kimi"`
	Claude AgentCoderEntry `json:"claude"`
	Codex  AgentCoderEntry `json:"codex"`
}

// AgentToken is the public-facing row. Raw token is only returned
// at creation; subsequent reads only see the hash. host_* columns
// are filled on the agent's first WS attach so the UI can show the
// machine that's holding this token.
type AgentToken struct {
	ID             string           `json:"id"`
	UserID         string           `json:"user_id"`
	Name           string           `json:"name"`
	CoderConfig    AgentCoderConfig `json:"coder_config"`
	HostOS         string           `json:"host_os"`
	HostArch       string           `json:"host_arch"`
	HostName       string           `json:"host_name"`
	HostIP         string           `json:"host_ip"`
	LastAttachedAt *time.Time       `json:"last_attached_at,omitempty"`
	LastUsedAt     *time.Time       `json:"last_used_at,omitempty"`
	RevokedAt      *time.Time       `json:"revoked_at,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
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
func (s *Server) createAgentToken(userID, name string, coders AgentCoderConfig) (string, *AgentToken, error) {
	if userID == "" {
		return "", nil, errors.New("userID is empty")
	}
	raw, hashHex, err := generateAgentToken()
	if err != nil {
		return "", nil, err
	}
	codersJSON, err := json.Marshal(coders)
	if err != nil {
		return "", nil, err
	}
	id := generateResourceID()
	createdAt := time.Now().UTC()
	if _, err := s.insertAgentTokenRow(id, userID, strings.TrimSpace(name), hashHex, string(codersJSON), createdAt); err != nil {
		return "", nil, err
	}
	tok := &AgentToken{
		ID:          id,
		UserID:      userID,
		Name:        strings.TrimSpace(name),
		CoderConfig: coders,
		CreatedAt:   createdAt,
	}
	return raw, tok, nil
}

// insertAgentTokenRow is the shared INSERT used by createAgentToken
// (dock-side mint) and handleInternalAgentTokenIssue (plugin dual-write
// via SDK). Idempotent on id: ON CONFLICT DO NOTHING returns
// inserted=false so a retry from a plugin's transient-failure path
// doesn't 23505. coderConfigJSON must be a valid JSON literal (caller's
// responsibility — pass "{}" if no marker).
func (s *Server) insertAgentTokenRow(id, userID, name, tokenHash, coderConfigJSON string, createdAt time.Time) (inserted bool, err error) {
	if strings.TrimSpace(id) == "" {
		return false, errors.New("id is empty")
	}
	if strings.TrimSpace(userID) == "" {
		return false, errors.New("userID is empty")
	}
	if strings.TrimSpace(tokenHash) == "" {
		return false, errors.New("tokenHash is empty")
	}
	cfg := strings.TrimSpace(coderConfigJSON)
	if cfg == "" {
		cfg = "{}"
	}
	res, err := s.db.Exec(
		`INSERT INTO agent_tokens (id, user_id, name, token_hash, coder_config, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (id) DO NOTHING`,
		id, userID, name, tokenHash, cfg, createdAt,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Server) listAgentTokensForUser(userID string) ([]AgentToken, error) {
	rows, err := s.db.Query(
		`SELECT id, user_id, name,
		        COALESCE(coder_config::text, '{}'),
		        COALESCE(host_os, ''), COALESCE(host_arch, ''),
		        COALESCE(host_name, ''), COALESCE(host_ip, ''),
		        last_attached_at, last_used_at, revoked_at, created_at
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
		var coderJSON string
		var lastAttachedAt, lastUsedAt, revokedAt sql.NullTime
		if err := rows.Scan(
			&t.ID, &t.UserID, &t.Name,
			&coderJSON,
			&t.HostOS, &t.HostArch, &t.HostName, &t.HostIP,
			&lastAttachedAt, &lastUsedAt, &revokedAt, &t.CreatedAt,
		); err != nil {
			return nil, err
		}
		if coderJSON != "" {
			_ = json.Unmarshal([]byte(coderJSON), &t.CoderConfig)
		}
		if lastAttachedAt.Valid {
			v := lastAttachedAt.Time
			t.LastAttachedAt = &v
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

// updateAgentTokenHostInfo records the host details from the
// agent's WS hello frame on first attach. Call once per attach;
// repeated attaches just bump last_attached_at.
func (s *Server) updateAgentTokenHostInfo(tokenID, hostOS, hostArch, hostName, hostIP string) error {
	_, err := s.db.Exec(
		`UPDATE agent_tokens
		 SET host_os = COALESCE(NULLIF($2, ''), host_os),
		     host_arch = COALESCE(NULLIF($3, ''), host_arch),
		     host_name = COALESCE(NULLIF($4, ''), host_name),
		     host_ip = COALESCE(NULLIF($5, ''), host_ip),
		     last_attached_at = NOW()
		 WHERE id = $1`,
		tokenID, hostOS, hostArch, hostName, hostIP,
	)
	return err
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
