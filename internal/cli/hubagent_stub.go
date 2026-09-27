package cli

import "io"

// runAgent is temporarily stubbed until internal/agentd lands (T2/T3).
func runAgent(options CommandOptions, out, errOut io.Writer) int {
	writeLine(errOut, "homer agent: 尚未实现（等待 hub/agentd 包交付后接线）")
	return 1
}
