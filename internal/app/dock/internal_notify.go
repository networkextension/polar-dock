package dock

// internal_notify.go — POST /internal/v1/notify, the plugin → user notification
// fan-out. Replaces the Phase 1-B stub.
//
// Recipients: `user_id` (one user; room lives in their personal workspace) or
// `workspace_id` (+ optional `roles` filter, e.g. ["owner","admin"] for
// billing notices; default = every member).
//
// Channels (each best-effort, reported in the response):
//   * room  — a per-workspace system room (name = notifyRoomName, created by the
//             `system` user) receives a text message from `system`; recipients
//             are added as members if missing; the usual chat:events fanout
//             makes it show up live + unread. This is the one channel that
//             always works.
//   * email — every recipient with an email, iff a mailer is configured
//             (SMTP_*). Silently 0 otherwise.
//   * push  — still not wired (no APNs pipeline for non-chat payloads); we
//             report the pushable-device count as `skipped` like before.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const notifyRoomName = "系统通知"

type internalNotifyReq struct {
	UserID      string         `json:"user_id"`
	WorkspaceID string         `json:"workspace_id"`
	Roles       []string       `json:"roles"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Kind        string         `json:"kind"`
	URL         string         `json:"url"`
	Data        map[string]any `json:"data"`
}

type notifyRecipient struct {
	UserID string
	Email  string
}

func (s *Server) handleInternalNotify(c *gin.Context) {
	var req internalNotifyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	if req.UserID == "" && req.WorkspaceID == "" {
		internalErr(c, http.StatusBadRequest, "user_id or workspace_id required")
		return
	}
	if strings.TrimSpace(req.Title) == "" && strings.TrimSpace(req.Body) == "" {
		internalErr(c, http.StatusBadRequest, "title or body required")
		return
	}
	ctx := c.Request.Context()

	// ── resolve workspace + recipients ─────────────────────────────────
	var (
		wsID       string
		recipients []notifyRecipient
	)
	if req.WorkspaceID != "" {
		if _, err := s.getTeam(req.WorkspaceID); err != nil {
			internalErr(c, http.StatusNotFound, "workspace not found")
			return
		}
		wsID = req.WorkspaceID
		members, err := s.listTeamMembersWithUser(wsID)
		if err != nil {
			internalErr(c, http.StatusInternalServerError, "member lookup failed")
			return
		}
		roleOK := map[string]bool{}
		for _, r := range req.Roles {
			roleOK[strings.ToLower(strings.TrimSpace(r))] = true
		}
		for _, m := range members {
			if len(roleOK) > 0 && !roleOK[strings.ToLower(m.Role)] {
				continue
			}
			if req.UserID != "" && m.UserID != req.UserID {
				continue
			}
			recipients = append(recipients, notifyRecipient{UserID: m.UserID, Email: m.Email})
		}
	} else {
		u, err := s.getUserByID(req.UserID)
		if err != nil || u == nil {
			internalErr(c, http.StatusNotFound, "user not found")
			return
		}
		wid, err := s.personalWorkspaceForUser(req.UserID)
		if err != nil || wid == "" {
			internalErr(c, http.StatusInternalServerError, "workspace resolve failed")
			return
		}
		wsID = wid
		recipients = []notifyRecipient{{UserID: u.ID, Email: u.Email}}
	}
	if len(recipients) == 0 {
		c.JSON(http.StatusOK, gin.H{"delivered": gin.H{"room": 0, "email": 0, "push": 0}, "recipients": 0, "workspace_id": wsID, "note": "no recipients"})
		return
	}

	// ── channel: room ──────────────────────────────────────────────────
	roomDelivered := 0
	var roomID int64
	rid, msgID, err := s.postSystemNotice(ctx, wsID, recipients, req)
	if err != nil {
		log.Printf("[notify] room post failed ws=%s: %v", wsID, err)
	} else {
		roomID = rid
		roomDelivered = len(recipients)
		_ = msgID
	}

	// ── channel: email ─────────────────────────────────────────────────
	emailDelivered := 0
	if s.mailer != nil && s.mailer.Enabled() {
		subj := strings.TrimSpace(req.Title)
		if subj == "" {
			subj = "Polar 通知"
		}
		text := strings.TrimSpace(req.Body)
		if req.URL != "" {
			text += "\n\n" + req.URL
		}
		for _, r := range recipients {
			if strings.TrimSpace(r.Email) == "" {
				continue
			}
			if err := s.mailer.Send(ctx, MailMessage{To: r.Email, Subject: subj, PlainText: text}); err != nil {
				log.Printf("[notify] email to %s failed: %v", r.Email, err)
				continue
			}
			emailDelivered++
		}
	}

	// ── channel: push (still stub) ─────────────────────────────────────
	pushSkipped := 0
	for _, r := range recipients {
		if devs, err := s.listPushableUserDevices(r.UserID); err == nil {
			pushSkipped += len(devs)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"delivered":    gin.H{"room": roomDelivered, "email": emailDelivered, "push": 0},
		"push_skipped": pushSkipped,
		"recipients":   len(recipients),
		"workspace_id": wsID,
		"room_id":      roomID,
		"kind":         req.Kind,
	})
}

// systemNoticeRoom finds (or creates) the workspace's system-notice room:
// kind=room, name=notifyRoomName, created by the `system` user, not archived.
func (s *Server) systemNoticeRoom(ctx context.Context, wsID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM chat_rooms
		  WHERE workspace_id=$1 AND kind='room' AND name=$2 AND created_by_user_id=$3 AND archived_at IS NULL
		  ORDER BY id LIMIT 1`, wsID, notifyRoomName, systemUserID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err := s.ensureSystemUser(); err != nil {
		return 0, err
	}
	r, err := s.createChatRoom(ctx, wsID, ChatRoomKindRoom, notifyRoomName, "平台/模块的系统通知（计费、告警、任务结果）", systemUserID)
	if err != nil {
		return 0, err
	}
	return r.ID, nil
}

