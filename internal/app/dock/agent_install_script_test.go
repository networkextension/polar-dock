package dock

import (
	"strings"
	"testing"
)

func TestInstallScriptDarwinIncludesIRecovery(t *testing.T) {
	in := installScriptInput{
		AgentName: "test-agent",
		ServerURL: "https://example.com",
		Token:     "polar_xxx",
		TargetOS:  "darwin",
	}
	out, err := renderAgentInstallScript(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	checks := []string{
		"brew install libimobiledevice",
		"irecovery on PATH",
		"iOS recovery auto-detect",
		`if [ "$OS" = "darwin" ]`,
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("install script missing %q", c)
		}
	}
}

func TestInstallScriptLinuxDoesNotEmitBrew(t *testing.T) {
	in := installScriptInput{
		AgentName: "test-agent",
		ServerURL: "https://example.com",
		Token:     "polar_xxx",
		TargetOS:  "linux",
	}
	out, err := renderAgentInstallScript(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Linux path uses the docker-run template, which deliberately
	// doesn't reach the libimobiledevice section (the docker image
	// already bundles deps).
	if strings.Contains(out, "brew install libimobiledevice") {
		t.Errorf("linux template should not contain brew install line")
	}
}
