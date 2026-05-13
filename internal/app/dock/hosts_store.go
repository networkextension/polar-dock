package dock

// Host module — Phase 0 store helpers.
//
// A Host is a named machine running polar-agent. It's workspace-scoped
// and linked 1:1 to an agent_tokens row (which holds the bearer auth).
// P0 only exercises the hosts table; host_skills + host_skill_runs are
// declared in applySchema for forward-compat with later phases but no
// helper here writes to them yet.
//
// Enrollment flow:
//   1. Admin calls POST /api/hosts/enroll → createEnrollmentToken()
//      mints a one-time bearer string + an agent_tokens row marked as
//      pending (coder_config={"pending_enrollment":true}). TTL is 1
//      hour, enforced by the consume step.
//   2. Operator runs polar-agent with that token → POST /api/hosts/
//      register → consumeEnrollmentToken() validates + flips the row
//      out of pending state, createHost() inserts the hosts row.
//   3. Agent attaches via WS → getHostByAgentToken() resolves host_id
//      and we cache it on agentConn (see agent_hub.go).
//
// Slug is auto-generated from name at create time; if a collision
// happens we append -2, -3, etc. inside the workspace.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Host is the API + DB representation of a registered remote machine.
type Host struct {
	ID                   string          `json:"id"`
	WorkspaceID          string          `json:"workspace_id"`
	Slug                 string          `json:"slug"`
	Name                 string          `json:"name"`
	AgentTokenID         *string         `json:"agent_token_id,omitempty"`
	OS                   string          `json:"os"`
	Arch                 string          `json:"arch"`
	LastSeenIP           string          `json:"last_seen_ip,omitempty"`
	AdvertisedSkillsJSON json.RawMessage `json:"advertised_skills,omitempty"`
	LastSeenAt           *time.Time      `json:"last_seen_at,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
}

// AdvertisedSkill mirrors the agent-side skill descriptor the WS
// handler persists via updateHostAdvertisedSkills. Kept as a struct
// (rather than json.RawMessage) so callers can introspect without
// re-parsing.
type AdvertisedSkill struct {
	Kind         string         `json:"kind"`
	Version      string         `json:"version,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
}

// pendingEnrollmentMarker is the coder_config JSON shape we use to
// mark an agent_tokens row as "minted but not yet consumed by an
// agent". Cleared by consumeEnrollmentToken on first successful use.
type pendingEnrollmentMarker struct {
	PendingEnrollment bool      `json:"pending_enrollment"`
	WorkspaceID       string    `json:"workspace_id"`
	HostName          string    `json:"host_name"`
	ExpiresAt         time.Time `json:"expires_at"`
}

const enrollmentTokenTTL = time.Hour

// ---- hosts CRUD --------------------------------------------------------

