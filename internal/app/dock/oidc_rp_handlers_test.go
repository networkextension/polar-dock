package dock

import "testing"

func TestSafeReturnTo(t *testing.T) {
	cases := map[string]string{
		"/teams.html":          "/teams.html",          // local path → kept
		"/a/b?x=1":             "/a/b?x=1",             // local with query → kept
		"":                     "/dashboard.html",      // empty → default
		"https://evil.com":     "/dashboard.html",      // absolute URL → blocked
		"//evil.com":           "/dashboard.html",      // protocol-relative → blocked
		"/\\evil.com":          "/dashboard.html",      // backslash trick → blocked
		"javascript:alert(1)":  "/dashboard.html",      // scheme → blocked (no leading /)
	}
	for in, want := range cases {
		if got := safeReturnTo(in); got != want {
			t.Errorf("safeReturnTo(%q)=%q want %q", in, got, want)
		}
	}
}
