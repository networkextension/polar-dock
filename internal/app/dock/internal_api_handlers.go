package dock

// internal_api_handlers.go — Phase 1-A: just the smoke ping route. The
// nine v1 business endpoints (auth/verify, users, teams, llm-configs,
// notify, plugin-registry/heartbeat, llm/chat-completion) land in 1-B.
//
// /internal/v1/ping is intentionally outside the documented v1
// contract: it exists so an operator can curl + HMAC sign a request
// after rolling out 1-A to validate the middleware end-to-end. Doc
// note in doc/arch/internal-api-v1.md will mark it "operator smoke
// only, not a contractual endpoint".

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleInternalPing(c *gin.Context) {
	plugin, _ := c.Get("plugin_name")
	pluginStr, _ := plugin.(string)
	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"plugin":      pluginStr,
		"server_time": time.Now().UTC().Format(time.RFC3339),
	})
}
