package main

// kimi-cli passthrough mode. When attach is started with --kimi we
// declare the "kimi" capability in our hello frame, and the
// platform routes chat messages here as `chat_message` envelopes
// instead of running its own tool-call loop.
//
// Each chat_message is forwarded to a fresh `kimi-cli` invocation:
//
//   kimi-cli --work-dir=$workdir --quiet --yolo --continue --prompt=$content
//
// Where:
//   --quiet    = --print + --output-format text + --final-message-only
//                (clean text, no TUI, only the assistant's final reply)
//   --yolo     = auto-approve every tool call
//   --continue = resume the previous session for this work-dir
//   --prompt   = the user's chat message (avoids stdin escaping)
//
// First-message edge case: --continue errors with "No previous
// session found for the working directory" before any session
// exists. We detect that exact string and retry once without
// --continue, which lets kimi-cli create the initial session.
//
// Per 用户 ("io 转发到 chat user"): no platform prompt construction,
// no per-tool plumbing — kimi-cli is the agent. One bot + one
// workdir = one rolling kimi session.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// chatMessage matches the platform's agentChatMessage envelope.
type chatMessage struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	ThreadID int64  `json:"thread_id"`
	Content  string `json:"content"`
}

type chatReply struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Content string `json:"content,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	kimiBinary           = "kimi-cli"
	kimiDefaultTimeout   = 30 * time.Minute
	kimiTimeoutEnvVar    = "POLAR_KIMI_TIMEOUT"
	kimiMaxStdout        = 200_000
	kimiMaxStderr        = 50_000
)

// kimiTimeout reads POLAR_KIMI_TIMEOUT (Go duration string like
// "45m", "1h") from env on each invocation; falls back to 30min.
// Done per-call instead of init-time so the user can change it
// without restarting attach.
func kimiTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(kimiTimeoutEnvVar)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return kimiDefaultTimeout
}

// kimiInvoke spawns kimi-cli and returns its captured output. Always
// returns a non-nil reply (the OK flag distinguishes success from
// failure); only orchestration errors (binary missing, etc.) come
// back as the err return.
func kimiInvoke(ctx context.Context, workdir string, msg chatMessage, verbose bool) (*chatReply, error) {
	reply, retryNoContinue, err := runKimi(ctx, workdir, msg, true, verbose)
	if err != nil {
		return nil, err
	}
	if retryNoContinue {
		if verbose {
			log.Printf("[kimi] no previous session, retrying without --continue")
		}
		return runKimiAndReturn(ctx, workdir, msg, false, verbose)
	}
	return reply, nil
}

// runKimi runs kimi-cli once. retryNoContinue is true when the
// invocation failed specifically because --continue had no prior
// session — the caller should retry without the flag.
//
// Output handling: stdout + stderr are tee'd in real time to the
// polar-agent's own stdout/stderr (so the user watching their
// `attach` terminal sees progress while kimi works) AND collected
// into bytes.Buffer for the final chat_reply / error-detection
// path. The buffer cap (kimiMaxStdout/kimiMaxStderr) bounds memory
// for chatty kimi runs.
func runKimi(ctx context.Context, workdir string, msg chatMessage, withContinue, verbose bool) (*chatReply, bool, error) {
	timeout := kimiTimeout()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"--work-dir=" + workdir,
		"--quiet",
		"--yolo",
	}
	if withContinue {
		args = append(args, "--continue")
	}
	args = append(args, "--prompt="+msg.Content)

	if verbose {
		log.Printf("[kimi] spawn %s --work-dir=%s --quiet --yolo%s --prompt=<%d bytes> timeout=%s (thread=%d)",
			kimiBinary, workdir, continueFlagLog(withContinue), len(msg.Content), timeout, msg.ThreadID)
	}
	cmd := exec.CommandContext(cctx, kimiBinary, args...)
	cmd.Dir = workdir

	// Stream stdout + stderr to the operator's terminal in real
	// time, AND capture into bounded buffers for the chat reply.
	stdoutBuf := &capBuffer{cap: kimiMaxStdout}
	stderrBuf := &capBuffer{cap: kimiMaxStderr}
	cmd.Stdout = io.MultiWriter(prefixWriter{w: os.Stdout, prefix: "[kimi] "}, stdoutBuf)
	cmd.Stderr = io.MultiWriter(prefixWriter{w: os.Stderr, prefix: "[kimi:err] "}, stderrBuf)

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start kimi-cli: %w", err)
	}
	waitErr := cmd.Wait()
	if verbose {
		log.Printf("[kimi] done in %s (exit_err=%v)", time.Since(startedAt).Truncate(time.Millisecond), waitErr)
	}
	stdoutBytes := stdoutBuf.Bytes()
	stderrBytes := stderrBuf.Bytes()

	reply := &chatReply{
		Kind:    "chat_reply",
		ID:      msg.ID,
		Content: string(stdoutBytes),
		Stderr:  string(stderrBytes),
	}
	if cctx.Err() == context.DeadlineExceeded {
		reply.OK = false
		reply.Error = fmt.Sprintf("kimi-cli timed out after %s (set POLAR_KIMI_TIMEOUT=<duration> on attach to extend, e.g. 1h)", timeout)
		return reply, false, nil
	}
	if waitErr != nil {
		// Detect the --continue / "no previous session" case so the
		// caller can retry on a clean session.
		if withContinue && strings.Contains(reply.Stderr, "No previous session") {
			return nil, true, nil
		}
		reply.OK = false
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			reply.Error = fmt.Sprintf("kimi-cli exit %d", ee.ExitCode())
		} else {
			reply.Error = waitErr.Error()
		}
		return reply, false, nil
	}
	reply.OK = true
	if strings.TrimSpace(reply.Content) == "" && reply.Stderr != "" {
		reply.Content = "(kimi-cli 没有 stdout，详见 stderr)"
	}
	return reply, false, nil
}

// runKimiAndReturn is the retry path: same as runKimi but treats
// the retryNoContinue signal as fatal (we already retried).
func runKimiAndReturn(ctx context.Context, workdir string, msg chatMessage, withContinue, verbose bool) (*chatReply, error) {
	reply, _, err := runKimi(ctx, workdir, msg, withContinue, verbose)
	return reply, err
}

func continueFlagLog(b bool) string {
	if b {
		return " --continue"
	}
	return ""
}

// capBuffer is an io.Writer that appends bytes up to a cap, then
// drops further input. Used so a runaway kimi run can't blow
// memory while still surfacing a reasonable head of output to the
// chat reply.
type capBuffer struct {
	cap int
	buf []byte
	cut bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= b.cap {
		return len(p), nil
	}
	room := b.cap - len(b.buf)
	if len(p) <= room {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	b.buf = append(b.buf, p[:room]...)
	if !b.cut {
		b.buf = append(b.buf, []byte("\n...[truncated]\n")...)
		b.cut = true
	}
	return len(p), nil
}

func (b *capBuffer) Bytes() []byte {
	return b.buf
}

// prefixWriter prepends a tag to every line emitted to w. Lines
// are detected by '\n'; anything mid-line gets the prefix on the
// next flush. Good enough for kimi's progress output.
type prefixWriter struct {
	w      io.Writer
	prefix string
	atLine bool // true when next byte starts a line; init via lazy
	init   bool
}

func (pw prefixWriter) Write(p []byte) (int, error) {
	// Capture-by-value receiver means mutating atLine wouldn't
	// stick across calls. Maintain state via static-buffered
	// strategy: just write prefix-once-per-write-call. Coarse but
	// keeps the writer cheap — we accept slightly off-by-one
	// formatting for back-to-back partial-line writes.
	if pw.w == nil {
		return len(p), nil
	}
	if len(p) == 0 {
		return 0, nil
	}
	// Insert prefix at start of each line.
	out := make([]byte, 0, len(p)+len(pw.prefix))
	out = append(out, []byte(pw.prefix)...)
	for i, c := range p {
		out = append(out, c)
		if c == '\n' && i < len(p)-1 {
			out = append(out, []byte(pw.prefix)...)
		}
	}
	if _, err := pw.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// emitChatReply serializes + writes a chat_reply through the
// connection's write helper.
func emitChatReply(send func([]byte) error, reply *chatReply) {
	b, err := json.Marshal(reply)
	if err != nil {
		log.Printf("[kimi] marshal chat_reply: %v", err)
		return
	}
	if err := send(b); err != nil {
		log.Printf("[kimi] write chat_reply: %v", err)
	}
}
