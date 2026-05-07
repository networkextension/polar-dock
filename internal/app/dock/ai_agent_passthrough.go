package dock

// Multi-tool passthrough. The bot's attached polar-agent has a
// local coding tool (kimi-cli / claude / codex / custom) and
// declared the "passthrough" capability at hello (legacy "kimi"
// also accepted). Instead of running the platform LLM tool loop,
// we forward the user's raw chat content to the agent; it spawns
// the configured tool with the message as input, captures stdout,
// returns it as the bot reply verbatim.
//
// Architectural note (per 用户): this is "io 转发到 chat user" —
// chat IS the runtime. No platform-side prompt construction, no
// platform AI key, no per-tool plumbing; the local tool already
// has all of that. Platform just routes bytes.

import (
	"errors"
	"fmt"
	"strings"
)

func (a *aiAgent) generateReplyWithPassthrough(task aiAgentTask, conn *agentConn) (string, error) {
	if a.server == nil || a.server.agentHub == nil {
		return "", errors.New("agent hub unavailable")
	}
	tool := conn.tool
	if tool == "" {
		tool = "tool"
	}
	content := strings.TrimSpace(task.Content)
	if content == "" {
		return fmt.Sprintf("（空消息，未触发 %s）", tool), nil
	}
	reply, err := a.server.agentHub.dispatchChatMessage(conn.botUserID, task.ThreadID, content)
	if err != nil {
		switch {
		case errors.Is(err, errAgentNotAttached):
			return fmt.Sprintf("Bot 的本地 polar-agent 已断开，无法运行 %s。请重新 `polar-agent attach --tool=%s`。", tool, tool), nil
		case errors.Is(err, errAgentNoKimiCapability):
			return "本地 polar-agent 没声明 passthrough 能力（启动时缺少 --tool=<name>）。", nil
		case errors.Is(err, errAgentTimeout):
			return fmt.Sprintf("本地 polar-agent 在 2 小时内没返回结果，平台超时（%s 子进程可能还在跑）。", tool), nil
		case errors.Is(err, errAgentDisconnected):
			return fmt.Sprintf("%s 还在跑的时候，本地 agent 断线了。", tool), nil
		}
		return fmt.Sprintf("%s 路由失败：%s", tool, err.Error()), nil
	}
	if !reply.OK {
		// Failure path. Surface whichever sources have useful info:
		//   1. reply.Error      — orchestration message (timeout, exit code, etc.)
		//   2. reply.Content    — partial stdout the tool produced
		//                         before the failure (CRITICAL for
		//                         timeout — the tool may have
		//                         written code already)
		//   3. reply.Stderr     — runtime / build errors
		// Stitched together with section markers so the user can
		// iterate based on the partial progress.
		var sections []string
		if msg := strings.TrimSpace(reply.Error); msg != "" {
			sections = append(sections, "⚠️ "+msg)
		}
		if out := strings.TrimSpace(reply.Content); out != "" {
			sections = append(sections, fmt.Sprintf("**%s 已输出（截至中断）**：\n\n%s", tool, out))
		}
		if errOut := strings.TrimSpace(reply.Stderr); errOut != "" {
			sections = append(sections, "**stderr**：\n```\n"+errOut+"\n```")
		}
		if len(sections) == 0 {
			sections = []string{fmt.Sprintf("%s 退出失败但没有错误信息", tool)}
		}
		return strings.Join(sections, "\n\n"), nil
	}
	out := strings.TrimSpace(reply.Content)
	if out == "" {
		return fmt.Sprintf("（%s 没有输出）", tool), nil
	}
	return out, nil
}
