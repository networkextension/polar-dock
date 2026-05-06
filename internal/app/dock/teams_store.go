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
