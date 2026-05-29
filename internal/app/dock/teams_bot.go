package dock

// teams_bot.go — R2-2: every team has one default AI "team bot" as its
// minimum config (locked decision: 每个 team 一个 AI team bot 最低配置).
//
// Provisioning is idempotent and reuses ensureBotForAgent (the same
// passthrough-bot upsert used for auto-registered agents): probe by
// (workspace, name), create if missing. It runs on team approval (new
// collaboration teams) and as a startup backfill (existing active teams,
// incl. home teams). The bot's reply/LLM binding (platform-default per
// decision E) + default-room seating are wired in a follow-up (R2-2b);
// this slice establishes the invariant that every active team owns a bot.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"
)

// teamBotName is the deterministic display name for a team's default bot.
// Unique per (workspace, name), which is the key ensureBotForAgent
// probes, so provisioning is naturally idempotent.
func teamBotName(team *Team) string {
	name := team.Name
	if name == "" {
		name = "团队"
	}
	return name + " 助手"
}

// teamBotSystemPrompt is the default persona for a team's bot.
func teamBotSystemPrompt(team *Team) string {
	return "你是「" + team.Name + "」工作区的默认 AI 助手。" +
		"用简洁、友好的中文协助团队成员；被 @ 到时给出有帮助的回答。"
}

// provisionTeamBot ensures the given (active) team has its default AI
// team bot. Idempotent. R2-2b: the bot is a *classic* bot bound to the
// workspace's default/available LLM (own > shared > platform — decision
// E) so it can actually reply in rooms. If the workspace has no usable
// LLM yet, it falls back to a passthrough placeholder row; a later run
// (once an LLM exists) heals it into a reply-capable classic bot.
// Returns whether a bot row was freshly created.
func (s *Server) provisionTeamBot(workspaceID string) (bool, error) {
	team, err := s.getTeam(workspaceID)
	if err != nil {
		return false, err
	}
	if team == nil {
		return false, nil
	}
	name := teamBotName(team)
	llmID, _ := s.getWorkspaceDefaultLLMConfigID(team.ID) // 0 when no usable LLM

	// Probe for an existing team bot.
	var botID int64
	var botUserID string
	var curLLM sql.NullInt64
	probeErr := s.db.QueryRow(
		`SELECT id, bot_user_id, llm_config_id FROM bot_users WHERE workspace_id = $1 AND name = $2 LIMIT 1`,
		team.ID, name,
	).Scan(&botID, &botUserID, &curLLM)
	if probeErr != nil && !errors.Is(probeErr, sql.ErrNoRows) {
		return false, probeErr
	}

	created := false
	switch {
	case botID != 0:
		// Exists. Heal a placeholder (no LLM) into a reply-capable
		// classic bot once the workspace has a usable LLM.
		if curLLM.Int64 <= 0 && llmID > 0 {
			if _, err := s.db.Exec(
				`UPDATE bot_users
				    SET bot_kind = $2, llm_config_id = $3,
				        system_prompt = COALESCE(NULLIF(system_prompt, ''), $4),
				        updated_at = NOW()
				  WHERE id = $1`,
				botID, BotKindClassic, llmID, teamBotSystemPrompt(team),
			); err != nil {
				return false, err
			}
		}
	case llmID > 0:
		// Create classic (reply-capable) when an LLM is available.
		bot, cerr := s.createBotUser(CreateBotUserInput{
			WorkspaceID:  team.ID,
			OwnerUserID:  team.OwnerUserID,
			Name:         name,
			Description:  "团队默认 AI 助手（最低配置，可在团队设置中更换模型）",
			SystemPrompt: teamBotSystemPrompt(team),
			LLMConfigID:  llmID,
			BotKind:      BotKindClassic,
		}, time.Now().UTC())
		if cerr != nil {
			return false, cerr
		}
		if bot != nil {
			botUserID, created = bot.BotUserID, true
		}
	default:
		// No LLM yet — passthrough placeholder to establish the invariant.
		id, didCreate, berr := s.ensureBotForAgent(
			team.ID, team.OwnerUserID, name,
			"团队默认 AI 助手（最低配置；待工作区配置模型后自动启用）", nil,
		)
		if berr != nil {
			return false, berr
		}
		botUserID, created = id, didCreate
	}

	// R2-2c: seat the team bot in a default room for *explicit
	// collaboration teams* only (home teams stay roomless to avoid a
	// room-per-user). Non-fatal — a room hiccup must not fail the
	// (already-done) bot provisioning.
	if botUserID != "" && !isHomeTeam(team) {
		if err := s.ensureTeamRoom(team, botUserID); err != nil {
			log.Printf("teams: default room for team %s failed: %v", team.ID, err)
		}
	}
	return created, nil
}

// teamRoomName is the deterministic name of a collaboration team's
// default group room. Dedup key for ensureTeamRoom.
func teamRoomName(team *Team) string {
	return team.Name + " · 团队群"
}

// ensureTeamRoom (R2-2c) gives an explicit collaboration team a default
// group chat room and seats its AI team bot, so the bot is
// @-mentionable out of the box. Idempotent — dedups the room by
// (workspace, name) and the membership via addChatRoomMember's upsert.
// Caller restricts this to non-home teams.
func (s *Server) ensureTeamRoom(team *Team, botUserID string) error {
	ctx := context.Background()
	name := teamRoomName(team)
	var roomID int64
	err := s.db.QueryRow(
		`SELECT id FROM chat_rooms WHERE workspace_id = $1 AND name = $2 ORDER BY created_at LIMIT 1`,
		team.ID, name,
	).Scan(&roomID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if roomID == 0 {
		room, cerr := s.createChatRoom(ctx, team.ID, ChatRoomKindRoom, name,
			"团队默认群聊，@"+teamBotName(team)+" 即可与 AI 助手对话。", team.OwnerUserID)
		if cerr != nil {
			return cerr
		}
		roomID = room.ID
	}
	return s.addChatRoomMember(ctx, roomID, botUserID, ChatRoomRoleMember)
}

// backfillTeamBots provisions the default team bot for every active
// team at startup. Idempotent; logs how many were freshly created.
// Failures on individual teams are logged but don't abort the sweep.
func (s *Server) backfillTeamBots() error {
	teams, err := s.listTeamsByStatus(TeamStatusActive)
	if err != nil {
		return err
	}
	created := 0
	for _, t := range teams {
		didCreate, perr := s.provisionTeamBot(t.ID)
		if perr != nil {
			log.Printf("teams: team-bot backfill for %s failed: %v", t.ID, perr)
			continue
		}
		if didCreate {
			created++
		}
	}
	if created > 0 {
		log.Printf("teams: R2-2 provisioned %d default team bot(s)", created)
	}
	return nil
}
