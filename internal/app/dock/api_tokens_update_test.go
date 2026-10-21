package dock

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gin-auth-app/internal/apiscopes"
)

// firstScopes returns one non-admin and one admin scope from the embedded
// catalog so the tests don't hard-code route names.
func firstScopes(t *testing.T) (normal, admin string) {
	t.Helper()
	for _, g := range apiScopeCatalog() {
		for _, sc := range g.Scopes {
			if apiscopes.IsAdminScope(sc) {
				if admin == "" {
					admin = sc
				}
			} else if normal == "" {
				normal = sc
			}
		}
	}
	if normal == "" || admin == "" {
		t.Fatalf("catalog lacks scopes: normal=%q admin=%q", normal, admin)
	}
	return normal, admin
}

func TestValidateAPITokenPatch(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	normal, _ := firstScopes(t)
	str := func(s string) *string { return &s }
	strs := func(s ...string) *[]string { return &s }
	num := func(n int) *int { return &n }
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }

	cases := []struct {
		name    string
		n       *string
		scopes  *[]string
		ips     *[]string
		rpm     *int
		expires json.RawMessage
		wantErr string
		check   func(t *testing.T, p apiTokenPatch)
	}{
		{name: "empty body", wantErr: "input.invalid"},
		{name: "blank name", n: str("   "), wantErr: "input.invalid"},
		{name: "name trimmed", n: str("  现场机 "), check: func(t *testing.T, p apiTokenPatch) {
			if p.Name == nil || *p.Name != "现场机" {
				t.Fatalf("name = %v", p.Name)
			}
		}},
		{name: "unknown scope", scopes: strs(normal, "nope:read"), wantErr: "api.unknown_scope"},
		{name: "blank scope", scopes: strs(" "), wantErr: "api.unknown_scope"},
		{name: "known scopes trimmed + wildcard", scopes: strs(" "+normal+" ", "*"), check: func(t *testing.T, p apiTokenPatch) {
			if p.Scopes == nil || len(*p.Scopes) != 2 || (*p.Scopes)[0] != normal {
				t.Fatalf("scopes = %v", p.Scopes)
			}
		}},
		{name: "empty scopes = full access allowed", scopes: strs(), check: func(t *testing.T, p apiTokenPatch) {
			if p.Scopes == nil || len(*p.Scopes) != 0 {
				t.Fatalf("scopes = %v", p.Scopes)
			}
		}},
		{name: "bad ip", ips: strs("10.0.0.0/8", "not-ip"), wantErr: "api.invalid_ip"},
		{name: "good ips", ips: strs(" 10.0.0.0/8", "::1"), check: func(t *testing.T, p apiTokenPatch) {
			if p.IPAllowlist == nil || (*p.IPAllowlist)[0] != "10.0.0.0/8" {
				t.Fatalf("ips = %v", p.IPAllowlist)
			}
		}},
		{name: "clear ips", ips: strs(), check: func(t *testing.T, p apiTokenPatch) {
			if p.IPAllowlist == nil || len(*p.IPAllowlist) != 0 {
				t.Fatalf("ips = %v", p.IPAllowlist)
			}
		}},
		{name: "rpm zero", rpm: num(0), wantErr: "input.invalid"},
		{name: "rpm negative", rpm: num(-5), wantErr: "input.invalid"},
		{name: "rpm over cap", rpm: num(apiTokenMaxRPM + 1), wantErr: "input.invalid"},
		{name: "rpm ok", rpm: num(600), check: func(t *testing.T, p apiTokenPatch) {
			if p.RPM == nil || *p.RPM != 600 {
				t.Fatalf("rpm = %v", p.RPM)
			}
		}},
		{name: "expires absent leaves unchanged", rpm: num(60), check: func(t *testing.T, p apiTokenPatch) {
			if p.ExpiresSet {
				t.Fatal("ExpiresSet on absent field")
			}
		}},
		{name: "expires null = never", expires: raw("null"), check: func(t *testing.T, p apiTokenPatch) {
			if !p.ExpiresSet || p.ExpiresAt != nil {
				t.Fatalf("set=%v at=%v", p.ExpiresSet, p.ExpiresAt)
			}
		}},
		{name: "expires future time", expires: raw(`"2027-01-01T00:00:00+08:00"`), check: func(t *testing.T, p apiTokenPatch) {
			want := time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC)
			if !p.ExpiresSet || p.ExpiresAt == nil || !p.ExpiresAt.Equal(want) {
				t.Fatalf("set=%v at=%v", p.ExpiresSet, p.ExpiresAt)
			}
		}},
		{name: "expires past", expires: raw(`"2026-10-01T00:00:00Z"`), wantErr: "input.invalid"},
		{name: "expires now", expires: raw(`"2026-10-21T12:00:00Z"`), wantErr: "input.invalid"},
		{name: "expires garbage", expires: raw(`"next week"`), wantErr: "input.invalid"},
		{name: "expires number", expires: raw(`123`), wantErr: "input.invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, errKey := validateAPITokenPatch(c.n, c.scopes, c.ips, c.rpm, c.expires, nil, now)
			if errKey != c.wantErr {
				t.Fatalf("errKey = %q, want %q", errKey, c.wantErr)
			}
			if c.check != nil {
				c.check(t, p)
			}
		})
	}
}

