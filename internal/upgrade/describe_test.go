package upgrade

import "testing"

// TestIsNewerGitDescribe pins the comparison of git-describe-stamped source
// builds (the hw deploy stamps `git describe --tags`): a describe with N
// commits after tag v1.3.4 outranks the plain tag and loses to a longer
// distance from the same tag.
func TestIsNewerGitDescribe(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v1.3.4-17-g57f9982", "1.3.4", true},  // source build ahead of its tag
		{"v1.3.4-17-g57f9982", "v1.3.4", true}, // ditto with v prefix
		{"v1.3.4-17-g57f9982", "1.3.5", false}, // still below a newer release
		{"v1.3.4-17-g57f9982", "v1.3.4-18-gabc", false},
		{"v1.3.4-18-gabc", "v1.3.4-17-g57f9982", true},
		{"v1.3.4-dirty", "1.3.4", false}, // dirty without distance is ambiguous: not newer
	}
	for _, tc := range cases {
		if got := IsNewer(tc.candidate, tc.current); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

func TestIsParseableVersionShapes(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"1.3.4", true},
		{"v1.3.4", true},
		{"dev", false},
		{"", false},
		{"v1.3.4-17-g57f9982", true},
		{"v1.3.4-dirty", false},
		{"v1.3.4-abc", false},
	}
	for _, tc := range cases {
		if got := IsParseableVersion(tc.raw); got != tc.want {
			t.Errorf("IsParseableVersion(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
