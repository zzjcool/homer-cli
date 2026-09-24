package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Run is the stable process entry point used by cmd/homer. args is normally
// os.Args (including argv[0]); accepting an already-stripped argv as well is
// convenient for focused package tests.
func Run(args []string) int {
	return runWithIO(args, os.Stdout, os.Stderr)
}

func runWithIO(args []string, out, errOut io.Writer) int {
	argv := withoutExecutable(args)
	parsed := SplitCommand(argv)

	if parsed.Command == CommandHelp {
		writeLine(out, USAGE)
		return 0
	}
	if parsed.Command == "" {
		if len(parsed.Rest) > 0 {
			writeLine(errOut, fmt.Sprintf("未知命令: %s", parsed.Rest[0]))
			writeLine(errOut, "")
		}
		writeLine(errOut, USAGE)
		return 1
	}

	if parsed.Command == CommandSecret {
		return runSecret(parsed.Rest, out, errOut)
	}

	allowPositionals := parsed.Command == CommandHome
	options, parseErr := parseOptions(parsed.Command, parsed.Rest, allowPositionals)
	if parseErr != nil {
		return usageError(parsed.Command, parseErr.Error(), out, errOut)
	}
	if options.Help {
		writeLine(out, commandUsage(parsed.Command))
		return 0
	}
	if validationErr := validateCommandOptions(parsed.Command, options); validationErr != nil {
		return usageError(parsed.Command, validationErr.Error(), out, errOut)
	}
	if parsed.Command == CommandHome {
		if len(options.Positionals) == 0 {
			return usageError(parsed.Command, "缺少 <repo-url> 位置参数", out, errOut)
		}
		if len(options.Positionals) > 1 {
			return usageError(parsed.Command, fmt.Sprintf("多余的参数: %s", joinArgs(options.Positionals[1:])), out, errOut)
		}
		if options.Mode != "" && options.Mode != "pull" && options.Mode != "merge" && options.Mode != "skip" {
			return usageError(parsed.Command, "--mode 只能是 pull / merge / skip", out, errOut)
		}
	}

	// P0 deliberately has no side effects outside argument validation. Later
	// waves replace this one line with the real command implementation while
	// preserving dispatch, usage, and exit-code behavior.
	writeLine(errOut, fmt.Sprintf("homer %s: 尚未实现", parsed.Command))
	return 1
}

func withoutExecutable(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	base := filepath.Base(args[0])
	if base == "homer" || base == "homer.exe" {
		return args[1:]
	}
	return args
}

func writeLine(writer io.Writer, text string) {
	_, _ = fmt.Fprintln(writer, text)
}

func joinArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	result := args[0]
	for _, arg := range args[1:] {
		result += " " + arg
	}
	return result
}

func usageError(command Command, message string, out, errOut io.Writer) int {
	writeLine(errOut, fmt.Sprintf("homer %s: %s", command, message))
	writeLine(errOut, "")
	writeLine(errOut, commandUsage(command))
	_ = out // kept in the signature so future command usage can select a stream
	return 1
}

func validateCommandOptions(command Command, options CommandOptions) error {
	unsupported := func(flag string) error {
		return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 %s", command, flag))
	}

	switch command {
	case CommandInit:
		if options.Yes || options.NoPush || options.AcceptLocal || options.AcceptRemote || options.Offline || options.Verbose || options.Adapter != "" || options.Category != "" || options.Mode != "" {
			return unsupported("该选项")
		}
	case CommandStatus:
		if options.Yes || options.NoPush || options.AcceptLocal || options.AcceptRemote || options.Offline || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
			return unsupported("该选项")
		}
	case CommandDiff:
		if options.Yes || options.NoPush || options.AcceptLocal || options.AcceptRemote || options.Offline || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Mode != "" {
			return unsupported("该选项")
		}
	case CommandPush:
		if options.AcceptLocal || options.AcceptRemote || options.Offline || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
			return unsupported("该选项")
		}
	case CommandPull:
		if options.NoPush || options.AcceptLocal || options.AcceptRemote || options.Offline || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
			return unsupported("该选项")
		}
	case CommandMerge:
		if options.Yes || options.NoPush || options.Offline || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
			return unsupported("该选项")
		}
		if options.AcceptLocal && options.AcceptRemote {
			return usageArgumentError("--accept-local 与 --accept-remote 不能同时使用")
		}
	case CommandHome:
		if options.NoPush || options.AcceptLocal || options.AcceptRemote || options.Offline || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" {
			return unsupported("该选项")
		}
	case CommandDoctor:
		if options.Yes || options.NoPush || options.AcceptLocal || options.AcceptRemote || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
			return unsupported("该选项")
		}
	default:
		return usageArgumentError(fmt.Sprintf("未知命令: %s", command))
	}
	return nil
}

func runSecret(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		writeLine(errOut, "homer secret: 缺少子命令（keygen | push | pull | list）")
		writeLine(errOut, "")
		writeLine(errOut, commandUsage(CommandSecret))
		return 1
	}
	if args[0] == "-h" || args[0] == "--help" {
		writeLine(out, commandUsage(CommandSecret))
		return 0
	}

	subcommand := args[0]
	if subcommand != "keygen" && subcommand != "push" && subcommand != "pull" && subcommand != "list" {
		return usageError(CommandSecret, fmt.Sprintf("未知子命令: %s", subcommand), out, errOut)
	}

	options, parseErr := parseOptions(CommandSecret, args[1:], false)
	if parseErr != nil {
		return usageError(CommandSecret, parseErr.Error(), out, errOut)
	}
	if options.Help {
		writeLine(out, commandUsage(CommandSecret))
		return 0
	}
	if validationErr := validateSecretOptions(subcommand, options); validationErr != nil {
		return usageError(CommandSecret, validationErr.Error(), out, errOut)
	}

	writeLine(errOut, fmt.Sprintf("homer secret %s: 尚未实现", subcommand))
	return 1
}

func validateSecretOptions(subcommand string, options CommandOptions) error {
	unsupported := func(flag string) error {
		return usageArgumentError(fmt.Sprintf("secret %s 不支持选项 %s", subcommand, flag))
	}
	if options.AcceptLocal || options.AcceptRemote || options.Offline || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
		return unsupported("该选项")
	}
	switch subcommand {
	case "keygen", "list":
		if options.Yes || options.NoPush {
			return unsupported("该选项")
		}
	case "push":
		if options.Yes && options.NoPush {
			return usageArgumentError("secret push: --yes 与 --no-push 可以同时使用，但此处仅为占位")
		}
	case "pull":
		if options.NoPush {
			return unsupported("--no-push")
		}
	}
	return nil
}
