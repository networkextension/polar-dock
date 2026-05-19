package dock

import (
	"reflect"
	"testing"
)

func TestParseBotMentions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"no mention", "just text", nil},
		{"single ascii", "@lily hi", []string{"lily"}},
		{"two distinct", "@lily and @bram", []string{"lily", "bram"}},
		{"dedup", "@lily @lily @lily", []string{"lily"}},
		{"first-seen order preserved", "@b @a @c @a", []string{"b", "a", "c"}},
		{"trailing punctuation", "@lily, what?", []string{"lily"}},
		{"underscore + hyphen", "@bot_one @bot-two", []string{"bot_one", "bot-two"}},
		{"case sensitive", "@Lily @lily", []string{"Lily", "lily"}},
		{"CJK name", "@小明 你好", []string{"小明"}},
		{"mixed ASCII+CJK", "@灵魂导师 + @bram", []string{"灵魂导师", "bram"}},
		{"bare @ ignored", "@ @foo @", []string{"foo"}},
		{"@@ collapses to one mention", "@@bram", []string{"bram"}},
		{"long name truncated at 32 chars", "@" + repeat("a", 40) + " ok", []string{repeat("a", 32)}},
		{"@ at end with no name", "trailing @", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBotMentions(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseBotMentions(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
