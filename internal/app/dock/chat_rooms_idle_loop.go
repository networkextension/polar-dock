package dock

// chat_rooms_idle_loop.go — qunliao P3: background goroutine that
// gives bots autonomy. Every 30s the loop polls chat_room_agent_loops
// for enabled rows whose interval has elapsed, asks the bot's LLM
// "should you post right now? if yes, what?" via a JSON-decision
// prompt, and inserts the result as a normal room message if so.
//
// Spawned once at server start (app.go alongside runChatEventSubscriber).
// Single goroutine driver; per-bot decisions fan out into ephemeral
// goroutines so a slow LLM doesn't block the next tick. Same
// fire-and-forget pattern as chat_rooms_agent.go's fanoutOneBotReply.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// idleLoopTickInterval — how often the driver wakes up to scan due
// rows. Independent of any single bot's interval_secs; the per-bot
// gap is enforced in the SQL `last_decided_at + interval_secs <= now`.
const idleLoopTickInterval = 30 * time.Second

// idleLoopBatchSize bounds rows pulled per tick — keeps a workspace
// with hundreds of enabled loops from monopolizing one tick.
const idleLoopBatchSize = 32

// idleLoopMaxTokens — decision prompt + reply are short by design;
// 256 is enough for a 2-3 sentence chime-in.
const idleLoopMaxTokens = 256

// idleDecision is what the LLM is asked to emit for a tick.
type idleDecision struct {
	Action  string `json:"action"`            // "post" | "nothing"
	Content string `json:"content,omitempty"` // required when action="post"
}

// runRoomAgentIdleLoop is the long-lived driver. Exits when ctx is
// cancelled (graceful shutdown). Logs errors, never panics.
func (s *Server) runRoomAgentIdleLoop(ctx context.Context) {
	if s == nil || s.aiAgent == nil {
		log.Printf("room idle loop: server or aiAgent nil, refusing to start")
		return
	}
	t := time.NewTicker(idleLoopTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.idleLoopTick(ctx)
		}
	}
}

func (s *Server) idleLoopTick(ctx context.Context) {
	now := time.Now().UTC()
	loops, err := s.dueChatRoomAgentLoops(ctx, now, idleLoopBatchSize)
	if err != nil {
		log.Printf("room idle loop: dueChatRoomAgentLoops failed: %v", err)
		return
	}
	for _, l := range loops {
		loopRow := l
		go s.idleLoopDecideOne(ctx, loopRow, now)
	}
}

