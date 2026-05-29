package dock

// Pure-memory tests for the R1c "home team" identity. The DB-touching
// conversion (convertLegacyPersonalTeams) + slug-based lookups are
// exercised by the boot-smoke CI job; here we only pin the pure logic
// that decides whether a team is its owner's home team.

import "testing"

func TestPersonalTeamSlugDeterministic(t *testing.T) {
	a := personalTeamSlug("u_abc")
	b := personalTeamSlug("u_abc")
	if a != b {
		t.Fatalf("slug not deterministic: %q != %q", a, b)
	}
	if personalTeamSlug("u_abc") == personalTeamSlug("u_xyz") {
		t.Fatal("distinct users produced the same home-team slug")
	}
	if len(a) == 0 || a[:9] != "personal-" {
		t.Fatalf("unexpected slug shape: %q", a)
	}
}

func TestIsHomeTeam(t *testing.T) {
	owner := "u_owner1"
	cases := []struct {
		name string
		team *Team
		want bool
	}{
		{"nil", nil, false},
		{"home team (slug matches owner)", &Team{Slug: personalTeamSlug(owner), OwnerUserID: owner, Kind: TeamKindTeam}, true},
		{"home team still recognized when legacy kind=personal", &Team{Slug: personalTeamSlug(owner), OwnerUserID: owner, Kind: TeamKindPersonal}, true},
		{"explicit team (random slug)", &Team{Slug: "acme-co", OwnerUserID: owner, Kind: TeamKindTeam}, false},
		{"slug for a different owner", &Team{Slug: personalTeamSlug("someone_else"), OwnerUserID: owner, Kind: TeamKindTeam}, false},
		{"empty owner", &Team{Slug: "personal-deadbeef0000", OwnerUserID: "", Kind: TeamKindTeam}, false},
	}
	for _, tc := range cases {
		if got := isHomeTeam(tc.team); got != tc.want {
			t.Errorf("%s: isHomeTeam = %v, want %v", tc.name, got, tc.want)
		}
	}
}
