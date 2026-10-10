package web

import "testing"

// TestHubVersionIsNewer freezes the console upgrade-hint semantics:
// a release-versioned hub outranks older agents; an unparseable "dev" hub
// outranks every release agent (the agent update path downloads the hub's
// own binary, so a source-built hub is never "behind"); equal or older hub
// versions and dev agents never set the hint.
func TestHubVersionIsNewer(t *testing.T) {
	cases := []struct {
		hub, agent string
		want       bool
	}{
		{"1.3.5", "1.3.4", true},
		{"1.3.4", "1.3.4", false},
		{"1.3.3", "1.3.4", false},
		{"dev", "1.3.4", true},
		{"dev", "1.3.3", true},
		{"dev", "dev", false},
		{"v1.4.0", "1.3.9", true},
	}
	for _, tc := range cases {
		if got := hubVersionIsNewer(tc.hub, tc.agent); got != tc.want {
			t.Errorf("hubVersionIsNewer(%q, %q) = %v, want %v", tc.hub, tc.agent, got, tc.want)
		}
	}
}
