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

import "log"

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

// provisionTeamBot ensures the given (active) team has its default AI
// team bot. Idempotent — a no-op if the bot already exists. Returns
// whether a bot was created.
func (s *Server) provisionTeamBot(workspaceID string) (bool, error) {
	team, err := s.getTeam(workspaceID)
	if err != nil {
		return false, err
	}
	if team == nil {
		return false, nil
	}
	_, created, err := s.ensureBotForAgent(
		team.ID, team.OwnerUserID, teamBotName(team),
		"团队默认 AI 助手（最低配置，可在团队设置中更换模型）", nil,
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
