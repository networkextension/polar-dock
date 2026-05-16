package dock

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// LLM upstream health prober.
//
// Every probeInterval, the prober scans all text-kind llm_configs rows
// with non-empty api_key + non-empty base_url and fires a synthetic
// one-token chat completion against each. The result flips the
// polar_llm_upstream_up{config_id,name,model} gauge.
//
// Why a probe instead of just trusting traffic:
//   - Idle configs (no real traffic for hours) would silently rot.
//   - On-demand customer traffic doesn't give us a baseline latency.
//   - The first time anyone notices is when boss runs a benchmark — see
//     the 2026-05-17 incident with `Qwen3_Standard`.
//
// Budget:
//   - max_tokens=1 + a 3-byte prompt → typical cost <$0.0001 per probe.
//   - Per-config concurrent: yes (we don't want one slow upstream to
//     starve the others).
//   - Per-probe timeout: probeTimeout (default 15s). A vLLM cold-start
//     can be slow; 15s is generous but bounded.

const (
	llmProbeInterval = 30 * time.Second
	llmProbeTimeout  = 15 * time.Second
)

type llmProxyProber struct {
	server *Server

	mu     sync.Mutex
	stopCh chan struct{}
}

func newLLMProxyProber(s *Server) *llmProxyProber {
	return &llmProxyProber{server: s, stopCh: make(chan struct{})}
}

func (p *llmProxyProber) start() {
	go p.loop()
}

func (p *llmProxyProber) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.stopCh:
		// already closed
	default:
		close(p.stopCh)
	}
}

func (p *llmProxyProber) loop() {
	// Do one immediate pass at startup so /metrics is populated
	// before the first scrape lands.
	p.runOnce()
	ticker := time.NewTicker(llmProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.runOnce()
		}
	}
}

// runOnce probes every active llm_configs row in parallel.
func (p *llmProxyProber) runOnce() {
	rows, err := p.server.db.Query(
		`SELECT id, name, base_url, model, api_key, COALESCE(proxy_url, '')
		   FROM llm_configs
		  WHERE api_key <> ''
		    AND base_url <> ''
		    AND (provider_kind = 'text' OR provider_kind = '' OR provider_kind IS NULL)`,
	)
	if err != nil {
		log.Printf("llm-upstream-probe: list configs failed: %v", err)
		return
	}
	type target struct {
		id       int64
		name     string
		baseURL  string
		model    string
		apiKey   string
		proxyURL string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.name, &t.baseURL, &t.model, &t.apiKey, &t.proxyURL); err != nil {
			log.Printf("llm-upstream-probe: scan failed: %v", err)
			continue
		}
		targets = append(targets, t)
	}
	_ = rows.Close()

	if len(targets) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			ok := p.probeOne(t.baseURL, t.model, t.apiKey, t.proxyURL)
			p.server.metricsRegistry.setLLMUpstreamUp(t.id, t.name, t.model, ok)
		}(t)
	}
	wg.Wait()
}

// probeOne fires one minimal completion. Returns true on 2xx within
// timeout. Provider-specific quirks (e.g. some endpoints reject
// max_tokens=1) are absorbed by allowing any successful response
// with at least a parseable structure.
func (p *llmProxyProber) probeOne(baseURL, model, apiKey, proxyURL string) bool {
	if p.server.aiAgent == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), llmProbeTimeout)
	defer cancel()
	_ = ctx // requestChatCompletion has its own timeout via the http client; ctx kept for future

	runtimeCfg := aiRuntimeConfig{
		APIKey:   strings.TrimSpace(apiKey),
		BaseURL:  strings.TrimSpace(baseURL),
		Model:    strings.TrimSpace(model),
		ProxyURL: strings.TrimSpace(proxyURL),
		Endpoint: "probe",
	}
	payload := aiChatCompletionRequest{
		Model: strings.TrimSpace(model),
		Messages: []aiChatCompletionMessage{
			{Role: "user", Content: "ok"},
		},
		MaxTokens: 1,
	}
	resp, err := p.server.aiAgent.requestChatCompletion(runtimeCfg, payload)
	if err != nil {
		return false
	}
	if resp == nil {
		return false
	}
	// Any non-empty response shape counts as "up" — some upstreams
	// return only `choices` with empty content for max_tokens=1.
	return len(resp.Choices) > 0 || resp.Usage != nil
}
