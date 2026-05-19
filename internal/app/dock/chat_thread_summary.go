package dock

// System-agent thread summary (capability D from doc/system-agent-design.md).
// One row per chat_thread; the system agent emits a 1–2 sentence
// "上次停在哪" so a user re-opening an old conversation can pick up
// without scrolling. Stale-detection compares
// up_to_message_id with chat_threads.last_message_id.

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	// Thresholds at which a stale GET triggers an async refresh.
	// Either lots of new messages or a long elapsed gap qualifies.
	threadSummaryStaleMessageDelta = 20
	threadSummaryStaleAge          = 24 * time.Hour
	// Hard cap on how many messages we feed the summarization LLM.
	// Anything older falls off; 50 is a balance between context and
	// cost for a 1–2 sentence summary.
	threadSummaryContextSize = 50
)

type ChatThreadSummary struct {
	ID            int64      `json:"id"`
	ThreadID      int64      `json:"thread_id"`
	Summary       string     `json:"summary"`
	UpToMessageID *int64     `json:"up_to_message_id,omitempty"`
	MessageCount  int        `json:"message_count"`
	LLMConfigID   *int64     `json:"llm_config_id,omitempty"`
	LLMName       string     `json:"llm_name"`
	LLMModel      string     `json:"llm_model"`
	GeneratedAt   *time.Time `json:"generated_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (s *Server) getChatThreadSummary(threadID int64) (*ChatThreadSummary, error) {
	var summary ChatThreadSummary
	var upTo sql.NullInt64
	var llmConfigID sql.NullInt64
	var generatedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, thread_id, summary, up_to_message_id, message_count,
		        llm_config_id, llm_name, llm_model, generated_at, updated_at
		   FROM chat_thread_summaries
		  WHERE thread_id = $1`,
		threadID,
	).Scan(
		&summary.ID, &summary.ThreadID, &summary.Summary, &upTo, &summary.MessageCount,
		&llmConfigID, &summary.LLMName, &summary.LLMModel, &generatedAt, &summary.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if upTo.Valid {
		v := upTo.Int64
		summary.UpToMessageID = &v
	}
	if llmConfigID.Valid {
		v := llmConfigID.Int64
		summary.LLMConfigID = &v
	}
	if generatedAt.Valid {
		t := generatedAt.Time
		summary.GeneratedAt = &t
	}
	return &summary, nil
}

// upsertChatThreadSummary writes a (re-)generated summary. The
// `summary` text plus the LLM snapshot + the up_to_message_id at the
// time of generation are all stored together so future stale checks
// can compare against the current thread head.
func (s *Server) upsertChatThreadSummary(
	threadID int64,
	summary string,
	upToMessageID *int64,
	messageCount int,
	llmConfigID *int64,
	llmName, llmModel string,
	generatedAt time.Time,
) error {
	_, err := s.db.Exec(
		`INSERT INTO chat_thread_summaries
		   (thread_id, summary, up_to_message_id, message_count,
		    llm_config_id, llm_name, llm_model, generated_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
		 ON CONFLICT (thread_id) DO UPDATE SET
		   summary = EXCLUDED.summary,
		   up_to_message_id = EXCLUDED.up_to_message_id,
		   message_count = EXCLUDED.message_count,
		   llm_config_id = EXCLUDED.llm_config_id,
		   llm_name = EXCLUDED.llm_name,
		   llm_model = EXCLUDED.llm_model,
		   generated_at = EXCLUDED.generated_at,
		   updated_at = EXCLUDED.updated_at`,
		threadID, strings.TrimSpace(summary), upToMessageID, messageCount,
		llmConfigID, strings.TrimSpace(llmName), strings.TrimSpace(llmModel), generatedAt,
	)
	return err
}

// isThreadSummaryStale returns true when the summary we have is
// out-of-date enough to be worth regenerating. Either: there are
// more than N new messages since up_to_message_id, OR the summary
// is older than threadSummaryStaleAge. nil summary is always stale.
func (s *Server) isThreadSummaryStale(summary *ChatThreadSummary, latestMessageID int64, latestMessageAt *time.Time) bool {
	if summary == nil || strings.TrimSpace(summary.Summary) == "" {
		return true
	}
	if summary.UpToMessageID == nil {
		return true
	}
	if latestMessageID-*summary.UpToMessageID >= threadSummaryStaleMessageDelta {
		return true
	}
	if latestMessageAt != nil && summary.GeneratedAt != nil {
		if latestMessageAt.Sub(*summary.GeneratedAt) >= threadSummaryStaleAge {
			return true
		}
	}
	return false
}

