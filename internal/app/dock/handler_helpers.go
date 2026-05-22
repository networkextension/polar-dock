package dock

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// itoa is a tiny helper to build $N parameter placeholders without
// pulling strconv. Relocated from video_store.go when video was
// extracted to polar-video. Still used by projects_store.go.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	buf := [12]byte{}
	pos := len(buf)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = digits[n%10]
		n /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// jsonFenceRe matches ```json fences. Relocated from expense_extract.go
// (deleted with the expense extraction); chat_rooms_idle_loop.go still
// uses it to strip code fences from LLM responses.
var jsonFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// generateVideoPoster shells out to ffmpeg to grab a 0.5s frame as a
// poster image. Relocated from video_processing.go (deleted with video
// extraction); post_handlers.go still uses it when a user attaches a
// video to a post.
func generateVideoPoster(ctx context.Context, videoPath, posterPath string) error {
	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-y",
		"-ss", "00:00:00.500",
		"-i", videoPath,
		"-frames:v", "1",
		"-q:v", "2",
		posterPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg poster generation failed: %w: %s", err, string(output))
	}
	return nil
}

// upsertLLMConfigKindAndExtras sets provider_kind + extras on an
// llm_configs row, workspace-scoped. Relocated from video_store.go;
// llm_bot_handlers.go still uses it when promoting a config to a
// specific kind (e.g. selecting it as the system video config).
func (s *Server) upsertLLMConfigKindAndExtras(workspaceID string, id int64, kind string, extras json.RawMessage, now time.Time) error {
	if extras == nil {
		extras = json.RawMessage(`{}`)
	}
	_, err := s.db.Exec(
		`UPDATE llm_configs
		    SET provider_kind = $3, extras = $4, updated_at = $5
		  WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID, kind, []byte(extras), now,
	)
	return err
}

// requireUserID is a tiny shared guard that pulls the session user id from
// the gin context the auth middleware sets. Returns ("", false) and writes
// a 500 if the context is malformed (would indicate a middleware misorder
// rather than a user error). Relocated from video_studio.go when video
// was extracted to polar-video.
func requireUserID(c *gin.Context) (string, bool) {
	v, _ := c.Get("user_id")
	id, ok := v.(string)
	if !ok || id == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return "", false
	}
	return id, true
}

// parseInt64Param parses a positive int64 path param or writes 400 + returns
// (0, false). Relocated from video_studio.go.
func parseInt64Param(c *gin.Context, name string) (int64, bool) {
	raw := c.Param(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return 0, false
	}
	return id, true
}

// envIntDefault reads an int env var with a fallback. Originally lived
// in wg_gc.go; relocated here when wg extracted to polar-wg.
// Callers: expense_ocr, expense_extract_multimodal, iosdist_status_sync,
// llm_billing_store, store.
func envIntDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func sanitizeFilename(input string) string {
	if input == "" {
		return "untitled"
	}
	var b strings.Builder
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	return out
}

func normalizeTagSlug(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	return strings.ToLower(sanitizeFilename(input))
}

func buildUploadFilename(original string) string {
	ext := strings.ToLower(filepath.Ext(original))
	if ext == "" || len(ext) > 8 {
		ext = ".img"
	} else {
		for _, r := range ext[1:] {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				ext = ".img"
				break
			}
		}
	}
	return fmt.Sprintf("%s_%s%s", time.Now().Format("20060102_150405"), generateSessionID()[:8], ext)
}

func buildDerivedUploadFilename(original, suffix, fallbackExt string) string {
	base := strings.TrimSuffix(filepath.Base(original), filepath.Ext(original))
	base = sanitizeFilename(base)
	if base == "" {
		base = "upload"
	}

	ext := strings.ToLower(strings.TrimSpace(fallbackExt))
	if ext == "" || !strings.HasPrefix(ext, ".") {
		ext = ".dat"
	}

	return fmt.Sprintf("%s_%s%s", base, sanitizeFilename(suffix), ext)
}
