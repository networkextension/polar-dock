package app

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID, err := c.Cookie(SessionCookieName)
		if err != nil {
			jsonError(c, http.StatusUnauthorized, "unauthorized")
			return
		}
		sess := s.getSession(sessionID)
		if sess == nil {
			c.SetCookie(SessionCookieName, "", -1, "/", "", false, true)
			jsonError(c, http.StatusUnauthorized, "unauthorized")
			return
		}
		c.Set("user_id", sess.UserID)
		c.Set("username", sess.Username)
		c.Set("role", sess.Role)
		c.Next()
	}
}
