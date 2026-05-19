package dock

// chat_rooms_agent.go — qunliao P1: when a user @-mentions a bot in a
// room message, fan out one LLM call per matched bot and persist each
// reply as a new room message. Fire-and-forget goroutine pattern,
// mirrors chat_thread_summary_handlers.go:73 (no waitgroup, errors
// logged not propagated). All bot LLM calls ride through the existing
// billing gate + cost ledger (recordAgentLLMCall).

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"
)

// roomFanoutContextLimit caps the recent-message context we feed the
// bot. 20 messages ≈ a normal scroll-back; larger blows the context
// window for cheaper models without much marginal improvement.
const roomFanoutContextLimit = 20

// roomFanoutMaxTokens bounds the reply length for non-streaming P1
// fanouts. P3 idle loops will pick something looser when that ships.
const roomFanoutMaxTokens = 512

// fanoutRoomMentions handles every @bot in the just-posted message.
// Called from handleChatRoomMessageSend after the user's row inserts
// (so the bot's reply lists strictly after). Runs the parser → lookup
// → LLM call → insert pipeline per matched bot, in parallel.
//
// Silent on miss (bot not in workspace, not in room, no LLM config) —
// shouldn't surface to the human; an @typo isn't an error. Quota +
// upstream failures land as a placeholder room message from the bot
// so the user sees what happened instead of a phantom silence.
func (s *Server) fanoutRoomMentions(workspaceID string, roomID, triggerMsgID int64, senderUserID, content string, names []string) {
	if s == nil || s.aiAgent == nil || len(names) == 0 {
		return
	}
	for _, name := range names {
		botName := name
		go s.fanoutOneBotReply(workspaceID, roomID, triggerMsgID, senderUserID, content, botName)
	}
}

func (s *Server) fanoutOneBotReply(workspaceID string, roomID, triggerMsgID int64, senderUserID, originalContent, botName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	bot, err := s.findBotInRoom(ctx, workspaceID, roomID, botName)
	if err != nil {
		log.Printf("room mention: lookup failed workspace=%s room=%d name=%q: %v", workspaceID, roomID, botName, err)
		return
	}
	if bot == nil {
		// Silent — wrong @ name. Don't post anything; the user can re-@.
		return
	}
	if bot.LLMConfigID <= 0 {
		log.Printf("room mention: bot %q (id=%d) has no llm_config_id; skipping reply", bot.Name, bot.ID)
		return
	}

	// Billing gate. checkChatBillingGate returns (blocked, userMessage).
	// On block we *do* post — as the bot — so the room sees what
	// happened. This mirrors PR #262's expense extract behavior.
	if blocked, msg := s.checkChatBillingGate(workspaceID, bot.LLMConfigID); blocked {
		_, body := classifyBotError(msg, bot.Name)
		s.insertAndBroadcastBotReply(ctx, roomID, bot.BotUserID, body)
		return
	}

	cfg, apiKey, err := s.getAvailableLLMConfigWithAPIKey(workspaceID, bot.LLMConfigID)
	if err != nil || cfg == nil {
		log.Printf("room mention: llm cfg unavailable workspace=%s bot=%q cfg=%d: %v", workspaceID, bot.Name, bot.LLMConfigID, err)
		return
	}
	if strings.TrimSpace(apiKey) == "" {
		log.Printf("room mention: bot %q llm_config %d has no api_key", bot.Name, bot.LLMConfigID)
		return
	}

	// Build context from last N room messages, oldest-first. The bot's
	// own past replies are mapped to role=assistant so the LLM sees its
	// own thread; everyone else is role=user, prefixed with the
	// username so multi-speaker context is legible.
	msgs, err := s.listRoomMessages(ctx, roomID, nil, roomFanoutContextLimit)
	if err != nil {
		log.Printf("room mention: load history failed room=%d bot=%q: %v", roomID, bot.Name, err)
		return
	}
	chatMsgs := buildRoomChatHistory(msgs, bot.BotUserID, bot.SystemPrompt)

	runtimeCfg := aiRuntimeConfig{
		APIKey:        apiKey,
		BaseURL:       cfg.BaseURL,
		Model:         cfg.Model,
		SystemPrompt:  bot.SystemPrompt,
		Streaming:     false,
		ProxyURL:      cfg.ProxyURL,
		LLMConfigID:   cfg.ID,
		LLMConfigName: cfg.Name,
		WorkspaceID:   workspaceID,
		Endpoint:      "room_mention",
		// IsSystemBot stays false — custom bots pay full cost via pricing rules.
	}

	start := time.Now()
	resp, err := s.aiAgent.requestChatCompletion(runtimeCfg, aiChatCompletionRequest{
		Model:     cfg.Model,
		Messages:  chatMsgs,
		MaxTokens: roomFanoutMaxTokens,
	})
	latencyMS := int(time.Since(start).Milliseconds())

	// Cost ledger every time — success or fail. PR #262 wired this
	// helper so per-team usage is always attributable.
	status := http.StatusOK
	errText := ""
	prompt, completion, total := 0, 0, 0
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
		workspaceID, cfg.ID, cfg.Model, cfg.Model,
		prompt, completion, total, latencyMS, status, errText, nil,
	); recErr != nil {
		log.Printf("room mention: ledger record failed workspace=%s bot=%q: %v", workspaceID, bot.Name, recErr)
	}

	if err != nil {
		log.Printf("room mention: llm failed workspace=%s bot=%q: %v", workspaceID, bot.Name, err)
		_, body := classifyBotError(err.Error(), bot.Name)
		s.insertAndBroadcastBotReply(ctx, roomID, bot.BotUserID, body)
		return
	}
	if resp == nil || len(resp.Choices) == 0 {
		log.Printf("room mention: empty response workspace=%s bot=%q", workspaceID, bot.Name)
		return
	}
	reply := strings.TrimSpace(resp.Choices[0].Message.Content)
	if reply == "" {
		// Empty reply isn't an error (model decided to stay silent). Don't
		// post a phantom row.
		return
	}
	s.insertAndBroadcastBotReply(ctx, roomID, bot.BotUserID, reply)
}

