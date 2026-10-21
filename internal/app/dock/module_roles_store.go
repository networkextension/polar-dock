package dock

// module_roles_store.go — per-module user roles.
//
// Why a separate axis from users.role: users.role is the *platform*
// role, where 'admin' is (legacy) site admin and gates dock's own
// console. A plugin module has its own cast — 风洞 (opera) has
// admin / researcher / audience — and handing a windtunnel 管理员 a
// dock site-admin row would be a privilege escalation across the whole
// platform. So module grants live here, are keyed by module name, and
// surface only to the module that owns them:
//
//   - /internal/v1/auth/verify  → the calling plugin's own grant
//   - GET /api/me               → module_roles map (UI landing pages)
//
// Grants arrive two ways: redeeming a role-bearing invite code at
// registration (see handleRegister), or a site admin granting one
// directly (see module_roles_handlers.go).
//
// Contract mirror for opera: polar-opera packages/sdk/src/auth-identity.ts
// (WT-ADMIN-* → admin, WT-CREATOR-* → researcher a.k.a. "creator",
// no code → audience).

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

var (
	errInvalidModuleName = errors.New("invalid module name")
	errInvalidModuleRole = errors.New("invalid module role")
)

var moduleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
var moduleRolePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// moduleRolePolicies — the role vocabulary a module recognises, and the
// human-readable prefix its invite codes carry. A module listed here is
// closed-vocabulary: a typo'd role is rejected at mint time rather than
// discovered as a silently-powerless account later. Modules not listed
// fall back to "any well-formed role, prefix = MODULE".
var moduleRolePolicies = map[string]map[string]string{
	// opera / 风洞实验. "researcher" is what the windtunnel UI calls
	// "creator" (主创) — same people, two names, see auth-identity.ts.
	"opera": {
		"admin":      "WT-ADMIN",
		"researcher": "WT-CREATOR",
		"audience":   "WT-AUDIENCE",
	},
}

func normalizeModuleName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeModuleRole(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// moduleRoleVocabulary returns the roles a module accepts (role → invite
// prefix), or nil when the module is open-vocabulary. A registered module
// manifest that declares roles wins (module_manifest.go); otherwise the
// hard-coded moduleRolePolicies apply.
func moduleRoleVocabulary(module string) map[string]string {
	m := normalizeModuleName(module)
	if vocab := manifestRegistry.roleVocabulary(m); vocab != nil {
		return vocab
	}
	return moduleRolePolicies[m]
}

// validateModuleRole checks a (module, role) pair and returns the
// invite-code prefix to mint codes with.
func validateModuleRole(module, role string) (string, error) {
	m := normalizeModuleName(module)
	r := normalizeModuleRole(role)
	if !moduleNamePattern.MatchString(m) {
		return "", errInvalidModuleName
	}
	if vocab := moduleRoleVocabulary(m); vocab != nil {
		prefix, ok := vocab[r]
		if !ok {
			return "", errInvalidModuleRole
		}
		return prefix, nil
	}
	if !moduleRolePattern.MatchString(r) {
		return "", errInvalidModuleRole
	}
	return strings.ToUpper(m), nil
}

// grantModuleRole upserts a user's role inside a module. sourceCode is
// the invite code that produced the grant (empty for admin grants) and
// is kept for the audit trail.
func (s *Server) grantModuleRole(module, userID, role, grantedBy, sourceCode string, now time.Time) error {
	m := normalizeModuleName(module)
	r := normalizeModuleRole(role)
	if m == "" || r == "" || strings.TrimSpace(userID) == "" {
		return errInvalidModuleRole
	}
	_, err := s.db.Exec(
		`INSERT INTO module_user_roles (module, user_id, role, granted_by, source_code, granted_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (module, user_id) DO UPDATE
		    SET role = EXCLUDED.role,
		        granted_by = EXCLUDED.granted_by,
		        source_code = EXCLUDED.source_code,
		        granted_at = EXCLUDED.granted_at`,
		m,
		strings.TrimSpace(userID),
		r,
		strings.TrimSpace(grantedBy),
		normalizeInviteCode(sourceCode),
		now,
	)
	if err == nil {
		s.verifyCacheDel("modrole", moduleRoleCacheKey(m, userID))
	}
	return err
}

func (s *Server) revokeModuleRole(module, userID string) (bool, error) {
	res, err := s.db.Exec(
		`DELETE FROM module_user_roles WHERE module = $1 AND user_id = $2`,
		normalizeModuleName(module),
		strings.TrimSpace(userID),
	)
	if err != nil {
		return false, err
	}
	s.verifyCacheDel("modrole", moduleRoleCacheKey(module, userID))
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// getModuleRole returns "" (no error) when the user has no grant in
// that module — callers decide what the module's default role is.
func (s *Server) getModuleRole(module, userID string) (string, error) {
	m := normalizeModuleName(module)
	if m == "" || strings.TrimSpace(userID) == "" {
		return "", nil
	}
	var role string
	err := s.db.QueryRow(
		`SELECT role FROM module_user_roles WHERE module = $1 AND user_id = $2`,
		m,
		strings.TrimSpace(userID),
	).Scan(&role)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return role, nil
}

// moduleRolesForUser → {module: role} for /api/me, so a portal can send
// the user to the right module landing page right after login.
func (s *Server) moduleRolesForUser(userID string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(userID) == "" {
		return out, nil
	}
	rows, err := s.db.Query(
		`SELECT module, role FROM module_user_roles WHERE user_id = $1 ORDER BY module ASC`,
		strings.TrimSpace(userID),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var module, role string
		if err := rows.Scan(&module, &role); err != nil {
			return nil, err
		}
		out[module] = role
	}
	return out, rows.Err()
}

// listModuleRoles — admin view. module="" lists every module.
func (s *Server) listModuleRoles(module string, limit int) ([]ModuleUserRole, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT r.module, r.user_id, COALESCE(u.username, ''), COALESCE(u.email, ''),
		        r.role, r.granted_by, r.source_code, r.granted_at
		   FROM module_user_roles r
		   LEFT JOIN users u ON u.id = r.user_id
		  WHERE ($1 = '' OR r.module = $1)
		  ORDER BY r.granted_at DESC
		  LIMIT $2`,
		normalizeModuleName(module),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]ModuleUserRole, 0, limit)
	for rows.Next() {
		var item ModuleUserRole
		if err := rows.Scan(&item.Module, &item.UserID, &item.Username, &item.Email,
			&item.Role, &item.GrantedBy, &item.SourceCode, &item.GrantedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
