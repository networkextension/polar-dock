package dock

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 契约见 doc/arch/module-manifest.md（与 polar-sdk / 签发页 / 平台共用）。

func operaManifest() *ModuleManifest {
	return &ModuleManifest{
		Module:          "opera",
		ManifestVersion: "v1",
		Roles: []ModuleManifestRole{
			{ID: "admin", Name: "管理员", InvitePrefix: "WT-ADMIN"},
			{ID: "researcher", Name: "主创", InvitePrefix: "WT-CREATOR"},
			{ID: "audience", Name: "观众", InvitePrefix: "WT-AUDIENCE"},
		},
		Scopes: []ModuleManifestScope{
			{ID: "opera:sessions:read", Group: "场次", Name: "读场次"},
			{ID: "opera:participants:read", Group: "观众", Name: "读参与者", Sensitive: true},
			{ID: "opera:device_qc:write", Group: "设备", Name: "报质检"},
		},
		Presets: []ModuleManifestPreset{
			{ID: "site-machine", Name: "现场机", Scopes: []string{"opera:sessions:read", "opera:device_qc:write"}},
		},
		Routes: []ModuleManifestRoute{
			{Method: "get", Path: "/api/opera/v1/sessions/{session_id}", Scope: "opera:sessions:read"},
		},
	}
}

// withManifests swaps the global registry for the test and restores it.
func withManifests(t *testing.T, ms ...*ModuleManifest) {
	t.Helper()
	manifestRegistry.mu.RLock()
	saved := manifestRegistry.byID
	manifestRegistry.mu.RUnlock()
	next := map[string]*ModuleManifest{}
	for _, m := range ms {
		next[m.Module] = m
	}
	manifestRegistry.replaceAll(next)
	t.Cleanup(func() { manifestRegistry.replaceAll(saved) })
}

func TestValidateManifest_Accepts(t *testing.T) {
	m := operaManifest()
	m.Scopes = append(m.Scopes, ModuleManifestScope{ID: "opera:*", Group: "全部"})
	if err := validateManifest("opera", m); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if m.Routes[0].Method != "GET" {
		t.Fatalf("method not normalised: %q", m.Routes[0].Method)
	}
	// 4-part scope is fine (≥3 parts).
	m2 := operaManifest()
	m2.Scopes = append(m2.Scopes, ModuleManifestScope{ID: "opera:sessions:phase:write"})
	if err := validateManifest("opera", m2); err != nil {
		t.Fatalf("4-part scope rejected: %v", err)
	}
}

