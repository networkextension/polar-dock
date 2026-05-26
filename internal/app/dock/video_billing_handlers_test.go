package dock

// Pure-logic tests for video_billing_handlers helpers. DB + HTTP
// round-trip is covered by manual smoke (doc/llm/video-billing.md
// §Verification).

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
)

func gctx(q url.Values) *gin.Context {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	u := &url.URL{Path: "/", RawQuery: q.Encode()}
	c.Request, _ = http.NewRequest(http.MethodGet, u.String(), nil)
	return c
}

func TestParseSinceUntil_DefaultsMonthToNow(t *testing.T) {
	c := gctx(url.Values{})
	since, until, err := parseSinceUntil(c)
	if err != nil {
		t.Fatal(err)
	}
	if since.Day() != 1 || since.Hour() != 0 {
		t.Fatalf("default since should be start of month UTC, got %v", since)
	}
	if !until.After(since) {
		t.Fatalf("until must be after since, got since=%v until=%v", since, until)
	}
}

func TestParseSinceUntil_AcceptsRFC3339AndDateOnly(t *testing.T) {
	c := gctx(url.Values{"since": []string{"2026-05-01"}, "until": []string{"2026-05-31T23:59:59Z"}})
	since, until, err := parseSinceUntil(c)
	if err != nil {
		t.Fatal(err)
	}
	if since.Month().String() != "May" || since.Day() != 1 {
		t.Fatalf("since YYYY-MM-DD not parsed: %v", since)
	}
	if until.Hour() != 23 || until.Minute() != 59 {
		t.Fatalf("until RFC3339 not parsed: %v", until)
	}
}

func TestParseSinceUntil_RejectsGarbage(t *testing.T) {
	c := gctx(url.Values{"since": []string{"nope"}})
	if _, _, err := parseSinceUntil(c); err == nil {
		t.Fatal("expected error for garbage since")
	}
}

func TestParseSinceUntil_RejectsInvertedRange(t *testing.T) {
	c := gctx(url.Values{
		"since": []string{"2026-05-31"},
		"until": []string{"2026-05-01"},
	})
	if _, _, err := parseSinceUntil(c); err == nil {
		t.Fatal("expected error when until <= since")
	}
}
