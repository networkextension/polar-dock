package dock

// Agent Identity v4 — see doc/arch/agent-identity-v4.md.
//
// Splits the v3 (hosts row = physical machine + UNIQUE machine_uuid)
// design into two tables:
//
//   hosts  = physical asset, PK = sha256(salt + raw_machine_uuid)[:32]
//   agents = logical instance, PK = ag_<random32hex>, FK → hosts
//
// One host : N agents. The raw machine_uuid is sent once per register
// (via the SDK's AgentRegister method) and never persisted.
//
// This file contains:
//   - hashHostID(rawUUID) — server-side salted derivation
//   - upsertHostV4(host_id, …) — INSERT-or-refresh by id
//   - mintAgent(host_id, name, …) — INSERT into agents
//   - registerAgent(req) — full chain (steps 1-6 from the design doc)
//   - ensureWorkspaceProxyToken(ws, name) — idempotent llm_proxy_tokens
//   - ensureBotForAgent(ws, name, …) — idempotent bot_user creation
//
// All callable from the /internal/v1/* handlers in
// internal_api_handlers.go.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// devHostUUIDSalt is the fallback used when $POLAR_HOST_UUID_SALT
	// is unset. NEVER use this in prod — it makes host_id values
	// reproducible by anyone with knowledge of the constant.
	devHostUUIDSalt = "polar-host-uuid-default-salt-CHANGE-ME"
)

var (
	hostUUIDSaltOnce sync.Once
	hostUUIDSaltVal  []byte
)

// hostUUIDSalt returns the configured salt as bytes. Caches the result
// + warns once if the env var is unset.
func hostUUIDSalt() []byte {
	hostUUIDSaltOnce.Do(func() {
		v := strings.TrimSpace(os.Getenv("POLAR_HOST_UUID_SALT"))
		if v == "" {
			log.Printf("[v4] WARN POLAR_HOST_UUID_SALT unset — using insecure dev fallback; set this env var on every dock deploy.")
			v = devHostUUIDSalt
		}
		hostUUIDSaltVal = []byte(v)
	})
	return hostUUIDSaltVal
}

// hashHostID returns sha256(salt + raw)[:32] hex. Empty raw is an
// error: v4 register MUST carry the raw uuid (the SDK enforces this
// client-side; we re-check defensively).
func hashHostID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("hashHostID: empty raw machine uuid")
	}
	h := sha256.New()
	h.Write(hostUUIDSalt())
	h.Write([]byte(raw))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:32], nil
}

// newAgentID returns "ag_" + 32 hex chars. ~128 bits of entropy.
func newAgentID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ag_" + hex.EncodeToString(b), nil
}

// upsertHostV4 INSERTs a host row if id is new, otherwise refreshes
// last_seen_at + hardware facts. Slug is derived from name on insert
// only — re-registers keep the original slug because it's a URL
// component.
func (s *Server) upsertHostV4(hostID, workspaceID, name, hostOS, hostArch string, now time.Time) error {
	hostID = strings.TrimSpace(hostID)
	workspaceID = strings.TrimSpace(workspaceID)
	name = strings.TrimSpace(name)
	if hostID == "" || workspaceID == "" || name == "" {
		return errors.New("upsertHostV4: id/workspace/name required")
	}
	slug, err := s.uniqueHostSlugInWorkspace(workspaceID, name)
	if err != nil {
		// Slug collision on UPDATE path is fine — fall back to a stable
		// "host-<id-prefix>" if the operator picked a name that collides
		// in another row of the same workspace.
		slug = "host-" + hostID[:8]
	}
	_, err = s.db.Exec(
		`INSERT INTO hosts (id, workspace_id, slug, name, os, arch, last_seen_at, first_seen_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $7)
		 ON CONFLICT (id) DO UPDATE
		   SET name           = EXCLUDED.name,
		       os             = COALESCE(NULLIF(EXCLUDED.os, ''),   hosts.os),
		       arch           = COALESCE(NULLIF(EXCLUDED.arch, ''), hosts.arch),
		       last_seen_at   = EXCLUDED.last_seen_at`,
		hostID, workspaceID, slug, name, hostOS, hostArch, now,
	)
	return err
}

