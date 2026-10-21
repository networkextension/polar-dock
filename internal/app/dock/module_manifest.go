package dock

// module_manifest.go — a plugin module declares its own permission surface
// to dock: role vocabulary (+ invite-code prefixes), API-token scopes,
// scope presets and its route → scope table.
//
// Why: before this, a module's roles were hard-coded in dock
// (moduleRolePolicies) and dock's token issuer only knew dock's own /api
// scopes, so a `polar_` token could not be scoped to, say, opera's
// "read sessions" — knownScope rejected `opera:…`. Now the module PUTs a
// manifest at startup; dock validates it, stores it (module_manifests),
// keeps an in-process registry for the hot paths (knownScope,
// validateModuleRole, the token-UI scope catalog) and mirrors it to redis
// under {prefix}:manifest:{module}.
//
// dock does NOT enforce module scopes — verify hands the module its slice
// of a token's scopes (filterScopesForModule) and the module enforces
// against its own route table. The routes are stored for documentation /
// UI and for cross-checking that every route points at a declared scope.
//
// Contract: doc/arch/module-manifest.md (shared with polar-sdk, the token UI
// and polar-WindTunnel — keep them in sync).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// ModuleManifest is the body of PUT /internal/v1/modules/:module/manifest.
type ModuleManifest struct {
	Module          string                 `json:"module"`
	ManifestVersion string                 `json:"manifest_version"`
	Roles           []ModuleManifestRole   `json:"roles"`
	Scopes          []ModuleManifestScope  `json:"scopes"`
	Presets         []ModuleManifestPreset `json:"presets"`
	Routes          []ModuleManifestRoute  `json:"routes"`
}

type ModuleManifestRole struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	InvitePrefix string `json:"invite_prefix,omitempty"`
}

type ModuleManifestScope struct {
	ID        string `json:"id"`
	Group     string `json:"group"`
	Name      string `json:"name"`
	Sensitive bool   `json:"sensitive"`
}

type ModuleManifestPreset struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

type ModuleManifestRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Scope  string `json:"scope"`
}

const (
	manifestMaxBodyBytes = 256 << 10 // 256 KiB
	manifestMaxScopes    = 200
	manifestMaxPresets   = 50
	manifestMaxRoutes    = 1000
	manifestMaxRoles     = 50
	manifestMaxText      = 200 // names / groups / version
	manifestMaxPath      = 512
)

var (
	manifestScopePartPattern   = regexp.MustCompile(`^[a-z0-9_]+$`)
	manifestPresetIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	manifestInvitePrefixPattrn = regexp.MustCompile(`^[A-Z][A-Z0-9_-]{0,31}$`)
	manifestMethods            = map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}
)

// errManifestModuleMismatch maps to 403; every other validation error to 400.
var errManifestModuleMismatch = errors.New("manifest module does not match calling plugin")

