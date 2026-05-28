package dock

import "testing"

func TestAssetBlobPath_NamespacesByWorkspace(t *testing.T) {
	cases := []struct {
		name        string
		uploadDir   string
		workspaceID *string
		sha         string
		want        string
	}{
		{
			name:      "tenant blob nests under workspace + sha[:2]",
			uploadDir: "/data",
			workspaceID: func() *string {
				s := "t_root"
				return &s
			}(),
			sha:  "abcdef123",
			want: "/data/assets/t_root/ab/abcdef123",
		},
		{
			name:        "platform blob uses _platform segment",
			uploadDir:   "/data",
			workspaceID: nil,
			sha:         "deadbeef",
			want:        "/data/assets/_platform/de/deadbeef",
		},
		{
			name:        "short sha falls back to _short bucket",
			uploadDir:   "/data",
			workspaceID: nil,
			sha:         "x",
			want:        "/data/assets/_platform/_short/x",
		},
		{
			name:      "empty pointer treated as platform",
			uploadDir: "/data",
			workspaceID: func() *string {
				s := ""
				return &s
			}(),
			sha:  "abcdef",
			want: "/data/assets/_platform/ab/abcdef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := assetBlobPath(tc.uploadDir, tc.workspaceID, tc.sha)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestCanCallerReadAsset(t *testing.T) {
	mkAsset := func(ws *string, vis string) *Asset {
		return &Asset{WorkspaceID: ws, Visibility: vis}
	}
	ws := func(s string) *string { return &s }

	cases := []struct {
		name    string
		asset   *Asset
		caller  string
		wantOK  bool
		comment string
	}{
		{"tenant reading own private", mkAsset(ws("t_root"), "private"), "t_root", true, ""},
		{"tenant reading other tenant", mkAsset(ws("t_root"), "workspace"), "t_other", false, "no cross-tenant read"},
		{"tenant reading public platform", mkAsset(nil, "public"), "t_root", true, ""},
		{"tenant reading private platform", mkAsset(nil, "private"), "t_root", false, "needs admin gate (P1b)"},
		{"tenant reading link platform", mkAsset(nil, "link"), "t_root", false, "share-link gate (P1c)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canCallerReadAsset(tc.asset, tc.caller); got != tc.wantOK {
				t.Errorf("got %v want %v (%s)", got, tc.wantOK, tc.comment)
			}
		})
	}
}

func TestNullStringOrPtr(t *testing.T) {
	nullCase := nullStringOrPtr(nil)
	if nullCase.Valid {
		t.Errorf("nil pointer should produce Valid=false")
	}
	s := "hello"
	valid := nullStringOrPtr(&s)
	if !valid.Valid || valid.String != "hello" {
		t.Errorf("non-nil pointer broke: %+v", valid)
	}
	empty := ""
	emptyCase := nullStringOrPtr(&empty)
	if !emptyCase.Valid || emptyCase.String != "" {
		t.Errorf("empty-string pointer should still be Valid=true (caller pre-checks): %+v", emptyCase)
	}
}
