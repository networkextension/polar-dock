package dock

// resolveHostSkillAdmin is the shared authz prelude for the dock-side
// host_skill endpoints that survive after H9 (the shell + VNC WS open /
// sessions / kick paths). It confirms the host belongs to the active
// workspace, the host_skill belongs to the host, and the caller is
// workspace admin. Returns the host_skill_id on success; the gin
// context is already terminated on failure.
//
// The function previously lived in host_skill_credentials_handlers.go
// next to the credentials vault REST handlers; that file moved to
// polar-hosts in H9, but the agent-dispatch shell/VNC handlers still
// need this check, so it stays in dock.

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) resolveHostSkillAdmin(c *gin.Context) (int64, bool) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return 0, false
	}
	if !teamRoleAllows(workspaceRole(c), TeamRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要管理员权限"})
		return 0, false
	}
	hostID := strings.TrimSpace(c.Param("id"))
	skillRaw := strings.TrimSpace(c.Param("skillId"))
	if hostID == "" || skillRaw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 host / skill id"})
		return 0, false
	}
	skillID, err := strconv.ParseInt(skillRaw, 10, 64)
	if err != nil || skillID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 skill id"})
		return 0, false
	}
	host, err := s.getHostByID(hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return 0, false
	}
	if host == nil || host.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "host 不存在或不在当前工作区"})
		return 0, false
	}
	var owningHost string
	err = s.db.QueryRow(`SELECT host_id FROM host_skills WHERE id = $1`, skillID).Scan(&owningHost)
	if errors.Is(err, sql.ErrNoRows) || owningHost != hostID {
		c.JSON(http.StatusNotFound, gin.H{"error": "skill 不存在或不属于该 host"})
		return 0, false
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return 0, false
	}
	return skillID, true
}
