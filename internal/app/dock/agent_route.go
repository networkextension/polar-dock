package dock

// agent_route.go — Phase 4b: dispatch routing wrappers.
//
// After the nginx /ws/agent → `:8095` cutover, agents connect to polar-hosts
// rather than dock. dock's local agentHub is empty. Each wrapper tries the
// local hub first (in-process, fast) then falls back to polar-hosts via HTTP.
//
// Pre-cutover: local hub always hits → zero extra HTTP overhead.
// Post-cutover: local hub always misses → every dispatch goes to polar-hosts.
// The flag `POLAR_HOSTS_TOUCH_URL` drives whether the fallback base URL is set.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// remoteAgentInfo is the deserialised presence response from polar-hosts.
type remoteAgentInfo struct {
	Attached       bool     `json:"attached"`
	BotUserID      string   `json:"bot_user_id"`
	HasPassthrough bool     `json:"has_passthrough"`
	Tool           string   `json:"tool"`
	Workdir        string   `json:"workdir"`
	Capabilities   []string `json:"capabilities"`
	HostID         string   `json:"host_id"`
}

// ── Presence ──────────────────────────────────────────────────────────────

// routeAgentLookup returns agent info from the local hub or from polar-hosts.
// Returns nil when no agent is attached (either locally or remotely).
func (s *Server) routeAgentLookup(key string) *remoteAgentInfo {
	if s.agentHub != nil {
		if conn := s.agentHub.lookup(key); conn != nil {
			return &remoteAgentInfo{
				Attached:       true,
				BotUserID:      conn.botUserID,
				HasPassthrough: conn.hasPassthrough(),
				Tool:           conn.tool,
				Workdir:        conn.workdir,
				Capabilities:   conn.capabilities,
				HostID:         conn.hostID,
			}
		}
	}
	if s.polarHostsBaseURL == "" {
		return nil
	}
	info, err := s.fetchRemoteAgentPresence(key)
	if err != nil || info == nil || !info.Attached {
		return nil
	}
	return info
}

func (s *Server) fetchRemoteAgentPresence(botID string) (*remoteAgentInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := s.polarHostsBaseURL + "/internal/v1/agents/presence?bot_id=" + botID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presence: HTTP %d", resp.StatusCode)
	}
	var info remoteAgentInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ── Tool call ─────────────────────────────────────────────────────────────

func (s *Server) routeAgentToolCall(botUserID, callID, tool string, args map[string]any) (*agentToolResult, error) {
	if s.agentHub != nil {
		result, err := s.agentHub.dispatchToolCall(botUserID, callID, tool, args)
		if !errors.Is(err, errAgentNotAttached) {
			return result, err
		}
	}
	if s.polarHostsBaseURL == "" {
		return nil, errAgentNotAttached
	}
	return s.remoteDispatchToolCall(botUserID, callID, tool, args)
}

