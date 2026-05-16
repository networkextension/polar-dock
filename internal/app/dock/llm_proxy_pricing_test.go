package dock

import (
	"math"
	"testing"
)

func TestEstimateLLMProxyCallCostKnownModels(t *testing.T) {
	tests := []struct {
		model      string
		prompt     int
		completion int
		want       float64 // tolerated within 1e-9
	}{
		// gpt-4o-mini: 0.00015 prompt + 0.0006 completion per 1K
		{"gpt-4o-mini", 1000, 1000, 0.00015 + 0.0006},
		{"gpt-4o-mini-2024-07-18", 2000, 500, 2*0.00015 + 0.5*0.0006},
		// gpt-4o: 0.0025 + 0.01
		{"gpt-4o", 1000, 1000, 0.0025 + 0.01},
		// Anthropic claude sonnet 3.5
		{"claude-3-5-sonnet-20241022", 1000, 1000, 0.003 + 0.015},
		// Kimi k2.5 should hit kimi-k2 prefix
		{"kimi-k2.5", 1000, 1000, 0.00056 + 0.00224},
		// Gemini flash
		{"gemini-2.5-flash", 1000, 1000, 0.000075 + 0.0003},
	}
	for _, tt := range tests {
		got, ok := estimateLLMProxyCallCost(tt.model, tt.prompt, tt.completion)
		if !ok {
			t.Errorf("%s: expected match, got !ok", tt.model)
			continue
		}
		if math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("%s: got %.10f, want %.10f", tt.model, got, tt.want)
		}
	}
}

func TestEstimateLLMProxyCallCostUnknownModel(t *testing.T) {
	_, ok := estimateLLMProxyCallCost("totally-made-up-model-xyz", 1000, 1000)
	if ok {
		t.Fatalf("expected !ok for unknown model")
	}
}

func TestEstimateLLMProxyCallCostZeroTokens(t *testing.T) {
	_, ok := estimateLLMProxyCallCost("gpt-4o-mini", 0, 0)
	if ok {
		t.Fatalf("expected !ok when both token counts are 0 (no signal)")
	}
}

func TestEstimateLLMProxyCallCostCaseInsensitive(t *testing.T) {
	got, ok := estimateLLMProxyCallCost("GPT-4O-MINI", 1000, 1000)
	if !ok {
		t.Fatalf("case-insensitive lookup should match")
	}
	want := 0.00015 + 0.0006
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %.10f, want %.10f", got, want)
	}
}
