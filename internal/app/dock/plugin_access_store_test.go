package dock

// Pure-memory tests for R3 plugin-access request scope validation. The
// DB-touching CRUD + the schema migration are exercised by the
// boot-smoke CI job; here we pin the pure scope/route rules.

import "testing"

func TestNormalizePluginAccessScope(t *testing.T) {
	cases := []struct {
		name          string
		scope, route  string
		wantScope     string
		wantRoute     string
		wantErr       bool
	}{
		{"empty defaults to module", "", "", pluginAccessScopeModule, "", false},
		{"module explicit", "module", "", pluginAccessScopeModule, "", false},
		{"module uppercased", "MODULE", "", pluginAccessScopeModule, "", false},
		{"module with route is invalid", "module", "/api/x", "", "", true},
		{"api with route", "api", "/api/expense/items", pluginAccessScopeAPI, "/api/expense/items", false},
		{"api route trimmed", "api", "  /api/x  ", pluginAccessScopeAPI, "/api/x", false},
		{"api without route is invalid", "api", "", "", "", true},
		{"unknown scope is invalid", "everything", "", "", "", true},
	}
	for _, tc := range cases {
		gotScope, gotRoute, err := normalizePluginAccessScope(tc.scope, tc.route)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got scope=%q route=%q", tc.name, gotScope, gotRoute)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if gotScope != tc.wantScope || gotRoute != tc.wantRoute {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", tc.name, gotScope, gotRoute, tc.wantScope, tc.wantRoute)
		}
	}
}
