package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// runAgentLoop opens a WebSocket to /ws/agent, reads tool_call /
// chat_message envelopes, dispatches them appropriately, and sends
// back tool_result / chat_reply. Reconnects with exponential
// backoff on disconnect. spec is non-nil when running in
// passthrough mode (kimi/claude/codex/...); nil = tool-call loop.
func runAgentLoop(cfg AgentConfig, botID, workdir string, verbose bool, spec *toolSpec) error {
	backoff := 1 * time.Second
	for {
		err := runOneSession(cfg, botID, workdir, verbose, spec)
		if err == nil {
			return nil
		}
		log.Printf("session ended: %v — reconnect in %s", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func runOneSession(cfg AgentConfig, botID, workdir string, verbose bool, spec *toolSpec) error {
	wsURL, err := buildWSURL(cfg.Server, cfg.Token, botID, workdir)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	log.Printf("connected to %s", strings.SplitN(wsURL, "?", 2)[0])

	// Serialize all writes — gorilla/websocket forbids concurrent
	// writers and we have multiple goroutines (one per tool_call /
	// chat_message) producing replies.
	var writeMu sync.Mutex
	send := func(payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.TextMessage, payload)
	}

	hello := map[string]any{
		"kind":         "hello",
		"version":      "polar-agent 0.3",
		"workdir":      workdir,
		"capabilities": helloCapabilities(spec != nil),
	}
	if spec != nil {
		hello["tool"] = spec.Name
	}
	if b, err := json.Marshal(hello); err == nil {
		_ = send(b)
	}

	conn.SetReadLimit(8 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPingHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return conn.WriteControl(websocket.PongMessage, nil, time.Now().Add(5*time.Second))
	})

	exec := newExecutor(workdir, verbose)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			log.Printf("bad message: %v", err)
			continue
		}
		switch head.Kind {
		case "tool_call":
			var call toolCall
			if err := json.Unmarshal(raw, &call); err != nil {
				log.Printf("tool_call parse: %v", err)
				continue
			}
			go func(c toolCall) {
				result := exec.dispatch(ctx, c)
				if b, err := json.Marshal(result); err == nil {
					if err := send(b); err != nil {
						log.Printf("write tool_result: %v", err)
					}
				}
			}(call)
		case "chat_message":
			var msg chatMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				log.Printf("chat_message parse: %v", err)
				continue
			}
			if spec == nil {
				// Server shouldn't route a chat_message here without
				// the passthrough capability, but be defensive.
				emitChatReply(send, &chatReply{
					Kind:  "chat_reply",
					ID:    msg.ID,
					OK:    false,
					Error: "agent not started in passthrough mode (--tool=<name>)",
				})
				continue
			}
			go func(m chatMessage) {
				reply, err := invokeTool(ctx, spec, workdir, m, verbose)
				if err != nil {
					emitChatReply(send, &chatReply{
						Kind:  "chat_reply",
						ID:    m.ID,
						OK:    false,
						Error: err.Error(),
					})
					return
				}
				emitChatReply(send, reply)
			}(msg)
		case "ping":
			// app-level ping (websocket protocol pings handled above)
		default:
			log.Printf("unknown kind=%s", head.Kind)
		}
	}
}

func helloCapabilities(passthrough bool) []string {
	caps := []string{"tools"}
	if passthrough {
		// "passthrough" is the new generic capability. "kimi" is
		// kept alongside for backward compat with platforms that
		// haven't been updated yet — they keyed routing on the
		// literal "kimi" string.
		caps = append(caps, "passthrough", "kimi")
	}
	return caps
}

func buildWSURL(server, token, botID, workdir string) (string, error) {
	u, err := url.Parse(strings.TrimRight(server, "/") + "/ws/agent")
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	q := u.Query()
	q.Set("token", token)
	q.Set("bot_id", botID)
	q.Set("workdir", workdir)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
