package dock

// Site-wide editable prompt store. Replaces hardcoded prompt
// constants scattered across handlers (decompose, pickup body,
// markdown/post assist, AI defaults, tool-use addendum) with a
// DB-backed registry. Admins edit via /api/prompts; runtime
// loaders consult the cache; renders use Go text/template so
// authors can express conditional sections and variable substitution.
//
// Cache: in-memory map keyed by slug, populated on bootstrap +
// refreshed on PUT. Read path is a single mutex-guarded map lookup,
// no DB round-trip per chat request.
//
// Render fallback: if a template is missing or fails to parse/exec,
// the loader returns the code-baked default (rendered) so a
// half-edited prompt doesn't break user-facing AI calls.

import (
	"bytes"
	"database/sql"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
)

type PromptTemplate struct {
	Slug           string    `json:"slug"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Content        string    `json:"content"`
	DefaultContent string    `json:"default_content"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// promptCache is a server-scoped in-memory map. The Server holds a
// pointer to one of these; loaders read it under RLock, the upsert
// handler refreshes it after every write.
type promptCache struct {
	mu sync.RWMutex
	m  map[string]string // slug → content (raw, unparsed)
}

func newPromptCache() *promptCache {
	return &promptCache{m: map[string]string{}}
}

func (c *promptCache) get(slug string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[slug]
	return v, ok
}

func (c *promptCache) set(slug, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[slug] = content
}

func (c *promptCache) replaceAll(items map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = items
}

// promptDefault is one entry in the canonical default registry.
// Code lists every slug it knows about here; bootstrapPromptTemplates
// runs on server start and inserts missing rows / updates the
// default_content column without overwriting user edits.
type promptDefault struct {
	Slug        string
	Name        string
	Description string
	Content     string
}

// bootstrapPromptTemplates runs on server start. For each known
// default: insert a row if missing (using default content as the
// initial editable content); for existing rows, refresh
// default_content + name + description but leave the user's edited
// content alone.
func (s *Server) bootstrapPromptTemplates(defaults []promptDefault) error {
	for _, d := range defaults {
		_, err := s.db.Exec(
			`INSERT INTO prompt_templates (slug, name, description, content, default_content, updated_at)
			 VALUES ($1, $2, $3, $4, $4, now())
			 ON CONFLICT (slug) DO UPDATE SET
			   name = EXCLUDED.name,
			   description = EXCLUDED.description,
			   default_content = EXCLUDED.default_content`,
			d.Slug, d.Name, d.Description, d.Content,
		)
		if err != nil {
			return err
		}
	}
	return s.refreshPromptCache()
}

func (s *Server) refreshPromptCache() error {
	if s.promptCache == nil {
		s.promptCache = newPromptCache()
	}
	rows, err := s.db.Query(`SELECT slug, content FROM prompt_templates`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var slug, content string
		if err := rows.Scan(&slug, &content); err != nil {
			return err
		}
		m[slug] = content
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.promptCache.replaceAll(m)
	return nil
}

func (s *Server) listPromptTemplates() ([]PromptTemplate, error) {
	rows, err := s.db.Query(
		`SELECT slug, name, description, content, default_content, updated_at
		 FROM prompt_templates ORDER BY slug`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PromptTemplate{}
	for rows.Next() {
		var p PromptTemplate
		if err := rows.Scan(&p.Slug, &p.Name, &p.Description, &p.Content, &p.DefaultContent, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *Server) getPromptTemplate(slug string) (*PromptTemplate, error) {
	row := s.db.QueryRow(
		`SELECT slug, name, description, content, default_content, updated_at
		 FROM prompt_templates WHERE slug = $1`,
		strings.TrimSpace(slug),
	)
	var p PromptTemplate
	if err := row.Scan(&p.Slug, &p.Name, &p.Description, &p.Content, &p.DefaultContent, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (s *Server) updatePromptTemplateContent(slug, content string) error {
	res, err := s.db.Exec(
		`UPDATE prompt_templates SET content = $2, updated_at = now() WHERE slug = $1`,
		strings.TrimSpace(slug), content,
	)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return errors.New("prompt slug not found")
	}
	return s.refreshPromptCache()
}

// renderPrompt loads the template at slug from cache, parses it as
// a Go text/template, and executes it against data. fallback is the
// code-baked default — used if the slug is unknown OR if the
// user's edited template fails to parse/execute. Errors are logged
// (so an admin notices) but never propagated to the caller; the
// caller always gets a string suitable for sending to the LLM.
func (s *Server) renderPrompt(slug string, data any, fallback string) string {
	raw := fallback
	if s.promptCache != nil {
		if cached, ok := s.promptCache.get(slug); ok && strings.TrimSpace(cached) != "" {
			raw = cached
		}
	}
	rendered, err := renderTemplateString(slug, raw, data)
	if err == nil {
		return rendered
	}
	log.Printf("[prompt] render %s failed (%v); falling back to default", slug, err)
	if fb, fbErr := renderTemplateString(slug+"(fallback)", fallback, data); fbErr == nil {
		return fb
	}
	// Both user-edited AND code-default failed to parse — return
	// the raw fallback so we surface SOMETHING to the LLM rather
	// than an empty system prompt.
	return fallback
}

func renderTemplateString(name, content string, data any) (string, error) {
	tpl, err := template.New(name).Option("missingkey=zero").Parse(content)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
