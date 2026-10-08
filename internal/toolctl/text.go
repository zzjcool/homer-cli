package toolctl

import (
	"strings"
	"unicode/utf8"
)

// tidy makes captured command output readable in a browser: terminal escape
// sequences go, a carriage return becomes a line break (progress bars redraw
// one line), blank runs collapse, and only the last maxLines lines, at most
// maxBytes, are kept.
func tidy(text string, maxLines, maxBytes int) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := make([]string, 0, 32)
	blank := false
	for _, raw := range strings.Split(stripControls(text), "\n") {
		line := strings.TrimRight(raw, " \t")
		if line == "" {
			if blank || len(lines) == 0 {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		lines = append(lines, line)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	joined := strings.Join(lines, "\n")
	if len(joined) > maxBytes {
		joined = joined[len(joined)-maxBytes:]
		// Do not start in the middle of a multi-byte character.
		for len(joined) > 0 && !utf8.RuneStart(joined[0]) {
			joined = joined[1:]
		}
	}
	return joined
}

// lastLine is the last non-empty line of text, cleaned and clipped.
func lastLine(text string) string {
	lines := strings.Split(tidy(text, 1<<10, 1<<16), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return clip(line, 240)
		}
	}
	return ""
}

func clip(text string, maxRunes int) string {
	text = strings.TrimSpace(stripControls(text))
	runes := []rune(text)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return text
}

// stripControls drops C0 controls (keeping newlines and tabs) and CSI
// sequences such as colors.
func stripControls(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		c := text[i]
		if c == 0x1b {
			if i+1 < len(text) && text[i+1] == '[' {
				i += 2
				for i < len(text) && (text[i] < 0x40 || text[i] > 0x7e) {
					i++
				}
				if i < len(text) {
					i++
				}
				continue
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			i += size
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}
