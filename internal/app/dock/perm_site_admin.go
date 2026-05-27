package dock

// perm_site_admin.go — site-admin identity + audit-log helper for
// the permission-model P0 (doc/arch/permission-model.md).
//
// **Site admin** = member of the root team (teams.is_root = TRUE)
// with team_members.role IN ('owner','admin'). This is the load-bearing
// definition; users.role='admin' is the legacy flag and is on the way
// out (P4 of the migration plan).
//
// Both checks are exposed here so future code paths can use them
// directly:
//   - IsSiteAdmin(userID)         → bool, error
//   - LogAdminAction(c, in)       → writes one admin_audit_log row
//   - SiteAdmin* category constants for action_category
//
// This PR is additive only — AdminMiddleware in middleware.go still
// uses users.role. P1 flips that to call IsSiteAdmin internally.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Action-category constants — match the four buckets in §0.1 of the
// design doc. Callers MUST use one of these strings so the table is
// grep-able and the future audit-log viewer can filter cleanly.
const (
	AuditCatProvisioning   = "provisioning"
	AuditCatReconciliation = "reconciliation"
	AuditCatAuditRead      = "audit_read"
	AuditCatConfig         = "config"
)

// Actor-role constants — the role at the time the action ran. Stored
// denormalised so demoting an admin later doesn't blank the trail.
const (
	ActorRoleSiteAdmin    = "site_admin"
	ActorRoleSiteOwner    = "site_owner"
	ActorRoleLegacyAdmin  = "users.role=admin" // legacy bridge during P0/P1
	ActorRoleSystem       = "system"
)

// siteAdminCacheTTL — how long to trust the result of a team_members
// lookup. Site-admin membership changes are rare; 30s is the same TTL
// we use for AuthVerify.
const siteAdminCacheTTL = 30 * time.Second

type siteAdminCacheEntry struct {
	isAdmin   bool
	storeTime time.Time
}

// siteAdminCache lives on Server (initialised lazily in IsSiteAdmin).
// Keyed by userID; never grows unbounded because the cardinality of
// site admins is small (handful of humans), and stale entries get
// overwritten on next lookup.
type siteAdminCache struct {
	mu      sync.Mutex
	entries map[string]siteAdminCacheEntry
}

// IsSiteAdmin returns true when userID is a member of the root team
// with team_members.role IN ('owner','admin'). Returns (false, nil)
// when there's no root team yet — that's the pre-seed state and the
// safe default is "no one is site admin".
//
// Cached for siteAdminCacheTTL per userID. The cache lives on the
// Server, lazily initialised, so concurrent boots / tests are fine.
func (s *Server) IsSiteAdmin(userID string) (bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, nil
	}
	// Lazy-init the cache; safe under contention because we re-check
	// under the lock.
	if s.siteAdminCache == nil {
		s.siteAdminCache = &siteAdminCache{entries: map[string]siteAdminCacheEntry{}}
	}
	c := s.siteAdminCache

	c.mu.Lock()
	if entry, ok := c.entries[userID]; ok && time.Since(entry.storeTime) < siteAdminCacheTTL {
		c.mu.Unlock()
		return entry.isAdmin, nil
	}
	c.mu.Unlock()

	var role string
	err := s.db.QueryRow(`
		SELECT tm.role
		FROM team_members tm
		JOIN teams t ON t.id = tm.team_id
		WHERE t.is_root = TRUE AND tm.user_id = $1`,
		userID,
	).Scan(&role)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.mu.Lock()
		c.entries[userID] = siteAdminCacheEntry{isAdmin: false, storeTime: time.Now()}
		c.mu.Unlock()
		return false, nil
	case err != nil:
		return false, err
	}
	isAdmin := role == "owner" || role == "admin"
	c.mu.Lock()
	c.entries[userID] = siteAdminCacheEntry{isAdmin: isAdmin, storeTime: time.Now()}
	c.mu.Unlock()
	return isAdmin, nil
}

