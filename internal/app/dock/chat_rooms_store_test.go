package dock

import "testing"

// Pure-logic tests for chat_rooms_store helpers. DB-touching coverage
// happens via the smoke playbook in doc/qunliao/dev-plan.md §P0a (the
// rest of the dock package follows the same pattern — pure-logic in
// unit tests, integration via the deploy smoke).

func TestValidRoomKind(t *testing.T) {
	tests := []struct {
		kind string
		want bool
	}{
		{ChatRoomKindRoom, true},
		{ChatRoomKindDM, true},
		{ChatRoomKindWhisper, true},
		{ChatRoomKindConvene, true},
		{"", false},
		{"channel", false}, // intentional — we don't accept Slack-isms
		{"ROOM", false},    // case-sensitive
	}
	for _, tc := range tests {
		if got := validRoomKind(tc.kind); got != tc.want {
			t.Errorf("validRoomKind(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestValidRoomRole(t *testing.T) {
	tests := []struct {
		role string
		want bool
	}{
		{ChatRoomRoleOwner, true},
		{ChatRoomRoleMember, true},
		{ChatRoomRoleObserver, true},
		{"", false},
		{"admin", false}, // workspace role, not room role
		{"MEMBER", false},
	}
	for _, tc := range tests {
		if got := validRoomRole(tc.role); got != tc.want {
			t.Errorf("validRoomRole(%q) = %v, want %v", tc.role, got, tc.want)
		}
	}
}

func TestChatRoomKindConstantsMatchSchemaCheck(t *testing.T) {
	// Belt-and-braces: if anyone renames a constant, this test forces a
	// look at store.go's CHECK constraint (chat_rooms_kind_check) too.
	want := map[string]bool{"room": true, "dm": true, "whisper": true, "convene": true}
	got := map[string]bool{
		ChatRoomKindRoom:    true,
		ChatRoomKindDM:      true,
		ChatRoomKindWhisper: true,
		ChatRoomKindConvene: true,
	}
	if len(got) != len(want) {
		t.Fatalf("constant set size mismatch: got %d want %d", len(got), len(want))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("constant for kind %q missing — schema CHECK + Go constants drifted", k)
		}
	}
}

func TestChatRoomRoleConstantsMatchSchemaCheck(t *testing.T) {
	want := map[string]bool{"owner": true, "member": true, "observer": true}
	got := map[string]bool{
		ChatRoomRoleOwner:    true,
		ChatRoomRoleMember:   true,
		ChatRoomRoleObserver: true,
	}
	if len(got) != len(want) {
		t.Fatalf("constant set size mismatch: got %d want %d", len(got), len(want))
	}
	for k := range want {
		if !got[k] {
			t.Errorf("constant for role %q missing — schema CHECK + Go constants drifted", k)
		}
	}
}
