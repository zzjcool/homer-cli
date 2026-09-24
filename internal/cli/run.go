package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
	"github.com/zzjcool/homer-cli/internal/core"
	syncx "github.com/zzjcool/homer-cli/internal/sync"
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

	switch parsed.Command {
	case CommandInit:
		report, err := commands.RunInit(commands.InitOptions{
			HomerHome: options.Home,
			Adapters:  options.Adapters,
			JSON:      options.JSON,
		}, commands.InitRunOptions{Force: options.Force})
		if err != nil {
			return commandError(parsed.Command, err, errOut)
		}
		if options.JSON {
			writeLine(out, RenderInitJSON(report))
		} else {
			writeLine(out, RenderInit(report))
		}
		return 0

	case CommandStatus:
		report, err := commands.RunStatus(commands.StatusOptions{
			HomerHome: options.Home,
			JSON:      options.JSON,
			Verbose:   options.Verbose,
		})
		if err != nil {
			return commandError(parsed.Command, err, errOut)
		}
		if options.JSON {
			writeLine(out, RenderStatusJSON(report))
		} else {
			writeLine(out, RenderStatus(report, RenderStatusOptions{Verbose: options.Verbose}))
		}
		// Drift is information, not a failing process status.
		return 0

	case CommandDiff:
		text, err := commands.RunDiff(commands.DiffOptions{
			HomerHome: options.Home,
			Adapter:   options.Adapter,
			Category:  options.Category,
		})
		if err != nil {
			return commandError(parsed.Command, err, errOut)
		}
		if text != "" {
			writeLine(out, text)
		}
		return 0

	case CommandPush:
		report := commands.RunPush(commands.PushOptions{
			HomerHome: options.Home,
			JSON:      options.JSON,
			Yes:       options.Yes,
			NoPush:    options.NoPush,
		}, nil)
		if options.JSON {
			writeLine(out, commands.RenderPushJSON(report))
		} else {
			writeLine(out, commands.RenderPushReport(report))
		}
		return report.ExitCode()

	case CommandPull:
		report := commands.RunPull(commands.PullOptions{
			HomerHome: options.Home,
			JSON:      options.JSON,
			Yes:       options.Yes,
		}, nil)
		if options.JSON {
			writeLine(out, commands.RenderPullJSON(report))
		} else {
			writeLine(out, commands.RenderPullReport(report))
		}
		return report.ExitCode()

	case CommandMerge:
		report := commands.RunMerge(commands.MergeOptions{
			HomerHome:    options.Home,
			JSON:         options.JSON,
			AcceptLocal:  options.AcceptLocal,
			AcceptRemote: options.AcceptRemote,
		}, nil)
		if options.JSON {
			writeLine(out, commands.RenderMergeJSON(report))
		} else {
			writeLine(out, commands.RenderMergeReport(report))
		}
		return report.ExitCode()

	case CommandHome:
		mode := commands.HomeOptions{HomerHome: options.Home, JSON: options.JSON, Yes: options.Yes, RepoURL: options.Positionals[0]}
		if options.Mode != "" {
			mode.Mode = syncx.FirstContactMode(options.Mode)
		}
		report := commands.RunHome(mode, nil)
		if options.JSON {
			writeLine(out, commands.RenderHomeJSON(report))
		} else {
			writeLine(out, commands.RenderHomeReport(report))
		}
		return report.ExitCode()

	case CommandDoctor:
		if options.JSON {
			return commands.ExecuteDoctor(commands.DoctorOptions{
				HomerHome: options.Home,
				JSON:      true,
				Offline:   options.Offline,
			}, nil, out)
		}
		report := commands.RunDoctor(commands.DoctorOptions{
			HomerHome: options.Home,
			JSON:      false,
			Offline:   options.Offline,
		}, nil)
		writeLine(out, commands.RenderDoctorReport(report))
		return report.ExitCode()

	default:
		writeLine(errOut, fmt.Sprintf("homer %s: 尚未实现", parsed.Command))
		return 1
	}
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
	_ = out // kept in the signature so usage can later choose a stream
	return 1
}

func commandError(command Command, err error, errOut io.Writer) int {
	if err == nil {
		return 0
	}
	var cliErr core.CliError
	if errors.As(err, &cliErr) {
		writeLine(errOut, cliErr.Error())
		return cliErr.ExitCode()
	}
	writeLine(errOut, fmt.Sprintf("homer %s: %s", command, err.Error()))
	return 1
}