// validateManifest checks a manifest submitted by plugin `caller` and
// normalises it in place (trimmed strings, upper-case methods, lower-case
// module). Rules: doc/arch/module-manifest.md §Validation.
func validateManifest(caller string, m *ModuleManifest) error {
	if m == nil {
		return errors.New("empty manifest")
	}
	caller = normalizeModuleName(caller)
	m.Module = normalizeModuleName(m.Module)
	if caller == "" || m.Module != caller {
		return errManifestModuleMismatch
	}
	if m.Module == "admin" {
		return errors.New(`module name "admin" is reserved`)
	}
	if !moduleNamePattern.MatchString(m.Module) {
		return errors.New("invalid module name")
	}
	m.ManifestVersion = strings.TrimSpace(m.ManifestVersion)
	if len(m.ManifestVersion) > manifestMaxText {
		return errors.New("manifest_version too long")
	}
	if len(m.Scopes) > manifestMaxScopes {
		return fmt.Errorf("too many scopes (max %d)", manifestMaxScopes)
	}
	if len(m.Presets) > manifestMaxPresets {
		return fmt.Errorf("too many presets (max %d)", manifestMaxPresets)
	}
	if len(m.Routes) > manifestMaxRoutes {
		return fmt.Errorf("too many routes (max %d)", manifestMaxRoutes)
	}
	if len(m.Roles) > manifestMaxRoles {
		return fmt.Errorf("too many roles (max %d)", manifestMaxRoles)
	}

	// Roles.
	seenRole := map[string]bool{}
	for i := range m.Roles {
		r := &m.Roles[i]
		r.ID = strings.TrimSpace(r.ID)
		r.Name = strings.TrimSpace(r.Name)
		r.InvitePrefix = strings.TrimSpace(r.InvitePrefix)
		if !moduleRolePattern.MatchString(r.ID) {
			return fmt.Errorf("invalid role id %q", r.ID)
		}
		if seenRole[r.ID] {
			return fmt.Errorf("duplicate role id %q", r.ID)
		}
		seenRole[r.ID] = true
		if len(r.Name) > manifestMaxText {
			return fmt.Errorf("role %q name too long", r.ID)
		}
		if r.InvitePrefix != "" && !manifestInvitePrefixPattrn.MatchString(r.InvitePrefix) {
			return fmt.Errorf("invalid invite_prefix %q for role %q", r.InvitePrefix, r.ID)
		}
	}

	// Scopes.
	declared := map[string]bool{}
	for i := range m.Scopes {
		sc := &m.Scopes[i]
		sc.ID = strings.TrimSpace(sc.ID)
		sc.Group = strings.TrimSpace(sc.Group)
		sc.Name = strings.TrimSpace(sc.Name)
		if err := validateManifestScopeID(m.Module, sc.ID); err != nil {
			return err
		}
		if declared[sc.ID] {
			return fmt.Errorf("duplicate scope id %q", sc.ID)
		}
		declared[sc.ID] = true
		if len(sc.Group) > manifestMaxText || len(sc.Name) > manifestMaxText {
			return fmt.Errorf("scope %q group/name too long", sc.ID)
		}
	}

	// Presets.
	seenPreset := map[string]bool{}
	for i := range m.Presets {
		p := &m.Presets[i]
		p.ID = strings.TrimSpace(p.ID)
		p.Name = strings.TrimSpace(p.Name)
		if !manifestPresetIDPattern.MatchString(p.ID) {
			return fmt.Errorf("invalid preset id %q", p.ID)
		}
		if seenPreset[p.ID] {
			return fmt.Errorf("duplicate preset id %q", p.ID)
		}
		seenPreset[p.ID] = true
		if len(p.Name) > manifestMaxText {
			return fmt.Errorf("preset %q name too long", p.ID)
		}
		if len(p.Scopes) == 0 {
			return fmt.Errorf("preset %q has no scopes", p.ID)
		}
		for j, sc := range p.Scopes {
			sc = strings.TrimSpace(sc)
			p.Scopes[j] = sc
			if !declared[sc] {
				return fmt.Errorf("preset %q references undeclared scope %q", p.ID, sc)
			}
		}
	}

	// Routes.
	for i := range m.Routes {
		r := &m.Routes[i]
		r.Method = strings.ToUpper(strings.TrimSpace(r.Method))
		r.Path = strings.TrimSpace(r.Path)
		r.Scope = strings.TrimSpace(r.Scope)
		if !manifestMethods[r.Method] {
			return fmt.Errorf("route %d: invalid method %q", i, r.Method)
		}
		if !strings.HasPrefix(r.Path, "/") || len(r.Path) > manifestMaxPath {
			return fmt.Errorf("route %d: invalid path %q", i, r.Path)
		}
		if !declared[r.Scope] {
			return fmt.Errorf("route %s %s references undeclared scope %q", r.Method, r.Path, r.Scope)
		}
	}
	return nil
}

// validateManifestScopeID: `<module>:<res>:<action>[:…]` (≥3 non-empty
// [a-z0-9_] parts after the module) or exactly `<module>:*`. Two-part
// scopes are refused so a module scope can never look like dock's own
// `domain:action` scopes (hosts:write …); an exact clash with dock's
// catalog is refused as well.
func validateManifestScopeID(module, id string) error {
	if id == module+":*" {
		return nil
	}
	if !strings.HasPrefix(id, module+":") {
		return fmt.Errorf("scope %q must start with %q", id, module+":")
	}
	rest := strings.Split(strings.TrimPrefix(id, module+":"), ":")
	if len(rest) < 2 {
		return fmt.Errorf("scope %q must be <module>:<resource>:<action>", id)
	}
	for _, p := range rest {
		if !manifestScopePartPattern.MatchString(p) {
			return fmt.Errorf("scope %q: invalid segment %q", id, p)
		}
	}
	if dockScopeKnown(id) {
		return fmt.Errorf("scope %q collides with a dock scope", id)
	}
	return nil
}

// canonicalManifestSHA is sha256 over the normalised manifest's JSON
// (encoding/json emits struct fields in declaration order, so this is
// stable for an identical, already-validated manifest).
func canonicalManifestSHA(m *ModuleManifest) (string, []byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), b, nil
}

// ── in-process registry ────────────────────────────────────────────────

type moduleManifestRegistry struct {
	mu   sync.RWMutex
	byID map[string]*ModuleManifest
}

var manifestRegistry = &moduleManifestRegistry{byID: map[string]*ModuleManifest{}}

func (r *moduleManifestRegistry) get(module string) *ModuleManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[normalizeModuleName(module)]
}

func (r *moduleManifestRegistry) set(m *ModuleManifest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[m.Module] = m
}

