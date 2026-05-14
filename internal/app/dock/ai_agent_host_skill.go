package dock

// Host module P1c.3 — dock-side dispatch when bot.host_skill_id is set.
//
// Flow per chat message that lands on a bot bound to a host_skill:
//   1. Resolve host_skill row + the live agentConn for its host.
//   2. Create host_skill_runs row (status='starting') → use its id as
//      run_id in the skill.start envelope.
//   3. Build the env map from host_skill_credentials (decrypted
//      server-side; raw values never leave the function scope).
//   4. Send skill.start over the agent's WS.
//   5. Drain skill.event frames keyed by run_id until exit, then
//      finalize the run row + format the bot's chat reply from the
//      exit data.
//
// Legacy bots with host_skill_id IS NULL keep their current path —
// generateReplyWithPassthrough / generateReplyWithTools in ai_agent.go
// — untouched. The branch in ai_agent.go checks getBotHostSkillID
// before falling through to those.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// hostSkillDispatchTimeout is the absolute wall-clock cap on one
// skill.start round-trip from envelope-send to exit-event. Long
// enough for a deep code edit run, tight enough that a dead agent
// recovers in human time.
const hostSkillDispatchTimeout = 30 * time.Minute

func (a *aiAgent) generateReplyWithHostSkill(task aiAgentTask, hostSkillID int64) (string, error) {
	if a.server == nil || a.server.agentHub == nil {
		return "", errors.New("agent hub unavailable")
	}

	info, err := a.server.getHostSkillForDispatch(hostSkillID)
	if err != nil {
		return "", fmt.Errorf("resolve host_skill=%d: %w", hostSkillID, err)
	}
	if info == nil {
		return fmt.Sprintf("⚠️ Bot 绑定了 host_skill=%d 但记录不存在（可能已被删除）。请在 Bot 设置里解除绑定。", hostSkillID), nil
	}

	conn := a.server.agentHub.lookupByHostID(info.HostID)
	if conn == nil {
		return fmt.Sprintf("⚠️ Bot 绑定的 host (id=%s) 上没有在线的 polar-agent。请确认远端 agent 已 attach。", info.HostID), nil
	}

	now := time.Now().UTC()
	runID, err := a.server.createHostSkillRun(hostSkillID, now)
	if err != nil {
		return "", fmt.Errorf("create host_skill_run: %w", err)
	}

	// Build the per-call config blob. Today only the coder skill is
	// implemented; tool selection comes from the host_skill's
	// config_json (Mode + Tool from the advertise payload — see
	// skills.CoderConfig). We unmarshal just enough to find the
	// passthrough tool; tool-loop bots without a default tool get an
	// error reply.
	tool := pickCoderToolForDispatch(info.ConfigJSON)
	if info.Kind == "coder" && tool == "" {
		_ = a.server.finalizeHostSkillRun(runID, "failed", "no tool resolved on tool-loop host_skill", time.Now().UTC())
		return "⚠️ host_skill 是 tool-loop 模式，没有默认工具，dock 无法决定要跑哪个 coder。请在 Bot 或 host_skill 配置里指定 tool。", nil
	}

	// Inject env vars from the credential vault. fetchHostSkillCredentialPlaintext
	// is the ONLY decrypt path — values never cross an API boundary
	// other than into this skill.start envelope.
	env, credKeys, err := a.collectHostSkillEnv(hostSkillID)
	if err != nil {
		// A vault read error shouldn't abort the run; log + continue
		// with no env. The CLI will fail itself if the key was
		// required and missing.
		log.Printf("[host-skill] env collect skill=%d: %v", hostSkillID, err)
	}

	startCfg := map[string]any{
		"tool":    tool,
		"message": strings.TrimSpace(task.Content),
	}
	if sub := strings.TrimSpace(task.WorkdirSubpath); sub != "" {
		startCfg["workdir_subpath"] = sub
	}
	if git := strings.TrimSpace(task.GitRemoteURL); git != "" {
		startCfg["git_remote_url"] = git
	}
	if len(env) > 0 {
		startCfg["env"] = env
	}

	log.Printf("[host-skill] dispatch run=%d skill=%d kind=%s host=%s tool=%s creds=%v",
		runID, hostSkillID, info.Kind, info.HostID, tool, credKeys)

	ch := conn.trackSkillPending(runID)
	defer conn.untrackSkillPending(runID)

	env4envelope := skillStartEnvelope{
		Kind:      "skill.start",
		RunID:     runID,
		SkillKind: info.Kind,
		Config:    startCfg,
	}
	if err := a.server.agentHub.dispatchSkillStart(conn, env4envelope); err != nil {
		_ = a.server.finalizeHostSkillRun(runID, "failed", "send: "+err.Error(), time.Now().UTC())
		switch {
		case errors.Is(err, errAgentDisconnected):
			return "⚠️ 发送给 host 的 polar-agent 失败：连接已断开。", nil
		case errors.Is(err, errAgentSendTimeout):
			return "⚠️ 发送给 host 的 polar-agent 超时（5s 内队列没消化）。", nil
		}
		return "⚠️ 发送 skill.start 失败：" + err.Error(), nil
	}

	deadline := time.After(hostSkillDispatchTimeout)
	for {
		select {
		case ev := <-ch:
			switch ev.EventKind {
			case "state":
				if status, _ := ev.Data["status"].(string); status != "" {
					_ = a.server.updateHostSkillRunStatus(runID, status)
				}
			case "log":
				// P1c.3 doesn't surface mid-run logs to the chat reply;
				// they'll land in a follow-up streaming PR.
			case "exit":
				return a.formatHostSkillExitReply(runID, ev, tool), nil
			default:
				log.Printf("[host-skill] run=%d unknown event_kind=%q", runID, ev.EventKind)
			}
		case <-conn.close:
			_ = a.server.finalizeHostSkillRun(runID, "failed", "agent disconnected mid-run", time.Now().UTC())
			return fmt.Sprintf("⚠️ %s 在跑的时候 host 的 polar-agent 断线了。", tool), nil
		case <-deadline:
			_ = a.server.finalizeHostSkillRun(runID, "failed",
				fmt.Sprintf("dispatch timeout after %s", hostSkillDispatchTimeout), time.Now().UTC())
			return fmt.Sprintf("⚠️ host_skill 在 %s 内没返回结果，dock 超时。子进程可能还在跑。", hostSkillDispatchTimeout), nil
		}
	}
}

