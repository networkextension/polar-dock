package dock

import "testing"

func TestIPAllowed(t *testing.T) {
	cases := []struct {
		name  string
		ip    string
		allow []string
		want  bool
	}{
		{"empty allowlist = unrestricted", "203.0.113.5", nil, true},
		{"empty slice = unrestricted", "203.0.113.5", []string{}, true},
		{"exact match", "127.0.0.1", []string{"127.0.0.1"}, true},
		{"exact no match", "203.0.113.5", []string{"127.0.0.1"}, false},
		{"cidr contains", "10.1.2.3", []string{"10.0.0.0/8"}, true},
		{"cidr excludes", "192.168.1.1", []string{"10.0.0.0/8"}, false},
		{"mixed list, matches second", "192.168.1.7", []string{"127.0.0.1", "192.168.0.0/16"}, true},
		{"garbage client ip denied when restricted", "not-an-ip", []string{"10.0.0.0/8"}, false},
		{"ipv6 exact", "::1", []string{"::1"}, true},
	}
	for _, c := range cases {
		if got := ipAllowed(c.ip, c.allow); got != c.want {
			t.Errorf("%s: ipAllowed(%q, %v) = %v, want %v", c.name, c.ip, c.allow, got, c.want)
		}
	}
}

func TestValidIPEntry(t *testing.T) {
	good := []string{"127.0.0.1", "10.0.0.0/8", "::1", "2001:db8::/32"}
	bad := []string{"", "nope", "10.0.0.0/99", "999.1.1.1"}
	for _, g := range good {
		if !validIPEntry(g) {
			t.Errorf("validIPEntry(%q) = false, want true", g)
		}
	}
	for _, b := range bad {
		if validIPEntry(b) {
			t.Errorf("validIPEntry(%q) = true, want false", b)
		}
	}
}
