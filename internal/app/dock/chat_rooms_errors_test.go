package dock

import (
	"strings"
	"testing"
)

func TestClassifyBotError(t *testing.T) {
	tests := []struct {
		name        string
		errText     string
		wantKind    BotErrorKind
		bodyMustHas string // substring the localized body must contain
	}{
		// Out of credit
		{"xai out of credit", "upstream LLM provider out of credit: Your team 3b70 has either used all available credits or reached its monthly spending limit", BotErrorOutOfCredit, "余额耗尽"},
		{"deepseek insufficient_balance", "Insufficient Balance", BotErrorOutOfCredit, "余额耗尽"},
		{"openrouter 402", "status 402 \"insufficient_credits\"", BotErrorOutOfCredit, "LLM 计费"},
		{"anthropic credit balance", "Your credit balance is too low to access the Anthropic API", BotErrorOutOfCredit, "余额耗尽"},
		// Auth
		{"401 invalid key", "401 Unauthorized: invalid api key", BotErrorAuth, "鉴权失败"},
		{"x-api-key missing", "missing x-api-key header", BotErrorAuth, "鉴权失败"},
		// Network
		{"dial tcp", "Post \"https://x.example.com\": dial tcp: lookup x.example.com: no such host", BotErrorNetwork, "连不上"},
		{"i/o timeout", "context deadline exceeded (i/o timeout)", BotErrorNetwork, "连不上"},
		// Content policy
		{"content policy", "response_blocked: content_policy violation", BotErrorContentPolicy, "内容审核"},
		{"safety filtered", "Output filtered: safety_violation", BotErrorContentPolicy, "内容审核"},
		// Fallback
		{"unknown error", "something completely new", BotErrorUnknown, "暂时无法回复"},
		{"empty err", "", BotErrorUnknown, "暂时无法回复"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, body := classifyBotError(tc.errText, "苹果硬件专家")
			if kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", kind, tc.wantKind)
			}
			if !strings.Contains(body, "苹果硬件专家") {
				t.Errorf("body should always name the bot, got %q", body)
			}
			if !strings.Contains(body, tc.bodyMustHas) {
				t.Errorf("body should contain %q, got %q", tc.bodyMustHas, body)
			}
		})
	}
}

func TestClassifyBotErrorFallbackBotName(t *testing.T) {
	_, body := classifyBotError("anything", "")
	if !strings.Contains(body, "Bot") {
		t.Errorf("empty bot name should fall back to literal Bot, got %q", body)
	}
}

func TestClassifyBotErrorFallbackTruncates(t *testing.T) {
	long := strings.Repeat("x", 250)
	_, body := classifyBotError(long, "alice")
	if !strings.Contains(body, "…") {
		t.Errorf("long error should be truncated with ellipsis, got %q", body)
	}
	if len(body) > 200 {
		t.Errorf("placeholder body should stay under ~200 chars, got %d", len(body))
	}
}
