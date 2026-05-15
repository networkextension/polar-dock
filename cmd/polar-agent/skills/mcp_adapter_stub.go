package skills

// `stub` adapter — deterministic test tools.
//
// Used by polar-agent-test scenarios to exercise the full
// JSON-RPC + adapter dispatch + tool-result-shape chain WITHOUT
// any external subprocess. The three tools cover the common return
// shapes the framework needs to handle:
//   - stub.add       → success with structured numeric result
//   - stub.error     → tool-level failure (JSON-RPC error)
//   - stub.delay_ms  → success after a real delay (lets scenarios
//                      validate timeout / concurrency)
//
// Not registered on production hosts unless the operator explicitly
// includes "stub" in the skill config's adapters list — this is a
// test surface, not a feature.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func init() {
	RegisterMCPAdapter("stub", func() (MCPAdapter, error) {
		return &stubAdapter{}, nil
	})
}

type stubAdapter struct{}

func (s *stubAdapter) Name() string { return "stub" }

func (s *stubAdapter) Tools() []MCPToolDef {
	return []MCPToolDef{
		{
			Name:        "stub.add",
			Description: "Return the sum of two numbers. Test tool — always succeeds with a numeric result.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "number"},
					"b": map[string]any{"type": "number"},
				},
				"required": []string{"a", "b"},
			},
		},
		{
			Name:        "stub.error",
			Description: "Always return a tool-execution error. Tests the JSON-RPC error path (-32000).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "stub.delay_ms",
			Description: "Sleep for the given milliseconds, then return ok. Tests timeout + concurrent dispatch.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ms": map[string]any{"type": "integer"},
				},
				"required": []string{"ms"},
			},
		},
	}
}

func (s *stubAdapter) Call(toolName string, args json.RawMessage) (map[string]any, error) {
	switch toolName {
	case "stub.add":
		var p struct {
			A float64 `json:"a"`
			B float64 `json:"b"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		sum := p.A + p.B
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("%v", sum)},
			},
			"sum": sum, // structured result alongside content for tools that prefer typed access
		}, nil
	case "stub.error":
		return nil, errors.New("stub.error: intentional failure for testing")
	case "stub.delay_ms":
		var p struct {
			MS int `json:"ms"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		// Cap at 10s defensively — a runaway delay would tie up a
		// scenario indefinitely.
		if p.MS < 0 {
			p.MS = 0
		}
		if p.MS > 10000 {
			p.MS = 10000
		}
		time.Sleep(time.Duration(p.MS) * time.Millisecond)
		return map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": fmt.Sprintf("slept %dms", p.MS)},
			},
		}, nil
	default:
		return nil, fmt.Errorf("stub adapter: unknown tool %q", toolName)
	}
}

func (s *stubAdapter) Close() error { return nil }
