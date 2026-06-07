package dock

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func isAPIRequest(c *gin.Context) bool {
	return strings.HasPrefix(c.Request.URL.Path, "/api/")
}

// clearAuthCookies expires both auth cookies so the browser drops them
// on the next response. Used on logout and on any unauthenticated API
// hit that carries a stale cookie.
//
// Domain attribute matches setAuthCookies: when s.cookieDomain is set
// (e.g. `.4950.store`), the browser only honors the Set-Cookie for
// expiry if the Domain matches what was originally set. Without
// matching the Domain on clear, the stale cookie sticks around.
func (s *Server) clearAuthCookies(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     AccessCookieName,
		Value:    "",
		Path:     "/",
		Domain:   s.cookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Expires:  time.Unix(0, 0),
	})
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    "",
		Path:     RefreshCookiePath,
		Domain:   s.cookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Expires:  time.Unix(0, 0),
	})
}

// setAuthCookies writes a fresh access + refresh pair onto the
// response. Keep in one place so cookie attributes stay consistent
// across login, register, passkey, and refresh handlers.
//
// Domain is read from s.cookieDomain. When empty (default), the
// browser scopes the cookie to the request host — `zen.4950.store`
// only. When set to `.4950.store` (leading dot), the cookie is sent
// for every `*.4950.store` subdomain so SSO works across plugin UIs
// (project.4950.store, expense.4950.store, …).
func (s *Server) setAuthCookies(c *gin.Context, accessToken, refreshToken string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     AccessCookieName,
		Value:    accessToken,
		Path:     "/",
		Domain:   s.cookieDomain,
		MaxAge:   int(AccessTokenTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     RefreshCookieName,
		Value:    refreshToken,
		Path:     RefreshCookiePath,
		Domain:   s.cookieDomain,
		MaxAge:   int(RefreshTokenTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// extractAccessToken returns the access token carried by the request.
// Bearer header wins over cookie so third-party clients (future open
// platform) can skip the cookie round-trip.
func extractAccessToken(c *gin.Context) string {
	if header := c.GetHeader("Authorization"); header != "" {
		if parts := strings.SplitN(header, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if tok := strings.TrimSpace(parts[1]); tok != "" {
				return tok
			}
		}
	}
	if tok, err := c.Cookie(AccessCookieName); err == nil {
		return tok
	}
	return ""
}

func extractBearerToken(c *gin.Context) string {
	if header := c.GetHeader("Authorization"); header != "" {
		if parts := strings.SplitN(header, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if tok := strings.TrimSpace(parts[1]); tok != "" {
				return tok
			}
		}
	}
	return ""
}

// extractRefreshToken reads the refresh token from cookie, request
// body, or Authorization header (in that order). The body fallback
// lets native clients carry the token outside of cookies.
func extractRefreshToken(c *gin.Context) string {
	if tok, err := c.Cookie(RefreshCookieName); err == nil && tok != "" {
		return tok
	}
	if header := c.GetHeader("Authorization"); header != "" {
		if parts := strings.SplitN(header, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if tok := strings.TrimSpace(parts[1]); tok != "" {
				return tok
			}
		}
	}
	return ""
}

func (s *Server) AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Open API token path (flag-gated): Authorization: Bearer polar_…
		// resolves to a workspace-pinned, owner-attributed context — the
		// public-API equivalent of a session. P0 grants full access (scopes
		// enforced in P1). When the flag is off, a polar_ bearer just falls
		// through and is rejected as an invalid access token (no change).
		if s.openAPITokens {
			if bt := extractBearerToken(c); strings.HasPrefix(bt, apiTokenPrefix) {
				if tok := s.resolveAPIToken(bt); tok != nil {
					c.Set("user_id", tok.OwnerUserID)
					c.Set("role", "api_token")
					c.Set("workspace_id", tok.WorkspaceID)
					c.Set("auth_kind", "api_token")
					c.Set("api_token_id", tok.ID)
					go s.touchAPIToken(tok.ID)
					c.Next()
					return
				}
				jsonError(c, http.StatusUnauthorized, "auth.unauthorized")
				c.Abort()
				return
			}
		}
		token := extractAccessToken(c)
		if token == "" {
			if isAPIRequest(c) {
				jsonError(c, http.StatusUnauthorized, "auth.unauthorized")
			} else {
				c.Redirect(http.StatusFound, "/login")
			}
			c.Abort()
			return
		}

		session := s.getAccessSession(token)
		if session == nil {
			s.clearAuthCookies(c)
			if isAPIRequest(c) {
				jsonError(c, http.StatusUnauthorized, "auth.unauthorized")
			} else {
				c.Redirect(http.StatusFound, "/login")
			}
			c.Abort()
			return
		}

		c.Set("user_id", session.UserID)
		c.Set("username", session.Username)
		if session.Role == "" {
			c.Set("role", "user")
		} else {
			c.Set("role", session.Role)
		}
		c.Set("session", session)

		// T4: resolve the active workspace. If X-Workspace-Id is
		// supplied and the user is a member, use it; otherwise fall
		// back to the user's personal team so handlers always have
		// a workspace_id available. Any failure is logged and the
		// request continues with workspace_id unset — handlers that
		// haven't migrated to the new ownership model still work.
		s.resolveActiveWorkspace(c, session.UserID)

		c.Next()
	}
}

// resolveActiveWorkspace inspects the X-Workspace-Id header. If
// present and the caller is a member of that team, set workspace_id
// + workspace_role in the gin context. Otherwise fall back to the
// user's personal team (auto-creating it if somehow missing). Soft
// failure: if anything goes wrong, leave workspace_id unset.
func (s *Server) resolveActiveWorkspace(c *gin.Context, userID string) {
	if userID == "" {
		return
	}
	requested := strings.TrimSpace(c.GetHeader("X-Workspace-Id"))
	if requested != "" {
		member, err := s.getTeamMember(requested, userID)
		if err == nil && member != nil {
			// R2-3 (flag-gated): refuse to make a non-active team the
			// active workspace, so resources can't be operated inside a
			// pending/rejected/suspended team. Soft gate — falls through
			// to the always-active home team rather than 403-ing, so the
			// user is never locked out. Stamps a marker the UI can read.
			if s.enforceTeamStatus {
				team, terr := s.getTeam(requested)
				if terr == nil && team != nil && team.Status != TeamStatusActive {
					c.Set("workspace_blocked_id", requested)
					c.Set("workspace_blocked_status", team.Status)
					// fall through to home team below
				} else {
					c.Set("workspace_id", requested)
					c.Set("workspace_role", member.Role)
					return
				}
			} else {
				c.Set("workspace_id", requested)
				c.Set("workspace_role", member.Role)
				return
			}
		}
		// Header present but invalid — fall through to personal team.
		// This is what should happen if the user gets removed from a
		// team but their browser still has the old workspace cached.
	}
	personal, err := s.ensurePersonalTeam(userID)
	if err != nil || personal == nil {
		return
	}
	c.Set("workspace_id", personal.ID)
	c.Set("workspace_role", TeamRoleOwner)
}

// AdminMiddleware — P1 of doc/arch/permission-model.md.
//
// The predicate switched from `users.role == 'admin'` to "is this user
// a member of the root team with role IN ('owner','admin')" — i.e.
// IsSiteAdmin(user_id). The two definitions agree today because the
// bootstrap rule (P0) auto-promoted the sole non-system admin into
// the root team as owner; future site admins are managed through
// team_members, never users.role.
//
// Rollback: set POLAR_LEGACY_ADMIN_ROLE=1 to revert to the old
// users.role check. Read once at process start so a hot toggle needs
// a restart — fine for a behavior cutover that should be rare.
//
// On success, sets `gate_via` on the context so LogAdminAction can
// stamp the audit row's actor_role correctly: "site_admin" when
// IsSiteAdmin passed, "users.role=admin" when the legacy fallback
// passed.
func (s *Server) AdminMiddleware() gin.HandlerFunc {
	legacy := strings.TrimSpace(os.Getenv("POLAR_LEGACY_ADMIN_ROLE")) == "1"
	return func(c *gin.Context) {
		if legacy {
			role, _ := c.Get("role")
			if roleStr, ok := role.(string); !ok || roleStr != "admin" {
				jsonError(c, http.StatusForbidden, "auth.forbidden")
				c.Abort()
				return
			}
			c.Set("gate_via", "users.role=admin")
			c.Next()
			return
		}
		userIDAny, _ := c.Get("user_id")
		userID, _ := userIDAny.(string)
		if userID == "" {
			jsonError(c, http.StatusForbidden, "auth.forbidden")
			c.Abort()
			return
		}
		ok, err := s.IsSiteAdmin(userID)
		if err != nil {
			log.Printf("perm: AdminMiddleware IsSiteAdmin(%s) lookup failed: %v — denying", userID, err)
			jsonError(c, http.StatusForbidden, "auth.forbidden")
			c.Abort()
			return
		}
		if !ok {
			jsonError(c, http.StatusForbidden, "auth.forbidden")
			c.Abort()
			return
		}
		c.Set("gate_via", "site_admin")
		c.Next()
	}
}

func (s *Server) GuestMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if token := extractAccessToken(c); token != "" {
			if session := s.getAccessSession(token); session != nil {
				if isAPIRequest(c) {
					jsonError(c, http.StatusConflict, "auth.already_logged_in")
				} else {
					c.Redirect(http.StatusFound, "/dashboard")
				}
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

// PolarAgentAuthMiddleware authenticates polar-agent CLI HTTP
// callers via an `Authorization: Bearer <agent_token>` header.
// Distinct from AgentAuthMiddleware (which auths Latch service-node
// agents). On success, sets agent_token_id + agent_user_id on the
// gin context so handlers can derive ownership / workspace scope.
func (s *Server) PolarAgentAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			c.Abort()
			return
		}
		raw := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		tokenID, userID, err := s.resolveAgentToken(raw)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
			c.Abort()
			return
		}
		if tokenID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "agent token invalid or revoked"})
			c.Abort()
			return
		}
		c.Set("agent_token_id", tokenID)
		c.Set("agent_user_id", userID)
		c.Next()
	}
}

// AgentAuthMiddleware was the latch-specific bearer-token auth used
// by /api/latch/agent/{register,heartbeat}. Latch moved to
// github.com/networkextension/polar-latch; this middleware now has no
// callers. polar-agent uses PolarAgentAuthMiddleware (above), which
// remains in dock.
