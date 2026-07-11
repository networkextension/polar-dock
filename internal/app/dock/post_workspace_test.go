package dock

import "testing"

func TestPostAccessAllowed(t *testing.T) {
	member := &TeamMember{Role: TeamRoleMember}
	cases := []struct {
		name     string
		siteRole string
		member   *TeamMember
		want     bool
	}{
		{"site admin bypasses without membership", "admin", nil, true},
		{"site admin with membership", "admin", member, true},
		{"regular user without membership", "user", nil, false},
		{"empty role without membership", "", nil, false},
		{"api_token role without membership", "api_token", nil, false},
		{"owner member", "user", &TeamMember{Role: TeamRoleOwner}, true},
		{"admin member", "user", &TeamMember{Role: TeamRoleAdmin}, true},
		{"plain member", "user", &TeamMember{Role: TeamRoleMember}, true},
		{"viewer member", "user", &TeamMember{Role: TeamRoleViewer}, true},
	}
	for _, tc := range cases {
		if got := postAccessAllowed(tc.siteRole, tc.member); got != tc.want {
			t.Errorf("%s: postAccessAllowed(%q, %v) = %v, want %v", tc.name, tc.siteRole, tc.member, got, tc.want)
		}
	}
}
