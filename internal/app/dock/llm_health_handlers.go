package dock

// GET /api/llm-configs/health — returns the prober's last-known
// upstream state per llm_config. Powers the Status column on
// /llm.html. Any logged-in user can read this: upstream health is
// platform-level state and there's nothing workspace-sensitive here.
//
// Probe cadence is 30s (llmProbeInterval); the UI should poll at a
// matching cadence (or longer). Snapshot is empty before the first
// probe pass completes (~startup + 0s, so within a couple seconds of
// dock boot it has rows for every probed config).

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type llmHealthDTO struct {
	ConfigID  int64  `json:"config_id"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	Up        bool   `json:"up"`
	Reason    string `json:"reason,omitempty"`
	CheckedAt string `json:"checked_at"`
}

func (s *Server) handleLLMConfigsHealth(c *gin.Context) {
	out := make([]llmHealthDTO, 0)
	if s.llmProxyProber != nil {
		for _, r := range s.llmProxyProber.Snapshot() {
			out = append(out, llmHealthDTO{
				ConfigID:  r.ConfigID,
				Name:      r.Name,
				Model:     r.Model,
				Up:        r.Up,
				Reason:    r.Reason,
				CheckedAt: r.CheckedAt.UTC().Format("2006-01-02T15:04:05Z"),
			})
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"items":          out,
		"probe_interval_seconds": int(llmProbeInterval.Seconds()),
	})
}
