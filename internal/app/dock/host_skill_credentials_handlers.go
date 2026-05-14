package dock

// HTTP handlers for the host_skill credentials vault (P1c.4):
//
//   GET    /api/hosts/:id/skills/:skillId/credentials
//   PUT    /api/hosts/:id/skills/:skillId/credentials/:key   body: {"value":"..."}
//   DELETE /api/hosts/:id/skills/:skillId/credentials/:key
//
// All three require TeamRoleAdmin on the active workspace.
// GET never returns raw plaintext — only the masked display string +
// the `encrypted` bit so the UI can badge unencrypted rows.

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// resolveHostSkill is the common authz prelude: confirms the host
// belongs to the active workspace, the host_skill belongs to the
// host, and the caller is workspace admin. Returns the host_skill_id
// on success; the gin context is already terminated on failure.
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
	// Verify the skill row belongs to this host.
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

func (s *Server) handleHostSkillCredentialsList(c *gin.Context) {
	skillID, ok := s.resolveHostSkillAdmin(c)
	if !ok {
		return
	}
	creds, err := s.listHostSkillCredentials(skillID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"credentials":       creds,
		"encryption_active": len(s.polarCredentialKey) == iosdistResourceKeyBytes,
	})
}

func (s *Server) handleHostSkillCredentialsPut(c *gin.Context) {
	skillID, ok := s.resolveHostSkillAdmin(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key 不能为空"})
		return
	}
	var body struct {
		Value string `json:"value" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if strings.TrimSpace(body.Value) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "value 不能为空"})
		return
	}
	cred, err := s.upsertHostSkillCredential(skillID, key, body.Value, userID, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"credential": cred})
}

func (s *Server) handleHostSkillCredentialsDelete(c *gin.Context) {
	skillID, ok := s.resolveHostSkillAdmin(c)
	if !ok {
		return
	}
	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key 不能为空"})
		return
	}
	if err := s.deleteHostSkillCredential(skillID, key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
