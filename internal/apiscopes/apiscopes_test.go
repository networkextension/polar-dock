package apiscopes

import "testing"

func TestScopeFor(t *testing.T) {
	cases := []struct {
		method, path, auth, want string
	}{
		{"GET", "/api/posts", "user", "posts:read"},
		{"POST", "/api/posts", "user", "posts:write"},
		{"DELETE", "/api/posts/:id", "user", "posts:write"},
		{"GET", "/api/rooms/:id/messages", "user", "rooms:read"},
		{"GET", "/api/admin/users", "admin", "admin:read"},
		{"PUT", "/api/admin/users/:id/password", "admin", "admin:write"},
		{"POST", "/api/login", "", ""},        // open route → no scope
		{"GET", "/ws/host/:id/shell", "user", ""}, // non-/api → no scope
	}
	for _, c := range cases {
		if got := ScopeFor(c.method, c.path, c.auth); got != c.want {
			t.Errorf("ScopeFor(%s %s auth=%q) = %q, want %q", c.method, c.path, c.auth, got, c.want)
		}
	}
}

func TestAllowsScope(t *testing.T) {
	cases := []struct {
		name     string
		granted  []string
		required string
		want     bool
	}{
		{"unscoped route always ok", []string{"posts:read"}, "", true},
		{"empty grant = full access", nil, "posts:write", true},
		{"empty grant reaches admin too", []string{}, "admin:write", true},
		{"exact match", []string{"posts:read"}, "posts:read", true},
		{"missing scope denied", []string{"posts:read"}, "posts:write", false},
		{"wildcard covers non-admin", []string{"*"}, "chats:write", true},
		{"wildcard excludes admin", []string{"*"}, "admin:read", false},
		{"admin granted explicitly", []string{"admin:read"}, "admin:read", true},
	}
	for _, c := range cases {
		if got := AllowsScope(c.granted, c.required); got != c.want {
			t.Errorf("%s: AllowsScope(%v, %q) = %v, want %v", c.name, c.granted, c.required, got, c.want)
		}
	}
}