// mintAgent inserts a new agents row. Returns ErrNoRows on
// (workspace_id, name) conflict so callers can surface that as a clean
// 409 / "rename your agent" error.
func (s *Server) mintAgent(agentID, workspaceID, hostID, name, agentTokenID, botUserID, agentOS, agentArch string, now time.Time) error {
	var botPtr any
	if strings.TrimSpace(botUserID) != "" {
		botPtr = botUserID
	}
	_, err := s.db.Exec(
		`INSERT INTO agents (id, workspace_id, host_id, name, bot_user_id, agent_token_id, os, arch, created_at, last_hello_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL)`,
		agentID, workspaceID, hostID, name, botPtr, agentTokenID, agentOS, agentArch, now,
	)
	return err
}

// getAgentByWorkspaceName returns (agent_id, host_id, bot_user_id) for
// the given (workspace, name). sql.ErrNoRows ↔ not found.
func (s *Server) getAgentByWorkspaceName(workspaceID, name string) (id, hostID, botUserID string, err error) {
	var botNullable sql.NullString
	err = s.db.QueryRow(
		`SELECT id, host_id, COALESCE(bot_user_id, '')
		   FROM agents
		  WHERE workspace_id = $1 AND name = $2 LIMIT 1`,
		workspaceID, name,
	).Scan(&id, &hostID, &botNullable)
	if err == nil && botNullable.Valid {
		botUserID = botNullable.String
	}
	return
}

