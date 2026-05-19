package dock

// chat_rooms_store.go — typed CRUD for the qunliao P0a multi-participant
// room model. Sits alongside chat_threads (1:1 DM) without replacing it.
// chat_messages is wired to rooms in P0b; here we only manage the room
// + member lifecycle so handlers + tests have something to talk to.
//
// Auth model: workspace owners/admins create rooms (gate in handlers,
// not here). Room.kind is the seed for whisper/convene later — for now
// only 'room' and 'dm' are produced; the CHECK constraint accepts all
// four so we don't need a follow-up migration when P5 lands.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Room kinds. Mirrors the chat_rooms_kind_check CHECK constraint.
const (
	ChatRoomKindRoom    = "room"
	ChatRoomKindDM      = "dm"
	ChatRoomKindWhisper = "whisper"
	ChatRoomKindConvene = "convene"
)

// Member roles. Mirrors chat_room_members_role_check.
const (
	ChatRoomRoleOwner    = "owner"
	ChatRoomRoleMember   = "member"
	ChatRoomRoleObserver = "observer"
)

// ChatRoom is one row in chat_rooms.
type ChatRoom struct {
	ID              int64      `json:"id"`
	WorkspaceID     string     `json:"workspace_id"`
	Kind            string     `json:"kind"`
	Name            string     `json:"name"`
	Topic           string     `json:"topic"`
	CreatedByUserID string     `json:"created_by_user_id"`
	CreatedAt       time.Time  `json:"created_at"`
	ArchivedAt      *time.Time `json:"archived_at,omitempty"`
}

// ChatRoomMember is one row in chat_room_members.
type ChatRoomMember struct {
	RoomID            int64     `json:"room_id"`
	UserID            string    `json:"user_id"`
	Role              string    `json:"role"`
	JoinedAt          time.Time `json:"joined_at"`
	LastReadMessageID *int64    `json:"last_read_message_id,omitempty"`
}

// validRoomKind returns true for any kind the CHECK constraint accepts.
func validRoomKind(kind string) bool {
	switch kind {
	case ChatRoomKindRoom, ChatRoomKindDM, ChatRoomKindWhisper, ChatRoomKindConvene:
		return true
	}
	return false
}

// validRoomRole returns true for any role the CHECK constraint accepts.
func validRoomRole(role string) bool {
	switch role {
	case ChatRoomRoleOwner, ChatRoomRoleMember, ChatRoomRoleObserver:
		return true
	}
	return false
}