func (s *Server) createHost(workspaceID, name, agentTokenID, hostOS, hostArch string, now time.Time) (*Host, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	name = strings.TrimSpace(name)
	if workspaceID == "" || name == "" {
		return nil, errors.New("workspace_id and name are required")
	}
	slug, err := s.uniqueHostSlugInWorkspace(workspaceID, name)
	if err != nil {
		return nil, fmt.Errorf("generate slug: %w", err)
	}
	host := &Host{
		ID:          "h_" + generateResourceID(),
		WorkspaceID: workspaceID,
		Slug:        slug,
		Name:        name,
		OS:          strings.TrimSpace(hostOS),
		Arch:        strings.TrimSpace(hostArch),
		CreatedAt:   now,
	}
	if strings.TrimSpace(agentTokenID) != "" {
		v := strings.TrimSpace(agentTokenID)
		host.AgentTokenID = &v
	}
	if _, err := s.db.Exec(
		`INSERT INTO hosts (id, workspace_id, slug, name, agent_token_id, os, arch, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		host.ID, host.WorkspaceID, host.Slug, host.Name,
		host.AgentTokenID, host.OS, host.Arch, host.CreatedAt,
	); err != nil {
		return nil, err
	}
	return host, nil
}

func (s *Server) getHostByID(id string) (*Host, error) {
	return s.scanHost(s.db.QueryRow(hostSelectColumns+` WHERE id = $1`, id))
}

func (s *Server) getHostBySlug(workspaceID, slug string) (*Host, error) {
	return s.scanHost(s.db.QueryRow(hostSelectColumns+` WHERE workspace_id = $1 AND slug = $2`, workspaceID, slug))
}

// getHostByAgentToken is on the hot path of every agent WS message
// (called once at attach to cache host_id on agentConn). Backed by the
// partial index idx_hosts_agent_token in the schema.
func (s *Server) getHostByAgentToken(agentTokenID string) (*Host, error) {
	if strings.TrimSpace(agentTokenID) == "" {
		return nil, nil
	}
	return s.scanHost(s.db.QueryRow(hostSelectColumns+` WHERE agent_token_id = $1 LIMIT 1`, agentTokenID))
}

func (s *Server) listHostsForWorkspace(workspaceID string) ([]Host, error) {
	rows, err := s.db.Query(
		hostSelectColumns+` WHERE workspace_id = $1 ORDER BY last_seen_at DESC NULLS LAST, created_at DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Host{}
	for rows.Next() {
		h, err := s.scanHostRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

// updateHostAdvertisedSkills replaces the JSON snapshot and bumps
// last_seen_at + last_seen_ip in one statement. Called from the WS
// dispatch when a skill.advertise message arrives.
func (s *Server) updateHostAdvertisedSkills(hostID string, skills []AdvertisedSkill, remoteIP string, now time.Time) error {
	if strings.TrimSpace(hostID) == "" {
		return errors.New("host_id is required")
	}
	if skills == nil {
		skills = []AdvertisedSkill{}
	}
	payload, err := json.Marshal(skills)
	if err != nil {
		return fmt.Errorf("marshal skills: %w", err)
	}
	_, err = s.db.Exec(
		`UPDATE hosts
		 SET advertised_skills_json = $2,
		     last_seen_at = $3,
		     last_seen_ip = COALESCE(NULLIF($4, ''), last_seen_ip)
		 WHERE id = $1`,
		hostID, string(payload), now, strings.TrimSpace(remoteIP),
	)
	return err
}

// touchHostLastSeen is the no-skills heartbeat — called on every WS
// frame so the UI's "online N seconds ago" stays accurate without
// requiring an advertise.
func (s *Server) touchHostLastSeen(hostID string, now time.Time) error {
	if strings.TrimSpace(hostID) == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE hosts SET last_seen_at = $2 WHERE id = $1`, hostID, now)
	return err
}

func (s *Server) deleteHost(hostID string) error {
	_, err := s.db.Exec(`DELETE FROM hosts WHERE id = $1`, hostID)
	return err
}

// ---- enrollment --------------------------------------------------------

// createEnrollmentToken mints a one-time-use bearer string for an
// operator-initiated "+ Add Host" flow. Stored as a normal agent_tokens
// row (so the agent WS handshake doesn't need a special code path) but
// marked pending via coder_config so consumeEnrollmentToken can
// distinguish from a fully-registered token.
func (s *Server) createEnrollmentToken(userID, workspaceID, hostName string, now time.Time) (string, string, time.Time, error) {
	userID = strings.TrimSpace(userID)
	workspaceID = strings.TrimSpace(workspaceID)
	hostName = strings.TrimSpace(hostName)
	if userID == "" || workspaceID == "" || hostName == "" {
		return "", "", time.Time{}, errors.New("user_id, workspace_id, and host_name are required")
	}
	raw, hashHex, err := generateAgentToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	expiresAt := now.Add(enrollmentTokenTTL)
	marker := pendingEnrollmentMarker{
		PendingEnrollment: true,
		WorkspaceID:       workspaceID,
		HostName:          hostName,
		ExpiresAt:         expiresAt,
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		return "", "", time.Time{}, err
	}
	tokenID := generateResourceID()
	if _, err := s.db.Exec(
		`INSERT INTO agent_tokens (id, user_id, name, token_hash, coder_config, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenID, userID, "enroll:"+hostName, hashHex, string(markerJSON), now,
	); err != nil {
		return "", "", time.Time{}, err
	}
	return tokenID, raw, expiresAt, nil
}

// consumeEnrollmentToken validates a pending token, returns the
// (workspace_id, host_name) the admin chose at mint time, clears the
// pending marker, and returns the (tokenID, userID) so the caller can
// proceed to createHost. Errors map cleanly to HTTP statuses:
//   - sql.ErrNoRows           → 401 (bad token)
//   - errEnrollmentExpired    → 410 (timed out, 1h TTL)
//   - errEnrollmentAlready    → 409 (already consumed)
func (s *Server) consumeEnrollmentToken(rawToken string, now time.Time) (tokenID, userID, workspaceID, hostName string, err error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		err = sql.ErrNoRows
		return
	}
	hash := hashAgentToken(rawToken)
	var coderJSON string
	err = s.db.QueryRow(
		`SELECT id, user_id, COALESCE(coder_config::text, '{}')
		 FROM agent_tokens
		 WHERE token_hash = $1 AND revoked_at IS NULL LIMIT 1`,
		hash,
	).Scan(&tokenID, &userID, &coderJSON)
	if err != nil {
		return
	}
	var marker pendingEnrollmentMarker
	if jerr := json.Unmarshal([]byte(coderJSON), &marker); jerr != nil || !marker.PendingEnrollment {
		err = errEnrollmentAlready
		return
	}
	if now.After(marker.ExpiresAt) {
		err = errEnrollmentExpired
		return
	}
	// Clear the pending marker so subsequent uses get errEnrollmentAlready.
	if _, uerr := s.db.Exec(
		`UPDATE agent_tokens SET coder_config = '{}'::jsonb WHERE id = $1`,
		tokenID,
	); uerr != nil {
		err = uerr
		return
	}
	workspaceID = marker.WorkspaceID
	hostName = marker.HostName
	return
}

var (
	errEnrollmentExpired = errors.New("enrollment token expired")
	errEnrollmentAlready = errors.New("enrollment token already consumed")
)

// ---- internals ---------------------------------------------------------

const hostSelectColumns = `SELECT id, workspace_id, slug, name,
	    agent_token_id, COALESCE(os, ''), COALESCE(arch, ''),
	    COALESCE(last_seen_ip, ''),
	    COALESCE(advertised_skills_json::text, '[]'),
	    last_seen_at, created_at
	  FROM hosts`

// scanHost handles both QueryRow + Query results via the io.Reader-ish
// scanner pattern.
type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Server) scanHost(row rowScanner) (*Host, error) {
	h, err := scanHostInto(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

func (s *Server) scanHostRow(row rowScanner) (*Host, error) {
	return scanHostInto(row)
}

func scanHostInto(row rowScanner) (*Host, error) {
	var h Host
	var agentTokenID sql.NullString
	var lastSeenAt sql.NullTime
	var skillsText string
	if err := row.Scan(
		&h.ID, &h.WorkspaceID, &h.Slug, &h.Name,
		&agentTokenID, &h.OS, &h.Arch, &h.LastSeenIP,
		&skillsText, &lastSeenAt, &h.CreatedAt,
	); err != nil {
		return nil, err
	}
	if agentTokenID.Valid {
		v := agentTokenID.String
		h.AgentTokenID = &v
	}
	if lastSeenAt.Valid {
		v := lastSeenAt.Time
		h.LastSeenAt = &v
	}
	h.AdvertisedSkillsJSON = json.RawMessage(skillsText)
	return &h, nil
}

var slugSanitize = regexp.MustCompile(`[^a-z0-9-]+`)

// uniqueHostSlugInWorkspace turns "My Box" into "my-box", checks the
// (workspace_id, slug) UNIQUE constraint, and appends -2/-3 etc. until
// it finds a free name. Bounded to 50 attempts to defend against
// pathological inputs that all collide.
func (s *Server) uniqueHostSlugInWorkspace(workspaceID, name string) (string, error) {
	base := strings.ToLower(strings.TrimSpace(name))
	base = strings.ReplaceAll(base, " ", "-")
	base = slugSanitize.ReplaceAllString(base, "")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "host"
	}
	if len(base) > 48 {
		base = base[:48]
	}
	candidate := base
	for i := 1; i <= 50; i++ {
		var exists bool
		err := s.db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM hosts WHERE workspace_id = $1 AND slug = $2)`,
			workspaceID, candidate,
		).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
		i++
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return "", errors.New("could not find free slug after 50 attempts")
}
