package dock

import "strings"

// LLM proxy cost ledger — P6.
//
// Best-effort USD estimate per call, derived from a static table of
// per-1K-token prices. Not authoritative for billing — the upstream
// provider invoice is. Intended for trend signals + monthly quota
// enforcement (P4) and dashboard rollups (P7c).
//
// The price table is intentionally hardcoded for now: pricing data
// changes slowly (months between provider updates), and putting it
// in a DB table adds an editor surface no admin has asked for.
// When pricing becomes self-service, promote this to a
// llm_proxy_pricing table + admin UI.

type llmProxyPrice struct {
	// Per-1K-token USD price. PromptUSD applies to prompt_tokens;
	// CompletionUSD applies to completion_tokens.
	PromptUSD     float64
	CompletionUSD float64
}

// llmProxyPriceTable maps lowercased model-name PREFIXES to prices.
// First match wins, so put more-specific entries before
// less-specific. "anything" catches the generic case so we never
// return zero from an unknown model.
//
// Source: provider public pricing pages as of 2026-05. Rough rule of
// thumb: completion costs ~3-5x prompt for chat-tuned models.
var llmProxyPriceTable = []struct {
	Prefix string
	Price  llmProxyPrice
}{
	// OpenAI
	{"gpt-4o-mini", llmProxyPrice{0.00015, 0.0006}},
	{"gpt-4o", llmProxyPrice{0.0025, 0.01}},
	{"gpt-4-turbo", llmProxyPrice{0.01, 0.03}},
	{"gpt-4", llmProxyPrice{0.03, 0.06}},
	{"gpt-3.5", llmProxyPrice{0.0005, 0.0015}},
	{"o1-mini", llmProxyPrice{0.003, 0.012}},
	{"o1", llmProxyPrice{0.015, 0.06}},

	// Anthropic
	{"claude-haiku", llmProxyPrice{0.0008, 0.004}},
	{"claude-3-5-haiku", llmProxyPrice{0.0008, 0.004}},
	{"claude-3-haiku", llmProxyPrice{0.00025, 0.00125}},
	{"claude-sonnet", llmProxyPrice{0.003, 0.015}},
	{"claude-3-5-sonnet", llmProxyPrice{0.003, 0.015}},
	{"claude-3-sonnet", llmProxyPrice{0.003, 0.015}},
	{"claude-3-opus", llmProxyPrice{0.015, 0.075}},
	{"claude-opus", llmProxyPrice{0.015, 0.075}},

	// Google Gemini
	{"gemini-2.5-flash", llmProxyPrice{0.000075, 0.0003}},
	{"gemini-2.0-flash", llmProxyPrice{0.000075, 0.0003}},
	{"gemini-1.5-flash", llmProxyPrice{0.000075, 0.0003}},
	{"gemini-2.5-pro", llmProxyPrice{0.00125, 0.005}},
	{"gemini-1.5-pro", llmProxyPrice{0.00125, 0.005}},
	{"gemini-pro", llmProxyPrice{0.00125, 0.005}},

	// Moonshot Kimi
	{"kimi-k2", llmProxyPrice{0.00056, 0.00224}},
	{"kimi", llmProxyPrice{0.00084, 0.00336}},
	{"moonshot", llmProxyPrice{0.00084, 0.00336}},

	// DeepSeek
	{"deepseek-r1", llmProxyPrice{0.00055, 0.0022}},
	{"deepseek-chat", llmProxyPrice{0.00027, 0.0011}},
	{"deepseek-coder", llmProxyPrice{0.00014, 0.00028}},
	{"deepseek", llmProxyPrice{0.00027, 0.0011}},

	// xAI Grok
	{"grok-4", llmProxyPrice{0.003, 0.015}},
	{"grok-2", llmProxyPrice{0.002, 0.01}},
	{"grok", llmProxyPrice{0.002, 0.01}},

	// Bytedance Doubao / Volces
	{"doubao-pro", llmProxyPrice{0.0011, 0.0014}},
	{"doubao", llmProxyPrice{0.0001, 0.0003}},
}

// estimateLLMProxyCallCost returns the best-effort USD cost for a
// call. Returns (0, false) when the model has no entry in the price
// table — the caller should leave cost_usd as NULL in that case so
// downstream aggregators can tell "no signal" apart from "free".
func estimateLLMProxyCallCost(modelResolved string, promptTokens, completionTokens int) (float64, bool) {
	m := strings.ToLower(strings.TrimSpace(modelResolved))
	if m == "" || (promptTokens == 0 && completionTokens == 0) {
		return 0, false
	}
	for _, entry := range llmProxyPriceTable {
		if strings.HasPrefix(m, entry.Prefix) {
			cost := float64(promptTokens)*entry.Price.PromptUSD/1000.0 +
				float64(completionTokens)*entry.Price.CompletionUSD/1000.0
			return cost, true
		}
	}
	return 0, false
}