// idleLoopDecideOne handles one (room, bot) decision cycle. Steps:
//  1. Skip if already at hourly cap (no LLM call wasted).
//  2. Mark decided NOW (claims the tick — avoids double-fire on slow LLMs).
//  3. Billing gate; if blocked, disable loop + log.
//  4. Build context from last 20 room messages.
//  5. Ask LLM for {action, content}.
//  6. If action=post + content non-empty: insert room message + WS publish.
//  7. Record cost ledger regardless.
func (s *Server) idleLoopDecideOne(parentCtx context.Context, l ChatRoomAgentLoop, now time.Time) {
	ctx, cancel := context.WithTimeout(parentCtx, 90*time.Second)
	defer cancel()

	// Hourly cap pre-check. If hour_window_start is fresh + counter is
	// at the cap, skip the LLM call entirely. The window rotates in
	// markChatRoomAgentLoopDecided so untouched loops naturally roll over.
	if l.HourWindowStart != nil && now.Sub(*l.HourWindowStart) < time.Hour && l.DecisionsThisHour >= l.MaxPerHour {
		log.Printf("room idle: throttled room=%d bot=%s (%d/%d this hour)", l.RoomID, l.BotUserID, l.DecisionsThisHour, l.MaxPerHour)
		// Still bump last_decided_at so we don't hot-spin on this row.
		if err := s.markChatRoomAgentLoopDecided(ctx, l.RoomID, l.BotUserID, now, false); err != nil {
			log.Printf("room idle: mark decided (throttled) failed room=%d bot=%s: %v", l.RoomID, l.BotUserID, err)
		}
		return
	}

	room, err := s.getChatRoom(ctx, l.RoomID)
	if err != nil || room == nil || room.ArchivedAt != nil {
		log.Printf("room idle: room missing/archived room=%d: %v", l.RoomID, err)
		_ = s.markChatRoomAgentLoopDecided(ctx, l.RoomID, l.BotUserID, now, false)
		return
	}

	bot, err := s.getBotUserByUserID(l.BotUserID)
	if err != nil || bot == nil {
		log.Printf("room idle: bot missing room=%d bot=%s: %v", l.RoomID, l.BotUserID, err)
		_ = s.markChatRoomAgentLoopDecided(ctx, l.RoomID, l.BotUserID, now, false)
		return
	}
	if bot.LLMConfigID <= 0 {
		log.Printf("room idle: bot %q has no llm_config_id, disabling loop", bot.Name)
		_ = s.disableChatRoomAgentLoop(ctx, l.RoomID, l.BotUserID)
		return
	}

	if blocked, msg := s.checkChatBillingGate(room.WorkspaceID, bot.LLMConfigID); blocked {
		log.Printf("room idle: billing gate blocked room=%d bot=%q — auto-disabling. reason=%s", l.RoomID, bot.Name, msg)
		if dErr := s.disableChatRoomAgentLoop(ctx, l.RoomID, l.BotUserID); dErr != nil {
			log.Printf("room idle: disable on quota fail room=%d bot=%s: %v", l.RoomID, l.BotUserID, dErr)
		}
		return
	}

	cfg, apiKey, err := s.getAvailableLLMConfigWithAPIKey(room.WorkspaceID, bot.LLMConfigID)
	if err != nil || cfg == nil || strings.TrimSpace(apiKey) == "" {
		log.Printf("room idle: llm cfg unavailable room=%d bot=%q cfg=%d: %v", l.RoomID, bot.Name, bot.LLMConfigID, err)
		_ = s.markChatRoomAgentLoopDecided(ctx, l.RoomID, l.BotUserID, now, false)
		return
	}

	// Mark decided BEFORE the LLM call so a slow upstream doesn't
	// cause the next 30s tick to fire this loop again.
	if err := s.markChatRoomAgentLoopDecided(ctx, l.RoomID, l.BotUserID, now, false); err != nil {
		log.Printf("room idle: mark decided (pre) failed room=%d bot=%s: %v", l.RoomID, l.BotUserID, err)
	}

	msgs, err := s.listRoomMessages(ctx, l.RoomID, nil, roomFanoutContextLimit)
	if err != nil {
		log.Printf("room idle: load history failed room=%d: %v", l.RoomID, err)
		return
	}

	systemPrompt := buildIdleSystemPrompt(bot, room)
	chatMsgs := buildRoomChatHistory(msgs, bot.BotUserID, systemPrompt)
	chatMsgs = append(chatMsgs, aiChatCompletionMessage{
		Role: "user",
		Content: `（系统）请基于以上对话决定你现在是否要主动发言。严格输出 JSON：` +
			"\n" + `{"action":"post"|"nothing","content":"..."}` + "\n" +
			`如果 action 为 "post"，content 必填，1-3 句话，符合你的角色。否则 content 留空。`,
	})

	// Qunliao β: respect per-bot model_override.
	modelToUse := cfg.Model
	if strings.TrimSpace(bot.ModelOverride) != "" {
		modelToUse = strings.TrimSpace(bot.ModelOverride)
	}
	runtimeCfg := aiRuntimeConfig{
		APIKey:        apiKey,
		BaseURL:       cfg.BaseURL,
		Model:         modelToUse,
		SystemPrompt:  systemPrompt,
		Streaming:     false,
		ProxyURL:      cfg.ProxyURL,
		LLMConfigID:   cfg.ID,
		LLMConfigName: cfg.Name,
		WorkspaceID:   room.WorkspaceID,
		Endpoint:      "room_idle_loop",
	}

	start := time.Now()
	resp, err := s.aiAgent.requestChatCompletion(runtimeCfg, aiChatCompletionRequest{
		Model:     modelToUse,
		Messages:  chatMsgs,
		MaxTokens: idleLoopMaxTokens,
	})
	latencyMS := int(time.Since(start).Milliseconds())

	// Ledger every attempt.
	prompt, completion, total := 0, 0, 0
	status := http.StatusOK
	errText := ""
	if resp != nil && resp.Usage != nil {
		prompt = resp.Usage.PromptTokens
		completion = resp.Usage.CompletionTokens
		total = resp.Usage.TotalTokens
	}
	if err != nil {
		status = http.StatusInternalServerError
		errText = truncateForLog(err.Error(), 200)
	}
	if recErr := s.recordAgentLLMCall(
		room.WorkspaceID, cfg.ID, modelToUse, modelToUse,
		prompt, completion, total, latencyMS, status, errText, nil,
	); recErr != nil {
		log.Printf("room idle: ledger record failed room=%d bot=%q: %v", l.RoomID, bot.Name, recErr)
	}

	if err != nil {
		log.Printf("room idle: llm failed room=%d bot=%q: %v", l.RoomID, bot.Name, err)
		return
	}
	if resp == nil || len(resp.Choices) == 0 {
		return
	}
	decision, parseErr := parseIdleDecision(resp.Choices[0].Message.Content)
	if parseErr != nil {
		log.Printf("room idle: decision parse failed room=%d bot=%q: %v (raw=%s)", l.RoomID, bot.Name, parseErr, truncateForLog(resp.Choices[0].Message.Content, 200))
		return
	}
	if decision.Action != "post" {
		// Bot chose silence; nothing to insert. Tick already marked.
		return
	}
	content := strings.TrimSpace(decision.Content)
	if content == "" {
		log.Printf("room idle: bot %q said post but content empty; skipping", bot.Name)
		return
	}
	// Snapshot LLM telemetry onto the row so the metadata pill renders.
	cost, _ := s.estimateLLMProxyCallCostWithConfig(cfg.ID, modelToUse, prompt, completion)
	s.insertAndBroadcastBotReplyWithMetrics(ctx, l.RoomID, bot.BotUserID, content, roomMessageMetrics{
		LLMConfigID:      cfg.ID,
		LLMConfigName:    cfg.Name,
		LLMModel:         modelToUse,
		LatencyMS:        latencyMS,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CostUSD:          cost,
	})
	// Re-mark with posted=true so last_posted_at + counter advance.
	if err := s.markChatRoomAgentLoopDecided(ctx, l.RoomID, l.BotUserID, time.Now().UTC(), true); err != nil {
		log.Printf("room idle: mark decided (post) failed room=%d bot=%s: %v", l.RoomID, l.BotUserID, err)
	}
}

