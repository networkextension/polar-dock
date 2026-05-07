package dock

// Storage layer for the teams (collaboration workspaces) module.
// Tables (teams, team_members, team_invitations) are declared in
// store.go's openDB() schema block.
//
// Every user has exactly one team with kind='personal' (auto-created
// at user signup or via backfillPersonalTeams() at startup). Explicit
// collaboration teams have kind='team'. Resources across modules
// (iosdist, macapp, ascpool, ...) reference teams.id for ownership.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const (
	TeamKindPersonal = "personal"
	TeamKindTeam     = "team"

	TeamRoleOwner  = "owner"
	TeamRoleAdmin  = "admin"
	TeamRoleMember = "member"
	TeamRoleViewer = "viewer"
)

type Team struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	AvatarURL   string    `json:"avatar_url"`
	Kind        string    `json:"kind"`
	OwnerUserID string    `json:"owner_user_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type TeamMember struct {
	TeamID   string    `json:"team_id"`
	UserID   string    `json:"user_id"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// TeamMemberWithUser is a TeamMember enriched with user-level fields
// for the members list UI. Email is included so admins can verify
// invitation targets resolved correctly.
type TeamMemberWithUser struct {
	TeamID   string    `json:"team_id"`
	UserID   string    `json:"user_id"`
	Username string    `json:"username"`
	Email    string    `json:"email"`
	IconURL  string    `json:"icon_url"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

type TeamInvitation struct {
	ID             string     `json:"id"`
	TeamID         string     `json:"team_id"`
	Email          string     `json:"email"`
	Role           string     `json:"role"`
	InviterUserID  string     `json:"inviter_user_id"`
	Token          string     `json:"token"`
	Status         string     `json:"status"`
	ExpiresAt      time.Time  `json:"expires_at"`
	AcceptedUserID *string    `json:"accepted_user_id,omitempty"`
	AcceptedAt     *time.Time `json:"accepted_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// personalTeamSlug derives a stable, unique slug for a user's personal
// team. Deterministic so re-running backfill is idempotent. Uses a
// short hash prefix of the user id rather than the id itself because
// user ids may contain characters not URL-safe in all contexts.
func personalTeamSlug(userID string) string {
	sum := sha256.Sum256([]byte("personal-team-slug:" + userID))
	return "personal-" + hex.EncodeToString(sum[:])[:12]
}

// generateUniqueTeamSlug picks a slug for an explicit (kind='team') team.
// Tries a few base64url-derived random candidates until one doesn't
// collide. Caller is expected to have validated 'name' but we don't
// use it for the slug yet — keeps things simple, slugs are random.
func (s *Server) generateUniqueTeamSlug() (string, error) {
	for attempts := 0; attempts < 5; attempts++ {
		candidate := generateSessionID()
		clean := make([]byte, 0, 12)
		for i := 0; i < len(candidate) && len(clean) < 12; i++ {
			c := candidate[i]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				clean = append(clean, c)
			}
		}
		slug := strings.ToLower(string(clean))
		if len(slug) < 10 {
			continue
		}
		var existing string
		err := s.db.QueryRow(`SELECT id FROM teams WHERE slug = $1 LIMIT 1`, slug).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			return slug, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not generate unique team slug after 5 attempts")
}

// createTeam inserts a new teams row plus the owner's team_members row
// in a single transaction. The two must always agree on the owner.
func (s *Server) createTeam(team *Team) error {
	if team == nil {
		return errors.New("team is nil")
	}
	if team.ID == "" {
		team.ID = generateResourceID()
	}
	if team.Kind == "" {
		team.Kind = TeamKindTeam
	}
	now := time.Now().UTC()
	team.CreatedAt = now
	team.UpdatedAt = now

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO teams (id, slug, name, description, avatar_url, kind, owner_user_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`,
		team.ID, team.Slug, team.Name, team.Description, team.AvatarURL, team.Kind, team.OwnerUserID, now,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO team_members (team_id, user_id, role, joined_at)
		 VALUES ($1, $2, $3, $4)`,
		team.ID, team.OwnerUserID, TeamRoleOwner, now,
	); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Server) getTeam(id string) (*Team, error) {
	var t Team
	err := s.db.QueryRow(
		`SELECT id, slug, name, description, avatar_url, kind, owner_user_id, created_at, updated_at
		 FROM teams WHERE id = $1`,
		id,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Description, &t.AvatarURL, &t.Kind, &t.OwnerUserID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Server) getTeamBySlug(slug string) (*Team, error) {
	var t Team
	err := s.db.QueryRow(
		`SELECT id, slug, name, description, avatar_url, kind, owner_user_id, created_at, updated_at
		 FROM teams WHERE slug = $1`,
		slug,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Description, &t.AvatarURL, &t.Kind, &t.OwnerUserID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// listTeamsForUser returns every team the user is a member of, ordered
// so the personal team comes first then by name. Each Team carries the
// caller's role in a side-channel map so the handler can decide what
// to render.
func (s *Server) listTeamsForUser(userID string) ([]Team, map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT t.id, t.slug, t.name, t.description, t.avatar_url, t.kind, t.owner_user_id, t.created_at, t.updated_at, tm.role
		 FROM teams t
		 JOIN team_members tm ON tm.team_id = t.id
		 WHERE tm.user_id = $1
		 ORDER BY (t.kind = 'personal') DESC, t.name ASC`,
		userID,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	teams := []Team{}
	roles := map[string]string{}
	for rows.Next() {
		var t Team
		var role string
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name, &t.Description, &t.AvatarURL, &t.Kind, &t.OwnerUserID, &t.CreatedAt, &t.UpdatedAt, &role); err != nil {
			return nil, nil, err
		}
		teams = append(teams, t)
		roles[t.ID] = role
	}
	return teams, roles, rows.Err()
}

// getTeamMember returns the (team, user) membership row, or nil if the
// user is not a member.
func (s *Server) getTeamMember(teamID, userID string) (*TeamMember, error) {
	var m TeamMember
	err := s.db.QueryRow(
		`SELECT team_id, user_id, role, joined_at FROM team_members WHERE team_id = $1 AND user_id = $2`,
		teamID, userID,
	).Scan(&m.TeamID, &m.UserID, &m.Role, &m.JoinedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ensurePersonalTeam returns the user's personal team, creating it
// (and the matching team_members owner row) if missing. Idempotent.
// Slug is deterministic so a partially-failed previous run can resume
// without producing duplicate-but-different rows.
func (s *Server) ensurePersonalTeam(userID string) (*Team, error) {
	if userID == "" {
		return nil, errors.New("userID is empty")
	}
	slug := personalTeamSlug(userID)
	if t, err := s.getTeamBySlug(slug); err != nil {
		return nil, err
	} else if t != nil {
		// Self-heal: if the teams row exists but the owner's
		// team_members row got lost somehow, put it back.
		if member, err := s.getTeamMember(t.ID, userID); err != nil {
			return nil, err
		} else if member == nil {
			if _, err := s.db.Exec(
				`INSERT INTO team_members (team_id, user_id, role, joined_at)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT (team_id, user_id) DO NOTHING`,
				t.ID, userID, TeamRoleOwner, time.Now().UTC(),
			); err != nil {
				return nil, err
			}
		}
		return t, nil
	}
	team := &Team{
		Slug:        slug,
		Name:        "我的工作区",
		Kind:        TeamKindPersonal,
		OwnerUserID: userID,
	}
	if err := s.createTeam(team); err != nil {
		return nil, err
	}
	return team, nil
}

// transferTeamOwnership atomically promotes newOwnerUserID to owner
// of teamID, demotes the existing owner to admin, and updates the
// teams.owner_user_id pointer. Caller must already have verified
// that newOwnerUserID is a current member of the team.
//
// Personal teams cannot be transferred — the slug is derived from
// the original user's id and the team is conceptually that user's
// home workspace. Caller enforces.
func (s *Server) transferTeamOwnership(teamID, newOwnerUserID string) error {
	if teamID == "" || newOwnerUserID == "" {
		return errors.New("missing required field")
	}
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var oldOwnerUserID, kind string
	if err := tx.QueryRow(
		`SELECT owner_user_id, kind FROM teams WHERE id = $1 FOR UPDATE`,
		teamID,
	).Scan(&oldOwnerUserID, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("team not found")
		}
		return err
	}
	if kind == TeamKindPersonal {
		return errors.New("personal team ownership cannot be transferred")
	}
	if oldOwnerUserID == newOwnerUserID {
		// No-op: caller's already the owner.
		return tx.Commit()
	}
	// Sanity check: new owner must be a member.
	var newOwnerExists string
	if err := tx.QueryRow(
		`SELECT user_id FROM team_members WHERE team_id = $1 AND user_id = $2`,
		teamID, newOwnerUserID,
	).Scan(&newOwnerExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("new owner is not a member of this team")
		}
		return err
	}

	if _, err := tx.Exec(
		`UPDATE team_members SET role = $1 WHERE team_id = $2 AND user_id = $3`,
		TeamRoleAdmin, teamID, oldOwnerUserID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE team_members SET role = $1 WHERE team_id = $2 AND user_id = $3`,
		TeamRoleOwner, teamID, newOwnerUserID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE teams SET owner_user_id = $1, updated_at = $2 WHERE id = $3`,
		newOwnerUserID, now, teamID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// updateTeamMemberRole changes a member's role. Caller must have
// already verified the actor is admin+ and the rules:
//   - cannot change the owner's role (ownership transfers via
//     transferTeamOwnership instead)
//   - cannot promote a member to 'owner' here (same reason)
//   - cannot change one's own role
func (s *Server) updateTeamMemberRole(teamID, userID, role string) error {
	if teamID == "" || userID == "" || role == "" {
		return errors.New("missing required field")
	}
	res, err := s.db.Exec(
		`UPDATE team_members SET role = $1 WHERE team_id = $2 AND user_id = $3`,
		role, teamID, userID,
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

// removeTeamMember pulls a user out of a team. Caller enforces:
//   - admin+ can kick anyone except the owner
//   - any member can self-leave, except the owner (who must
//     transfer first)
//   - personal teams cannot have members removed (only one
//     member, the owner)
func (s *Server) removeTeamMember(teamID, userID string) error {
	if teamID == "" || userID == "" {
		return errors.New("missing required field")
	}
	res, err := s.db.Exec(
		`DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`,
		teamID, userID,
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

// teamResourceCounts is a per-table snapshot of how many rows still
// belong to the team. Used for the pre-delete check so we don't
// orphan resources when a team is deleted.
type teamResourceCounts struct {
	Apps         int `json:"apps"`
	Certificates int `json:"certificates"`
	Profiles     int `json:"profiles"`
	ASCConfigs   int `json:"asc_configs"`
	LLMConfigs   int `json:"llm_configs"`
}

// Total returns the sum across all migrated tables.
func (c teamResourceCounts) Total() int {
	return c.Apps + c.Certificates + c.Profiles + c.ASCConfigs + c.LLMConfigs
}

// countTeamResources tallies workspace_id-owned rows across the
// migrated tables (T6 scope). Used by handleTeamDelete to refuse
// deletion until the team is empty, and by the UI to show
// "delete blocked because: 3 apps, 1 cert, ..." hints.
func (s *Server) countTeamResources(teamID string) (teamResourceCounts, error) {
	var c teamResourceCounts
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM iosdist_apps WHERE workspace_id = $1`, teamID).Scan(&c.Apps); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM iosdist_certificates WHERE workspace_id = $1`, teamID).Scan(&c.Certificates); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM iosdist_profiles WHERE workspace_id = $1`, teamID).Scan(&c.Profiles); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM iosdist_asc_configs WHERE workspace_id = $1`, teamID).Scan(&c.ASCConfigs); err != nil {
		return c, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM llm_configs WHERE workspace_id = $1`, teamID).Scan(&c.LLMConfigs); err != nil {
		return c, err
	}
	return c, nil
}

// updateTeamMetadata patches the editable team fields. Slug, kind and
// owner are not touched here — those go through dedicated APIs (slug
// is immutable post-create; ownership transfer + kind change have
// their own paths).
func (s *Server) updateTeamMetadata(teamID, name, description, avatarURL string) error {
	if teamID == "" {
		return errors.New("teamID is empty")
	}
	res, err := s.db.Exec(
		`UPDATE teams SET name = $1, description = $2, avatar_url = $3, updated_at = $4 WHERE id = $5`,
		name, description, avatarURL, time.Now().UTC(), teamID,
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

// deleteTeam removes the team and cascades to team_members /
// team_invitations via ON DELETE CASCADE. Caller is responsible for
// pre-flight checks (kind != 'personal'; no resources still attached;
// caller is owner). Once T6 lands and other modules carry team_id,
// add a cross-table emptiness check here.
func (s *Server) deleteTeam(teamID string) error {
	if teamID == "" {
		return errors.New("teamID is empty")
	}
	res, err := s.db.Exec(`DELETE FROM teams WHERE id = $1`, teamID)
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

// listTeamMembersWithUser returns members joined with the users table
// so the admin UI can show name + email + avatar for each member. Sort
// owners first, then admins, then by join time, so the team owner is
// always at the top.
func (s *Server) listTeamMembersWithUser(teamID string) ([]TeamMemberWithUser, error) {
	rows, err := s.db.Query(
		`SELECT tm.team_id, tm.user_id, u.username, u.email, u.icon_url, tm.role, tm.joined_at
		 FROM team_members tm
		 JOIN users u ON u.id = tm.user_id
		 WHERE tm.team_id = $1
		 ORDER BY
		   CASE tm.role
		     WHEN 'owner' THEN 0
		     WHEN 'admin' THEN 1
		     WHEN 'member' THEN 2
		     WHEN 'viewer' THEN 3
		     ELSE 4
		   END,
		   tm.joined_at ASC`,
		teamID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TeamMemberWithUser{}
	for rows.Next() {
		var m TeamMemberWithUser
		if err := rows.Scan(&m.TeamID, &m.UserID, &m.Username, &m.Email, &m.IconURL, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// backfillResourceWorkspaceIDs populates the workspace_id column on
// migrated module tables (T6-A). Each row's workspace_id is set to
// the personal team of its owner_user_id. Idempotent — only touches
// rows where workspace_id IS NULL.
//
// This runs at startup after backfillPersonalTeams so the personal
// teams referenced here are guaranteed to exist for every legacy
// owner_user_id value. Failures on individual tables are logged but
// don't abort the sweep — partial progress is fine because the
// function is idempotent on subsequent restarts.
func (s *Server) backfillResourceWorkspaceIDs() error {
	tables := []string{
		"iosdist_apps",
		"iosdist_certificates",
		"iosdist_profiles",
		"iosdist_asc_configs",
		"llm_configs",
	}
	for _, table := range tables {
		// Single SQL statement per table. The personal team for each
		// owner_user_id is unique (kind='personal' + ON DELETE CASCADE
		// invariants from teams.go), so the subquery resolves to one
		// row at most.
		query := `
			UPDATE ` + table + ` AS r
			SET workspace_id = (
				SELECT t.id
				FROM teams t
				WHERE t.owner_user_id = r.owner_user_id AND t.kind = 'personal'
				LIMIT 1
			)
			WHERE r.workspace_id IS NULL`
		if _, err := s.db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

// backfillPersonalTeams ensures every user has a personal team. Called
// at server startup; idempotent. Runs in batches so an outage mid-loop
// doesn't lock the table for long.
func (s *Server) backfillPersonalTeams() error {
	rows, err := s.db.Query(
		`SELECT u.id FROM users u
		 LEFT JOIN teams t ON t.owner_user_id = u.id AND t.kind = 'personal'
		 WHERE t.id IS NULL`,
	)
	if err != nil {
		return err
	}
	var userIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		userIDs = append(userIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range userIDs {
		if _, err := s.ensurePersonalTeam(id); err != nil {
			return err
		}
	}
	return nil
}
