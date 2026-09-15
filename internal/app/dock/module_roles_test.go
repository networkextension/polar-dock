package dock

// Pure tests for the module-role plumbing: the code→role contract with
// polar-opera (auth-identity.ts), and the admin-side validation that
// runs before any DB call. Grants/redemption themselves need Postgres
// and are covered by the PR's manual smoke procedure.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestValidateModuleRole_OperaVocabulary(t *testing.T) {
	// The prefixes are the ones polar-opera's classifyInviteCode()
	// recognises — if these drift, a code minted here reads as
	// "unrecognized" in the windtunnel UI.
	cases := map[string]string{
		"admin":      "WT-ADMIN",
		"researcher": "WT-CREATOR",
		"audience":   "WT-AUDIENCE",
	}
	for role, wantPrefix := range cases {
		got, err := validateModuleRole("opera", role)
		if err != nil {
			t.Fatalf("validateModuleRole(opera, %s): %v", role, err)
		}
		if got != wantPrefix {
			t.Fatalf("prefix for %s: got %q want %q", role, got, wantPrefix)
		}
	}
}

func TestValidateModuleRole_RejectsUnknownRoleInClosedModule(t *testing.T) {
	// "creator" is the *frontend* word; dock's vocabulary is
	// researcher. Accepting both would fork the contract.
	for _, role := range []string{"creator", "director", "", "ADMIN!"} {
		if _, err := validateModuleRole("opera", role); err == nil {
			t.Fatalf("validateModuleRole(opera, %q) accepted an unknown role", role)
		}
	}
}

func TestValidateModuleRole_OpenModuleFallsBackToModulePrefix(t *testing.T) {
	prefix, err := validateModuleRole("film", "curator")
	if err != nil {
		t.Fatalf("open-vocabulary module rejected: %v", err)
	}
	if prefix != "FILM" {
		t.Fatalf("prefix: got %q want FILM", prefix)
	}
	if _, err := validateModuleRole("Bad Module", "curator"); err == nil {
		t.Fatal("malformed module name accepted")
	}
}

func TestGenerateInviteCode_DisplayAndStoredForms(t *testing.T) {
	display, stored := generateInviteCode("WT-CREATOR")
	if !strings.HasPrefix(display, "WT-CREATOR-") {
		t.Fatalf("display form lost its prefix: %q", display)
	}
	// The stored form is what redemption compares against, so pasting
	// either the dashed or the squashed code must resolve to one row.
	if stored != normalizeInviteCode(display) {
		t.Fatalf("stored %q is not the normalized display %q", stored, display)
	}
	if stored != normalizeInviteCode(strings.ToLower(display)) {
		t.Fatal("redemption is case-sensitive; it must not be")
	}

	plainDisplay, plainStored := generateInviteCode("")
	if !strings.HasPrefix(plainDisplay, "IM") {
		t.Fatalf("prefix-less code changed shape: %q", plainDisplay)
	}
	if plainStored != plainDisplay {
		t.Fatalf("plain code should already be normalized: %q vs %q", plainStored, plainDisplay)
	}
}

func TestInviteCodeHint_NeverLeaksWholeCode(t *testing.T) {
	code := "WTCREATORA1B2C3D4"
	hint := inviteCodeHint(code)
	if strings.Contains(hint, code) || len(hint) >= len(code) {
		t.Fatalf("hint %q leaks the code", hint)
	}
	if hint != "WTCR…D4" {
		t.Fatalf("hint: got %q want WTCR…D4", hint)
	}
}

func TestHandleInviteCodeGenerate_RejectsHalfSpecifiedGrant(t *testing.T) {
	// module without role (or the reverse) would mint a code that looks
	// like a grant and silently isn't. Rejected before any DB call, so
	// a nil-db Server is enough here.
	for _, body := range []string{`{"module":"opera"}`, `{"role":"admin"}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/site-settings/invite-codes", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")

		(&Server{}).handleInviteCodeGenerate(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status got %d want 400", body, w.Code)
		}
	}
}

func TestHandleInviteCodeGenerate_RejectsUnknownRole(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/site-settings/invite-codes",
		strings.NewReader(`{"module":"opera","role":"creator"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleInviteCodeGenerate(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body["error"] == "" {
		t.Fatal("error message missing")
	}
}

func TestHandleModuleRoleGrant_RejectsBadRoleBeforeLookup(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/site-settings/module-roles",
		strings.NewReader(`{"module":"opera","role":"superuser","email":"a@b.c"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	(&Server{}).handleModuleRoleGrant(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", w.Code)
	}
}

func TestHandleModuleRoleRevoke_RequiresBothKeys(t *testing.T) {
	for _, query := range []string{"", "?module=opera", "?user_id=u1"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodDelete, "/api/site-settings/module-roles"+query, nil)

		(&Server{}).handleModuleRoleRevoke(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status got %d want 400", query, w.Code)
		}
	}
}
