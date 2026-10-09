package web

import (
	"strings"
	"testing"
)

// TestRenderedInstallScriptNoBareVarBeforeMultibyte guards against the
// macOS /bin/sh (bash 3.2) parsing hazard: without a UTF-8 locale, bash 3.2
// swallows the lead byte of a fullwidth punctuation mark that immediately
// follows a bare $VAR (e.g. "$LOG，PID: ..."), treats it as part of the
// variable name, and dies with "unbound variable" under set -eu — exactly
// the production incident where a darwin machine running the mismatch
// branch crashed on the first echo. Every variable reference followed by
// a non-ASCII byte must be braced.
func TestRenderedInstallScriptNoBareVarBeforeMultibyte(t *testing.T) {
	script := RenderInstallScript("https://homerhw.openaaas.org", "linux", "amd64")
	src := script
	for i := 0; i < len(src)-1; i++ {
		if src[i] != '$' {
			continue
		}
		j := i + 1
		if src[j] == '{' || src[j] == '(' {
			continue // braced or command substitution
		}
		// bare special variables are single characters
		if strings.IndexByte("!#?$*-@0123456789", src[j]) >= 0 {
			if j+1 < len(src) && src[j+1] >= 0x80 {
				t.Fatalf("bare special variable $%c is followed by a multi-byte char (bash 3.2 hazard): %.40s", src[j], src[i:])
			}
			continue
		}
		for j < len(src) && (src[j] == '_' || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= '0' && src[j] <= '9')) {
			j++
		}
		if j > i+1 && j < len(src) && src[j] >= 0x80 {
			t.Fatalf("bare $%s is followed by a multi-byte char (bash 3.2 hazard): %.40s", src[i+1:j], src[i:])
		}
	}
}