func TestValidateManifest_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		caller string
		mut    func(m *ModuleManifest)
		want   string // substring of the error
	}{
		{"module mismatch", "zone", func(m *ModuleManifest) {}, "does not match"},
		{"admin module", "admin", func(m *ModuleManifest) {
			m.Module = "admin"
			m.Scopes, m.Presets, m.Routes = nil, nil, nil
		}, "reserved"},
		{"bad module name", "9opera", func(m *ModuleManifest) { m.Module = "9opera"; m.Scopes, m.Presets, m.Routes = nil, nil, nil }, "invalid module name"},
		{"2-part scope", "opera", func(m *ModuleManifest) {
			m.Scopes = append(m.Scopes, ModuleManifestScope{ID: "opera:write"})
		}, "<module>:<resource>:<action>"},
		{"dock-style hosts:write", "opera", func(m *ModuleManifest) {
			m.Scopes = append(m.Scopes, ModuleManifestScope{ID: "hosts:write"})
		}, "must start with"},
		{"other module prefix", "opera", func(m *ModuleManifest) {
			m.Scopes = append(m.Scopes, ModuleManifestScope{ID: "zone:dns:write"})
		}, "must start with"},
		{"uppercase segment", "opera", func(m *ModuleManifest) {
			m.Scopes = append(m.Scopes, ModuleManifestScope{ID: "opera:Sessions:read"})
		}, "invalid segment"},
		{"empty segment", "opera", func(m *ModuleManifest) {
			m.Scopes = append(m.Scopes, ModuleManifestScope{ID: "opera::read"})
		}, "invalid segment"},
		{"duplicate scope", "opera", func(m *ModuleManifest) {
			m.Scopes = append(m.Scopes, m.Scopes[0])
		}, "duplicate scope"},
		{"undeclared preset scope", "opera", func(m *ModuleManifest) {
			m.Presets[0].Scopes = append(m.Presets[0].Scopes, "opera:works:read")
		}, "undeclared scope"},
		{"undeclared route scope", "opera", func(m *ModuleManifest) {
			m.Routes[0].Scope = "opera:works:read"
		}, "undeclared scope"},
		{"bad route method", "opera", func(m *ModuleManifest) { m.Routes[0].Method = "FETCH" }, "invalid method"},
		{"bad role id", "opera", func(m *ModuleManifest) { m.Roles[0].ID = "Admin" }, "invalid role id"},
		{"duplicate role", "opera", func(m *ModuleManifest) { m.Roles = append(m.Roles, m.Roles[0]) }, "duplicate role"},
		{"bad invite prefix", "opera", func(m *ModuleManifest) { m.Roles[0].InvitePrefix = "wt admin" }, "invite_prefix"},
		{"too many scopes", "opera", func(m *ModuleManifest) {
			m.Scopes = make([]ModuleManifestScope, manifestMaxScopes+1)
		}, "too many scopes"},
		{"too many presets", "opera", func(m *ModuleManifest) {
			m.Presets = make([]ModuleManifestPreset, manifestMaxPresets+1)
		}, "too many presets"},
		{"too many routes", "opera", func(m *ModuleManifest) {
			m.Routes = make([]ModuleManifestRoute, manifestMaxRoutes+1)
		}, "too many routes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := operaManifest()
			tc.mut(m)
			err := validateManifest(tc.caller, m)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateManifest_DockScopeCollision(t *testing.T) {
	// dock's own catalog is 2-part today, so inject a 3-part one to prove the check.
	scopeOnce.Do(loadScopes)
	allScopeSet["opera:sessions:read"] = true
	defer delete(allScopeSet, "opera:sessions:read")
	err := validateManifest("opera", operaManifest())
	if err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("err = %v, want collision", err)
	}
}

func TestCanonicalManifestSHA_Stable(t *testing.T) {
	a, b := operaManifest(), operaManifest()
	_ = validateManifest("opera", a)
	_ = validateManifest("opera", b)
	sa, _, _ := canonicalManifestSHA(a)
	sb, _, _ := canonicalManifestSHA(b)
	if sa != sb || len(sa) != 64 {
		t.Fatalf("sha unstable: %s vs %s", sa, sb)
	}
	b.Scopes[0].Name = "改了"
	if sc, _, _ := canonicalManifestSHA(b); sc == sa {
		t.Fatal("sha must change with content")
	}
}

func TestKnownScope_WithRegistry(t *testing.T) {
	withManifests(t)
	if knownScope("opera:sessions:read") {
		t.Fatal("unregistered module scope accepted")
	}
	if !knownScope("*") {
		t.Fatal("wildcard must stay known")
	}
	m := operaManifest()
	_ = validateManifest("opera", m)
	withManifests(t, m)
	for _, ok := range []string{"opera:sessions:read", "opera:participants:read", "opera:*", "*"} {
		if !knownScope(ok) {
			t.Errorf("knownScope(%q) = false", ok)
		}
	}
	for _, bad := range []string{"opera:works:read", "zone:*", "opera", "opera:", "opera:sessions"} {
		if knownScope(bad) {
			t.Errorf("knownScope(%q) = true", bad)
		}
	}
}

func TestModuleRoleVocabulary_ManifestWinsAndFallsBack(t *testing.T) {
	// No manifest: hard-coded opera policy (module_roles_test.go also pins this).
	withManifests(t)
	if p, err := validateModuleRole("opera", "researcher"); err != nil || p != "WT-CREATOR" {
		t.Fatalf("fallback: %q %v", p, err)
	}
	// Manifest with roles: closed vocabulary from the manifest; default prefix.
	m := &ModuleManifest{Module: "film", Roles: []ModuleManifestRole{
		{ID: "curator"}, {ID: "viewer", InvitePrefix: "FILM-SEE"},
	}}
	withManifests(t, m)
	if p, err := validateModuleRole("film", "curator"); err != nil || p != "FILM-CURATOR" {
		t.Fatalf("default prefix: %q %v", p, err)
	}
	if p, err := validateModuleRole("film", "viewer"); err != nil || p != "FILM-SEE" {
		t.Fatalf("declared prefix: %q %v", p, err)
	}
	if _, err := validateModuleRole("film", "director"); err == nil {
		t.Fatal("undeclared role accepted for a module with a manifest")
	}
	// Opera manifest with the WT-* prefixes keeps the polar-opera contract.
	om := operaManifest()
	withManifests(t, om)
	for role, want := range map[string]string{"admin": "WT-ADMIN", "researcher": "WT-CREATOR", "audience": "WT-AUDIENCE"} {
		if p, err := validateModuleRole("opera", role); err != nil || p != want {
			t.Fatalf("opera %s: %q %v", role, p, err)
		}
	}
	// Manifest without roles → hard-coded policy still applies.
	withManifests(t, &ModuleManifest{Module: "opera"})
	if _, err := validateModuleRole("opera", "creator"); err == nil {
		t.Fatal("roleless manifest must fall back to closed opera vocabulary")
	}
}

func TestBuildModuleScopeCatalog(t *testing.T) {
	m := operaManifest()
	_ = validateManifest("opera", m)
	cat := buildModuleScopeCatalog([]*ModuleManifest{m, {Module: "empty"}}, func(string) string { return "风洞实验" })
	if len(cat) != 1 {
		t.Fatalf("modules without scopes must be skipped: %d", len(cat))
	}
	b, _ := json.Marshal(cat[0])
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	for _, k := range []string{"module", "display_name", "groups", "presets"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("missing %q in %s", k, b)
		}
	}
	if cat[0].DisplayName != "风洞实验" || len(cat[0].Groups) != 3 || cat[0].Groups[0].Group != "场次" {
		t.Fatalf("catalog: %s", b)
	}
	if !cat[0].Groups[1].Scopes[0].Sensitive {
		t.Fatal("sensitive flag lost")
	}
	if cat2 := buildModuleScopeCatalog([]*ModuleManifest{m}, func(string) string { return "" }); cat2[0].DisplayName != "opera" {
		t.Fatal("display name must fall back to module")
	}
}