// postSystemNotice puts every recipient in the workspace's system room (if
// not already a member) and posts the notice as `system`.
func (s *Server) postSystemNotice(ctx context.Context, wsID string, recipients []notifyRecipient, req internalNotifyReq) (int64, int64, error) {
	roomID, err := s.systemNoticeRoom(ctx, wsID)
	if err != nil {
		return 0, 0, err
	}
	for _, r := range recipients {
		if r.UserID == systemUserID {
			continue
		}
		if err := s.addChatRoomMember(ctx, roomID, r.UserID, ChatRoomRoleMember); err != nil {
			// addChatRoomMember is expected to be idempotent-ish; a real
			// failure just means this user misses the message, not the whole post.
			log.Printf("[notify] add member %s to room %d: %v", r.UserID, roomID, err)
		}
	}
	content := formatNotice(req)
	now := time.Now().UTC()
	msgID, err := s.insertRoomMessage(ctx, roomID, systemUserID, "text", content, now)
	if err != nil {
		return roomID, 0, err
	}
	s.publishChatInternalEvent(chatInternalEvent{
		Event:     chatEventRoomMessageCreated,
		RoomID:    roomID,
		MessageID: msgID,
		SenderID:  systemUserID,
	})
	return roomID, msgID, nil
}

func formatNotice(req internalNotifyReq) string {
	var b strings.Builder
	if t := strings.TrimSpace(req.Title); t != "" {
		b.WriteString(t)
	}
	if body := strings.TrimSpace(req.Body); body != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(body)
	}
	if u := strings.TrimSpace(req.URL); u != "" {
		b.WriteString("\n")
		b.WriteString(u)
	}
	if k := strings.TrimSpace(req.Kind); k != "" {
		b.WriteString(fmt.Sprintf("\n#%s", k))
	}
	return b.String()
}
