package dock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
