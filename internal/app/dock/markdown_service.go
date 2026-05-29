package dock

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// saveMarkdownDocument persists the markdown body to disk and writes
// the metadata row. visibility is the three-tier value
// (private/workspace/public). workspaceID is required only for
// workspace tier — caller is responsible for verifying that the
// workspace_id is one the user is a member of.
func (s *Server) saveMarkdownDocument(userID, title, content, editorMode, visibility string, workspaceID *string, now time.Time) (*MarkdownEntry, string, error) {
	if err := os.MkdirAll(s.markdownDir, 0o755); err != nil {
		return nil, "", err
	}

	safeTitle := sanitizeFilename(title)
	timestamp := now.Format("20060102_150405")
	filename := safeTitle + "_" + timestamp + "_" + sanitizeFilename(userID) + ".md"
	path := filepath.Join(s.markdownDir, filename)

	if !strings.HasPrefix(strings.TrimSpace(content), "#") {
		content = "# " + title + "\n\n" + content
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, "", err
	}

	mode := normalizeEditorMode(editorMode)
	summary, coverURL := extractMarkdownMeta(content)
	if visibility == "" {
		visibility = MarkdownVisibilityPrivate
	}
	// R1 (Plan B): every markdown entry is team-owned — workspace_id is
	// never NULL. `visibility` governs who can READ (private = author
	// only, workspace = team members, public = anyone); `workspace_id`
	// governs OWNERSHIP. Private/public entries are owned by the
	// author's own team. For the workspace tier the caller already
	// validated membership and passed the workspace_id through; for
	// every other tier (and any caller that passed nil) fall back to
	// the author's personal team so the row satisfies the NOT NULL
	// invariant enforced by enforceWorkspaceIDNotNull().
	if visibility != MarkdownVisibilityWorkspace || workspaceID == nil || strings.TrimSpace(*workspaceID) == "" {
		pw, perr := s.ensurePersonalTeam(userID)
		if perr != nil {
			_ = os.Remove(path)
			return nil, "", fmt.Errorf("resolve owner workspace: %w", perr)
		}
		wid := pw.ID
		workspaceID = &wid
	}
	entryID, err := s.createMarkdownEntryReturningID(userID, title, path, summary, coverURL, mode, visibility, workspaceID, now)
	if err != nil {
		_ = os.Remove(path)
		return nil, "", err
	}

	return &MarkdownEntry{
		ID:          entryID,
		UserID:      userID,
		Title:       title,
		Summary:     summary,
		CoverURL:    coverURL,
		FilePath:    path,
		IsPublic:    visibility == MarkdownVisibilityPublic,
		Visibility:  visibility,
		WorkspaceID: workspaceID,
		EditorMode:  mode,
		UploadedAt:  now,
	}, content, nil
}

// normalizeEditorMode validates the UI hint that distinguishes the
// markdown source editor from the rich WYSIWYG editor. Anything unknown
// collapses back to "markdown" so storage stays uniform regardless of
// what clients ship.
func normalizeEditorMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "rich":
		return "rich"
	default:
		return "markdown"
	}
}

var (
	mdImageRegex  = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)`)
	mdHTMLImgRegex = regexp.MustCompile(`(?i)<img[^>]+src=["']([^"']+)["']`)
)

// extractMarkdownMeta derives a plain-text summary and the first image URL
// from markdown content. Safe to call on any content; returns empty strings
// if nothing matches.
func extractMarkdownMeta(content string) (summary, coverURL string) {
	if match := mdImageRegex.FindStringSubmatch(content); len(match) > 1 {
		coverURL = strings.TrimSpace(match[1])
	} else if match := mdHTMLImgRegex.FindStringSubmatch(content); len(match) > 1 {
		coverURL = strings.TrimSpace(match[1])
	}
	summary = buildMarkdownPreview(stripFirstHeading(content), 200)
	if summary == "AI 文档回复" {
		summary = ""
	}
	return summary, coverURL
}

func stripFirstHeading(content string) string {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "#") {
		return trimmed
	}
	if idx := strings.Index(trimmed, "\n"); idx >= 0 {
		return strings.TrimSpace(trimmed[idx+1:])
	}
	return ""
}

func buildSystemMarkdownTitle(content string, now time.Time) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#")
		line = strings.TrimSpace(line)
		if line != "" {
			runes := []rune(line)
			if len(runes) > 60 {
				line = string(runes[:60])
			}
			return line
		}
	}
	return fmt.Sprintf("system-reply-%s", now.Format("20060102-150405"))
}

func buildMarkdownPreview(content string, maxLength int) string {
	if maxLength <= 0 {
		maxLength = 80
	}
	replacer := strings.NewReplacer(
		"\r", " ",
		"\n", " ",
		"`", "",
		"#", "",
		"*", "",
		">", "",
		"[", "",
		"]", "",
		"(", "",
		")", "",
	)
	text := strings.TrimSpace(replacer.Replace(content))
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "AI 文档回复"
	}
	runes := []rune(text)
	if len(runes) > maxLength {
		return string(runes[:maxLength]) + "..."
	}
	return text
}
