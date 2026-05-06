package dock

// Team-scoped authorization middleware. T5.
//
// RequireTeamRole resolves the caller's role on the team identified
// by a URL path parameter, enforces a minimum role, and stores the
// resolved values in the gin context for downstream handlers to read
// without a second DB lookup:
//
//   c.GetString("team_id")   — the validated team id
//   c.GetString("team_role") — the caller's role on that team
//
// Wire on routes like:
//
//   api.PUT("/teams/:id",
//       s.AuthMiddleware(),
//       s.RequireTeamRole("id", TeamRoleAdmin),
//       s.handleTeamUpdate)
//
// The middleware assumes AuthMiddleware has already set user_id; if
// it hasn't, the request is rejected with 500 (defensive — this
// shouldn't happen in practice since RequireTeamRole is always
// chained after AuthMiddleware).

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RequireTeamRole returns a gin middleware enforcing minRole on the
// team identified by paramName in the URL path. paramName defaults
// to "id" if empty.
func (s *Server) RequireTeamRole(paramName, minRole string) gin.HandlerFunc {
	if paramName == "" {
		paramName = "id"
	}
	return func(c *gin.Context) {
		userIDValue, _ := c.Get("user_id")
		userID, _ := userIDValue.(string)
		if userID == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			c.Abort()
			return
		}
		teamID := strings.TrimSpace(c.Param(paramName))
		if teamID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
			c.Abort()
			return
		}
		member, err := s.getTeamMember(teamID, userID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			c.Abort()
			return
		}
		// 404 (rather than 403) when the user is not a member: don't
		// leak the team's existence to non-members.
		if member == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "工作区不存在或无权访问"})
			c.Abort()
			return
		}
		if !teamRoleAllows(member.Role, minRole) {
			c.JSON(http.StatusForbidden, gin.H{"error": "权限不足"})
			c.Abort()
			return
		}
		c.Set("team_id", teamID)
		c.Set("team_role", member.Role)
		c.Next()
	}
}

// requireTeamRoleFromCtx is the read-side counterpart for handlers
// that ran behind RequireTeamRole. Returns (teamID, role, true) on
// success. If the middleware didn't run, writes a 500 to c so the
// route registration mistake is loud.
func requireTeamRoleFromCtx(c *gin.Context) (string, string, bool) {
	teamID := c.GetString("team_id")
	role := c.GetString("team_role")
	if teamID == "" || role == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return "", "", false
	}
	return teamID, role, true
}
