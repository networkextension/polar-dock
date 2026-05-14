package dock

// Pure-logic tests for the host-skill dispatch helpers in
// ai_agent_host_skill.go. The end-to-end orchestrator
// (generateReplyWithHostSkill) is exercised via the manual smoke
// described in the P1c.3 PR description; this file covers the bits
// we can verify in-memory.

import (
	"strings"
	"testing"
)

func TestPickCoderToolForDispatchPassthrough(t *testing.T) {
	got := pickCoderToolForDispatch([]byte(`{"mode":"passthrough","tool":"kimi","tools":["kimi"]}`))
	if got != "kimi" {
		t.Errorf("passthrough mode: got %q, want kimi", got)
	}
}

func TestPickCoderToolForDispatchToolLoopEmpty(t *testing.T) {
	// Tool-loop config has no `tool` key; result must be empty so
	// the dispatcher can surface a clear "specify tool" error.
	got := pickCoderToolForDispatch([]byte(`{"mode":"tool-loop","tools":["kimi","claude","codex"]}`))
	if got != "" {
		t.Errorf("tool-loop mode: got %q, want empty", got)
	}
}

func TestPickCoderToolForDispatchInvalidJSON(t *testing.T) {
	got := pickCoderToolForDispatch([]byte(`{not-json`))
	if got != "" {
		t.Errorf("malformed json: got %q, want empty", got)
	}
}

func TestPickCoderToolForDispatchEmpty(t *testing.T) {
	if got := pickCoderToolForDispatch(nil); got != "" {
		t.Errorf("nil: got %q, want empty", got)
	}
	if got := pickCoderToolForDispatch([]byte{}); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

func TestFormatHostSkillExitReplySuccess(t *testing.T) {
	// We can't call the receiver method without a Server, but the
	// reply-formatting logic in formatHostSkillExitReply touches the
	// DB only via finalizeHostSkillRun (best-effort, error ignored).
	// To test the pure render path, replicate the logic via a sub
	// function — kept private to the test file.
	out := renderHostSkillExitReplyForTest(true, "tool wrote output", "", "", "kimi")
	if out != "tool wrote output" {
		t.Errorf("happy path: got %q", out)
	}
}

func TestFormatHostSkillExitReplyEmpty(t *testing.T) {
	out := renderHostSkillExitReplyForTest(true, "", "", "", "claude")
	if !strings.Contains(out, "没有输出") {
		t.Errorf("empty success: got %q", out)
	}
}

func TestFormatHostSkillExitReplyFailureWithAllSections(t *testing.T) {
	out := renderHostSkillExitReplyForTest(false, "partial output", "stderr blob", "boom: timeout", "codex")
	for _, needle := range []string{"⚠️ boom: timeout", "partial output", "stderr blob"} {
		if !strings.Contains(out, needle) {
			t.Errorf("failure all-sections: missing %q in %q", needle, out)
		}
	}
}

func TestFormatHostSkillExitReplyFailureNoData(t *testing.T) {
	out := renderHostSkillExitReplyForTest(false, "", "", "", "kimi")
	if !strings.Contains(out, "退出失败但没有错误信息") {
		t.Errorf("failure-no-data: got %q", out)
	}
}

// renderHostSkillExitReplyForTest mirrors the pure-render half of
// formatHostSkillExitReply without the DB finalize call, so we can
// unit-test the formatting without a Server. Keep in lockstep with
// the real implementation.
func renderHostSkillExitReplyForTest(ok bool, content, stderr, errMsg, tool string) string {
	if ok {
		out := strings.TrimSpace(content)
		if out == "" {
			return "（" + tool + " 没有输出）"
		}
		return out
	}
	var sections []string
	if msg := strings.TrimSpace(errMsg); msg != "" {
		sections = append(sections, "⚠️ "+msg)
	}
	if out := strings.TrimSpace(content); out != "" {
		sections = append(sections, "**"+tool+" 已输出（截至中断）**：\n\n"+out)
	}
	if errOut := strings.TrimSpace(stderr); errOut != "" {
		sections = append(sections, "**stderr**：\n```\n"+errOut+"\n```")
	}
	if len(sections) == 0 {
		sections = []string{tool + " 退出失败但没有错误信息"}
	}
	return strings.Join(sections, "\n\n")
}
