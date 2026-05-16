package dock

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Agent-auth surface for the knowledge-base.
//
// Distinct from rev_handlers.go which is user-session + admin-gated.
// These routes accept a Polar Agent bearer token (same auth shape as
// /api/agent/auto-register), so a polar-agent process can write
// observations without an operator clicking through the UI.
//
// Currently:
//   POST /api/agent/library/devices/upsert — recovery watcher posts
//     freshly-detected iOS-in-recovery devices here. Body is the same
//     RevDevice JSON shape as the user-side upsert; agent_token_id
//     is captured into metadata_json.added_by_agent so the audit
//     trail shows which host saw the device first.

func (s *Server) handleAgentRevDeviceUpsert(c *gin.Context) {
	var d RevDevice
	if err := c.ShouldBindJSON(&d); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	if d.CPID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cpid required"})
		return
	}
	out, err := s.upsertRevDevice(&d, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"device": out})
}
