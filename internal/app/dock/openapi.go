package dock

// openapi.go — runtime OpenAPI 3 export (P4). Builds a spec from the embedded
// api_catalog.gen.json (the same scope-annotated catalog the middleware enforces
// and the /api-reference.html page renders) so the doc is always current with
// the code. Served at GET /api/openapi.json. Feeds external developers + the
// mobile-SDK codegen.

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

type openAPIField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

type openAPIEndpoint struct {
	Method  string         `json:"method"`
	Path    string         `json:"path"`
	Handler string         `json:"handler"`
	Auth    string         `json:"auth"`
	Scope   string         `json:"scope"`
	Body    []openAPIField `json:"body"`
	Query   []openAPIField `json:"query"`
	PathP   []openAPIField `json:"path_params"`
	Headers []openAPIField `json:"headers"`
}

var (
	openAPIOnce sync.Once
	openAPISpec map[string]any
)

// ginPathToOpenAPI converts gin's `/api/x/:id` to OpenAPI's `/api/x/{id}`.
func ginPathToOpenAPI(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, ":") {
			segs[i] = "{" + s[1:] + "}"
		} else if strings.HasPrefix(s, "*") {
			segs[i] = "{" + s[1:] + "}"
		}
	}
	return strings.Join(segs, "/")
}

func openAPIType(t string) string {
	switch t {
	case "int", "int64", "integer":
		return "integer"
	case "bool", "boolean":
		return "boolean"
	case "float", "float64", "number":
		return "number"
	default:
		return "string"
	}
}

func domainOf(path string) string {
	if !strings.HasPrefix(path, "/api/") {
		return "other"
	}
	rest := strings.TrimPrefix(path, "/api/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "other"
	}
	return rest
}

func (s *Server) buildOpenAPISpec() map[string]any {
	var doc struct {
		Endpoints []openAPIEndpoint `json:"endpoints"`
	}
	_ = json.Unmarshal(apiCatalogJSON, &doc)

	paths := map[string]any{}
	for _, e := range doc.Endpoints {
		if !strings.HasPrefix(e.Path, "/api/") {
			continue
		}
		oapiPath := ginPathToOpenAPI(e.Path)
		item, _ := paths[oapiPath].(map[string]any)
		if item == nil {
			item = map[string]any{}
		}

		params := make([]map[string]any, 0)
		for _, q := range e.Query {
			params = append(params, map[string]any{
				"name": q.Name, "in": "query", "required": q.Required,
				"schema": map[string]any{"type": openAPIType(q.Type)},
			})
		}
		for _, p := range e.PathP {
			params = append(params, map[string]any{
				"name": p.Name, "in": "path", "required": true,
				"schema": map[string]any{"type": openAPIType(p.Type)},
			})
		}
		for _, h := range e.Headers {
			params = append(params, map[string]any{
				"name": h.Name, "in": "header", "required": h.Required,
				"schema": map[string]any{"type": openAPIType(h.Type)},
			})
		}

		op := map[string]any{
			"operationId": e.Handler,
			"tags":        []string{domainOf(e.Path)},
			"responses":   map[string]any{"200": map[string]any{"description": "OK"}},
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if len(e.Body) > 0 {
			props := map[string]any{}
			required := make([]string, 0)
			for _, b := range e.Body {
				props[b.Name] = map[string]any{"type": openAPIType(b.Type)}
				if b.Required {
					required = append(required, b.Name)
				}
			}
			schema := map[string]any{"type": "object", "properties": props}
			if len(required) > 0 {
				schema["required"] = required
			}
			op["requestBody"] = map[string]any{
				"content": map[string]any{"application/json": map[string]any{"schema": schema}},
			}
		}
		if e.Scope != "" {
			op["security"] = []map[string]any{{"PolarToken": []string{e.Scope}}}
			op["x-polar-scope"] = e.Scope
		}
		if e.Auth != "" {
			op["x-polar-auth"] = e.Auth
		}
		item[strings.ToLower(e.Method)] = op
		paths[oapiPath] = item
	}

	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Polar API",
			"description": "Polar dock public API. Authenticate with a polar_ open-API token (Bearer); each operation lists its required scope under x-polar-scope.",
			"version":     openAPIVersion(),
		},
		"servers": []map[string]any{{"url": s.navCoreBase()}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"PolarToken": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "polar_…",
					"description":  "Open API token issued at /api-tokens.html",
				},
			},
		},
		"paths": paths,
	}
}

// handleOpenAPISpec serves the generated OpenAPI 3 document. AuthMiddleware-gated
// (any member); the reference page lives in the authed UI.
func (s *Server) handleOpenAPISpec(c *gin.Context) {
	openAPIOnce.Do(func() { openAPISpec = s.buildOpenAPISpec() })
	c.JSON(http.StatusOK, openAPISpec)
}

// openAPIVersion is the spec's info.version — the dock build's git sha, or
// "dev" when unset.
func openAPIVersion() string {
	if strings.TrimSpace(buildGitSHA) != "" {
		return buildGitSHA
	}
	return "dev"
}