// getChatThread loads a chat_threads row by id; returns (nil, nil)
// when not found. Lightweight read used by the summary stale check.
func (s *Server) getChatThread(threadID int64) (*ChatThread, error) {
	var thread ChatThread
	var lastMessageID sql.NullInt64
	var lastMessageAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, user_low, user_high, created_at, last_message, last_message_id, last_message_at
		   FROM chat_threads WHERE id = $1`,
		threadID,
	).Scan(
		&thread.ID, &thread.UserLow, &thread.UserHigh, &thread.CreatedAt,
		&thread.LastMessage, &lastMessageID, &lastMessageAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if lastMessageID.Valid {
		v := lastMessageID.Int64
		thread.LastMessageID = &v
	}
	if lastMessageAt.Valid {
		t := lastMessageAt.Time
		thread.LastMessageAt = &t
	}
	return &thread, nil
}

// threadSummaryPromptData feeds the system_agent.thread_summary
// template. PlatformName comes from site_settings; other fields are
// referenced from inside the prompt's example wording.
type threadSummaryPromptData struct {
	PlatformName string
}

// generateThreadSummary builds the LLM prompt from the latest N
// messages on a chat thread, calls the platform's AI_AGENT_* config,
// and writes the result to chat_thread_summaries. Caller is
// responsible for permission (only thread participants should
// trigger this); the service trusts what it's asked to summarize.
//
// Returns the saved row (or nil if the LLM configuration is missing
// — the design says fall back gracefully rather than 500'ing the
// caller's chat).
//
// LLM resolution: intentional — uses ONLY the platform-level
// AI_AGENT_* env config, NOT the workspace default LLM fallback that
// capabilities A/B/C use via resolveSystemAgentLLM. Reason: chat
// threads (chat_threads.user_low/user_high) are not workspace-scoped
// — they're 1:1 between two users — so deriving a "workspace default"
// is ambiguous (which participant's workspace?). Platform-only is
// the well-defined choice. Don't "fix" this by adding workspace
// fallback unless you also resolve the user-vs-thread workspace
// mapping question.
func (s *Server) generateThreadSummary(threadID int64) (*ChatThreadSummary, error) {
	if s.aiAgent == nil {
		return nil, errors.New("ai agent not initialized")
	}
	// Platform system LLM = the row marked is_system=TRUE. When unset,
	// design says skip rather than fail; caller surfaces an empty
	// summary with a "未配置" note.
	sysCfg, sysKey, err := s.getSystemLLMConfig()
	if err != nil {
		return nil, err
	}
	if sysCfg == nil || strings.TrimSpace(sysKey) == "" {
		return nil, nil
	}
	apiKey := strings.TrimSpace(sysKey)
	baseURL := strings.TrimSpace(sysCfg.BaseURL)
	model := strings.TrimSpace(sysCfg.Model)
	proxyURL := strings.TrimSpace(sysCfg.ProxyURL)
	if apiKey == "" || baseURL == "" || model == "" {
		return nil, nil
	}

	messages, _, err := s.listChatMessages(threadID, nil, threadSummaryContextSize, 0, nil)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		// Empty thread — write an empty summary row so future calls
		// don't keep retrying (cheaper than re-checking). up_to is
		// nil; messageCount=0; the GET handler can decide to hide
		// the bar entirely.
		now := time.Now()
		err := s.upsertChatThreadSummary(threadID, "", nil, 0, nil, "", "", now)
		if err != nil {
			return nil, err
		}
		return s.getChatThreadSummary(threadID)
	}

	// Build the user-side dump of recent messages. Format:
	//   [HH:MM] @username: <content one-liner>
	// One line per message. Truncate each content to ~200 chars so a
	// thread with a single huge paste doesn't blow our context.
	var sb strings.Builder
	sb.WriteString("以下是这个 chat thread 的最近消息（按时间正序）：\n\n")
	var latestID int64
	for _, m := range messages {
		if m.ID > latestID {
			latestID = m.ID
		}
		ts := m.CreatedAt.Format("01/02 15:04")
		who := m.SenderUsername
		if who == "" {
			who = m.SenderID
		}
		content := strings.TrimSpace(m.Content)
		content = strings.ReplaceAll(content, "\n", " ")
		if len(content) > 200 {
			content = content[:200] + "...[truncated]"
		}
		sb.WriteString("[")
		sb.WriteString(ts)
		sb.WriteString("] @")
		sb.WriteString(who)
		sb.WriteString(": ")
		sb.WriteString(content)
		sb.WriteString("\n")
	}

	siteName := "Polar"
	if site, err := s.getSiteSettings(); err == nil && site != nil && strings.TrimSpace(site.Name) != "" {
		siteName = site.Name
	}
	systemPrompt := s.renderPrompt(promptSlugThreadSummary, threadSummaryPromptData{PlatformName: siteName}, "")

	runtimeConfig := aiRuntimeConfig{
		APIKey:        apiKey,
		BaseURL:       baseURL,
		Model:         model,
		ProxyURL:      proxyURL,
		SystemPrompt:  systemPrompt,
		Endpoint:      "thread_summary",
		LLMConfigID:   sysCfg.ID,
		LLMConfigName: sysCfg.Name,
	}
	payload := aiChatCompletionRequest{
		Model: model,
		Messages: []aiChatCompletionMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: sb.String()},
		},
		MaxTokens: 200,
	}
	resp, err := s.aiAgent.requestChatCompletion(runtimeConfig, payload)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("empty llm response")
	}
	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		return nil, errors.New("empty llm summary")
	}

	now := time.Now()
	cfgIDPtr := &sysCfg.ID
	if err := s.upsertChatThreadSummary(
		threadID, summary, &latestID, len(messages),
		cfgIDPtr,
		sysCfg.Name, model,
		now,
	); err != nil {
		return nil, err
	}
	return s.getChatThreadSummary(threadID)
}