// invalidateSiteAdminCache — called when team_members rows change
// (member added/removed/role-changed in the root team) so the next
// IsSiteAdmin call re-queries instead of returning stale data.
//
// Best-effort: if cache hasn't been built yet, no-op.
func (s *Server) invalidateSiteAdminCache(userID string) {
	if s.siteAdminCache == nil {
		return
	}
	s.siteAdminCache.mu.Lock()
	delete(s.siteAdminCache.entries, strings.TrimSpace(userID))
	s.siteAdminCache.mu.Unlock()
}

// AdminAuditIn is the input to LogAdminAction. See doc/arch/permission-model.md
// §3.5 for the contract.
//
// Payload should capture the before/after delta of the mutation:
//   - create:  {"after": <row>}
//   - update:  {"before": <row>, "after": <row>}
//   - delete:  {"before": <row>}
// Secrets (plugin tokens, API keys, password hashes) MUST be redacted
// before serialisation — that's the caller's responsibility.
type AdminAuditIn struct {
	Category     string         // AuditCat* constant
	Action       string         // free-form, snake_case (e.g. "plugin_enable")
	TargetTeamID string         // "" when action targets the site itself
	TargetKind   string         // 'team' | 'plugin_module' | 'llm_config' | ...
	TargetID     string         // free-form id of the targeted row
	Payload      map[string]any // before/after delta
	Note         string         // human-readable free text (optional)
}

// LogAdminAction writes one admin_audit_log row. Call AFTER the
// underlying mutation succeeds so we never log an action that didn't
// happen. The actor identity is read from the gin context (set by
// AuthMiddleware): user_id + role.
//
// Errors are logged but not returned — audit is best-effort and the
// caller shouldn't fail the user-visible request just because the
// audit row didn't land. Symmetry with how observability metrics are
// recorded.
//
// Source IP + User-Agent are pulled from the request headers and
// stored verbatim for forensics.
func (s *Server) LogAdminAction(c *gin.Context, in AdminAuditIn) {
	if s == nil || s.db == nil {
		return
	}
	if strings.TrimSpace(in.Category) == "" || strings.TrimSpace(in.Action) == "" {
		log.Printf("LogAdminAction: missing category/action (action=%q category=%q) — skipping", in.Action, in.Category)
		return
	}
	userIDAny, _ := c.Get("user_id")
	userID, _ := userIDAny.(string)
	if strings.TrimSpace(userID) == "" {
		log.Printf("LogAdminAction: no user_id on context for action=%s — skipping (anonymous mutation?)", in.Action)
		return
	}
	// P1: AdminMiddleware stamps `gate_via` so we know which predicate
	// let the request through. Fall back to inspecting `role` if the
	// stamp is missing (e.g. an admin endpoint that doesn't use
	// AdminMiddleware but still calls LogAdminAction directly).
	actorRole := ActorRoleSiteAdmin
	if gateViaAny, ok := c.Get("gate_via"); ok {
		if g, _ := gateViaAny.(string); g == "users.role=admin" {
			actorRole = ActorRoleLegacyAdmin
		}
	} else {
		// No gate stamp — old behavior: infer from session role.
		roleAny, _ := c.Get("role")
		if role, _ := roleAny.(string); role == "admin" {
			actorRole = ActorRoleLegacyAdmin
		}
	}

	payload := in.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("LogAdminAction: marshal payload for action=%s failed: %v — storing {}", in.Action, err)
		payloadJSON = []byte("{}")
	}

	var targetTeamID interface{}
	if t := strings.TrimSpace(in.TargetTeamID); t != "" {
		targetTeamID = t
	}

	sourceIP := c.ClientIP()
	userAgent := c.GetHeader("User-Agent")
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}

	_, err = s.db.Exec(`
		INSERT INTO admin_audit_log
			(actor_user_id, actor_role, action_category, action,
			 target_team_id, target_kind, target_id, payload, note,
			 source_ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11)`,
		userID, actorRole, in.Category, in.Action,
		targetTeamID, in.TargetKind, in.TargetID, string(payloadJSON), in.Note,
		sourceIP, userAgent,
	)
	if err != nil {
		log.Printf("LogAdminAction: insert for action=%s actor=%s failed: %v", in.Action, userID, err)
	}
}

