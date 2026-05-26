package dock

// internal_billing_handlers.go — plugin-facing /internal/v1/billing/*
// endpoints. Plugins POST per-shot (or per-call) billing rows here so
// the dock-side workspace ledger is the single source of truth for
// recharge / month-spend queries.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// internalVideoShotCallReq mirrors the shape of polar-sdk's
// VideoShotCallRecord struct. All pointer / omitempty fields are
// optional so a price-table miss still produces a valid row.
type internalVideoShotCallReq struct {
	WorkspaceID        string         `json:"workspace_id" binding:"required"`
	ProjectID          int64          `json:"project_id" binding:"required"`
	ShotID             int64          `json:"shot_id" binding:"required"`
	Provider           string         `json:"provider" binding:"required"`
	Model              string         `json:"model,omitempty"`
	Resolution         string         `json:"resolution,omitempty"`
	DurationChargedSec float64        `json:"duration_charged_sec,omitempty"`
	FPS                int            `json:"fps,omitempty"`
	FramesTotal        int            `json:"frames_total,omitempty"`
	CostUSD            float64        `json:"cost_usd,omitempty"`
	CostPerFrameUSD    float64        `json:"cost_per_frame_usd,omitempty"`
	BillingMeta        map[string]any `json:"billing_meta,omitempty"`
}

// handleInternalVideoShotCall ingests one shot's billing payload from
// polar-video. Idempotent: re-posting the same shot_id returns 200
// with `deduped: true` rather than 409 so the plugin's poll-loop
// retry path is a no-op rather than an error to log.
func (s *Server) handleInternalVideoShotCall(c *gin.Context) {
	var req internalVideoShotCallReq
	if err := c.ShouldBindJSON(&req); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	call := &VideoShotCall{
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
		ShotID:      req.ShotID,
		Provider:    req.Provider,
		Model:       req.Model,
		Resolution:  req.Resolution,
		CreatedAt:   time.Now().UTC(),
	}
	if req.DurationChargedSec > 0 {
		v := req.DurationChargedSec
		call.DurationChargedSec = &v
	}
	if req.FPS > 0 {
		v := req.FPS
		call.FPS = &v
	}
	if req.FramesTotal > 0 {
		v := req.FramesTotal
		call.FramesTotal = &v
	}
	// CostUSD and CostPerFrameUSD: 0 is a legitimate value (the
	// price table missed but the row is still recorded for audit),
	// so always pass a pointer — the test of "is zero" happens at
	// the query layer, not here.
	cost := req.CostUSD
	call.CostUSD = &cost
	perFrame := req.CostPerFrameUSD
	call.CostPerFrameUSD = &perFrame

	if len(req.BillingMeta) > 0 {
		if raw, err := json.Marshal(req.BillingMeta); err == nil {
			call.BillingMeta = raw
		}
	}
	inserted, err := s.insertVideoShotCall(call)
	if err != nil {
		internalErr(c, http.StatusInternalServerError, "ledger write failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"recorded": true,
		"deduped":  !inserted,
		"id":       call.ID,
	})
}
