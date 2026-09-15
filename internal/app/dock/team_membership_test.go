package dock

// Tests for "a module role without a team is a dead account" — the gap that
// let someone redeem a WT-CREATOR- code, register successfully, and then be
// told "you are not in any team" with no way forward.
//
// Same stance as module_roles_test.go: these are the pure, pre-DB checks.
// The DB-backed half (redeem → team_members row) needs Postgres and is in
// the PR's smoke procedure.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAddTeamMember_RefusesPrivilegedRoles(t *testing.T) {
	// This is the whole reason addTeamMember exists as a chokepoint.
	// IsSiteAdmin answers "member of the root team with role owner|admin",
	// and on a single-tenant deployment the default team IS the root team —
	// so an invite code carrying team_role=admin would hand out site
	// administrator to everyone who redeemed it.
	//
	// nil *Server is deliberate: validation must reject before touching s.db,
	// otherwise a bad role only fails once it has already reached Postgres.
	var s *Server
	for _, role := range []string{"owner", "admin", "OWNER", "root", "superuser"} {
		err := s.addTeamMember("team-1", "user-1", role)
		if err == nil {
			t.Fatalf("addTeamMember accepted role %q — that mints site admins", role)
		}
		if !strings.Contains(err.Error(), role) {
			t.Fatalf("error for %q should name the offending role, got: %v", role, err)
		}
	}
}

func TestAddTeamMember_RequiresBothKeys(t *testing.T) {
	var s *Server
	if err := s.addTeamMember("", "user-1", TeamRoleMember); err == nil {
		t.Fatal("empty team_id accepted")
	}
	if err := s.addTeamMember("team-1", "  ", TeamRoleMember); err == nil {
		t.Fatal("blank user_id accepted")
	}
}

func TestHandleInviteCodeGenerate_RejectsPrivilegedTeamRole(t *testing.T) {
	// Caught at mint time, not redemption: a code minted with team_role=admin
	// would look fine in the list for days and then fail for every person who
	// used it — or worse, succeed.
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{
		"count": 1, "module": "opera", "role": "researcher", "team_role": "admin",
	})
	c.Request = httptest.NewRequest(http.MethodPost, "/api/site-settings/invite-codes", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInviteCodeGenerate(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("team_role=admin should be 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleSiteSettingsUpdate_SingleTenantNeedsADefaultTeam(t *testing.T) {
	// multi_tenant_enabled=false with no default team is exactly the
	// configuration this setting exists to prevent: registrations succeed
	// and land nowhere. Refuse it instead of saving a no-op.
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body, _ := json.Marshal(map[string]any{
		"name": "Polar-", "multi_tenant_enabled": false, "default_team_id": "",
	})
	c.Request = httptest.NewRequest(http.MethodPut, "/api/site-settings", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleSiteSettingsUpdate(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("single-tenant without default_team_id should be 400, got %d: %s", w.Code, w.Body.String())
	}
}