// registerAgent is the v4 register chain entry. Returns the response
// body shape AgentRegisterResponse from the SDK (kept untyped here so
// this file doesn't need to import polar-sdk).
//
// Idempotency: same (workspace, name) → returns the existing agent_id
// + existing token row's plaintext is NOT returnable (we no longer
// have the plaintext). For now we error on the duplicate name with a
// hint to use a unique --name. The design notes (open/deferred) call
// out the UNIQUE(workspace_id, name) constraint as potentially too
// strict; this is the safer side of that debate.
func (s *Server) registerAgent(_ context.Context, req internalAgentRegisterReq) (map[string]any, error) {
	name := strings.TrimSpace(req.Name)
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	if name == "" || workspaceID == "" {
		return nil, errors.New("name and workspace_id required")
	}

	hostID, err := hashHostID(req.MachineUUIDRaw)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	// 1. UPSERT hosts.
	if err := s.upsertHostV4(hostID, workspaceID, name, req.OS, req.Arch, now); err != nil {
		return nil, fmt.Errorf("upsert host: %w", err)
	}

	// Resolve workspace owner for downstream auto-creates (token,
	// proxy token, bot all attach to this user).
	var ownerUserID string
	if err := s.db.QueryRow(
		`SELECT owner_user_id FROM teams WHERE id = $1 LIMIT 1`,
		workspaceID,
	).Scan(&ownerUserID); err != nil {
		return nil, fmt.Errorf("workspace owner lookup: %w", err)
	}

	// 2. Idempotency check on (workspace, name). If the agent already
	//    exists we DON'T mint a new token — caller should re-issue an
	//    enroll if they lost the agent.toml.
	if existingID, existingHost, existingBot, lookupErr := s.getAgentByWorkspaceName(workspaceID, name); lookupErr == nil && existingID != "" {
		if existingHost != hostID {
			log.Printf("[v4 register] WARN agent %s name=%q migrated host: %s → %s", existingID, name, existingHost, hostID)
			_, _ = s.db.Exec(`UPDATE agents SET host_id = $2 WHERE id = $1`, existingID, hostID)
		}
		// Re-issue the per-agent proxy token (decrypts the stored copy,
		// or mints one for agents that predate P2). Best-effort.
		proxyRaw, perr := s.ensureAgentProxyToken(existingID, workspaceID, ownerUserID, now)
		if perr != nil {
			log.Printf("[v4 register] agent %s proxy token (idempotent): %v", existingID, perr)
		}
		proxyBase, defaultModel := s.agentProxyResponseFields(workspaceID)
		return map[string]any{
			"agent_id":        existingID,
			"host_id":         hostID,
			"bot_user_id":     existingBot,
			"agent_token_raw": "",
			"server":          s.canonicalServerURL(),
			"idempotent":      true,
			"proxy_token":     proxyRaw,
			"proxy_base_url":  proxyBase,
			"default_model":   defaultModel,
		}, nil
	} else if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return nil, fmt.Errorf("agent lookup: %w", lookupErr)
	}

	// 3. Mint agent_token. Reuses generateAgentToken from agent_store.go.
	rawToken, hashHex, err := generateAgentToken()
	if err != nil {
		return nil, fmt.Errorf("mint token: %w", err)
	}
	agentTokenID := generateResourceID()
	if _, err := s.insertAgentTokenRow(agentTokenID, ownerUserID, "agent:"+name, hashHex, "{}", now); err != nil {
		return nil, fmt.Errorf("agent_token insert: %w", err)
	}

	// 4. Ensure workspace agent-pool proxy token.
	proxyTokenID, _, err := s.ensureWorkspaceProxyToken(workspaceID, ownerUserID, "agent-pool")
	if err != nil {
		return nil, fmt.Errorf("ensure proxy token: %w", err)
	}

	// 5. Bot: respect caller override; otherwise auto-create.
	botUserID := strings.TrimSpace(req.BotUserID)
	if botUserID == "" {
		botName := fmt.Sprintf("bot-%s-%s", slugifyAgentName(name), shortAgentID(""))
		var bcreated bool
		botUserID, bcreated, err = s.ensureBotForAgent(
			workspaceID, ownerUserID, botName,
			"Auto-created for agent "+name,
			map[string]any{"proxy_token_id": proxyTokenID},
		)
		if err != nil {
			return nil, fmt.Errorf("ensure bot: %w", err)
		}
		_ = bcreated // logged inside ensureBotForAgent if needed
	}

	// 6. Mint agent_id + INSERT agents row.
	agentID, err := newAgentID()
	if err != nil {
		return nil, fmt.Errorf("agent_id: %w", err)
	}
	if err := s.mintAgent(agentID, workspaceID, hostID, name, agentTokenID, botUserID, req.OS, req.Arch, now); err != nil {
		return nil, fmt.Errorf("insert agent: %w", err)
	}

	// 7. Issue the per-agent LLM proxy token (agent:<id>). Best-effort:
	//    a failure here must not fail register — dispatch falls back to
	//    a per-run token and the agent simply has no own-token yet.
	proxyRaw, perr := s.ensureAgentProxyToken(agentID, workspaceID, ownerUserID, now)
	if perr != nil {
		log.Printf("[v4 register] agent %s proxy token: %v", agentID, perr)
	}
	proxyBase, defaultModel := s.agentProxyResponseFields(workspaceID)

	return map[string]any{
		"agent_id":        agentID,
		"host_id":         hostID,
		"bot_user_id":     botUserID,
		"agent_token_raw": rawToken,
		"server":          s.canonicalServerURL(),
		"proxy_token":     proxyRaw,
		"proxy_base_url":  proxyBase,
		"default_model":   defaultModel,
	}, nil
}

// canonicalServerURL returns the operator-facing URL the agent should use for
// its WS connect. Prefer the dock's configured public base URL (s.publicBaseURL,
// from $PUBLIC_BASE_URL) — the agent clobbers its --server with this value, so a
// loopback here left the whole fleet unable to connect. $POLAR_PUBLIC_BASE_URL
// stays an explicit override; localhost is the last-resort dev fallback.
func (s *Server) canonicalServerURL() string {
	if v := strings.TrimSpace(os.Getenv("POLAR_PUBLIC_BASE_URL")); v != "" {
		return v
	}
	if s.publicBaseURL != "" {
		return s.publicBaseURL
	}
	return "http://127.0.0.1:8080"
}

// slugifyAgentName mirrors uniqueHostSlugInWorkspace's input shaping
// but doesn't probe the DB (bot names live in a different table).
func slugifyAgentName(name string) string {
	base := strings.ToLower(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, " ", "-")
	base = slugSanitize.ReplaceAllString(base, "")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "agent"
	}
	if len(base) > 32 {
		base = base[:32]
	}
	return base
}

