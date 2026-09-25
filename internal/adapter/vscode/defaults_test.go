package vscode

import (
	"runtime"
	"testing"
)

func TestDefaultVSCodeRootByPlatform(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{goos: "linux", want: DefaultVSCodeRootLinux},
		{goos: "darwin", want: DefaultVSCodeRootDarwin},
	}
	for _, test := range cases {
		if got := DefaultVSCodeRoot(test.goos); got != test.want {
			t.Errorf("DefaultVSCodeRoot(%q) = %q, want %q", test.goos, got, test.want)
		}
	}
	if got, want := DefaultVSCodeAdapter.Root, DefaultVSCodeRoot(runtime.GOOS); got != want {
		t.Fatalf("default adapter root = %q, want %q for %s", got, want, runtime.GOOS)
	}
}