// replaceAll swaps the whole registry (startup / periodic reload).
func (r *moduleManifestRegistry) replaceAll(ms map[string]*ModuleManifest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID = ms
}

// modules returns the registered manifests sorted by module name.
func (r *moduleManifestRegistry) modules() []*ModuleManifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ModuleManifest, 0, len(r.byID))
	for _, m := range r.byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}

// hasScope: a declared scope of a registered module, or `<registered module>:*`.
func (r *moduleManifestRegistry) hasScope(scope string) bool {
	i := strings.IndexByte(scope, ':')
	if i <= 0 {
		return false
	}
	m := r.get(scope[:i])
	if m == nil {
		return false
	}
	if scope == m.Module+":*" {
		return true
	}
	for _, sc := range m.Scopes {
		if sc.ID == scope {
			return true
		}
	}
	return false
}

// roleVocabulary returns role → invite prefix for a module whose manifest
// declares roles, or nil (no manifest / no roles → caller falls back to
// moduleRolePolicies). A role without invite_prefix gets
// UPPER(module)-UPPER(role).
func (r *moduleManifestRegistry) roleVocabulary(module string) map[string]string {
	m := r.get(module)
	if m == nil || len(m.Roles) == 0 {
		return nil
	}
	out := make(map[string]string, len(m.Roles))
	for _, role := range m.Roles {
		prefix := role.InvitePrefix
		if prefix == "" {
			prefix = strings.ToUpper(m.Module) + "-" + strings.ToUpper(role.ID)
		}
		out[role.ID] = prefix
	}
	return out
}

// ── store ──────────────────────────────────────────────────────────────

func (s *Server) manifestRedisKey(module string) string {
	prefix := DefaultRedisPrefix
	if s.redisPrefix != "" {
		prefix = s.redisPrefix
	}
	return fmt.Sprintf("%s:manifest:%s", prefix, module)
}

