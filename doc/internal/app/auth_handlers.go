package app

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleRegister(c *gin.Context) {
	var req struct {
		Username string `json:"username" binding:"required,min=2"`
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=6"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid input")
		return
	}

	existing, err := s.getUserByEmail(req.Email)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	if existing != nil {
		jsonError(c, http.StatusConflict, "email already registered")
		return
	}

	hashed, err := hashPassword(req.Password)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}

	user := &User{
		ID:        generateID()[:16],
		Username:  req.Username,
		Email:     req.Email,
		Password:  hashed,
		Role:      "user",
		CreatedAt: time.Now(),
	}
	if err := s.createUser(user); err != nil {
		if err == errEmailExists {
			jsonError(c, http.StatusConflict, "email already registered")
			return
		}
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}

	sessID, err := s.createSession(user)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	c.SetCookie(SessionCookieName, sessID, int(SessionDuration.Seconds()), "/", "", false, true)
	c.JSON(http.StatusCreated, gin.H{"user_id": user.ID, "username": user.Username})
}

func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid input")
		return
	}

	user, err := s.getUserByEmail(req.Email)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	if user == nil || !checkPassword(req.Password, user.Password) {
		jsonError(c, http.StatusUnauthorized, "invalid credentials")
		return
	}

	sessID, err := s.createSession(user)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	c.SetCookie(SessionCookieName, sessID, int(SessionDuration.Seconds()), "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"user_id": user.ID, "username": user.Username})
}

func (s *Server) handleLogout(c *gin.Context) {
	if sessID, err := c.Cookie(SessionCookieName); err == nil {
		_ = s.deleteSession(sessID)
	}
	c.SetCookie(SessionCookieName, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"message": "logged out"})
}

func (s *Server) handleMe(c *gin.Context) {
	userID, _ := c.Get("user_id")
	username, _ := c.Get("username")
	role, _ := c.Get("role")
	c.JSON(http.StatusOK, gin.H{
		"user_id":  userID,
		"username": username,
		"role":     role,
	})
}
