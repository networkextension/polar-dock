package dock

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GET /api/system/info — PUBLIC (no auth). Deliberate: deploy tooling
// hits this on a fresh box to learn the box's identity, free space,
// and reachability before pushing.
//
// Cache: 5s server-side (see sysinfo.go) so each scrape doesn't fork
// uname + df + sysctl.
func (s *Server) handleSystemInfo(c *gin.Context) {
	si := gatherSystemInfoCached()
	c.JSON(http.StatusOK, si)
}
