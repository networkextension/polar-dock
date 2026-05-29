package dock

// plugin_modules_store.go — Phase 1-A: plugin registry + HMAC token
// hashing for the internal API surface. Mirrors the proxy-token
// pattern (`llm_proxy_store.go::newLLMProxyTokenPlaintext`): plaintext
// shown once at creation, only sha256 hex stored in DB. The HMAC
// middleware (`internal_api_middleware.go`) looks plugins up via
// getPluginModuleByName + recomputes the request signature using the
// stored hash's matching plaintext (which the plugin holds).
//
// See doc/arch/internal-api-v1.md for the wire protocol.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

const (
	// pluginTokenPlaintextPrefix — pinned across all plugin tokens for
	// easy ops grep + same shape as polar_proxy_ tokens.
	pluginTokenPlaintextPrefix = "polar_plugin_"
	// pluginTokenRandBytes — 16 random bytes = 32 hex chars. Total
	// plaintext length: len(prefix) + 32 = 45.
	pluginTokenRandBytes = 16
)

// PluginModule is one row in plugin_modules. Fields mirror the schema
// 1:1 plus DerivedPlaintext, which is only set on creation/rotation
// (transport-only, never persisted server-side).
type PluginModule struct {
	Name             string     `json:"name"`
	DisplayName      string     `json:"display_name"`
	Enabled          bool       `json:"enabled"`
	Endpoint         string     `json:"endpoint,omitempty"`
	LastHeartbeatAt  *time.Time `json:"last_heartbeat,omitempty"`
	Version          string     `json:"version,omitempty"`
	Config           []byte     `json:"-"`               // raw jsonb bytes; handlers decide rendering
	CreatedAt        time.Time  `json:"created_at"`
	DerivedPlaintext string     `json:"plaintext,omitempty"` // set only on create / rotate
}

// CreatePluginModuleInput is the admin-handler payload, validated +
// turned into a row by insertPluginModule.
type CreatePluginModuleInput struct {
	Name        string
	DisplayName string
	Endpoint    string
	Config      []byte // optional jsonb (default '{}')
}

// newPluginPlaintext returns a fresh (plaintext, sha256-hex) pair.
// Used by insertPluginModule + rotatePluginModuleKey. Plaintext goes
// to the admin once; only the hash is stored.
func newPluginPlaintext() (plaintext, hashHex string) {
	b := make([]byte, pluginTokenRandBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal-ish — panic to surface; callers
		// only invoke this from admin-only handlers so a 500 is fine.
		panic("plugin module: rand.Read failed: " + err.Error())
	}
	plaintext = pluginTokenPlaintextPrefix + hex.EncodeToString(b)
	return plaintext, hashPluginToken(plaintext)
}

// hashPluginToken is the canonical hash for both inserts and lookups.
// SHA-256 hex, lowercase. DB comparison is exact equality, so no
// constant-time wrapper needed at the store layer (the HMAC
// verification in the middleware does use crypto/hmac.Equal).
func hashPluginToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// listPluginModules returns all rows, newest-created first. Used by
// the admin UI list page.
func (s *Server) listPluginModules() ([]PluginModule, error) {
	rows, err := s.db.Query(`
		SELECT name, display_name, enabled, endpoint, last_heartbeat,
		       version, config, created_at
		  FROM plugin_modules
		 ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PluginModule{}
	for rows.Next() {
		var m PluginModule
		var lastHB sql.NullTime
		if err := rows.Scan(&m.Name, &m.DisplayName, &m.Enabled, &m.Endpoint,
			&lastHB, &m.Version, &m.Config, &m.CreatedAt); err != nil {
			return nil, err
		}
		if lastHB.Valid {
			t := lastHB.Time
			m.LastHeartbeatAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// getPluginModuleByName returns the row needed to verify an HMAC-signed
// request. nil if no row. Middleware calls this on every gated request,
// so it should stay fast (PK lookup, no joins).
func (s *Server) getPluginModuleByName(name string) (*PluginModule, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", nil
	}
	var m PluginModule
	var lastHB sql.NullTime
	var keyHash string
	err := s.db.QueryRow(`
		SELECT name, display_name, enabled, endpoint, last_heartbeat,
		       version, config, created_at, plugin_key_hash
		  FROM plugin_modules WHERE name = $1`, name).
		Scan(&m.Name, &m.DisplayName, &m.Enabled, &m.Endpoint,
			&lastHB, &m.Version, &m.Config, &m.CreatedAt, &keyHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	if lastHB.Valid {
		t := lastHB.Time
		m.LastHeartbeatAt = &t
	}
	return &m, keyHash, nil
}

// insertPluginModule generates a fresh secret + inserts the row.
// Returns the row with DerivedPlaintext populated — the admin UI shows
// it once and the plugin operator copies it.
func (s *Server) insertPluginModule(in CreatePluginModuleInput) (*PluginModule, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	in.Endpoint = strings.TrimSpace(in.Endpoint)
	if in.Name == "" {
		return nil, errors.New("name required")
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Name
	}
	if len(in.Config) == 0 {
		in.Config = []byte(`{}`)
	}
	plaintext, hash := newPluginPlaintext()
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		INSERT INTO plugin_modules (name, display_name, enabled, endpoint,
		    version, plugin_key_hash, config, created_at)
		VALUES ($1, $2, TRUE, $3, '', $4, $5, $6)`,
		in.Name, in.DisplayName, in.Endpoint, hash, in.Config, now)
	if err != nil {
		return nil, err
	}
	return &PluginModule{
		Name:             in.Name,
		DisplayName:      in.DisplayName,
		Enabled:          true,
		Endpoint:         in.Endpoint,
		Config:           in.Config,
		CreatedAt:        now,
		DerivedPlaintext: plaintext,
	}, nil
}

