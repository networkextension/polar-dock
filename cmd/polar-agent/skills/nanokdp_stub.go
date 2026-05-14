//go:build !unix

package skills

// Windows stub: serial library go.bug.st/serial does support Windows
// but the Nanokdp skill ships unix-only for v1 (KDP workflows are
// macOS-centric). NewNanokdpSkill returns nil + main.go skips.

func NewNanokdpSkill() Skill { return nil }