// insertAndBroadcastBotReply persists a bot's text reply to the room
// and fires the WS event so all members see it in realtime. Mirrors
// what handleChatRoomMessageSend does for user-originated posts.
func (s *Server) insertAndBroadcastBotReply(ctx context.Context, roomID int64, botUserID, body string) {
	now := time.Now().UTC()
	msgID, err := s.insertRoomMessage(ctx, roomID, botUserID, "text", body, now)
	if err != nil {
		log.Printf("room mention: insert reply failed room=%d bot=%s: %v", roomID, botUserID, err)
		return
	}
	s.publishChatInternalEvent(chatInternalEvent{
		Event:     chatEventRoomMessageCreated,
		RoomID:    roomID,
		MessageID: msgID,
		SenderID:  botUserID,
	})
}

// buildRoomChatHistory turns the room's recent messages (newest-first
// from listRoomMessages) into the oldest-first OpenAI-compatible
// transcript the LLM expects. Bot's own past messages map to
// role=assistant; others to role=user with sender prefix so
// multi-speaker context is legible to the model.
func buildRoomChatHistory(msgs []ChatMessage, selfBotUserID, systemPrompt string) []aiChatCompletionMessage {
	out := make([]aiChatCompletionMessage, 0, len(msgs)+1)
	if strings.TrimSpace(systemPrompt) != "" {
		out = append(out, aiChatCompletionMessage{Role: "system", Content: systemPrompt})
	}
	// Reverse to oldest-first.
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Deleted || strings.TrimSpace(m.Content) == "" {
			continue
		}
		if m.SenderID == selfBotUserID {
			out = append(out, aiChatCompletionMessage{Role: "assistant", Content: m.Content})
		} else {
			label := strings.TrimSpace(m.SenderUsername)
			if label == "" {
				label = m.SenderID
			}
			out = append(out, aiChatCompletionMessage{Role: "user", Content: label + "：" + m.Content})
		}
	}
	return out
}
