package toolctl

import (
	"strings"
	"testing"
)

func TestTidyMakesTerminalOutputReadable(t *testing.T) {
	raw := "\x1b[32mDownloading\x1b[0m\r 10%\r 55%\r100%\r\n\n\n\x1b[1mdone\x1b[0m   \n\n"
	got := tidy(raw, 40, 4096)
	want := "Downloading\n 10%\n 55%\n100%\n\ndone"
	if got != want {
		t.Fatalf("tidy = %q, want %q", got, want)
	}
}

func TestTidyKeepsTheEndOfLongOutput(t *testing.T) {
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, "line "+strings.Repeat("x", 3))
	}
	lines[99] = "the failure"
	got := tidy(strings.Join(lines, "\n"), 5, 4096)
	parts := strings.Split(got, "\n")
	if len(parts) != 5 || parts[4] != "the failure" {
		t.Fatalf("tidy kept %q", got)
	}
	long := strings.Repeat("字", 3000) // 9000 bytes
	cut := tidy(long, 5, 100)
	if len(cut) > 100 || !strings.HasSuffix(long, cut) {
		t.Fatalf("byte cap: len=%d", len(cut))
	}
	for _, r := range cut {
		if r == '\uFFFD' {
			t.Fatal("the cap split a multi-byte character")
		}
	}
}

func TestLastLineIgnoresTrailingNoise(t *testing.T) {
	if got := lastLine("first\nsecond\n\n  \n"); got != "second" {
		t.Fatalf("lastLine = %q", got)
	}
	if got := lastLine(""); got != "" {
		t.Fatalf("lastLine(empty) = %q", got)
	}
	if got := lastLine(strings.Repeat("a", 1000)); len([]rune(got)) != 240 {
		t.Fatalf("lastLine did not clip: %d runes", len([]rune(got)))
	}
}

func TestStripControlsKeepsNewlinesAndTabs(t *testing.T) {
	if got := stripControls("a\x00b\x1b[31mred\x1b[0m\tc\nd\x7f"); got != "abred\tc\nd" {
		t.Fatalf("stripControls = %q", got)
	}
	// A lone escape at the end must not loop or panic.
	if got := stripControls("x\x1b"); got != "x" {
		t.Fatalf("stripControls trailing escape = %q", got)
	}
	if got := stripControls("x\x1b[31"); got != "x" {
		t.Fatalf("stripControls truncated CSI = %q", got)
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{limit: 5}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defgh"))
	if string(b.data) != "defgh" {
		t.Fatalf("data = %q", b.data)
	}
	n, err := b.Write([]byte("ij"))
	if n != 2 || err != nil || string(b.data) != "fghij" {
		t.Fatalf("n=%d err=%v data=%q", n, err, b.data)
	}
}
