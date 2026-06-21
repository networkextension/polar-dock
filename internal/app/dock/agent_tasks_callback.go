package dock

// agent_tasks_callback.go — P3: when a module-originated task finishes, dock
// pushes the result back to the requesting plugin via a signed POST (the module
// verifies with its own plugin key — same HMAC scheme as inbound /internal/v1,
// reversed). Best-effort: a goroutine with a bounded retry; outcome recorded in
// agent_tasks.callback_status. See polar-task doc/07 P3.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type taskCallbackArtifact struct {
	Kind        string `json:"kind"`
	Mime        string `json:"mime"`
	AssetID     int64  `json:"asset_id"`
	Bytes       int64  `json:"bytes"`
	DownloadURL string `json:"download_url,omitempty"`
}

type taskCallbackBody struct {
	TaskID       string                 `json:"task_id"`
	RequesterRef string                 `json:"requester_ref,omitempty"`
	Skill        string                 `json:"skill"`
	Status       string                 `json:"status"`
	Result       json.RawMessage        `json:"result,omitempty"`
	Error        string                 `json:"error,omitempty"`
	Artifacts    []taskCallbackArtifact `json:"artifacts"`
}

// fireTaskCallback delivers a completed task's result to its requester module.
// Safe to run in a goroutine; never panics out.
func (s *Server) fireTaskCallback(t *AgentTask, arts []TaskArtifact) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[task callback] panic: %v", r)
		}
	}()
	cbPath := strings.TrimSpace(t.CallbackPath)
	if t.RequesterModule == "" || cbPath == "" {
		return
	}
	if !strings.HasPrefix(cbPath, "/") {
		cbPath = "/" + cbPath
	}
	mod, keyHash, err := s.getPluginModuleByName(t.RequesterModule)
	if err != nil || mod == nil || strings.TrimSpace(mod.Endpoint) == "" {
		s.setTaskCallbackStatus(t.ID, "skipped")
		return
	}
	cbArts := make([]taskCallbackArtifact, 0, len(arts))
	for _, a := range arts {
		cbArts = append(cbArts, taskCallbackArtifact{
			Kind: a.Kind, Mime: a.Mime, AssetID: a.AssetID, Bytes: a.Bytes,
			DownloadURL: s.assetDownloadURL(a.AssetID, t.WorkspaceID),
		})
	}
	body, err := json.Marshal(taskCallbackBody{
		TaskID: t.ID, RequesterRef: t.RequesterRef, Skill: t.Skill, Status: t.Status,
		Result: t.Result, Error: t.Error, Artifacts: cbArts,
	})
	if err != nil {
		s.setTaskCallbackStatus(t.ID, "failed:marshal")
		return
	}
	url := strings.TrimRight(mod.Endpoint, "/") + cbPath

	var lastStatus int
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		sig := computePluginSig(keyHash, http.MethodPost, cbPath, ts, body)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if rerr != nil {
			cancel()
			s.setTaskCallbackStatus(t.ID, "failed:build")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Polar-Plugin-Name", t.RequesterModule)
		req.Header.Set("X-Polar-Plugin-Timestamp", ts)
		req.Header.Set("X-Polar-Plugin-Sig", fmt.Sprintf("%x", sig))
		resp, derr := http.DefaultClient.Do(req)
		cancel()
		if derr != nil {
			lastStatus = 0
			continue
		}
		lastStatus = resp.StatusCode
		resp.Body.Close()
		if lastStatus >= 200 && lastStatus < 300 {
			s.setTaskCallbackStatus(t.ID, "delivered")
			return
		}
	}
	s.setTaskCallbackStatus(t.ID, "failed:"+strconv.Itoa(lastStatus))
	log.Printf("[task callback] %s → %s undelivered (last status %d)", t.ID, url, lastStatus)
}