func unsupportedOptions(command Command, options CommandOptions, names ...string) error {
	for _, name := range names {
		switch name {
		case "yes":
			if options.Yes {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --yes", command))
			}
		case "no-push":
			if options.NoPush {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --no-push", command))
			}
		case "accept-local":
			if options.AcceptLocal {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --accept-local", command))
			}
		case "accept-remote":
			if options.AcceptRemote {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --accept-remote", command))
			}
		case "offline":
			if options.Offline {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --offline", command))
			}
		case "verbose":
			if options.Verbose {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --verbose", command))
			}
		case "force":
			if options.Force {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --force", command))
			}
		case "adapters":
			if len(options.Adapters) > 0 {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --adapters", command))
			}
		case "adapter":
			if options.Adapter != "" {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --adapter", command))
			}
		case "category":
			if options.Category != "" {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --category", command))
			}
		case "mode":
			if options.Mode != "" {
				return usageArgumentError(fmt.Sprintf("命令 %s 不支持选项 --mode", command))
			}
		}
	}
	return nil
}

func validateCommandOptions(command Command, options CommandOptions) error {
	switch command {
	case CommandInit:
		return unsupportedOptions(command, options, "yes", "no-push", "accept-local", "accept-remote", "offline", "verbose", "adapter", "category", "mode")
	case CommandStatus:
		return unsupportedOptions(command, options, "yes", "no-push", "accept-local", "accept-remote", "offline", "force", "adapters", "adapter", "category", "mode")
	case CommandDiff:
		// TS diff intentionally has no --json flag; keep strict command-local
		// parsing even though status/init expose machine-readable reports.
		if options.JSON {
			return usageArgumentError("命令 diff 不支持选项 --json")
		}
		return unsupportedOptions(command, options, "yes", "no-push", "accept-local", "accept-remote", "offline", "verbose", "force", "adapters", "mode")
	case CommandPush:
		return unsupportedOptions(command, options, "accept-local", "accept-remote", "offline", "verbose", "force", "adapters", "adapter", "category", "mode")
	case CommandPull:
		return unsupportedOptions(command, options, "no-push", "accept-local", "accept-remote", "offline", "verbose", "force", "adapters", "adapter", "category", "mode")
	case CommandMerge:
		if options.Yes {
			return usageArgumentError("命令 merge 不支持选项 --yes")
		}
		if options.NoPush {
			return usageArgumentError("命令 merge 不支持选项 --no-push")
		}
		if options.Offline {
			return usageArgumentError("命令 merge 不支持选项 --offline")
		}
		if options.Verbose {
			return usageArgumentError("命令 merge 不支持选项 --verbose")
		}
		if options.Force {
			return usageArgumentError("命令 merge 不支持选项 --force")
		}
		if len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
			return usageArgumentError("命令 merge 不支持该选项")
		}
		if options.AcceptLocal && options.AcceptRemote {
			return usageArgumentError("--accept-local 与 --accept-remote 不能同时使用")
		}
	case CommandHome:
		return unsupportedOptions(command, options, "no-push", "accept-local", "accept-remote", "offline", "verbose", "force", "adapters", "adapter", "category")
	case CommandDoctor:
		return unsupportedOptions(command, options, "yes", "no-push", "accept-local", "accept-remote", "verbose", "force", "adapters", "adapter", "category", "mode")
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

	return commands.ExecuteSecret(subcommand, commands.SecretCommandOptions{
		HomerHome: options.Home,
		JSON:      options.JSON,
		Yes:       options.Yes,
		NoPush:    options.NoPush,
	}, nil, out, errOut)
}

func validateSecretOptions(subcommand string, options CommandOptions) error {
	if options.AcceptLocal || options.AcceptRemote || options.Offline || options.Verbose || options.Force || len(options.Adapters) > 0 || options.Adapter != "" || options.Category != "" || options.Mode != "" {
		return usageArgumentError(fmt.Sprintf("secret %s 不支持该选项", subcommand))
	}
	switch subcommand {
	case "keygen", "list":
		if options.Yes || options.NoPush {
			return usageArgumentError(fmt.Sprintf("secret %s 不支持 --yes / --no-push", subcommand))
		}
	case "pull":
		if options.NoPush {
			return usageArgumentError("secret pull 不支持选项 --no-push")
		}
	}
	return nil
}