// bootstrapRootTeam — implements permission-model.md §3.3 boot rule:
//   1. If teams.is_root = TRUE exists → no-op.
//   2. Else if exactly one users.role = 'admin' exists → promote that
//      user's personal team to is_root + ensure team_members owner row.
//   3. Else → loud log telling the operator to run a manual step.
//
// Called once at startup, after ensureSystemUser() so the system user
// exists but is NOT counted as the "lone admin" (system is always
// role=admin by construction; it would always win otherwise).
//
// Never returns error: bootstrap failure shouldn't block dock boot,
// the existing AdminMiddleware (legacy users.role) keeps working
// during P0 anyway.
func (s *Server) bootstrapRootTeam() {
	// Step 1: already seeded?
	var rootCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM teams WHERE is_root = TRUE`).Scan(&rootCount); err != nil {
		log.Printf("perm: bootstrap probe failed: %v", err)
		return
	}
	if rootCount > 0 {
		return
	}

	// Step 2: exactly one non-system admin?
	rows, err := s.db.Query(`SELECT id FROM users WHERE role = 'admin' AND id <> 'system' ORDER BY created_at LIMIT 2`)
	if err != nil {
		log.Printf("perm: bootstrap admin scan failed: %v", err)
		return
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			candidates = append(candidates, id)
		}
	}
	rows.Close()
	if len(candidates) != 1 {
		// Step 3: ambiguous — operator must pick.
		log.Printf("perm: NO root team yet AND %d non-system admins found — refusing to auto-promote. "+
			"Pick one and run: UPDATE teams SET is_root=TRUE WHERE id IN (SELECT id FROM teams WHERE owner_user_id=<chosen>) LIMIT 1; "+
			"then add team_members(team_id, user_id, role='owner', joined_at=NOW()).", len(candidates))
		return
	}
	adminID := candidates[0]

	// Find that user's personal team (one of kind='personal' rows).
	var personalTeamID string
	err = s.db.QueryRow(`SELECT id FROM teams WHERE owner_user_id = $1 AND kind = 'personal' ORDER BY created_at LIMIT 1`, adminID).Scan(&personalTeamID)
	if err != nil {
		log.Printf("perm: bootstrap: lone admin %s has no personal team yet — skipping; will retry on next boot", adminID)
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("perm: bootstrap tx begin failed: %v", err)
		return
	}
	if _, err := tx.Exec(`UPDATE teams SET is_root = TRUE, updated_at = NOW() WHERE id = $1`, personalTeamID); err != nil {
		_ = tx.Rollback()
		log.Printf("perm: bootstrap promote team %s failed: %v", personalTeamID, err)
		return
	}
	// Idempotent owner row.
	if _, err := tx.Exec(`
		INSERT INTO team_members (team_id, user_id, role, joined_at)
		VALUES ($1, $2, 'owner', NOW())
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = 'owner'`,
		personalTeamID, adminID,
	); err != nil {
		_ = tx.Rollback()
		log.Printf("perm: bootstrap owner-membership insert failed: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("perm: bootstrap commit failed: %v", err)
		return
	}
	log.Printf("perm: bootstrapped root team — user_id=%s personal_team_id=%s (auto-promoted; sole admin)", adminID, personalTeamID)
}

// handleInternalSiteAdminCheck — small helper endpoint so plugins
// can ask "is this user a site admin?" without re-implementing the
// query. Wired in app.go on the /internal/v1 group. Useful for the
// future tenant-access gate (P1) where plugins want to bypass the
// workspace_plugin_access check for site admins.
//
// Query: ?user_id=<id>
// Resp:  {"user_id":"...", "is_site_admin": true|false}
func (s *Server) handleInternalSiteAdminCheck(c *gin.Context) {
	userID := strings.TrimSpace(c.Query("user_id"))
	if userID == "" {
		internalErr(c, http.StatusBadRequest, "user_id required")
		return
	}
	isAdmin, err := s.IsSiteAdmin(userID)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "site-admin lookup failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user_id":       userID,
		"is_site_admin": isAdmin,
	})
}
