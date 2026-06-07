package dock

// api_scopes.go — runtime side of open-API-token scopes (P1).
//
// The required scope for each /api/* route is computed at build time by
// cmd/api-catalog-gen (via internal/apiscopes) and baked into the embedded
// api_catalog.gen.json. Here we load that catalog once into:
//   - routeScopes:  "METHOD path" -> required scope (e.g. "GET /api/posts" -> "posts:read")
//   - allScopeList: the distinct scopes, grouped by domain, for the UI picker + validation
//
// Consuming the generated catalog (rather than recomputing) guarantees the
// enforced scope and the documented scope are identical. AuthMiddleware uses
// requiredScope + apiscopes.AllowsScope to gate api_token requests.

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"gin-auth-app/internal/apiscopes"
)

type catalogEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"`
	Scope  string `json:"scope"`
}

var (
	scopeOnce    sync.Once
	routeScopes  map[string]string // "METHOD path" -> scope
	allScopeSet  map[string]bool   // every known scope (for validation)
	scopeGroups  []scopeGroup      // grouped for the picker UI
)

// scopeGroup is one domain's read/write scopes for the create-token picker.
type scopeGroup struct {
	Domain string   `json:"domain"`
	Admin  bool     `json:"admin"`
	Scopes []string `json:"scopes"`
}

func loadScopes() {
	routeScopes = make(map[string]string)
	allScopeSet = make(map[string]bool)
	var doc struct {
		Endpoints []catalogEndpoint `json:"endpoints"`
	}
	_ = json.Unmarshal(apiCatalogJSON, &doc)
	byDomain := map[string]map[string]bool{} // domain -> set(scope)
	for _, e := range doc.Endpoints {
		key := strings.ToUpper(e.Method) + " " + e.Path
		routeScopes[key] = e.Scope
		if e.Scope == "" {
			continue
		}
		allScopeSet[e.Scope] = true
		dom := scopeDomainLabel(e.Scope)
		if byDomain[dom] == nil {
			byDomain[dom] = map[string]bool{}
		}
		byDomain[dom][e.Scope] = true
	}
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, d := range domains {
		scopes := make([]string, 0, len(byDomain[d]))
		for s := range byDomain[d] {
			scopes = append(scopes, s)
		}
		sort.Strings(scopes)
		scopeGroups = append(scopeGroups, scopeGroup{
			Domain: d,
			Admin:  apiscopes.IsAdminScope(scopes[0]),
			Scopes: scopes,
		})
	}
}

// scopeDomainLabel groups a scope for the picker: "posts:read" -> "posts",
// "admin:users:write" -> "admin:users".
func scopeDomainLabel(scope string) string {
	if i := strings.LastIndexByte(scope, ':'); i >= 0 {
		return scope[:i]
	}
	return scope
}

// requiredScope returns the scope guarding the given matched route, or "" when
// the route is un-scoped (open/pre-auth) or unknown.
func (s *Server) requiredScope(method, fullPath string) string {
	scopeOnce.Do(loadScopes)
	return routeScopes[strings.ToUpper(method)+" "+fullPath]
}

// knownScope reports whether scope is a real, grantable scope (for create
// validation). The "*" wildcard is always accepted.
func knownScope(scope string) bool {
	scopeOnce.Do(loadScopes)
	return scope == apiscopes.Wildcard || allScopeSet[scope]
}

// apiScopeCatalog returns the grouped scope list for the create-token UI.
func apiScopeCatalog() []scopeGroup {
	scopeOnce.Do(loadScopes)
	return scopeGroups
}