func TestManifestRedisKey(t *testing.T) {
	if got := (&Server{}).manifestRedisKey("opera"); got != "polar:manifest:opera" {
		t.Fatalf("got %q", got)
	}
}

// Handler: rejections happen before any DB call (Server.db is nil here).
func TestHandleModuleManifestPut_RejectsBeforeDB(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{}
	run := func(path, plugin, body string) (int, string) {
		r := gin.New()
		r.PUT("/internal/v1/modules/:module/manifest", func(c *gin.Context) {
			c.Set("plugin_name", plugin)
			s.handleInternalModuleManifestPut(c)
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	if code, _ := run("/internal/v1/modules/opera/manifest", "zone", `{}`); code != http.StatusForbidden {
		t.Fatalf("path/plugin mismatch: %d", code)
	}
	if code, _ := run("/internal/v1/modules/opera/manifest", "opera", `{"module":"zone"}`); code != http.StatusForbidden {
		t.Fatalf("body/plugin mismatch: %d", code)
	}
	if code, _ := run("/internal/v1/modules/opera/manifest", "opera", `{not json`); code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", code)
	}
	if code, body := run("/internal/v1/modules/opera/manifest", "opera", `{"module":"opera","scopes":[{"id":"opera:write"}]}`); code != http.StatusBadRequest || !strings.Contains(body, "error") {
		t.Fatalf("2-part scope: %d %s", code, body)
	}
	big := `{"module":"opera","manifest_version":"` + strings.Repeat("x", manifestMaxBodyBytes) + `"}`
	if code, _ := run("/internal/v1/modules/opera/manifest", "opera", big); code != http.StatusBadRequest {
		t.Fatalf("oversize: %d", code)
	}
}
