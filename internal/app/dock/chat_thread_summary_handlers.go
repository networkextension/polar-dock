package dock

// HTTP handlers for chat_thread_summaries (system_agent capability D).
//
// GET  /api/chats/:thread_id/summary           — fetch (lazy refresh
//                                                 if stale; returns
//                                                 the existing row
//                                                 immediately so the
//                                                 UI doesn't block)
// POST /api/chats/:thread_id/summary/refresh   — synchronous force
//                                                 refresh; blocks on
//                                                 the LLM call

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleChatThreadSummary(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	threadID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || threadID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的会话"})
		return
	}
	participant, err := s.isChatParticipant(threadID, userIDStr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if !participant {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权访问该会话"})
		return
	}

	summary, err := s.getChatThreadSummary(threadID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	// Stale-check needs the thread's latest message id + timestamp.
	// Cheap query (chat_threads has both columns).
	thread, err := s.getChatThread(threadID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	stale := false
	if thread != nil && thread.LastMessageID != nil {
		stale = s.isThreadSummaryStale(summary, *thread.LastMessageID, thread.LastMessageAt)
	}

	// Stale → kick off a refresh in the background. Don't block the
	// GET — UI gets the (possibly outdated) summary right now and
	// will pick up the new one on next page open. This is the
	// "good enough" behavior; clients that need fresh-now use the
	// /refresh POST.
	if stale {
		go func(tid int64) {
			if _, err := s.generateThreadSummary(tid); err != nil {
				log.Printf("thread summary refresh background: thread=%d err=%v", tid, err)
			}
		}(threadID)
	}

	c.JSON(http.StatusOK, gin.H{
		"summary": summary,
		"stale":   stale,
	})
}

func (s *Server) handleChatThreadSummaryRefresh(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	threadID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || threadID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的会话"})
		return
	}
	participant, err := s.isChatParticipant(threadID, userIDStr)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if !participant {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权访问该会话"})
		return
	}

	summary, err := s.generateThreadSummary(threadID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if summary == nil {
		// Platform AI_AGENT_* not configured. Return 503 with a
		// clear message so the UI can show "AI 助理未配置".
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "AI 助理未配置 — 联系管理员设置 AI_AGENT_API_KEY / AI_AGENT_BASE_URL / AI_AGENT_MODEL",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": summary, "stale": false})
}
