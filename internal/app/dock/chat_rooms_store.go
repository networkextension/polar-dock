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

// ChatRoomMember is one row in chat_room_members. Username + Icon +
// IsBot are denormalized from users + bot_users so the UI can render
// without a second round-trip; nil when the join misses (deleted user
// — shouldn't happen given ON DELETE CASCADE but the columns are safe
// fallbacks). BotName is the bot_users.name when this member is a
// bot, which is the @mention token that lookup uses (NOT user_id).
type ChatRoomMember struct {
	RoomID            int64     `json:"room_id"`
	UserID            string    `json:"user_id"`
	Role              string    `json:"role"`
	JoinedAt          time.Time `json:"joined_at"`
	LastReadMessageID *int64    `json:"last_read_message_id,omitempty"`
	Username          string    `json:"username,omitempty"`
	Icon              string    `json:"icon,omitempty"`
	IsBot             bool      `json:"is_bot,omitempty"`
	BotName           string    `json:"bot_name,omitempty"`
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
	// LEFT JOIN bot_users so non-bot members produce NULL bot_name;
	// users JOIN is inner because chat_room_members.user_id has FK to
	// users(id) ON DELETE CASCADE — a member without a user row would
	// be a broken state.
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.room_id, m.user_id, m.role, m.joined_at, m.last_read_message_id,
		       u.username, COALESCE(u.icon_url, ''),
		       b.name IS NOT NULL AS is_bot,
		       COALESCE(b.name, '') AS bot_name
		  FROM chat_room_members m
		  JOIN users u ON u.id = m.user_id
		  LEFT JOIN bot_users b ON b.bot_user_id = m.user_id
		 WHERE m.room_id = $1
		 ORDER BY m.joined_at ASC`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatRoomMember{}
	for rows.Next() {
		var m ChatRoomMember
		var lastRead sql.NullInt64
		if err := rows.Scan(&m.RoomID, &m.UserID, &m.Role, &m.JoinedAt, &lastRead,
			&m.Username, &m.Icon, &m.IsBot, &m.BotName); err != nil {
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

// insertRoomMessage writes one message scoped to a room (NOT a thread).
// Mirrors the chat_messages insert pattern used by createChatMessage*
// but leaves thread_id NULL — the schema relaxed that column to nullable
// in P0b so room-only rows don't need a sentinel. Returns the new id.
func (s *Server) insertRoomMessage(ctx context.Context, roomID int64, senderID, messageType, content string, createdAt time.Time) (int64, error) {
	return s.insertRoomMessageWithMetrics(ctx, roomID, senderID, messageType, content, createdAt, roomMessageMetrics{})
}

// roomMessageMetrics carries optional per-call telemetry to snapshot
// into the chat_messages row alongside the bot reply. Zero values =
// not recorded (rendered as "—" by the UI metadata pill).
type roomMessageMetrics struct {
	LLMConfigID      int64   // 0 = no llm involved
	LLMConfigName    string  // snapshot for stable history
	LLMModel         string  // snapshot — includes any model_override
	LatencyMS        int     // round-trip including network
	PromptTokens     int     // from upstream Usage block
	CompletionTokens int     // from upstream Usage block
	CostUSD          float64 // -1 = not estimable, 0 = estimable-zero
}

// insertRoomMessageWithMetrics is the bot-reply-aware variant. It
// writes the llm_config_id/name/model + latency + token + cost snapshot
// columns so the rooms UI can render the metadata pill without a
// second round-trip.
func (s *Server) insertRoomMessageWithMetrics(ctx context.Context, roomID int64, senderID, messageType, content string, createdAt time.Time, m roomMessageMetrics) (int64, error) {
	if roomID <= 0 {
		return 0, errors.New("roomID required")
	}
	if strings.TrimSpace(senderID) == "" {
		return 0, errors.New("senderID required")
	}
	if messageType == "" {
		messageType = "text"
	}
	// llm_config_name and llm_model are NOT NULL DEFAULT ''; pass the
	// empty string for absent values. The numeric metrics columns are
	// nullable, so we use any-typed args that can be nil.
	var (
		llmCfgArg     any = nil
		latencyArg    any = nil
		promptArg     any = nil
		completionArg any = nil
		costArg       any = nil
	)
	if m.LLMConfigID > 0 {
		llmCfgArg = m.LLMConfigID
	}
	if m.LatencyMS > 0 {
		latencyArg = int64(m.LatencyMS)
	}
	if m.PromptTokens > 0 {
		promptArg = m.PromptTokens
	}
	if m.CompletionTokens > 0 {
		completionArg = m.CompletionTokens
	}
	if m.CostUSD >= 0 && (m.PromptTokens > 0 || m.CompletionTokens > 0) {
		// Only record cost when we have tokens to bind it to — keeps
		// "estimable zero" from polluting reporting with 0-cost rows.
		costArg = m.CostUSD
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO chat_messages
		   (room_id, sender_id, message_type, failed, content, created_at,
		    llm_config_id, llm_config_name, llm_model, latency_ms,
		    prompt_tokens, completion_tokens, cost_usd)
		 VALUES ($1, $2, $3, FALSE, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 RETURNING id`,
		roomID, senderID, messageType, content, createdAt,
		llmCfgArg, m.LLMConfigName, m.LLMModel, latencyArg,
		promptArg, completionArg, costArg,
	).Scan(&id)
	return id, err
}