// Scopes already on the token survive validation even when the catalog no
// longer knows them (module dropped a scope; the UI echoes them back), but a
// newly added unknown scope is still rejected.
func TestValidateAPITokenPatchKeepsExistingUnknownScopes(t *testing.T) {
	now := time.Now()
	normal, admin := firstScopes(t)
	existing := []string{normal, "gone:legacy:read"}
	cases := []struct {
		name     string
		existing []string
		scopes   []string
		wantErr  string
		wantAdd  []string // addedScopes(existing, result) → what the admin gate sees
	}{
		{"keep stale existing scope", existing, []string{normal, "gone:legacy:read"}, "", nil},
		{"keep stale, trimmed", existing, []string{" gone:legacy:read "}, "", nil},
		{"drop stale scope", existing, []string{normal}, "", nil},
		{"add new unknown alongside stale", existing, []string{"gone:legacy:read", "other:new:read"}, "api.unknown_scope", nil},
		{"stale scope not on this token", []string{normal}, []string{"gone:legacy:read"}, "api.unknown_scope", nil},
		{"no existing scopes", nil, []string{"gone:legacy:read"}, "api.unknown_scope", nil},
		{"blank never allowed", []string{""}, []string{""}, "api.unknown_scope", nil},
		{"stale kept, new admin scope flagged for gate", existing, []string{"gone:legacy:read", admin}, "", []string{admin}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := c.scopes
			p, errKey := validateAPITokenPatch(nil, &sc, nil, nil, nil, c.existing, now)
			if errKey != c.wantErr {
				t.Fatalf("errKey = %q, want %q", errKey, c.wantErr)
			}
			if errKey != "" {
				return
			}
			got := addedScopes(c.existing, *p.Scopes)
			if len(got) != len(c.wantAdd) || (len(got) > 0 && got[0] != c.wantAdd[0]) {
				t.Fatalf("addedScopes = %v, want %v", got, c.wantAdd)
			}
		})
	}
}

// The handler's body struct must keep the three expires_at states apart
// through encoding/json (absent → nil RawMessage, null → "null").
func TestAPITokenPatchExpiresDecoding(t *testing.T) {
	type body struct {
		Name      *string         `json:"name"`
		ExpiresAt json.RawMessage `json:"expires_at"`
	}
	now := time.Now()
	for _, c := range []struct {
		in      string
		wantSet bool
		wantNil bool
	}{
		{`{"name":"x"}`, false, true},
		{`{"expires_at":null}`, true, true},
		{`{"expires_at":"2999-01-01T00:00:00Z"}`, true, false},
	} {
		var b body
		if err := json.Unmarshal([]byte(c.in), &b); err != nil {
			t.Fatal(err)
		}
		p, errKey := validateAPITokenPatch(b.Name, nil, nil, nil, b.ExpiresAt, nil, now)
		if errKey != "" {
			t.Fatalf("%s: errKey %q", c.in, errKey)
		}
		if p.ExpiresSet != c.wantSet || (p.ExpiresAt == nil) != c.wantNil {
			t.Fatalf("%s: set=%v at=%v", c.in, p.ExpiresSet, p.ExpiresAt)
		}
	}
}

