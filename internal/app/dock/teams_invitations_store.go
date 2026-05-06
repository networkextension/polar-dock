package dock

// Storage layer for team invitations. The schema (team_invitations
// table) is declared in store.go's openDB() block alongside teams +
// team_members. Helpers here are minimal — invitation lifecycle is
// limited and the few operations we do here don't need a generic CRUD
// layer.
//
// Lifecycle:
//   pending → accepted | declined | revoked | expired
//
// 'expired' is set lazily — selectByToken treats expires_at < now as
// status='expired' and refuses to act. A daily / startup sweep can
// rewrite the column for cleanliness; not strictly required.

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

const (
	TeamInvitationStatusPending  = "pending"
	TeamInvitationStatusAccepted = "accepted"
	TeamInvitationStatusDeclined = "declined"
	TeamInvitationStatusRevoked  = "revoked"
	TeamInvitationStatusExpired  = "expired"

	teamInvitationDefaultTTL = 7 * 24 * time.Hour
)

// generateInvitationToken returns a 43-char base64-url string (32
// random bytes). Unguessable.
func generateInvitationToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// createOrRefreshInvitation upserts a pending invitation for (team,
// lower(email)). If a pending row already exists, it gets a fresh
// token + new expires_at + role updated. Other statuses (accepted /
// declined / revoked / expired) are left as historical records and a
// brand-new row is inserted.
func (s *Server) createOrRefreshInvitation(teamID, email, role, inviterUserID string, ttl time.Duration) (*TeamInvitation, error) {
	if teamID == "" || email == "" || role == "" || inviterUserID == "" {
		return nil, errors.New("missing required field")
	}
	emailLower := strings.ToLower(strings.TrimSpace(email))
	now := time.Now().UTC()
	expires := now.Add(ttl)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Look for an existing pending row first.
	var existingID string
	err = tx.QueryRow(
		`SELECT id FROM team_invitations
		 WHERE team_id = $1 AND LOWER(email) = $2 AND status = $3
		 LIMIT 1`,
		teamID, emailLower, TeamInvitationStatusPending,
	).Scan(&existingID)

	token, tokenErr := generateInvitationToken()
	if tokenErr != nil {
		return nil, tokenErr
	}

	var inv TeamInvitation
	switch {
	case errors.Is(err, sql.ErrNoRows):
		inv.ID = generateResourceID()
		inv.TeamID = teamID
		inv.Email = email
		inv.Role = role
		inv.InviterUserID = inviterUserID
		inv.Token = token
		inv.Status = TeamInvitationStatusPending
		inv.ExpiresAt = expires
		inv.CreatedAt = now
		if _, err := tx.Exec(
			`INSERT INTO team_invitations
			 (id, team_id, email, role, inviter_user_id, token, status, expires_at, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			inv.ID, inv.TeamID, inv.Email, inv.Role, inv.InviterUserID,
			inv.Token, inv.Status, inv.ExpiresAt, inv.CreatedAt,
		); err != nil {
			return nil, err
		}
	case err == nil:
		// Refresh existing pending row with new token + extended expiry.
		if _, err := tx.Exec(
			`UPDATE team_invitations
			 SET role = $1, token = $2, expires_at = $3, inviter_user_id = $4
			 WHERE id = $5`,
			role, token, expires, inviterUserID, existingID,
		); err != nil {
			return nil, err
		}
		// Read the refreshed row back so caller gets the canonical view.
		row := tx.QueryRow(
			`SELECT id, team_id, email, role, inviter_user_id, token, status, expires_at, accepted_user_id, accepted_at, created_at
			 FROM team_invitations WHERE id = $1`,
			existingID,
		)
		var acceptedUserID sql.NullString
		var acceptedAt sql.NullTime
		if err := row.Scan(&inv.ID, &inv.TeamID, &inv.Email, &inv.Role, &inv.InviterUserID,
			&inv.Token, &inv.Status, &inv.ExpiresAt, &acceptedUserID, &acceptedAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		if acceptedUserID.Valid {
			v := acceptedUserID.String
			inv.AcceptedUserID = &v
		}
		if acceptedAt.Valid {
			t := acceptedAt.Time
			inv.AcceptedAt = &t
		}
	default:
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &inv, nil
}

// listPendingInvitationsForTeam returns non-expired pending invitations
// for the team, newest first.
func (s *Server) listPendingInvitationsForTeam(teamID string) ([]TeamInvitation, error) {
	rows, err := s.db.Query(
		`SELECT id, team_id, email, role, inviter_user_id, token, status, expires_at, accepted_user_id, accepted_at, created_at
		 FROM team_invitations
		 WHERE team_id = $1 AND status = 'pending' AND expires_at > $2
		 ORDER BY created_at DESC`,
		teamID, time.Now().UTC(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []TeamInvitation{}
	for rows.Next() {
		var inv TeamInvitation
		var acceptedUserID sql.NullString
		var acceptedAt sql.NullTime
		if err := rows.Scan(&inv.ID, &inv.TeamID, &inv.Email, &inv.Role, &inv.InviterUserID,
			&inv.Token, &inv.Status, &inv.ExpiresAt, &acceptedUserID, &acceptedAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		if acceptedUserID.Valid {
			v := acceptedUserID.String
			inv.AcceptedUserID = &v
		}
		if acceptedAt.Valid {
			t := acceptedAt.Time
			inv.AcceptedAt = &t
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// getInvitationByToken returns the invitation if present. Caller is
// responsible for checking status + expires_at before acting.
func (s *Server) getInvitationByToken(token string) (*TeamInvitation, error) {
	if token == "" {
		return nil, nil
	}
	var inv TeamInvitation
	var acceptedUserID sql.NullString
	var acceptedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, team_id, email, role, inviter_user_id, token, status, expires_at, accepted_user_id, accepted_at, created_at
		 FROM team_invitations WHERE token = $1`,
		token,
	).Scan(&inv.ID, &inv.TeamID, &inv.Email, &inv.Role, &inv.InviterUserID,
		&inv.Token, &inv.Status, &inv.ExpiresAt, &acceptedUserID, &acceptedAt, &inv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if acceptedUserID.Valid {
		v := acceptedUserID.String
		inv.AcceptedUserID = &v
	}
	if acceptedAt.Valid {
		t := acceptedAt.Time
		inv.AcceptedAt = &t
	}
	return &inv, nil
}

// revokeInvitation marks a pending invitation revoked. Idempotent:
// already-non-pending records become no-ops.
func (s *Server) revokeInvitation(invID string) error {
	if invID == "" {
		return errors.New("invID is empty")
	}
	_, err := s.db.Exec(
		`UPDATE team_invitations SET status = 'revoked' WHERE id = $1 AND status = 'pending'`,
		invID,
	)
	return err
}

// acceptInvitation atomically: validate, INSERT team_member, UPDATE
// invitation status. Email match is enforced by the caller (handler
// has the user's verified email; this helper trusts it).
func (s *Server) acceptInvitation(token, userID string) (*TeamInvitation, error) {
	if token == "" || userID == "" {
		return nil, errors.New("missing required field")
	}
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var inv TeamInvitation
	var acceptedUserID sql.NullString
	var acceptedAt sql.NullTime
	err = tx.QueryRow(
		`SELECT id, team_id, email, role, inviter_user_id, token, status, expires_at, accepted_user_id, accepted_at, created_at
		 FROM team_invitations WHERE token = $1 FOR UPDATE`,
		token,
	).Scan(&inv.ID, &inv.TeamID, &inv.Email, &inv.Role, &inv.InviterUserID,
		&inv.Token, &inv.Status, &inv.ExpiresAt, &acceptedUserID, &acceptedAt, &inv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("invitation not found")
	}
	if err != nil {
		return nil, err
	}
	if inv.Status != TeamInvitationStatusPending {
		return nil, errors.New("invitation is not pending")
	}
	if inv.ExpiresAt.Before(now) {
		// Self-heal the row to 'expired' so it stays consistent.
		_, _ = tx.Exec(`UPDATE team_invitations SET status = 'expired' WHERE id = $1`, inv.ID)
		_ = tx.Commit()
		return nil, errors.New("invitation has expired")
	}

	// If the user is already a member of the team (somehow), don't
	// double-INSERT — just mark accepted and return.
	if _, err := tx.Exec(
		`INSERT INTO team_members (team_id, user_id, role, joined_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (team_id, user_id) DO NOTHING`,
		inv.TeamID, userID, inv.Role, now,
	); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`UPDATE team_invitations
		 SET status = 'accepted', accepted_user_id = $1, accepted_at = $2
		 WHERE id = $3`,
		userID, now, inv.ID,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	inv.Status = TeamInvitationStatusAccepted
	uid := userID
	inv.AcceptedUserID = &uid
	at := now
	inv.AcceptedAt = &at
	return &inv, nil
}

// declineInvitation marks a pending invitation declined. Same
// no-op-on-already-resolved semantics as revoke.
func (s *Server) declineInvitation(token string) (*TeamInvitation, error) {
	if token == "" {
		return nil, errors.New("token is empty")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var inv TeamInvitation
	var acceptedUserID sql.NullString
	var acceptedAt sql.NullTime
	err = tx.QueryRow(
		`SELECT id, team_id, email, role, inviter_user_id, token, status, expires_at, accepted_user_id, accepted_at, created_at
		 FROM team_invitations WHERE token = $1 FOR UPDATE`,
		token,
	).Scan(&inv.ID, &inv.TeamID, &inv.Email, &inv.Role, &inv.InviterUserID,
		&inv.Token, &inv.Status, &inv.ExpiresAt, &acceptedUserID, &acceptedAt, &inv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("invitation not found")
	}
	if err != nil {
		return nil, err
	}
	if inv.Status != TeamInvitationStatusPending {
		// Already resolved; just return the row as-is.
		return &inv, nil
	}
	if _, err := tx.Exec(`UPDATE team_invitations SET status = 'declined' WHERE id = $1`, inv.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	inv.Status = TeamInvitationStatusDeclined
	return &inv, nil
}

// listMyPendingInvitations returns non-expired pending invitations
// addressed to the given email (case-insensitive). Powers the inbox.
func (s *Server) listMyPendingInvitations(email string) ([]TeamInvitation, error) {
	emailLower := strings.ToLower(strings.TrimSpace(email))
	if emailLower == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT id, team_id, email, role, inviter_user_id, token, status, expires_at, accepted_user_id, accepted_at, created_at
		 FROM team_invitations
		 WHERE LOWER(email) = $1 AND status = 'pending' AND expires_at > $2
		 ORDER BY created_at DESC`,
		emailLower, time.Now().UTC(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamInvitation{}
	for rows.Next() {
		var inv TeamInvitation
		var acceptedUserID sql.NullString
		var acceptedAt sql.NullTime
		if err := rows.Scan(&inv.ID, &inv.TeamID, &inv.Email, &inv.Role, &inv.InviterUserID,
			&inv.Token, &inv.Status, &inv.ExpiresAt, &acceptedUserID, &acceptedAt, &inv.CreatedAt); err != nil {
			return nil, err
		}
		if acceptedUserID.Valid {
			v := acceptedUserID.String
			inv.AcceptedUserID = &v
		}
		if acceptedAt.Valid {
			t := acceptedAt.Time
			inv.AcceptedAt = &t
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// expireOldInvitations rewrites pending rows whose expires_at has
// passed. Cheap to run at startup. Idempotent.
func (s *Server) expireOldInvitations() error {
	_, err := s.db.Exec(
		`UPDATE team_invitations SET status = 'expired'
		 WHERE status = 'pending' AND expires_at < $1`,
		time.Now().UTC(),
	)
	return err
}
