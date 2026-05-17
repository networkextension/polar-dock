package dock

import (
	"strings"
	"testing"
)

func TestRenderWGInstallScript(t *testing.T) {
	s, err := renderWGInstallScript(wgInstallScriptInput{
		Server:        "https://zen.4950.store",
		BundleVersion: "20260517-abc12345",
		MeshCIDR:      "10.88.0.0/16",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	wants := []string{
		"#!/bin/bash",
		"SERVER='https://zen.4950.store'",
		"BUNDLE_VERSION='20260517-abc12345'",
		"MESH_CIDR='10.88.0.0/16'",
		"/v1/register",
		"/v1/bundle/$BUNDLE_VERSION",
		"/usr/local/bin/wgctl genkey",
		"launchctl kickstart",
	}
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("rendered script missing %q", w)
		}
	}
	// Defaults applied when caller passes empty MeshCIDR / version.
	s2, err := renderWGInstallScript(wgInstallScriptInput{Server: "http://localhost:8089"})
	if err != nil {
		t.Fatalf("render defaults: %v", err)
	}
	if !strings.Contains(s2, "BUNDLE_VERSION='latest'") {
		t.Errorf("default BUNDLE_VERSION not applied")
	}
	if !strings.Contains(s2, "MESH_CIDR='10.88.0.0/16'") {
		t.Errorf("default MESH_CIDR not applied")
	}
	// Empty server → error.
	if _, err := renderWGInstallScript(wgInstallScriptInput{Server: ""}); err == nil {
		t.Errorf("expected error on empty server URL")
	}
}

func TestExtractPublicIP(t *testing.T) {
	cases := []struct {
		name             string
		xff, xrip, raddr string
		want             string
	}{
		{"xff wins", "1.2.3.4, 5.6.7.8", "9.9.9.9", "10.0.0.1:5555", "1.2.3.4"},
		{"xff single", "1.2.3.4", "", "", "1.2.3.4"},
		{"xri fallback", "", "9.9.9.9", "10.0.0.1:5555", "9.9.9.9"},
		{"raddr fallback", "", "", "10.0.0.1:5555", "10.0.0.1"},
		{"all empty", "", "", "", ""},
		{"xff with spaces", "  1.2.3.4 , 5.6.7.8  ", "", "", "1.2.3.4"},
	}
	for _, c := range cases {
		if got := extractPublicIP(c.xff, c.xrip, c.raddr); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestFirstLANIP(t *testing.T) {
	if got := firstLANIP("192.168.1.5/24"); got != "192.168.1.5" {
		t.Errorf("got %q", got)
	}
	if got := firstLANIP("192.168.1.5"); got != "192.168.1.5" {
		t.Errorf("got %q", got)
	}
}

func TestFirstLANCIDR(t *testing.T) {
	got := firstLANCIDR([]WGLanAddr{{Iface: "en0", CIDR: ""}, {Iface: "en1", CIDR: "192.168.1.5/24"}})
	if got != "192.168.1.5/24" {
		t.Errorf("got %q", got)
	}
	if got := firstLANCIDR(nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestNewWGTokenPlaintext(t *testing.T) {
	p1, pref1 := newWGTokenPlaintext()
	p2, _ := newWGTokenPlaintext()
	if !strings.HasPrefix(p1, "polar_wg_") {
		t.Errorf("missing prefix: %q", p1)
	}
	if pref1 != p1[:len("polar_wg_")+8] {
		t.Errorf("prefix mismatch: %q vs %q", pref1, p1[:len("polar_wg_")+8])
	}
	if p1 == p2 {
		t.Errorf("randoms collided (incredible)")
	}
}

func TestValidatePubkey(t *testing.T) {
	if err := validatePubkey(""); err == nil {
		t.Errorf("empty pubkey accepted")
	}
	if err := validatePubkey(strings.Repeat("A", 200)); err == nil {
		t.Errorf("oversize pubkey accepted")
	}
	if err := validatePubkey("TqbeoU9mc1234567890abcdef="); err != nil {
		t.Errorf("normal pubkey rejected: %v", err)
	}
}
