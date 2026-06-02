package dock

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolvePluginUpdateViaRelease(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path + "?" + r.URL.RawQuery
		switch r.URL.Query().Get("module") {
		case "buildings":
			_, _ = w.Write([]byte(`{"version":"0.0.3","sha256":"deadbeef","dl_url":"https://prov.example/v1/blob/deadbeef?token=1:2"}`))
		case "missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	s := &Server{}

	t.Setenv("POLAR_RELEASE_BASE", srv.URL)

	// hit: newer version → directive with the signed provider URL.
	dir, err := s.resolvePluginUpdateViaRelease("buildings", "0.0.2", "darwin", "arm64")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if dir == nil || dir.Version != "0.0.3" || dir.SHA256 != "deadbeef" {
		t.Fatalf("unexpected directive: %+v", dir)
	}
	if dir.URL != "https://prov.example/v1/blob/deadbeef?token=1:2" {
		t.Errorf("URL should be the absolute provider URL, got %q", dir.URL)
	}
	if seenPath != "/release/resolve?module=buildings&channel=stable&platform=darwin-arm64" {
		t.Errorf("platform must be <os>-<arch>; got query %q", seenPath)
	}

	// already current → nil.
	if dir, _ := s.resolvePluginUpdateViaRelease("buildings", "0.0.3", "darwin", "arm64"); dir != nil {
		t.Error("same version should yield no directive")
	}

	// 404 (no release) → nil,nil (fall through to legacy).
	if dir, err := s.resolvePluginUpdateViaRelease("missing", "0.0.1", "darwin", "arm64"); dir != nil || err != nil {
		t.Errorf("404 should be (nil,nil); got dir=%v err=%v", dir, err)
	}

	// non-2xx → error (caller logs + falls back).
	if _, err := s.resolvePluginUpdateViaRelease("boom", "0.0.1", "darwin", "arm64"); err == nil {
		t.Error("500 should return an error so the caller falls back")
	}
}

func TestResolvePluginUpdateViaRelease_FeatureOff(t *testing.T) {
	t.Setenv("POLAR_RELEASE_BASE", "")
	s := &Server{}
	dir, err := s.resolvePluginUpdateViaRelease("buildings", "0.0.1", "darwin", "arm64")
	if dir != nil || err != nil {
		t.Errorf("feature off should be (nil,nil); got dir=%v err=%v", dir, err)
	}
}
