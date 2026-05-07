package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// runAgentLoop opens a WebSocket to /ws/agent, reads tool_call
// messages, dispatches them to the local executor, and sends back
// tool_result. Reconnects with exponential backoff on disconnect.
func runAgentLoop(cfg AgentConfig, botID, workdir string, verbose bool) error {
	backoff := 1 * time.Second
	for {
		err := runOneSession(cfg, botID, workdir, verbose)
		if err == nil {
			return nil
		}
		log.Printf("session ended: %v — reconnect in %s", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		// Reset backoff after a successful long-lived session ?
		// Track via err nil for >60s; left out for MVP.
	}
}

func runOneSession(cfg AgentConfig, botID, workdir string, verbose bool) error {
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

	// Optional handshake: tell the server who we are.
	hello := map[string]any{
		"kind":    "hello",
		"version": "polar-agent 0.1",
		"workdir": workdir,
	}
	if b, err := json.Marshal(hello); err == nil {
		_ = conn.WriteMessage(websocket.TextMessage, b)
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
					if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
						log.Printf("write tool_result: %v", err)
					}
				}
			}(call)
		case "ping":
			// app-level ping (websocket protocol pings handled above)
		default:
			log.Printf("unknown kind=%s", head.Kind)
		}
	}
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
