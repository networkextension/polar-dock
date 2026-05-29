package dock

import (
	"strings"
	"testing"
)

// Pure-memory test: insertAssetFromFile input validation. The full
// path (hash + provider stream + DB insert) is exercised in the
// dual-write smoke step on .57 since it needs a real DB + provider.
// This test catches the "operator forgot a required field" case
// without spinning up a server.
func TestInsertAssetFromFileRequiresFields(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name    string
		in      AssetInsertInput
		wantSub string
	}{
		{
			name:    "missing workspace",
			in:      AssetInsertInput{Kind: "post-image", Name: "x", SrcPath: "/tmp/x"},
			wantSub: "WorkspaceID required",
		},
		{
			name:    "missing kind",
			in:      AssetInsertInput{WorkspaceID: "t_x", Name: "x", SrcPath: "/tmp/x"},
			wantSub: "Kind and Name required",
		},
		{
			name:    "missing name",
			in:      AssetInsertInput{WorkspaceID: "t_x", Kind: "post-image", SrcPath: "/tmp/x"},
			wantSub: "Kind and Name required",
		},
		{
			name:    "missing src path",
			in:      AssetInsertInput{WorkspaceID: "t_x", Kind: "post-image", Name: "x"},
			wantSub: "SrcPath required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.insertAssetFromFile(tc.in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("want error containing %q, got %q", tc.wantSub, err.Error())
			}
		})
	}
}
