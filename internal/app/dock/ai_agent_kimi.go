package dock

// kimi-cli passthrough mode. The bot's attached polar-agent has
// kimi-cli installed and declared the "kimi" capability at hello.
// Instead of running our own LLM tool loop, we forward the user's
// raw chat content to the agent, which spawns
//   kimi-cli --work-dir=<workdir> --yolo --afk --continue
// piping the content as stdin and capturing stdout. The agent
// returns the captured stdout as chat_reply, which we surface to
// the user as the bot's reply verbatim.
//
// Architectural note (per 用户): this is "io 转发到 chat user" —
// chat IS the runtime. No platform-side prompt construction, no
// platform AI key, no per-tool plumbing; kimi-cli already has all
// of that. Platform just routes bytes.

import (
	"errors"
	"fmt"
	"strings"
)

func (a *aiAgent) generateReplyWithKimi(task aiAgentTask, conn *agentConn) (string, error) {
	if a.server == nil || a.server.agentHub == nil {
		return "", errors.New("agent hub unavailable")
	}
	content := strings.TrimSpace(task.Content)
	if content == "" {
		return "（空消息，未触发 kimi）", nil
	}
	reply, err := a.server.agentHub.dispatchChatMessage(conn.botUserID, task.ThreadID, content)
	if err != nil {
		switch {
		case errors.Is(err, errAgentNotAttached):
			return "Bot 的本地 polar-agent 已断开，无法运行 kimi-cli。请重新 `polar-agent attach --kimi`。", nil
		case errors.Is(err, errAgentNoKimiCapability):
			return "本地 polar-agent 没声明 kimi 能力（可能是用 --kimi 之外的模式启动的）。", nil
		case errors.Is(err, errAgentTimeout):
			return "kimi-cli 在 10 分钟内没返回结果，已超时。", nil
		case errors.Is(err, errAgentDisconnected):
			return "kimi-cli 还在跑的时候，本地 agent 断线了。", nil
		}
		return fmt.Sprintf("kimi 路由失败：%s", err.Error()), nil
	}
	if !reply.OK {
		// Surface kimi-cli's own stderr if any — usually the most
		// actionable info (e.g. login expired, work-dir missing).
		msg := reply.Error
		if msg == "" {
			msg = strings.TrimSpace(reply.Stderr)
		}
		if msg == "" {
			msg = "kimi-cli 退出失败但没有错误信息"
		}
		return fmt.Sprintf("⚠️ kimi-cli 报错：\n```\n%s\n```", msg), nil
	}
	out := strings.TrimSpace(reply.Content)
	if out == "" {
		return "（kimi-cli 没有输出）", nil
	}
	return out, nil
}