// formatHostSkillExitReply renders the bot's user-facing chat reply
// from a skill.event exit data blob + finalizes the run row.
func (a *aiAgent) formatHostSkillExitReply(runID int64, ev skillEventFrame, tool string) string {
	okFlag, _ := ev.Data["ok"].(bool)
	content, _ := ev.Data["content"].(string)
	stderr, _ := ev.Data["stderr"].(string)
	errMsg, _ := ev.Data["error"].(string)

	status := "succeeded"
	if !okFlag {
		status = "failed"
	}
	_ = a.server.finalizeHostSkillRun(runID, status, errMsg, time.Now().UTC())

	if okFlag {
		out := strings.TrimSpace(content)
		if out == "" {
			return fmt.Sprintf("（%s 没有输出）", tool)
		}
		return out
	}

	var sections []string
	if msg := strings.TrimSpace(errMsg); msg != "" {
		sections = append(sections, "⚠️ "+msg)
	}
	if out := strings.TrimSpace(content); out != "" {
		sections = append(sections, fmt.Sprintf("**%s 已输出（截至中断）**：\n\n%s", tool, out))
	}
	if errOut := strings.TrimSpace(stderr); errOut != "" {
		sections = append(sections, "**stderr**：\n```\n"+errOut+"\n```")
	}
	if len(sections) == 0 {
		sections = []string{fmt.Sprintf("%s 退出失败但没有错误信息", tool)}
	}
	return strings.Join(sections, "\n\n")
}

// pickCoderToolForDispatch extracts the dispatcher tool from a
// host_skills.config_json blob. For passthrough mode we use the
// pinned Tool; for tool-loop mode we leave it empty so the caller
// can decide whether to error out or use a per-bot preference.
//
// The config blob is whatever skills.CoderConfig serializes to —
// {mode, tool?, tools[], workdir?}. We unmarshal lazily into a map
// so we don't have to import the skills package from dock.
func pickCoderToolForDispatch(configJSON []byte) string {
	if len(configJSON) == 0 {
		return ""
	}
	var probe struct {
		Mode string `json:"mode"`
		Tool string `json:"tool"`
	}
	if err := json.Unmarshal(configJSON, &probe); err != nil {
		log.Printf("[host-skill] pickCoderTool: parse config_json failed: %v", err)
		return ""
	}
	return strings.TrimSpace(probe.Tool)
}

// collectHostSkillEnv fetches every credential row for the skill and
// returns them as an env map ready for the skill.start config blob.
// Returns the list of keys it injected (NOT values) so the caller
// can log them for audit without leaking secrets.
func (a *aiAgent) collectHostSkillEnv(hostSkillID int64) (map[string]string, []string, error) {
	creds, err := a.server.listHostSkillCredentials(hostSkillID)
	if err != nil {
		return nil, nil, err
	}
	if len(creds) == 0 {
		return nil, nil, nil
	}
	out := make(map[string]string, len(creds))
	keys := make([]string, 0, len(creds))
	for _, c := range creds {
		plain, err := a.server.fetchHostSkillCredentialPlaintext(c.HostSkillID, c.Key)
		if err != nil {
			log.Printf("[host-skill] decrypt credential skill=%d key=%s: %v", c.HostSkillID, c.Key, err)
			continue
		}
		if plain == "" {
			continue
		}
		out[c.Key] = plain
		keys = append(keys, c.Key)
	}
	return out, keys, nil
}
