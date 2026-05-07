package main

// kimi-cli passthrough mode. When attach is started with --kimi we
// declare the "kimi" capability in our hello frame, and the
// platform routes chat messages here as `chat_message` envelopes
// instead of running its own tool-call loop. Each chat_message
// gets piped to a fresh `kimi-cli --work-dir=$workdir --yolo --afk
// --continue` invocation; stdout becomes the bot's reply.
//
// Per 用户 ("io 转发到 chat user"): we don't try to track sessions
// across threads — kimi-cli's own --continue handles work-dir-
// scoped resume. One bot + one workdir = one rolling kimi session.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	kimiBinary  = "kimi-cli"
	kimiTimeout = 10 * time.Minute
	// stdout/stderr caps before we send the reply back. kimi-cli
	// can be chatty; we cap so a runaway loop doesn't exceed the
	// 8 MiB ws read limit on the platform side.
	kimiMaxStdout = 200_000
	kimiMaxStderr = 50_000
)

// kimiInvoke spawns kimi-cli with the chat content as stdin and
// returns its captured output. Returns (reply, nil) on any kimi
// outcome (success or failure) — the OK flag inside reply
// distinguishes. Returns (nil, err) only for orchestration errors
// (binary missing, can't pipe stdin, etc.).
func kimiInvoke(ctx context.Context, workdir string, msg chatMessage, verbose bool) (*chatReply, error) {
	cctx, cancel := context.WithTimeout(ctx, kimiTimeout)
	defer cancel()

	args := []string{
		"--work-dir=" + workdir,
		"--yolo",
		"--afk",
		"--continue",
	}
	if verbose {
		log.Printf("[kimi] spawn %s %v (thread=%d, %d bytes input)",
			kimiBinary, args, msg.ThreadID, len(msg.Content))
	}
	cmd := exec.CommandContext(cctx, kimiBinary, args...)
	cmd.Dir = workdir

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start kimi-cli: %w", err)
	}

	go func() {
		defer stdinPipe.Close()
		_, _ = io.WriteString(stdinPipe, msg.Content)
	}()

	stdoutBytes, _ := io.ReadAll(stdoutPipe)
	stderrBytes, _ := io.ReadAll(stderrPipe)
	waitErr := cmd.Wait()

	reply := &chatReply{
		Kind:    "chat_reply",
		ID:      msg.ID,
		Content: truncate(string(stdoutBytes), kimiMaxStdout),
		Stderr:  truncate(string(stderrBytes), kimiMaxStderr),
	}
	if cctx.Err() == context.DeadlineExceeded {
		reply.OK = false
		reply.Error = fmt.Sprintf("kimi-cli timed out after %s", kimiTimeout)
		return reply, nil
	}
	if waitErr != nil {
		reply.OK = false
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			reply.Error = fmt.Sprintf("kimi-cli exit %d", ee.ExitCode())
		} else {
			reply.Error = waitErr.Error()
		}
		return reply, nil
	}
	reply.OK = true
	if strings.TrimSpace(reply.Content) == "" && reply.Stderr != "" {
		// Some CLIs print useful info to stderr on success; surface
		// a small note so the user sees something rather than a
		// blank reply.
		reply.Content = "(kimi-cli 没有 stdout，详见 stderr)"
	}
	return reply, nil
}

// emitChatReply serializes + writes a chat_reply through the
// connection's write channel. Marshals here so the caller doesn't
// need to know JSON.
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
