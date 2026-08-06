package dock

// module_roles_handlers.go — site-admin management of per-module roles
// and of the invite codes that grant them. All routes sit behind
// AuthMiddleware + AdminMiddleware (see app.go).
//
// Two ways a module role is created:
//   1. a user redeems a role-bearing invite code at registration
//      (handleRegister) — the self-service path;
//   2. an admin grants one here — the backfill/fix path, which is what
//      keeps existing accounts working after a module starts using
//      roles.
//
// Store + rationale: module_roles_store.go.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// GET /api/site-settings/module-roles?module=opera&limit=100
func (s *Server) handleModuleRoleList(c *gin.Context) {
	module := normalizeModuleName(c.Query("module"))
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 limit 参数"})
			return
		}
		limit = parsed
	}
	items, err := s.listModuleRoles(module, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "模块角色加载失败"})
		return
	}
	resp := gin.H{"module_roles": items}
	// Hand the admin UI the module's role vocabulary so the grant form
	// is a select, not a free-text field that typos into a dead role.
	if vocab := moduleRoleVocabulary(module); vocab != nil {
		roles := make([]string, 0, len(vocab))
		for role := range vocab {
			roles = append(roles, role)
		}
		sort.Strings(roles)
		resp["roles"] = roles
	}
	c.JSON(http.StatusOK, resp)
}

// POST /api/site-settings/module-roles  {module, role, user_id|email}
func (s *Server) handleModuleRoleGrant(c *gin.Context) {
	var req struct {
		Module string `json:"module"`
		Role   string `json:"role"`
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "common.invalid_input")
		return
	}
	module := normalizeModuleName(req.Module)
	role := normalizeModuleRole(req.Role)
	if _, err := validateModuleRole(module, role); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的模块或角色"})
		return
	}

	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		email := strings.TrimSpace(req.Email)
		if email == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "需要 user_id 或 email"})
			return
		}
		user, err := s.getUserByEmail(email)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "用户查询失败"})
			return
		}
		if user == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
			return
		}
		userID = user.ID
	} else if user, err := s.getUserByID(userID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "用户查询失败"})
		return
	} else if user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
		return
	}

	adminID, _ := c.Get("user_id")
	adminIDStr, _ := adminID.(string)
	if err := s.grantModuleRole(module, userID, role, adminIDStr, "", time.Now()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "角色授予失败"})
		return
	}

	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "module_role.grant",
		TargetKind: "user",
		TargetID:   userID,
		Payload:    map[string]any{"module": module, "role": role},
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "角色已授予",
		"module":  module,
		"role":    role,
		"user_id": userID,
	})
}

// DELETE /api/site-settings/module-roles?module=opera&user_id=…
func (s *Server) handleModuleRoleRevoke(c *gin.Context) {
	module := normalizeModuleName(c.Query("module"))
	userID := strings.TrimSpace(c.Query("user_id"))
	if module == "" || userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 module 与 user_id"})
		return
	}
	removed, err := s.revokeModuleRole(module, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "角色撤销失败"})
		return
	}
	if !removed {
		c.JSON(http.StatusNotFound, gin.H{"error": "该用户在此模块没有角色"})
		return
	}

	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "module_role.revoke",
		TargetKind: "user",
		TargetID:   userID,
		Payload:    map[string]any{"module": module},
	})

	c.JSON(http.StatusOK, gin.H{"message": "角色已撤销"})
}

// POST /api/site-settings/invite-codes/disable  {code}
//
// Kills an unredeemed code. A code that was already redeemed keeps its
// row (the audit trail of who used it); revoking the role it produced
// is handleModuleRoleRevoke's job.
func (s *Server) handleInviteCodeDisable(c *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "需要 code"})
		return
	}
	disabled, err := s.disableInviteCode(req.Code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "邀请码停用失败"})
		return
	}
	if !disabled {
		c.JSON(http.StatusNotFound, gin.H{"error": "邀请码不存在或已停用"})
		return
	}

	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "invite_code.disable",
		TargetKind: "site_setting",
		TargetID:   "invite_codes",
		// Codes are secrets — audit the hint, never the code itself.
		Payload: map[string]any{"code_hint": inviteCodeHint(normalizeInviteCode(req.Code))},
	})

	c.JSON(http.StatusOK, gin.H{"message": "邀请码已停用"})
}

// inviteCodeHint reduces a code to head…tail for audit rows.
func inviteCodeHint(code string) string {
	switch {
	case len(code) <= 4:
		return code
	case len(code) <= 8:
		return code[:2] + "…" + code[len(code)-2:]
	default:
		return code[:4] + "…" + code[len(code)-2:]
	}
}