// shortAgentID returns 8 random hex chars when id is empty, or
// id's last-segment slice otherwise. Used for bot name disambiguation
// across same-name agents.
func shortAgentID(id string) string {
	if strings.HasPrefix(id, "ag_") && len(id) > 11 {
		return id[3:11]
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ensureWorkspaceProxyToken returns (id, created, err). Looks up the
// workspace's llm_proxy_tokens row matching (workspace_id, name) and
// creates one with sensible defaults if missing. The created token's
// raw plaintext is NOT surfaced (this method is for internal bot
// binding; admins create user-visible tokens via the LLM billing UI).
func (s *Server) ensureWorkspaceProxyToken(workspaceID, ownerUserID, name string) (id int64, created bool, err error) {
	workspaceID = strings.TrimSpace(workspaceID)
	name = strings.TrimSpace(name)
	if workspaceID == "" || name == "" {
		return 0, false, errors.New("workspace_id and name required")
	}
	// Probe.
	err = s.db.QueryRow(
		`SELECT id FROM llm_proxy_tokens
		  WHERE workspace_id = $1 AND name = $2 AND revoked_at IS NULL
		  ORDER BY id ASC LIMIT 1`,
		workspaceID, name,
	).Scan(&id)
	if err == nil && id > 0 {
		return id, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	// Fall back to workspace owner when caller didn't supply one.
	if strings.TrimSpace(ownerUserID) == "" {
		_ = s.db.QueryRow(
			`SELECT owner_user_id FROM teams WHERE id = $1 LIMIT 1`,
			workspaceID,
		).Scan(&ownerUserID)
	}
	if strings.TrimSpace(ownerUserID) == "" {
		return 0, false, errors.New("no owner_user_id available for proxy token")
	}
	// Create with default rate-limit/quota/scope; admins can edit later.
	tok, cerr := s.createLLMProxyToken(workspaceID, ownerUserID, name, nil, 60, 0, "chat.completions", time.Now().UTC())
	if cerr != nil {
		return 0, false, cerr
	}
	return tok.ID, true, nil
}

// ensureBotForAgent returns (bot_user_id, created, err). Looks up an
// existing bot row by (workspace_id, name); creates one bound to a
// passthrough LLM config when missing. LLMConfig's "proxy_token_id"
// key — when present — is the bigserial id of the workspace's proxy
// token; the bot's llm_config_id is left NULL because passthrough bots
// resolve their upstream lazily.
func (s *Server) ensureBotForAgent(workspaceID, ownerUserID, name, description string, llmConfig map[string]any) (botUserID string, created bool, err error) {
	workspaceID = strings.TrimSpace(workspaceID)
	name = strings.TrimSpace(name)
	if workspaceID == "" || name == "" {
		return "", false, errors.New("workspace_id and name required")
	}
	// Probe by (workspace, name).
	err = s.db.QueryRow(
		`SELECT bot_user_id FROM bot_users
		  WHERE workspace_id = $1 AND name = $2 LIMIT 1`,
		workspaceID, name,
	).Scan(&botUserID)
	if err == nil && botUserID != "" {
		return botUserID, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if strings.TrimSpace(ownerUserID) == "" {
		_ = s.db.QueryRow(
			`SELECT owner_user_id FROM teams WHERE id = $1 LIMIT 1`,
			workspaceID,
		).Scan(&ownerUserID)
	}
	if strings.TrimSpace(ownerUserID) == "" {
		return "", false, errors.New("no owner_user_id for bot create")
	}
	// Use the passthrough kind so llm_config_id=0 is acceptable
	// (validateLLMConfig only runs for classic bots).
	bot, cerr := s.createBotUser(CreateBotUserInput{
		WorkspaceID: workspaceID,
		OwnerUserID: ownerUserID,
		Name:        name,
		Description: description,
		BotKind:     BotKindPassthrough,
	}, time.Now().UTC())
	if cerr != nil {
		return "", false, cerr
	}
	if bot == nil {
		return "", false, errors.New("createBotUser returned nil (workspace/llm validation)")
	}
	// llm_config hint left as a TODO: the dock-side resolution of
	// passthrough_proxy_token → bot is deferred to Phase E client work;
	// for now we just record the binding intent in bot_users.description.
	_ = llmConfig
	return bot.BotUserID, true, nil
}
