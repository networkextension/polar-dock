package dock

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// Agent-as-proxy-client: instead of plumbing the raw upstream LLM
// config (base_url + api_key) into the research_task envelope — which
// leaks the provider key to every host and leaves agent LLM spend
// un-billed and un-audited — dock hands the agent a short-lived proxy
// token and points it at dock's own /api/proxy/v1. Every agent LLM
// call then flows through the proxy: billed (llm_proxy_calls + cost
// ledger), rate-limited, quota-gated, and audited. Upstream keys never
// leave dock.
//
// Kill switch: POLAR_AGENT_LLM_DIRECT=1 restores the legacy
// direct-to-upstream behaviour (snapshot passes through unchanged).

// proxyPublicBaseURL is the origin the agent should hit for the LLM
// proxy. POLAR_PUBLIC_BASE_URL may point at a subdomain (it's reused
// for agent WS connect), so allow a dedicated override and fall back to
// the same core base the nav uses.
func (s *Server) proxyPublicBaseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("POLAR_PROXY_PUBLIC_BASE_URL")), "/"); v != "" {
		return v
	}
	return s.navCoreBase()
}

// agentLLMProxySnapshot rewrites a research LLM snapshot so the agent
// calls dock's proxy. It mints a per-run proxy token whose default
// route is the resolved llm_config (so the agent can keep sending the
// real model name and it routes correctly), embeds dock's proxy base +
// that token, and clears ProxyURL (the agent talks to dock directly,
// not through an upstream HTTP proxy). Fail-open: on any error it
// returns the original direct snapshot so dispatch still succeeds.
func (s *Server) agentLLMProxySnapshot(base agentResearchLLMSnapshot, workspaceID, ownerUserID, botUserID string, runID int64, now time.Time) agentResearchLLMSnapshot {
	if os.Getenv("POLAR_AGENT_LLM_DIRECT") == "1" {
		return base
	}
	proxyBase := s.proxyPublicBaseURL()
	if proxyBase == "" {
		log.Printf("agent llm proxy: no public base url; run=%d falls back to direct upstream", runID)
		return base
	}

	// P2: prefer the agent's own per-agent proxy token so spend is
	// attributed per-agent. Route the task's chosen config under a
	// stable alias on that token, then have the agent send the alias.
	if snap, ok := s.agentOwnProxySnapshot(base, botUserID, proxyBase, now); ok {
		return snap
	}

	// Fallback (pre-P2 agents with no own token): mint a per-run token.
	var defID *int64
	if base.ConfigID > 0 {
		cfgID := base.ConfigID
		defID = &cfgID
	}
	// rate_limit 600rpm so iterative runs aren't throttled; quota 0 =
	// unlimited at the token level — the per-config team quota / credit
	// balance still gates spend inside the chat handler.
	tok, err := s.createLLMProxyToken(workspaceID, ownerUserID, agentResearchTokenName(runID), defID, 600, 0, "chat.completions", now)
	if err != nil || tok == nil || strings.TrimSpace(tok.Plaintext) == "" {
		log.Printf("agent llm proxy: mint token run=%d: %v; falls back to direct", runID, err)
		return base
	}
	return agentResearchLLMSnapshot{
		ConfigID: base.ConfigID,
		Name:     base.Name,
		Model:    base.Model,
		Provider: base.Provider,
		BaseURL:  proxyBase + "/api/proxy/v1",
		APIKey:   tok.Plaintext,
		ProxyURL: "",
	}
}

func agentResearchTokenName(runID int64) string {
	return fmt.Sprintf("agent-research:run-%d", runID)
}

// agentOwnProxySnapshot routes a research task through the agent's
// per-agent proxy token (P2). It looks up the agent by bot, decrypts
// the stored token, upserts a route mapping a stable alias (cfg-<id>)
// to the task's chosen llm_config, and returns a snapshot pointing the
// agent at the proxy with that alias as the model. Returns (_, false)
// when the agent has no own token yet so the caller falls back to a
// per-run token.
func (s *Server) agentOwnProxySnapshot(base agentResearchLLMSnapshot, botUserID, proxyBase string, now time.Time) (agentResearchLLMSnapshot, bool) {
	ag, err := s.getAgentProxyByBotUserID(botUserID)
	if err != nil || ag == nil {
		return agentResearchLLMSnapshot{}, false
	}
	raw, derr := s.decryptStoredAgentProxyToken(ag.Enc, ag.Encrypted)
	if derr != nil || strings.TrimSpace(raw) == "" {
		log.Printf("agent llm proxy: decrypt per-agent token agent=%s: %v; using per-run fallback", ag.AgentID, derr)
		return agentResearchLLMSnapshot{}, false
	}
	model := base.Model
	if base.ConfigID > 0 {
		alias := fmt.Sprintf("cfg-%d", base.ConfigID)
		if rerr := s.upsertLLMProxyRoute(ag.TokenID, alias, base.ConfigID, now); rerr != nil {
			log.Printf("agent llm proxy: upsert route token=%d cfg=%d: %v; using per-run fallback", ag.TokenID, base.ConfigID, rerr)
			return agentResearchLLMSnapshot{}, false
		}
		model = alias
	}
	return agentResearchLLMSnapshot{
		ConfigID: base.ConfigID,
		Name:     base.Name,
		Model:    model,
		Provider: base.Provider,
		BaseURL:  proxyBase + "/api/proxy/v1",
		APIKey:   raw,
		ProxyURL: "",
	}, true
}

// revokeAgentResearchToken best-effort revokes the per-run proxy token
// once the run terminates, so a leaked envelope token can't be reused
// after the work is done. Silent on miss (kill switch / fail-open path
// minted no token).
func (s *Server) revokeAgentResearchToken(workspaceID string, runID int64, now time.Time) {
	if strings.TrimSpace(workspaceID) == "" {
		return
	}
	if _, err := s.db.Exec(
		`UPDATE llm_proxy_tokens
		    SET revoked_at = COALESCE(revoked_at, $3)
		  WHERE workspace_id = $1 AND name = $2`,
		workspaceID, agentResearchTokenName(runID), now,
	); err != nil {
		log.Printf("agent llm proxy: revoke token run=%d: %v", runID, err)
	}
}