// createChatRoom inserts a room and adds the creator as owner in the
// same transaction so listChatRoomsForUser sees it immediately.
func (s *Server) createChatRoom(ctx context.Context, workspaceID, kind, name, topic, createdBy string) (*ChatRoom, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return nil, errors.New("workspace_id required")
	}
	if strings.TrimSpace(createdBy) == "" {
		return nil, errors.New("created_by required")
	}
	if kind == "" {
		kind = ChatRoomKindRoom
	}
	if !validRoomKind(kind) {
		return nil, fmt.Errorf("invalid room kind: %s", kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	r := &ChatRoom{
		WorkspaceID:     workspaceID,
		Kind:            kind,
		Name:            strings.TrimSpace(name),
		Topic:           strings.TrimSpace(topic),
		CreatedByUserID: createdBy,
		CreatedAt:       now,
	}
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO chat_rooms (workspace_id, kind, name, topic, created_by_user_id, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING id`,
		r.WorkspaceID, r.Kind, r.Name, r.Topic, r.CreatedByUserID, r.CreatedAt,
	).Scan(&r.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chat_room_members (room_id, user_id, role, joined_at)
		 VALUES ($1,$2,$3,$4)`,
		r.ID, createdBy, ChatRoomRoleOwner, now,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

// getChatRoom by id; nil if not found.
func (s *Server) getChatRoom(ctx context.Context, id int64) (*ChatRoom, error) {
	r := &ChatRoom{}
	var archived sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, kind, name, topic, created_by_user_id, created_at, archived_at
		 FROM chat_rooms WHERE id=$1`, id,
	).Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.Name, &r.Topic, &r.CreatedByUserID, &r.CreatedAt, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if archived.Valid {
		t := archived.Time
		r.ArchivedAt = &t
	}
	return r, nil
}

// updateChatRoom mutates name/topic. Pass empty string to leave a field
// alone (this is the only field set we want editable in P0a).
func (s *Server) updateChatRoom(ctx context.Context, id int64, name, topic *string) error {
	if name == nil && topic == nil {
		return nil
	}
	parts := []string{}
	args := []any{}
	if name != nil {
		parts = append(parts, fmt.Sprintf("name=$%d", len(args)+1))
		args = append(args, strings.TrimSpace(*name))
	}
	if topic != nil {
		parts = append(parts, fmt.Sprintf("topic=$%d", len(args)+1))
		args = append(args, strings.TrimSpace(*topic))
	}
	args = append(args, id)
	q := fmt.Sprintf("UPDATE chat_rooms SET %s WHERE id=$%d", strings.Join(parts, ", "), len(args))
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// archiveChatRoom sets archived_at = now if not already archived.
func (s *Server) archiveChatRoom(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_rooms SET archived_at = now() WHERE id=$1 AND archived_at IS NULL`, id)
	return err
}

// listChatRoomsForUser returns rooms the user is a member of, newest
// first. Archived rooms are excluded unless includeArchived is true.
func (s *Server) listChatRoomsForUser(ctx context.Context, workspaceID, userID string, includeArchived bool) ([]ChatRoom, error) {
	q := `SELECT r.id, r.workspace_id, r.kind, r.name, r.topic, r.created_by_user_id, r.created_at, r.archived_at
	      FROM chat_rooms r
	      JOIN chat_room_members m ON m.room_id = r.id
	      WHERE r.workspace_id = $1 AND m.user_id = $2`
	if !includeArchived {
		q += ` AND r.archived_at IS NULL`
	}
	q += ` ORDER BY r.created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatRoom{}
	for rows.Next() {
		var r ChatRoom
		var archived sql.NullTime
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.Kind, &r.Name, &r.Topic, &r.CreatedByUserID, &r.CreatedAt, &archived); err != nil {
			return nil, err
		}
		if archived.Valid {
			t := archived.Time
			r.ArchivedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// addChatRoomMember inserts (room_id, user_id). Idempotent — re-adding
// an existing member returns nil without updating role (use
// updateChatRoomMemberRole for that).
func (s *Server) addChatRoomMember(ctx context.Context, roomID int64, userID, role string) error {
	if role == "" {
		role = ChatRoomRoleMember
	}
	if !validRoomRole(role) {
		return fmt.Errorf("invalid role: %s", role)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO chat_room_members (room_id, user_id, role, joined_at)
		 VALUES ($1,$2,$3, now())
		 ON CONFLICT (room_id, user_id) DO NOTHING`,
		roomID, userID, role,
	)
	return err
}

// removeChatRoomMember deletes the membership row. Returns true if a
// row was actually removed (so handlers can 404 cleanly).
func (s *Server) removeChatRoomMember(ctx context.Context, roomID int64, userID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM chat_room_members WHERE room_id=$1 AND user_id=$2`,
		roomID, userID,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// updateChatRoomMemberRole flips role for an existing member. Returns
// false if the member doesn't exist.
func (s *Server) updateChatRoomMemberRole(ctx context.Context, roomID int64, userID, role string) (bool, error) {
	if !validRoomRole(role) {
		return false, fmt.Errorf("invalid role: %s", role)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat_room_members SET role=$3 WHERE room_id=$1 AND user_id=$2`,
		roomID, userID, role,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// listChatRoomMembers — all members of a room, oldest joined first.
func (s *Server) listChatRoomMembers(ctx context.Context, roomID int64) ([]ChatRoomMember, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT room_id, user_id, role, joined_at, last_read_message_id
		 FROM chat_room_members WHERE room_id=$1 ORDER BY joined_at ASC`,
		roomID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatRoomMember{}
	for rows.Next() {
		var m ChatRoomMember
		var lastRead sql.NullInt64
		if err := rows.Scan(&m.RoomID, &m.UserID, &m.Role, &m.JoinedAt, &lastRead); err != nil {
			return nil, err
		}
		if lastRead.Valid {
			v := lastRead.Int64
			m.LastReadMessageID = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// getChatRoomMember returns the membership row, or nil if absent. Used
// by handlers to gate "is this user actually in this room" before
// admitting them into the WS broadcast set (P0b).
func (s *Server) getChatRoomMember(ctx context.Context, roomID int64, userID string) (*ChatRoomMember, error) {
	m := &ChatRoomMember{}
	var lastRead sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT room_id, user_id, role, joined_at, last_read_message_id
		 FROM chat_room_members WHERE room_id=$1 AND user_id=$2`,
		roomID, userID,
	).Scan(&m.RoomID, &m.UserID, &m.Role, &m.JoinedAt, &lastRead)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if lastRead.Valid {
		v := lastRead.Int64
		m.LastReadMessageID = &v
	}
	return m, nil
}