func TestAPITokenGateDecision(t *testing.T) {
	normal, admin := firstScopes(t)
	yes := func() (bool, error) { return true, nil }
	no := func() (bool, error) { return false, nil }
	boom := func() (bool, error) { return false, errors.New("db down") }
	called := false
	spy := func() (bool, error) { called = true; return false, nil }

	cases := []struct {
		name     string
		authKind string
		scopes   []string
		admin    func() (bool, error)
		want     string
	}{
		{"api_token caller, no scopes", "api_token", nil, yes, "api.session_required"},
		{"api_token caller even if site admin", "api_token", []string{admin}, yes, "api.session_required"},
		{"session, normal scopes", "", []string{normal, "*"}, no, ""},
		{"session, full access (empty)", "", nil, no, ""},
		{"session, admin scope, not site admin", "", []string{normal, admin}, no, "api.admin_scope_forbidden"},
		{"session, admin scope, lookup error", "", []string{admin}, boom, "api.admin_scope_forbidden"},
		{"session, admin scope, site admin", "", []string{admin}, yes, ""},
	}
	for _, c := range cases {
		if got := apiTokenGateDecision(c.authKind, c.scopes, c.admin); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if apiTokenGateDecision("", []string{normal}, spy); called {
		t.Error("site-admin lookup must not run without an admin scope")
	}
}

func TestAPITokenCanEdit(t *testing.T) {
	cases := []struct {
		caller, owner, role string
		want                bool
	}{
		{"u1", "u1", TeamRoleMember, true},
		{"u1", "u1", "", true},
		{"u2", "u1", TeamRoleOwner, true},
		{"u2", "u1", TeamRoleAdmin, true},
		{"u2", "u1", TeamRoleMember, false},
		{"u2", "u1", TeamRoleViewer, false},
		{"u2", "u1", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		if got := apiTokenCanEdit(c.caller, c.owner, c.role); got != c.want {
			t.Errorf("canEdit(%q,%q,%q) = %v, want %v", c.caller, c.owner, c.role, got, c.want)
		}
	}
}

func TestAPITokenPatchApply(t *testing.T) {
	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := APIToken{Name: "a", Scopes: []string{"x:read"}, IPAllowlist: []string{}, RateLimitRPM: 120, ExpiresAt: &exp}
	name, rpm := "a", 120
	// Same values → nothing changed.
	if _, ch := (apiTokenPatch{Name: &name, RPM: &rpm}).apply(cur); len(ch) != 0 {
		t.Fatalf("no-op patch changed %v", ch)
	}
	scopes := []string{"x:read", "y:write"}
	next, ch := (apiTokenPatch{Scopes: &scopes, ExpiresSet: true}).apply(cur)
	if len(ch) != 2 || ch[0] != "scopes" || ch[1] != "expires_at" {
		t.Fatalf("changed = %v", ch)
	}
	if next.ExpiresAt != nil || len(next.Scopes) != 2 || cur.ExpiresAt == nil {
		t.Fatalf("next=%+v cur=%+v", next, cur)
	}
	if got := addedScopes(cur.Scopes, scopes); len(got) != 1 || got[0] != "y:write" {
		t.Fatalf("addedScopes = %v", got)
	}
}

func TestAPITokenAuditViewHasNoSecret(t *testing.T) {
	v := apiTokenAuditView(&APIToken{Name: "n", Prefix: "polar_abc"})
	for _, k := range []string{"token_hash", "secret", "hash", "prefix"} {
		if _, ok := v[k]; ok {
			t.Errorf("audit view leaks %q", k)
		}
	}
}