// setPluginModuleEnabled flips the enabled bit. Returns true if a row
// was updated, false if the plugin doesn't exist.
func (s *Server) setPluginModuleEnabled(name string, enabled bool) (bool, error) {
	res, err := s.db.Exec(`UPDATE plugin_modules SET enabled = $2 WHERE name = $1`, name, enabled)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// rotatePluginModuleKey mints a fresh secret + replaces the stored
// hash. Returns the new plaintext (only chance to capture it).
func (s *Server) rotatePluginModuleKey(name string) (string, bool, error) {
	plaintext, hash := newPluginPlaintext()
	res, err := s.db.Exec(`UPDATE plugin_modules SET plugin_key_hash = $2 WHERE name = $1`,
		name, hash)
	if err != nil {
		return "", false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", false, nil
	}
	return plaintext, true, nil
}

// updatePluginHeartbeat refreshes last_heartbeat (and optionally
// version + endpoint) on the row. Called by Phase 1-B's
// /internal/v1/plugin-registry/heartbeat handler. Empty version /
// endpoint don't clobber existing values — only refresh what the
// plugin sent.
func (s *Server) updatePluginHeartbeat(name, version, endpoint string, uiRoutes, apiRoutes []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name required")
	}
	now := time.Now().UTC()
	// Preserve current values when the plugin sent empty strings.
	// uiRoutes / apiRoutes: nil / empty = preserve current value (don't
	// overwrite to []), so a plugin that doesn't yet declare routes
	// leaves the existing column alone. Plugins that DO declare routes
	// authoritatively replace the column on every heartbeat.
	// lib/pq sends []byte as bytea, which can't cast to jsonb. Pass
	// as string so it lands on the text wire format the ::jsonb cast
	// can decode. Matches the pattern in host_skill_runs_store.go.
	var uiR, apiR interface{}
	if len(uiRoutes) > 0 {
		uiR = string(uiRoutes)
	}
	if len(apiRoutes) > 0 {
		apiR = string(apiRoutes)
	}
	_, err := s.db.Exec(`
		UPDATE plugin_modules
		   SET last_heartbeat = $2,
		       version  = CASE WHEN $3 = '' THEN version  ELSE $3 END,
		       endpoint = CASE WHEN $4 = '' THEN endpoint ELSE $4 END,
		       ui_routes  = COALESCE($5::jsonb, ui_routes),
		       api_routes = COALESCE($6::jsonb, api_routes)
		 WHERE name = $1`,
		name, now, strings.TrimSpace(version), strings.TrimSpace(endpoint), uiR, apiR)
	return err
}

// deletePluginModule hard-deletes the row. Prefer disable; delete is
// here for cleanup of typos and decommissioned plugins.
func (s *Server) deletePluginModule(name string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM plugin_modules WHERE name = $1`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
