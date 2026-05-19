package dock

// mention.go — qunliao P1 helper. Parses `@bot_name` tokens out of a
// room message so the send handler can fan replies out to mentioned
// bots. Bot names are workspace-unique (PR #271 added the constraint).

import (
	"regexp"
	"strings"
)

// mentionRe matches `@` followed by 1-32 chars of:
//   - ASCII letters/digits/`_`/`-`
//   - CJK ideographs (\p{Han})
//
// Trailing punctuation/space terminates the token, so `@lily,` →
// "lily". Length caps the match to keep regex backtracking bounded.
var mentionRe = regexp.MustCompile(`@([A-Za-z0-9_\-\p{Han}]{1,32})`)

// parseBotMentions returns names found in @-form, in first-seen order,
// deduped (case-sensitive). Empty result is a valid "nothing to do".
//
// Doesn't validate that a name corresponds to a real bot — that's the
// caller's job via findBotInRoom; @nobody is silently ignored
// downstream rather than surfaced as an error.
func parseBotMentions(content string) []string {
	if content == "" {
		return nil
	}
	matches := mentionRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
