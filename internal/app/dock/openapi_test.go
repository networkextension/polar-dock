package dock

import "testing"

func TestGinPathToOpenAPI(t *testing.T) {
	cases := map[string]string{
		"/api/posts":             "/api/posts",
		"/api/posts/:id":         "/api/posts/{id}",
		"/api/rooms/:id/members": "/api/rooms/{id}/members",
		"/api/passkeys/:credentialId": "/api/passkeys/{credentialId}",
	}
	for in, want := range cases {
		if got := ginPathToOpenAPI(in); got != want {
			t.Errorf("ginPathToOpenAPI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildOpenAPISpec(t *testing.T) {
	s := &Server{}
	spec := s.buildOpenAPISpec()

	if spec["openapi"] != "3.0.3" {
		t.Fatalf("openapi version = %v, want 3.0.3", spec["openapi"])
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("paths empty")
	}
	// A known GET endpoint should be present with a scope-bearing security entry.
	posts, ok := paths["/api/posts"].(map[string]any)
	if !ok {
		t.Fatal("/api/posts missing from spec")
	}
	get, ok := posts["get"].(map[string]any)
	if !ok {
		t.Fatal("/api/posts GET missing")
	}
	if get["x-polar-scope"] != "posts:read" {
		t.Errorf("/api/posts GET scope = %v, want posts:read", get["x-polar-scope"])
	}
	if _, ok := get["security"]; !ok {
		t.Error("/api/posts GET missing security")
	}
	// No raw gin params should leak into path keys.
	for p := range paths {
		for i := 0; i < len(p); i++ {
			if p[i] == ':' {
				t.Errorf("path %q still contains a gin :param", p)
				break
			}
		}
	}
}
