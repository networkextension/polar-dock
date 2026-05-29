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
	var curLLM sql.NullInt64
	probeErr := s.db.QueryRow(
		`SELECT id, llm_config_id FROM bot_users WHERE workspace_id = $1 AND name = $2 LIMIT 1`,
		team.ID, name,
	).Scan(&botID, &curLLM)
	if probeErr != nil && !errors.Is(probeErr, sql.ErrNoRows) {
		return false, probeErr
	}

	if botID != 0 {
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
		return false, nil
	}

	// Create. Classic (reply-capable) when an LLM is available, else a
	// passthrough placeholder to establish the invariant.
	if llmID > 0 {
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
		return bot != nil, nil
	}
	_, created, err := s.ensureBotForAgent(
		team.ID, team.OwnerUserID, name,
		"团队默认 AI 助手（最低配置；待工作区配置模型后自动启用）", nil,
	)
	return created, err
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
