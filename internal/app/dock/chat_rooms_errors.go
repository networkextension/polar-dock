package dock

// chat_rooms_errors.go — qunliao: turn raw LLM upstream errors into
// localized placeholder messages with concrete action hints. Used by
// both P1 fanout (chat_rooms_agent.go) and P3 idle loop
// (chat_rooms_idle_loop.go) so error-time UX is consistent.

import "strings"

// BotErrorKind classifies why a bot reply couldn't be generated.
// Drives the placeholder text and (in future) per-kind metrics.
type BotErrorKind string

const (
	BotErrorOutOfCredit   BotErrorKind = "out_of_credit"
	BotErrorNetwork       BotErrorKind = "network"
	BotErrorAuth          BotErrorKind = "auth"
	BotErrorContentPolicy BotErrorKind = "content_policy"
	BotErrorUnknown       BotErrorKind = "unknown"
)

// classifyBotError maps an upstream/LLM error string into a kind + a
// localized placeholder body to post as the bot. The body always
// names the bot so multi-bot rooms stay legible, and includes a
// concrete action (link to /llm-billing.html / /bots.html / retry).
//
// `errText` is whatever the LLM call returned — typically wrapped in
// "upstream LLM provider out of credit: …" / "Post …: dial tcp …" /
// the provider's own JSON error. Pattern matches are case-insensitive
// and check ALL substrings before falling through.
func classifyBotError(errText, botName string) (BotErrorKind, string) {
	name := botName
	if strings.TrimSpace(name) == "" {
		name = "Bot"
	}
	low := strings.ToLower(errText)

	// Out-of-credit signatures across providers:
	//   - DeepSeek / Volcengine / xiaomi: "insufficient balance"
	//   - DashScope / mirothinker: "insufficient balance or quota"
	//   - xAI: "used all available credits or reached its monthly spending limit"
	//   - OpenRouter: "402" + "insufficient_credits"
	//   - Anthropic: "credit balance is too low"
	//   - Our proxy 502→402 mapping: "out of credit"
	if containsAny(low,
		"insufficient_balance", "insufficient balance",
		"out of credit", "available credit", "credit balance",
		"monthly spending limit", "exceeded quota", "rate_limit_exceeded",
		"402 ", "\"402\"", "you exceeded your", "billing",
	) {
		body := "（系统提示）「" + name + "」的 LLM 账户余额耗尽。" +
			"请到 [LLM 计费](/llm-billing.html) 充值或调高月预算，" +
			"或在 [Bot 管理](/bots.html) 给该 bot 换一个还有余额的 LLM。"
		return BotErrorOutOfCredit, body
	}

	// Auth / api_key issues:
	if containsAny(low,
		"unauthorized", "401 ", "\"401\"",
		"invalid api key", "invalid api_key", "incorrect api key",
		"api_key not", "missing api_key", "missing api key",
		"authentication", "x-api-key",
	) {
		body := "（系统提示）「" + name + "」LLM 鉴权失败 — " +
			"去 [Bot 管理](/bots.html) 检查这个 bot 绑定的 LLM 的 api_key 是否正确，或换一个 LLM。"
		return BotErrorAuth, body
	}

	// Network / transport (dock-side or upstream tcp):
	if containsAny(low,
		"i/o timeout", "context deadline exceeded",
		"connection refused", "no such host", "dial tcp",
		"connection reset", "eof", "tls handshake",
		"network is unreachable",
	) {
		body := "（系统提示）「" + name + "」暂时连不上 LLM 服务器，稍后再试。"
		return BotErrorNetwork, body
	}

	// Content / safety:
	if containsAny(low,
		"content_policy", "content policy",
		"safety_violation", "safety violation",
		"moderation", "prohibited", "harmful",
		"violates", "filtered",
	) {
		body := "（系统提示）「" + name + "」的回答被 LLM 内容审核拦下，换个说法试试。"
		return BotErrorContentPolicy, body
	}

	// Fallback — keep the first 100 chars of upstream so operators can
	// still debug from the room transcript without a full SQL trip.
	trim := strings.TrimSpace(errText)
	if len(trim) > 100 {
		trim = trim[:100] + "…"
	}
	body := "（系统提示）「" + name + "」暂时无法回复 — " + trim
	return BotErrorUnknown, body
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