// upsertModuleManifest stores a validated manifest. changed=false (and no
// write) when the stored body_sha256 is identical.
func (s *Server) upsertModuleManifest(m *ModuleManifest) (changed bool, err error) {
	sha, body, err := canonicalManifestSHA(m)
	if err != nil {
		return false, err
	}
	version := m.ManifestVersion
	if version == "" {
		version = sha
	}
	// lib/pq sends []byte as bytea, which can't cast to jsonb — pass string.
	res, err := s.db.Exec(
		`INSERT INTO module_manifests (module, version, body_sha256, body, updated_at)
		 VALUES ($1, $2, $3, $4::jsonb, now())
		 ON CONFLICT (module) DO UPDATE
		    SET version = EXCLUDED.version,
		        body_sha256 = EXCLUDED.body_sha256,
		        body = EXCLUDED.body,
		        updated_at = now()
		  WHERE module_manifests.body_sha256 IS DISTINCT FROM EXCLUDED.body_sha256`,
		m.Module, version, sha, string(body),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// loadModuleManifests reads every stored manifest. Rows that no longer
// unmarshal are skipped (logged), never fatal.
func (s *Server) loadModuleManifests() (map[string]*ModuleManifest, error) {
	rows, err := s.db.Query(`SELECT module, body FROM module_manifests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*ModuleManifest{}
	for rows.Next() {
		var module string
		var body []byte
		if err := rows.Scan(&module, &body); err != nil {
			return nil, err
		}
		var m ModuleManifest
		if err := json.Unmarshal(body, &m); err != nil {
			log.Printf("module manifest: skip %s: %v", module, err)
			continue
		}
		m.Module = normalizeModuleName(module)
		out[m.Module] = &m
	}
	return out, rows.Err()
}

// publishManifestRedis mirrors a manifest to {prefix}:manifest:{module}.
// Best-effort.
func (s *Server) publishManifestRedis(m *ModuleManifest) {
	if s.redis == nil || m == nil {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = s.redis.Set(context.Background(), s.manifestRedisKey(m.Module), b, 0).Err()
}

// refreshModuleManifests reloads the registry from Postgres and mirrors it
// to redis. Called at startup and periodically (so a second dock replica
// converges on a manifest PUT that landed elsewhere).
func (s *Server) refreshModuleManifests() {
	if s.db == nil {
		return
	}
	ms, err := s.loadModuleManifests()
	if err != nil {
		log.Printf("module manifest: load failed: %v", err)
		return
	}
	manifestRegistry.replaceAll(ms)
	// Off the boot path: an unreachable redis must not stall startup.
	go func() {
		for _, m := range ms {
			s.publishManifestRedis(m)
		}
	}()
}

const manifestRefreshInterval = 5 * time.Minute

// startModuleManifestRegistry loads the registry once and keeps it fresh.
func (s *Server) startModuleManifestRegistry(ctx context.Context) {
	s.refreshModuleManifests()
	go func() {
		t := time.NewTicker(manifestRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.refreshModuleManifests()
			}
		}
	}()
}

// ── PUT /internal/v1/modules/:module/manifest ──────────────────────────
// HMAC plugin auth; :module and body.module must both equal the calling
// plugin. 200 {ack, changed, version}; 400 {error}; 403 module mismatch.
func (s *Server) handleInternalModuleManifestPut(c *gin.Context) {
	pluginStr := callingPlugin(c)
	module := normalizeModuleName(c.Param("module"))
	if module == "" || module != normalizeModuleName(pluginStr) {
		internalErr(c, http.StatusForbidden, "module does not match auth header")
		return
	}
	raw, err := c.GetRawData()
	if err != nil {
		internalErr(c, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(raw) > manifestMaxBodyBytes {
		internalErr(c, http.StatusBadRequest, fmt.Sprintf("manifest too large (max %d bytes)", manifestMaxBodyBytes))
		return
	}
	var m ModuleManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		internalErr(c, http.StatusBadRequest, "invalid_json")
		return
	}
	if err := validateManifest(module, &m); err != nil {
		if errors.Is(err, errManifestModuleMismatch) {
			internalErr(c, http.StatusForbidden, err.Error())
			return
		}
		internalErr(c, http.StatusBadRequest, err.Error())
		return
	}
	changed, err := s.upsertModuleManifest(&m)
	if err != nil {
		log.Printf("module manifest: upsert %s failed: %v", m.Module, err)
		internalErr(c, http.StatusInternalServerError, "manifest write failed")
		return
	}
	version := m.ManifestVersion
	if version == "" {
		version, _, _ = canonicalManifestSHA(&m)
	}
	// Always refresh the in-process copy: a replica that restarted with a
	// stale registry heals on the module's next PUT even when changed=false.
	manifestRegistry.set(&m)
	s.publishManifestRedis(&m)
	c.JSON(http.StatusOK, gin.H{"ack": true, "changed": changed, "version": version})
}

// ── token-UI catalog (GET /api/api-token-scopes → "modules") ───────────

type moduleScopeCatalogScope struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Sensitive bool   `json:"sensitive"`
}

type moduleScopeCatalogGroup struct {
	Group  string                    `json:"group"`
	Scopes []moduleScopeCatalogScope `json:"scopes"`
}

type moduleScopeCatalogPreset struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

type moduleScopeCatalog struct {
	Module      string                     `json:"module"`
	DisplayName string                     `json:"display_name"`
	Groups      []moduleScopeCatalogGroup  `json:"groups"`
	Presets     []moduleScopeCatalogPreset `json:"presets"`
}

// buildModuleScopeCatalog renders registered manifests for the token UI.
// Groups keep the manifest's first-appearance order; displayName maps
// module → plugin display name (missing → module name).
func buildModuleScopeCatalog(ms []*ModuleManifest, displayName func(string) string) []moduleScopeCatalog {
	out := make([]moduleScopeCatalog, 0, len(ms))
	for _, m := range ms {
		if len(m.Scopes) == 0 {
			continue
		}
		cat := moduleScopeCatalog{Module: m.Module, DisplayName: m.Module,
			Groups: []moduleScopeCatalogGroup{}, Presets: []moduleScopeCatalogPreset{}}
		if displayName != nil {
			if dn := strings.TrimSpace(displayName(m.Module)); dn != "" {
				cat.DisplayName = dn
			}
		}
		idx := map[string]int{}
		for _, sc := range m.Scopes {
			g := sc.Group
			if g == "" {
				g = m.Module
			}
			i, ok := idx[g]
			if !ok {
				i = len(cat.Groups)
				idx[g] = i
				cat.Groups = append(cat.Groups, moduleScopeCatalogGroup{Group: g, Scopes: []moduleScopeCatalogScope{}})
			}
			name := sc.Name
			if name == "" {
				name = sc.ID
			}
			cat.Groups[i].Scopes = append(cat.Groups[i].Scopes, moduleScopeCatalogScope{ID: sc.ID, Name: name, Sensitive: sc.Sensitive})
		}
		for _, p := range m.Presets {
			name := p.Name
			if name == "" {
				name = p.ID
			}
			cat.Presets = append(cat.Presets, moduleScopeCatalogPreset{ID: p.ID, Name: name, Scopes: append([]string{}, p.Scopes...)})
		}
		out = append(out, cat)
	}
	return out
}

// pluginDisplayName looks up plugin_modules.display_name; "" on miss/error.
func (s *Server) pluginDisplayName(module string) string {
	if s.db == nil {
		return ""
	}
	pm, _, err := s.getPluginModuleByName(module)
	if err != nil || pm == nil {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("module manifest: display name %s: %v", module, err)
		}
		return ""
	}
	return pm.DisplayName
}
