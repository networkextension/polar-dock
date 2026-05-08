package dock

// Tool-aware chat completion loop. Engaged when the bot has a
// polar-agent attached to it: each LLM call advertises the agent's
// 4 tools, and any tool_calls in the response get dispatched
// through agentHub, with results fed back as "role: tool" messages
// for the next LLM turn. Loop ends when the LLM returns a normal
// content response (no tool_calls) or hits the iteration cap.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

const (
	aiToolMaxIterations = 8
	aiToolMaxResultLen  = 8000 // chars per tool_result content fed back to LLM
)

// agentToolSpecs is the function-calling schema advertised to the
// LLM when an agent is attached. Mirrors what cmd/polar-agent's
// executor implements.
func agentToolSpecs(workdir string) []aiToolSpec {
	if workdir == "" {
		workdir = "(unknown)"
	}
	return []aiToolSpec{
		{
			Type: "function",
			Function: aiToolFunction{
				Name: "read_file",
				Description: fmt.Sprintf(
					"Read a file relative to the working directory %s. Returns its contents.",
					workdir,
				),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "File path relative to the working dir",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: aiToolFunction{
				Name:        "write_file",
				Description: "Create or overwrite a file inside the working dir. Use this instead of dumping code in chat.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		{
			Type: "function",
			Function: aiToolFunction{
				Name:        "list_dir",
				Description: "List entries under a directory inside the working dir. Returns name/is_dir/size for each.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "Directory path. Defaults to '.'",
						},
					},
				},
			},
		},
		{
			Type: "function",
			Function: aiToolFunction{
				Name: "run_cmd",
				Description: "Run a whitelisted command in the working dir. Allowed cmds: git, swift, xcodebuild, xcrun, ls, cat, find, grep, swiftformat, swiftlint, open. 5min cap. Returns stdout/stderr/exit_code.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cmd":   map[string]any{"type": "string"},
						"args":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"stdin": map[string]any{"type": "string"},
					},
					"required": []string{"cmd"},
				},
			},
		},
	}
}

// agentToolsAddendumData drives the agent.tools_addendum template.
type agentToolsAddendumData struct {
	Workdir string
}

const defaultAgentToolsAddendumTemplate = `
You have a polar-agent attached on the user's Mac. The working
directory is: {{.Workdir}}

Use the provided tools (read_file / write_file / list_dir / run_cmd)
to inspect and modify the codebase. Important rules:

- DO NOT dump file contents or code in chat replies. Use write_file
  for new/changed files; the user reviews via git diff afterwards.
- Plan first, then execute. Brief progress notes in chat are fine,
  but don't repeat tool output verbatim.
- For multi-step changes, do them in one stretch (sequence tool
  calls in the same response when independent, or chain across
  turns when later steps depend on earlier results).
- When done, give a short summary in chat (what was changed, how
  to test, anything that still needs human attention).
- If a command fails, read the stderr carefully and fix the root
  cause; don't blindly retry.`

// agentToolSystemAddendum is appended to the bot's system prompt
// whenever an agent is attached. Tells the LLM the rules of the
// road — most importantly, "use tools, don't dump code in chat."
func (s *Server) agentToolSystemAddendum(workdir string) string {
	return s.renderPrompt(promptSlugAgentToolsAddendum, agentToolsAddendumData{
		Workdir: workdir,
	}, defaultAgentToolsAddendumTemplate)
}

// generateReplyWithTools runs the agent-aware multi-turn loop.
// Returns the final assistant message content (plain text). Any
// tool_calls along the way are silently executed via agentHub and
// fed back to the LLM. Up to aiToolMaxIterations rounds.
func (a *aiAgent) generateReplyWithTools(
	task aiAgentTask,
	runtimeConfig aiRuntimeConfig,
	botUserID string,
	workdir string,
) (string, error) {
	contextText, err := a.buildContext(task.ThreadID, task.LLMThreadID)
	if err != nil {
		log.Printf("build ai context failed: %v", err)
	}
	systemPrompt := strings.TrimSpace(runtimeConfig.SystemPrompt) + a.server.agentToolSystemAddendum(workdir)
	messages := []aiChatCompletionMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "system", Content: contextText},
		{Role: "user", Content: task.Content},
	}
	tools := agentToolSpecs(workdir)
	for iter := 0; iter < aiToolMaxIterations; iter++ {
		payload := aiChatCompletionRequest{
			Model:      runtimeConfig.Model,
			Messages:   messages,
			MaxTokens:  32768,
			Tools:      tools,
			ToolChoice: "auto",
		}
		result, err := a.requestChatCompletion(runtimeConfig, payload)
		if err != nil {
			return "", err
		}
		if len(result.Choices) == 0 {
			return "", errors.New("empty ai response")
		}
		choice := result.Choices[0]
		assistant := choice.Message
		// Append the assistant's turn into history regardless of
		// whether it has tool_calls or not — the LLM expects to see
		// its own previous tool_calls when we feed back results.
		messages = append(messages, assistant)

		if len(assistant.ToolCalls) == 0 {
			// LLM gave a final answer.
			return strings.TrimSpace(assistant.Content), nil
		}

		// Dispatch each tool call to the attached agent, append a
		// "role: tool" message per call carrying the result.
		for _, call := range assistant.ToolCalls {
			toolMsg := a.dispatchSingleToolCall(botUserID, call)
			messages = append(messages, toolMsg)
		}
		log.Printf("[ai-agent] iter=%d bot=%s dispatched %d tool call(s)", iter+1, botUserID, len(assistant.ToolCalls))
	}
	return "", fmt.Errorf("tool loop exceeded %d iterations without final answer", aiToolMaxIterations)
}

// dispatchSingleToolCall runs one tool call and produces the
// "role: tool" message that goes back into the LLM context. On
// failure (agent gone, timeout, etc.) we still produce a tool
// message — with the error in content — so the LLM can recover.
func (a *aiAgent) dispatchSingleToolCall(botUserID string, call aiToolCallMsg) aiChatCompletionMessage {
	out := aiChatCompletionMessage{
		Role:       "tool",
		ToolCallID: call.ID,
		Name:       call.Function.Name,
	}
	var args map[string]any
	if rawArgs := strings.TrimSpace(call.Function.Arguments); rawArgs != "" {
		if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
			out.Content = fmt.Sprintf(`{"ok":false,"error":"failed to parse arguments: %s"}`, err.Error())
			return out
		}
	}
	if a.server == nil || a.server.agentHub == nil {
		out.Content = `{"ok":false,"error":"agent hub unavailable"}`
		return out
	}
	callID := generateResourceID()
	result, err := a.server.agentHub.dispatchToolCall(botUserID, callID, call.Function.Name, args)
	if err != nil {
		out.Content = fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error())
		return out
	}
	// Serialize result back to JSON for the LLM. Cap content size
	// so a 50KB cat output doesn't blow the context window.
	payload := map[string]any{
		"ok":     result.OK,
		"output": result.Output,
	}
	if result.Stdout != "" {
		payload["stdout"] = truncate(result.Stdout, aiToolMaxResultLen)
	}
	if result.Stderr != "" {
		payload["stderr"] = truncate(result.Stderr, aiToolMaxResultLen)
	}
	if result.Error != "" {
		payload["error"] = result.Error
	}
	b, err := json.Marshal(payload)
	if err != nil {
		out.Content = `{"ok":false,"error":"serialize tool result failed"}`
		return out
	}
	out.Content = string(b)
	return out
}

// truncate caps a string to the first n runes (UTF-8 safe-ish; we
// don't bother with grapheme clusters for log truncation).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...[truncated]"
}