// buildIdleSystemPrompt wraps the bot's own system_prompt with role +
// room context. Kept compact — the LLM already saw the chat history,
// this just sets behavior for autonomy mode.
func buildIdleSystemPrompt(bot *BotUser, room *ChatRoom) string {
	parts := []string{}
	if strings.TrimSpace(bot.SystemPrompt) != "" {
		parts = append(parts, bot.SystemPrompt)
	}
	roomCtx := fmt.Sprintf("当前房间：%s", room.Name)
	if strings.TrimSpace(room.Topic) != "" {
		roomCtx += "；话题：" + room.Topic
	}
	parts = append(parts, roomCtx)
	parts = append(parts,
		`你叫 "`+bot.Name+`"。这是一次"自主决定是否要发言"的轮询，不是用户在 @ 你。`+
			`如果没有什么值得补充的，请选择保持沉默（action:"nothing"）。`+
			`不要重复别人已经说过的、不要客套寒暄。只在确实有价值时才发言。`)
	return strings.Join(parts, "\n\n")
}

// parseIdleDecision pulls {action, content} out of the LLM reply.
// Tolerates ```json fences using the same jsonFenceRe used in expense_extract.
func parseIdleDecision(raw string) (*idleDecision, error) {
	body := strings.TrimSpace(raw)
	if m := jsonFenceRe.FindStringSubmatch(body); len(m) == 2 {
		body = strings.TrimSpace(m[1])
	}
	var d idleDecision
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return nil, err
	}
	switch d.Action {
	case "post", "nothing":
		return &d, nil
	case "":
		// Tolerate models that just emit {} for "do nothing"
		d.Action = "nothing"
		return &d, nil
	default:
		return nil, fmt.Errorf("invalid action %q", d.Action)
	}
}
