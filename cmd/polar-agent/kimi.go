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
	"log"
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
	kimiBinary    = "kimi-cli"
	kimiTimeout   = 10 * time.Minute
	kimiMaxStdout = 200_000
	kimiMaxStderr = 50_000
)

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
func runKimi(ctx context.Context, workdir string, msg chatMessage, withContinue, verbose bool) (*chatReply, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, kimiTimeout)
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
		log.Printf("[kimi] spawn %s --work-dir=%s --quiet --yolo%s --prompt=<%d bytes> (thread=%d)",
			kimiBinary, workdir, continueFlagLog(withContinue), len(msg.Content), msg.ThreadID)
	}
	cmd := exec.CommandContext(cctx, kimiBinary, args...)
	cmd.Dir = workdir

	stdoutBytes, stderrBytes, waitErr := runCapture(cmd)

	reply := &chatReply{
		Kind:    "chat_reply",
		ID:      msg.ID,
		Content: truncate(string(stdoutBytes), kimiMaxStdout),
		Stderr:  truncate(string(stderrBytes), kimiMaxStderr),
	}
	if cctx.Err() == context.DeadlineExceeded {
		reply.OK = false
		reply.Error = fmt.Sprintf("kimi-cli timed out after %s", kimiTimeout)
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

func runCapture(cmd *exec.Cmd) ([]byte, []byte, error) {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start kimi-cli: %w", err)
	}
	stdoutBytes := readAll(stdoutPipe)
	stderrBytes := readAll(stderrPipe)
	return stdoutBytes, stderrBytes, cmd.Wait()
}

func readAll(rc interface{ Read(p []byte) (int, error) }) []byte {
	var out []byte
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			return out
		}
	}
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