// listRoomMessages returns up to `limit` most-recent messages in a room,
// optionally older than beforeID for backwards pagination. Joined to
// users for sender_username/icon. Mirrors listRecentChatMessages.
func (s *Server) listRoomMessages(ctx context.Context, roomID int64, beforeID *int64, limit int) ([]ChatMessage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT m.id, m.room_id, m.llm_thread_id, m.sender_id, u.username, u.icon_url,
	             m.message_type, m.failed, m.content,
	             m.markdown_entry_id, m.markdown_title, m.latency_ms,
	             m.streaming, m.seq, m.updated_at, m.created_at,
	             m.deleted_at, m.deleted_by, m.llm_config_id,
	             COALESCE(m.llm_config_name, ''), COALESCE(m.llm_model, ''),
	             m.prompt_tokens, m.completion_tokens, m.cost_usd
	      FROM chat_messages m
	      JOIN users u ON u.id = m.sender_id
	     WHERE m.room_id = $1`
	args := []any{roomID}
	if beforeID != nil && *beforeID > 0 {
		q += ` AND m.id < $2 ORDER BY m.id DESC LIMIT $3`
		args = append(args, *beforeID, limit)
	} else {
		q += ` ORDER BY m.id DESC LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatMessage{}
	for rows.Next() {
		var msg ChatMessage
		var roomIDOut sql.NullInt64
		var llmThreadID sql.NullInt64
		var markdownEntryID sql.NullInt64
		var latencyMs sql.NullInt64
		var updatedAt sql.NullTime
		var deletedAt sql.NullTime
		var deletedBy sql.NullString
		var llmConfigID sql.NullInt64
		var promptTokens, completionTokens sql.NullInt64
		var costUSD sql.NullFloat64
		if err := rows.Scan(
			&msg.ID, &roomIDOut, &llmThreadID,
			&msg.SenderID, &msg.SenderUsername, &msg.SenderIcon,
			&msg.MessageType, &msg.Failed, &msg.Content,
			&markdownEntryID, &msg.MarkdownTitle, &latencyMs,
			&msg.Streaming, &msg.Seq, &updatedAt, &msg.CreatedAt,
			&deletedAt, &deletedBy, &llmConfigID,
			&msg.LLMConfigName, &msg.LLMModel,
			&promptTokens, &completionTokens, &costUSD,
		); err != nil {
			return nil, err
		}
		if promptTokens.Valid {
			v := int(promptTokens.Int64)
			msg.PromptTokens = &v
		}
		if completionTokens.Valid {
			v := int(completionTokens.Int64)
			msg.CompletionTokens = &v
		}
		if costUSD.Valid {
			v := costUSD.Float64
			msg.CostUSD = &v
		}
		if roomIDOut.Valid {
			v := roomIDOut.Int64
			msg.RoomID = &v
		}
		if llmThreadID.Valid {
			msg.LLMThreadID = &llmThreadID.Int64
		}
		if markdownEntryID.Valid {
			msg.MarkdownEntryID = &markdownEntryID.Int64
		}
		if latencyMs.Valid {
			msg.LatencyMs = &latencyMs.Int64
		}
		if updatedAt.Valid {
			msg.UpdatedAt = &updatedAt.Time
		}
		if deletedAt.Valid {
			msg.DeletedAt = &deletedAt.Time
			msg.Deleted = true
			msg.Content = ""
		}
		if deletedBy.Valid {
			msg.DeletedBy = deletedBy.String
		}
		if llmConfigID.Valid {
			v := llmConfigID.Int64
			msg.LLMConfigID = &v
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

// ChatRoomAgentLoop is one row in chat_room_agent_loops (qunliao P3).
// Owner-controlled per-(room, bot) "speak up on your own" config.
type ChatRoomAgentLoop struct {
	RoomID            int64      `json:"room_id"`
	BotUserID         string     `json:"bot_user_id"`
	Enabled           bool       `json:"enabled"`
	IntervalSecs      int        `json:"interval_secs"`
	MaxPerHour        int        `json:"max_per_hour"`
	LastDecidedAt     *time.Time `json:"last_decided_at,omitempty"`
	LastPostedAt      *time.Time `json:"last_posted_at,omitempty"`
	DecisionsThisHour int        `json:"decisions_this_hour"`
	HourWindowStart   *time.Time `json:"hour_window_start,omitempty"`
}

// listChatRoomAgentLoops returns all loop rows for the room. Used by
// the owner-facing /api/rooms/:id/agent-loops GET handler.
func (s *Server) listChatRoomAgentLoops(ctx context.Context, roomID int64) ([]ChatRoomAgentLoop, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT room_id, bot_user_id, enabled, interval_secs, max_per_hour,
		       last_decided_at, last_posted_at, decisions_this_hour, hour_window_start
		  FROM chat_room_agent_loops
		 WHERE room_id = $1
		 ORDER BY bot_user_id`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatRoomAgentLoop{}
	for rows.Next() {
		var l ChatRoomAgentLoop
		var ld, lp, hw sql.NullTime
		if err := rows.Scan(&l.RoomID, &l.BotUserID, &l.Enabled, &l.IntervalSecs, &l.MaxPerHour,
			&ld, &lp, &l.DecisionsThisHour, &hw); err != nil {
			return nil, err
		}
		if ld.Valid {
			t := ld.Time
			l.LastDecidedAt = &t
		}
		if lp.Valid {
			t := lp.Time
			l.LastPostedAt = &t
		}
		if hw.Valid {
			t := hw.Time
			l.HourWindowStart = &t
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// upsertChatRoomAgentLoop sets the owner-controllable fields. Counter
// state (last_decided_at, decisions_this_hour, etc.) is preserved on
// UPDATE — toggling enabled doesn't reset throttle.
func (s *Server) upsertChatRoomAgentLoop(ctx context.Context, l ChatRoomAgentLoop) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_room_agent_loops (room_id, bot_user_id, enabled, interval_secs, max_per_hour)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (room_id, bot_user_id) DO UPDATE SET
		  enabled       = EXCLUDED.enabled,
		  interval_secs = EXCLUDED.interval_secs,
		  max_per_hour  = EXCLUDED.max_per_hour`,
		l.RoomID, l.BotUserID, l.Enabled, l.IntervalSecs, l.MaxPerHour)
	return err
}

// deleteChatRoomAgentLoop removes the row (≡ disable + forget settings).
func (s *Server) deleteChatRoomAgentLoop(ctx context.Context, roomID int64, botUserID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM chat_room_agent_loops WHERE room_id=$1 AND bot_user_id=$2`,
		roomID, botUserID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// dueChatRoomAgentLoops returns enabled loops that are past their next
// tick window. Used by the background goroutine. The caller is
// expected to bump last_decided_at immediately to avoid double-fire
// on slow LLMs.
func (s *Server) dueChatRoomAgentLoops(ctx context.Context, now time.Time, limit int) ([]ChatRoomAgentLoop, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT room_id, bot_user_id, enabled, interval_secs, max_per_hour,
		       last_decided_at, last_posted_at, decisions_this_hour, hour_window_start
		  FROM chat_room_agent_loops
		 WHERE enabled = TRUE
		   AND (last_decided_at IS NULL OR $1::timestamptz - last_decided_at >= make_interval(secs => interval_secs))
		 ORDER BY COALESCE(last_decided_at, '1970-01-01'::timestamptz) ASC
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatRoomAgentLoop{}
	for rows.Next() {
		var l ChatRoomAgentLoop
		var ld, lp, hw sql.NullTime
		if err := rows.Scan(&l.RoomID, &l.BotUserID, &l.Enabled, &l.IntervalSecs, &l.MaxPerHour,
			&ld, &lp, &l.DecisionsThisHour, &hw); err != nil {
			return nil, err
		}
		if ld.Valid {
			t := ld.Time
			l.LastDecidedAt = &t
		}
		if lp.Valid {
			t := lp.Time
			l.LastPostedAt = &t
		}
		if hw.Valid {
			t := hw.Time
			l.HourWindowStart = &t
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// markChatRoomAgentLoopDecided bumps last_decided_at and rotates the
// hourly throttle counter. Called for every tick — including "model
// chose nothing" — so spacing is by attempt, not by post.
func (s *Server) markChatRoomAgentLoopDecided(ctx context.Context, roomID int64, botUserID string, now time.Time, posted bool) error {
	// Hour window: if hour_window_start is NULL or older than 1h,
	// reset the counter; else just increment. Done in SQL so concurrent
	// goroutines don't race on a read-modify-write loop.
	q := `
		UPDATE chat_room_agent_loops
		   SET last_decided_at     = $3,
		       last_posted_at      = CASE WHEN $4 THEN $3 ELSE last_posted_at END,
		       hour_window_start   = CASE
		                               WHEN hour_window_start IS NULL OR $3::timestamptz - hour_window_start >= INTERVAL '1 hour'
		                               THEN $3
		                               ELSE hour_window_start
		                             END,
		       decisions_this_hour = CASE
		                               WHEN hour_window_start IS NULL OR $3::timestamptz - hour_window_start >= INTERVAL '1 hour'
		                               THEN 1
		                               ELSE decisions_this_hour + 1
		                             END
		 WHERE room_id = $1 AND bot_user_id = $2`
	_, err := s.db.ExecContext(ctx, q, roomID, botUserID, now, posted)
	return err
}

// disableChatRoomAgentLoop flips enabled = FALSE. Called by the
// background loop when a billing gate trips so we don't loop and
// burn through retries.
func (s *Server) disableChatRoomAgentLoop(ctx context.Context, roomID int64, botUserID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chat_room_agent_loops SET enabled = FALSE WHERE room_id = $1 AND bot_user_id = $2`,
		roomID, botUserID)
	return err
}

// findBotInRoom returns the bot_users row matching (workspace, name)
// only when that bot is also a member of roomID. Returns (nil, nil)
// when there's no such bot or membership — callers treat a miss as
// silent "wrong @, ignore".
//
// Relies on the uniq_bot_users_workspace_name constraint (PR #271) so
// the SELECT is deterministic without ORDER BY.
func (s *Server) findBotInRoom(ctx context.Context, workspaceID string, roomID int64, name string) (*BotUser, error) {
	if strings.TrimSpace(workspaceID) == "" || roomID <= 0 || strings.TrimSpace(name) == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT b.id, b.owner_user_id, b.workspace_id, b.bot_user_id, b.name,
		       b.description, b.system_prompt,
		       COALESCE(b.llm_config_id, 0), b.bot_kind,
		       COALESCE(b.preferred_tool, ''), b.is_auto_registered,
		       b.host_skill_id, b.created_at, b.updated_at
		  FROM bot_users b
		  JOIN chat_room_members m ON m.user_id = b.bot_user_id AND m.room_id = $3
		 WHERE b.workspace_id = $1 AND b.name = $2
		 LIMIT 1`,
		workspaceID, name, roomID)
	var bot BotUser
	var hostSkillID sql.NullInt64
	if err := row.Scan(
		&bot.ID, &bot.OwnerUserID, &bot.WorkspaceID, &bot.BotUserID, &bot.Name,
		&bot.Description, &bot.SystemPrompt,
		&bot.LLMConfigID, &bot.BotKind,
		&bot.PreferredTool, &bot.IsAutoRegistered,
		&hostSkillID, &bot.CreatedAt, &bot.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if hostSkillID.Valid {
		v := hostSkillID.Int64
		bot.HostSkillID = &v
	}
	return &bot, nil
}

// getChatRoomMember returns the membership row, or nil if absent. Used
// by handlers to gate "is this user actually in this room" before
// admitting them into the WS broadcast set (P0c).
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
