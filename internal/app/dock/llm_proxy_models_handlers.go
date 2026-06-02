package dock

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// GET /api/proxy/v1/models — OpenAI-compatible model list for the
// calling proxy token. Returns the models this token can actually
// reach so agents (and any OpenAI client) can discover them instead
// of hard-coding names:
//
//   - every alias_model in the token's llm_proxy_routes, and
//   - the token's default_llm_config_id model, advertised under the
//     config name, when a default is set.
//
// Auth is the same Bearer proxy token as the chat endpoint. The
// assembled list is cached per token_id for a short TTL — gating is
// by token, not user, so the cache is safe to share across requests
// for the same token.
type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type openAIModelList struct {
	Object string        `json:"object"`
	Data   []openAIModel `json:"data"`
}

type proxyModelsCacheEntry struct {
	list   openAIModelList
	expiry time.Time
}

var (
	proxyModelsCacheMu sync.Mutex
	proxyModelsCache   = map[int64]proxyModelsCacheEntry{}
)

const proxyModelsCacheTTL = 30 * time.Second

func (s *Server) handleLLMProxyModels(c *gin.Context) {
	authz := strings.TrimSpace(c.GetHeader("Authorization"))
	if !strings.HasPrefix(authz, "Bearer ") {
		openAIError(c, http.StatusUnauthorized, "invalid_api_key", "missing bearer token")
		return
	}
	plaintext := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	if plaintext == "" {
		openAIError(c, http.StatusUnauthorized, "invalid_api_key", "empty bearer token")
		return
	}
	tok, err := s.lookupLLMProxyTokenByHash(hashLLMProxyToken(plaintext))
	if err != nil {
		openAIError(c, http.StatusInternalServerError, "internal_error", "token lookup failed")
		return
	}
	if tok == nil {
		openAIError(c, http.StatusUnauthorized, "invalid_api_key", "token not recognized or revoked")
		return
	}

	now := time.Now()
	proxyModelsCacheMu.Lock()
	if ent, ok := proxyModelsCache[tok.ID]; ok && now.Before(ent.expiry) {
		list := ent.list
		proxyModelsCacheMu.Unlock()
		c.JSON(http.StatusOK, list)
		return
	}
	proxyModelsCacheMu.Unlock()

	list := s.assembleProxyModels(tok)

	proxyModelsCacheMu.Lock()
	proxyModelsCache[tok.ID] = proxyModelsCacheEntry{list: list, expiry: now.Add(proxyModelsCacheTTL)}
	proxyModelsCacheMu.Unlock()

	c.JSON(http.StatusOK, list)
}

// assembleProxyModels builds the model list for a token: alias routes
// first (the names the client should send), then the default config
// (advertised under its name) if not already present. Order is
// stable: routes by sort_order, default last.
func (s *Server) assembleProxyModels(tok *LLMProxyToken) openAIModelList {
	list := openAIModelList{Object: "list", Data: []openAIModel{}}
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		list.Data = append(list.Data, openAIModel{ID: id, Object: "model", OwnedBy: "polar"})
	}

	if routes, err := s.listLLMProxyRoutes(tok.ID); err == nil {
		for _, r := range routes {
			add(r.AliasModel)
		}
	}

	if tok.DefaultLLMConfigID != nil && *tok.DefaultLLMConfigID > 0 {
		if cfg, _, err := s.getLLMConfigForOwner(tok.WorkspaceID, *tok.DefaultLLMConfigID); err == nil && cfg != nil {
			// Advertise the default under its config name so a client
			// with no alias configured still has something to send.
			name := strings.TrimSpace(cfg.Name)
			if name == "" {
				name = "default-" + strconv.FormatInt(cfg.ID, 10)
			}
			add(name)
		}
	}

	return list
}
