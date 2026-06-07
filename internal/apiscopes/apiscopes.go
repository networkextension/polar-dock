// Package apiscopes is the single source of truth for mapping a dock HTTP
// route (method + path + auth tier) to the coarse open-API-token scope that
// guards it. It has NO dependencies so both the catalog generator
// (cmd/api-catalog-gen) and the dock runtime (internal/app/dock) import it —
// guaranteeing the documented scope and the enforced scope never drift.
//
// Taxonomy (coarse domain:action):
//   - action = "read" for GET/HEAD, else "write".
//   - domain = the 2nd path segment of /api/<domain>/… .
//   - non-admin route       -> "<domain>:<action>"   e.g. "posts:read"
//   - admin route (auth==admin) -> "admin:<action>"  e.g. "admin:write"  (coarse:
//     admin routes are already gated by AdminMiddleware/IsSiteAdmin, so the scope
//     is a single secondary read/write toggle rather than per-resource)
//   - open/pre-auth route (auth=="") or non-/api path -> "" (no scope required)
//
// A token's granted scopes are checked against the route's required scope by
// AllowsScope. Empty grant = full access (backward-compat with P0 tokens); the
// "*" wildcard covers every NON-admin scope but never an admin one.
package apiscopes

import "strings"

// Wildcard is the convenience scope granting all non-admin scopes.
const Wildcard = "*"

// Action returns "read" for safe methods, "write" otherwise.
func Action(method string) string {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "HEAD":
		return "read"
	default:
		return "write"
	}
}

// Domain returns the 2nd path segment of /api/<domain>/… , or "" if the path
// is not under /api or has no domain segment.
func Domain(path string) string {
	p := strings.TrimSpace(path)
	if !strings.HasPrefix(p, "/api/") {
		return ""
	}
	rest := strings.TrimPrefix(p, "/api/")
	seg := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		seg = rest[:i]
	}
	return seg
}

// ScopeFor maps a route to its required scope. auth is the catalog's auth tier
// ("admin" for AdminMiddleware-guarded routes, "" for open/pre-auth routes,
// anything else for ordinary session routes).
func ScopeFor(method, path, auth string) string {
	domain := Domain(path)
	if domain == "" {
		return "" // non-/api or domain-less → not scope-gated
	}
	if auth == "" {
		return "" // open / pre-auth (e.g. /api/login, /api/public/*)
	}
	action := Action(method)
	if auth == "admin" {
		return "admin:" + action
	}
	return domain + ":" + action
}

// IsAdminScope reports whether a scope string denotes an admin-only route.
func IsAdminScope(scope string) bool {
	return scope == "admin" || strings.HasPrefix(scope, "admin:")
}

// AllowsScope reports whether a token holding `granted` may call a route
// requiring `required`.
//
//   - required == ""      → always allowed (un-scoped route).
//   - len(granted) == 0   → allowed (empty grant = full access, P0 compat).
//   - exact match present  → allowed.
//   - "*" present AND required is not an admin scope → allowed.
//   - otherwise            → denied.
func AllowsScope(granted []string, required string) bool {
	if required == "" {
		return true
	}
	if len(granted) == 0 {
		return true
	}
	wildcard := false
	for _, g := range granted {
		if g == required {
			return true
		}
		if g == Wildcard {
			wildcard = true
		}
	}
	return wildcard && !IsAdminScope(required)
}
