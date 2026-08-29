package dock

// meta_handlers.go — self-describing API surface (Track 1 P0 of the
// module-platform plan, doc/arch/module-platform-plan.md).
//
// GET /api/_routes returns the full gin route table so the API can be
// discovered by a query instead of grepping source. Admin-gated — it
// exposes the internal route surface, which isn't secret but isn't for
// tenants either.

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// apiCatalogJSON is the build-time API contract catalog produced by
// cmd/api-catalog-gen (run via `go generate ./internal/app/dock` or
// `make api-catalog`). It enriches the route list with request-body
// fields, query/path/header params, and auth per endpoint.
//
//go:embed api_catalog.gen.json
var apiCatalogJSON []byte

type apiRouteInfo struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Handler string `json:"handler"`
}

// handleAPIRoutes dumps router.Routes() as JSON. Optional ?prefix=
// filters by path prefix (e.g. ?prefix=/api/assets). Sorted by path
// then method for stable, scannable output.
func (s *Server) handleAPIRoutes(c *gin.Context) {
	prefix := strings.TrimSpace(c.Query("prefix"))
	routes := s.router.Routes()
	out := make([]apiRouteInfo, 0, len(routes))
	for _, r := range routes {
		if prefix != "" && !strings.HasPrefix(r.Path, prefix) {
			continue
		}
		out = append(out, apiRouteInfo{
			Method:  r.Method,
			Path:    r.Path,
			Handler: shortHandlerName(r.Handler),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	c.JSON(http.StatusOK, gin.H{"count": len(out), "routes": out})
}

// handleAPICatalog serves the build-time contract catalog (body fields +
// query/path/header params + auth per endpoint). Optional ?prefix=
// filters by path. This is the enriched companion to /api/_routes.
//
//go:generate go run ../../../cmd/api-catalog-gen -dock . -out api_catalog.gen.json -doc ../../../doc/api/catalog.json
func (s *Server) handleAPICatalog(c *gin.Context) {
	prefix := strings.TrimSpace(c.Query("prefix"))
	if prefix == "" {
		c.Data(http.StatusOK, "application/json; charset=utf-8", apiCatalogJSON)
		return
	}
	var cat struct {
		Count     int              `json:"count"`
		Endpoints []map[string]any `json:"endpoints"`
	}
	if err := json.Unmarshal(apiCatalogJSON, &cat); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "catalog unavailable"})
		return
	}
	filtered := make([]map[string]any, 0, len(cat.Endpoints))
	for _, e := range cat.Endpoints {
		if p, _ := e["path"].(string); strings.HasPrefix(p, prefix) {
			filtered = append(filtered, e)
		}
	}
	c.JSON(http.StatusOK, gin.H{"count": len(filtered), "endpoints": filtered})
}

// shortHandlerName trims gin's fully-qualified handler name
// ("github.com/networkextension/polar-dock/internal/app/dock.(*Server).handleX-fm")
// down to the method name ("(*Server).handleX") for readability.
func shortHandlerName(full string) string {
	full = strings.TrimSuffix(full, "-fm")
	if i := strings.LastIndex(full, "."); i >= 0 {
		// keep the receiver+method: find the second-to-last dot.
		if j := strings.LastIndex(full[:i], "."); j >= 0 {
			return full[j+1:]
		}
		return full[i+1:]
	}
	return full
}