func (s *Server) remoteDispatchToolCall(botUserID, callID, tool string, args map[string]any) (*agentToolResult, error) {
	body, _ := json.Marshal(map[string]any{
		"bot_id":  botUserID,
		"call_id": callID,
		"tool":    tool,
		"args":    args,
	})
	ctx, cancel := context.WithTimeout(context.Background(), agentToolCallTimeout+10*time.Second)
	defer cancel()
	resp, err := s.doRemoteDispatch(ctx, "/internal/v1/agents/dispatch/tool-call", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, remoteDispatchError(resp.StatusCode, string(msg))
	}
	var result agentToolResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ── Chat passthrough ───────────────────────────────────────────────────────

func (s *Server) routeAgentChatMessage(botUserID string, threadID int64, content, workdirSubpath, gitRemoteURL string) (*agentChatReply, error) {
	if s.agentHub != nil {
		reply, err := s.agentHub.dispatchChatMessage(botUserID, threadID, content, workdirSubpath, gitRemoteURL)
		if !errors.Is(err, errAgentNotAttached) {
			return reply, err
		}
	}
	if s.polarHostsBaseURL == "" {
		return nil, errAgentNotAttached
	}
	return s.remoteDispatchChatMessage(botUserID, threadID, content, workdirSubpath, gitRemoteURL)
}

func (s *Server) remoteDispatchChatMessage(botUserID string, threadID int64, content, workdirSubpath, gitRemoteURL string) (*agentChatReply, error) {
	body, _ := json.Marshal(map[string]any{
		"bot_id":          botUserID,
		"thread_id":       threadID,
		"content":         content,
		"workdir_subpath": workdirSubpath,
		"git_remote_url":  gitRemoteURL,
	})
	ctx, cancel := context.WithTimeout(context.Background(), agentChatTimeout+10*time.Second)
	defer cancel()
	resp, err := s.doRemoteDispatch(ctx, "/internal/v1/agents/dispatch/chat-message", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, remoteDispatchError(resp.StatusCode, string(msg))
	}
	var reply agentChatReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// ── Fire-and-forget frame (research_task, task.wake) ──────────────────────

func (s *Server) routeAgentFrame(botUserID string, frame map[string]any) error {
	if s.agentHub != nil {
		conn := s.agentHub.lookup(botUserID)
		if conn != nil {
			payload, err := json.Marshal(frame)
			if err != nil {
				return err
			}
			select {
			case conn.send <- payload:
				return nil
			case <-conn.close:
				return errAgentDisconnected
			case <-time.After(5 * time.Second):
				return errAgentSendTimeout
			}
		}
	}
	if s.polarHostsBaseURL == "" {
		return errAgentNotAttached
	}
	body, _ := json.Marshal(map[string]any{"bot_id": botUserID, "frame": frame})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := s.doRemoteDispatch(ctx, "/internal/v1/agents/dispatch/frame", body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dispatch frame: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ── HTTP helper ───────────────────────────────────────────────────────────

func (s *Server) doRemoteDispatch(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.polarHostsBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// researchTaskToFrame converts an agentResearchTask struct to a generic
// map[string]any so routeAgentFrame can marshal it uniformly.
func researchTaskToFrame(t agentResearchTask) map[string]any {
	b, _ := json.Marshal(t)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func remoteDispatchError(status int, body string) error {
	switch status {
	case http.StatusServiceUnavailable:
		return errAgentNotAttached
	case http.StatusGatewayTimeout:
		return errAgentTimeout
	case http.StatusBadGateway:
		return errAgentDisconnected
	}
	return fmt.Errorf("remote dispatch HTTP %d: %s", status, body)
}

// ── Phase 4b follow-ups: host-keyed lookup/dispatch + kick ─────────────────
//
// After the /ws/agent cutover the agent for a host lives in polar-hosts'
// hub. These helpers keep dock features that address agents by host_id
// (skill install/uninstall) or need to kick an agent working: local hub
// first, then resolve the host's bot via the agents table and go remote.

// botUserIDForHost resolves the bot bound to a host from the agents table
// (v4: one agent row per host).
func (s *Server) botUserIDForHost(hostID string) string {
	var bot sql.NullString
	_ = s.db.QueryRow(`SELECT bot_user_id FROM agents WHERE host_id = $1 AND bot_user_id IS NOT NULL ORDER BY last_hello_at DESC NULLS LAST LIMIT 1`, hostID).Scan(&bot)
	if bot.Valid {
		return bot.String
	}
	return ""
}

// routeAgentFrameByHost sends a fire-and-forget frame to the agent attached
// for hostID (local hub or polar-hosts).
func (s *Server) routeAgentFrameByHost(hostID string, frame map[string]any) error {
	if s.agentHub != nil {
		if conn := s.agentHub.lookupByHostID(hostID); conn != nil {
			payload, err := json.Marshal(frame)
			if err != nil {
				return err
			}
			select {
			case conn.send <- payload:
				return nil
			case <-conn.close:
				return errAgentDisconnected
			case <-time.After(5 * time.Second):
				return errAgentSendTimeout
			}
		}
	}
	if s.polarHostsBaseURL == "" {
		return errAgentNotAttached
	}
	bot := s.botUserIDForHost(hostID)
	if bot == "" {
		return errAgentNotAttached
	}
	return s.routeAgentFrame(bot, frame)
}

// routeAgentKick closes the agent connection identified by agent_id and/or
// bot_user_id, locally or in polar-hosts. Returns true when a live
// connection was found and closed.
func (s *Server) routeAgentKick(agentID, botUserID string) bool {
	if s.agentHub != nil {
		conn := s.agentHub.lookupByAgentID(agentID)
		if conn == nil && botUserID != "" {
			conn = s.agentHub.lookup(botUserID)
		}
		if conn != nil {
			safeCloseAgentConn(conn)
			return true
		}
	}
	if s.polarHostsBaseURL == "" {
		return false
	}
	body, _ := json.Marshal(map[string]any{"agent_id": agentID, "bot_id": botUserID})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.doRemoteDispatch(ctx, "/internal/v1/agents/kick", body)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Kicked bool `json:"kicked"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode == http.StatusOK && out.Kicked
}
