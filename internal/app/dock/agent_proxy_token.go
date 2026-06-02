package dock

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Per-agent LLM proxy token (P2 of doc/arch/agent-proxy-and-task-dispatch.md).
//
// At register the agent is issued a durable proxy token named
// agent:<agent_id>, defaulting to the workspace's default llm_config.
// The raw is returned to the agent once (for agent-initiated proxy
// calls) AND stored on the agents row — encrypted when
// POLAR_CREDENTIAL_KEY is set, else plaintext with encrypted=false —
// so research dispatch can embed it without the agent binary round-
// tripping. Routing the spend through this token gives per-agent
// billing + audit attribution.

// agentProxyRow is the per-agent token state stored on the agents row.
type agentProxyRow struct {
	AgentID     string
	WorkspaceID string
	TokenID     int64
	Enc         string
	Encrypted   bool
}

// ensureAgentProxyToken returns the agent's raw per-agent proxy token,
// minting + storing it on first call and reading back the stored copy
// thereafter. Best-effort: returns ("", err) on failure and the caller
// proceeds without it (register still succeeds; dispatch falls back to
// a per-run token).
func (s *Server) ensureAgentProxyToken(agentID, workspaceID, ownerUserID string, now time.Time) (string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" || strings.TrimSpace(workspaceID) == "" {
		return "", errors.New("agentID + workspaceID required")
	}
	var enc sql.NullString
	var encrypted sql.NullBool
	err := s.db.QueryRow(
		`SELECT proxy_token_enc, proxy_token_encrypted FROM agents WHERE id = $1`,
		agentID,
	).Scan(&enc, &encrypted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if enc.Valid && strings.TrimSpace(enc.String) != "" {
		return s.decryptStoredAgentProxyToken(enc.String, encrypted.Bool)
	}

	// Mint fresh, defaulting to the workspace's default llm_config so
	// agent-initiated calls (no alias) still resolve to a model.
	var defID *int64
	if id, derr := s.getWorkspaceDefaultLLMConfigID(workspaceID); derr == nil && id > 0 {
		defID = &id
	}
	tok, err := s.createLLMProxyToken(workspaceID, ownerUserID, "agent:"+agentID, defID, 600, 0, "chat.completions", now)
	if err != nil || tok == nil || strings.TrimSpace(tok.Plaintext) == "" {
		return "", fmt.Errorf("mint agent proxy token: %w", err)
	}
	blob, sealed, encErr := s.encryptCoderCredential(tok.Plaintext)
	if encErr != nil {
		return "", encErr
	}
	stored := blob
	if !sealed {
		// No credential key configured — store plaintext, mark unsealed.
		stored = tok.Plaintext
	}
	if _, uerr := s.db.Exec(
		`UPDATE agents SET proxy_token_id = $2, proxy_token_enc = $3, proxy_token_encrypted = $4 WHERE id = $1`,
		agentID, tok.ID, stored, sealed,
	); uerr != nil {
		return "", uerr
	}
	return tok.Plaintext, nil
}

func (s *Server) decryptStoredAgentProxyToken(stored string, encrypted bool) (string, error) {
	if !encrypted {
		return stored, nil
	}
	return s.decryptCoderCredential(stored)
}

// getAgentProxyByBotUserID fetches the per-agent token state for the
// agent bound to a bot, for research dispatch. Returns (nil, nil) when
// no agent / no token yet (caller falls back to a per-run token).
func (s *Server) getAgentProxyByBotUserID(botUserID string) (*agentProxyRow, error) {
	botUserID = strings.TrimSpace(botUserID)
	if botUserID == "" {
		return nil, nil
	}
	var r agentProxyRow
	var tokenID sql.NullInt64
	var enc sql.NullString
	var encrypted sql.NullBool
	err := s.db.QueryRow(
		`SELECT id, workspace_id, proxy_token_id, proxy_token_enc, proxy_token_encrypted
		   FROM agents WHERE bot_user_id = $1 LIMIT 1`,
		botUserID,
	).Scan(&r.AgentID, &r.WorkspaceID, &tokenID, &enc, &encrypted)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if !tokenID.Valid || !enc.Valid || strings.TrimSpace(enc.String) == "" {
		return nil, nil
	}
	r.TokenID = tokenID.Int64
	r.Enc = enc.String
	r.Encrypted = encrypted.Bool
	return &r, nil
}

// upsertLLMProxyRoute idempotently maps an alias model to an llm_config
// on a token. Used at dispatch to route a research task's chosen config
// under the agent's per-agent token.
func (s *Server) upsertLLMProxyRoute(tokenID int64, alias string, llmConfigID int64, now time.Time) error {
	alias = strings.TrimSpace(alias)
	if tokenID <= 0 || alias == "" || llmConfigID <= 0 {
		return errors.New("tokenID, alias, llmConfigID required")
	}
	_, err := s.db.Exec(
		`INSERT INTO llm_proxy_routes (token_id, alias_model, llm_config_id, sort_order, created_at)
		 VALUES ($1, $2, $3, 0, $4)
		 ON CONFLICT (token_id, alias_model)
		 DO UPDATE SET llm_config_id = EXCLUDED.llm_config_id`,
		tokenID, alias, llmConfigID, now,
	)
	return err
}

// agentProxyResponseFields returns the proxy base URL + default model
// name to hand back to the agent in the register response.
func (s *Server) agentProxyResponseFields(workspaceID string) (baseURL, defaultModel string) {
	if base := s.proxyPublicBaseURL(); base != "" {
		baseURL = base + "/api/proxy/v1"
	}
	if id, err := s.getWorkspaceDefaultLLMConfigID(workspaceID); err == nil && id > 0 {
		if cfg, _, err := s.getLLMConfigForOwner(workspaceID, id); err == nil && cfg != nil {
			defaultModel = strings.TrimSpace(cfg.Name)
		}
	}
	return baseURL, defaultModel
}
