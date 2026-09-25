package cli

import (
	"fmt"
	"strings"
)

// Command is a top-level homer command.  The complete command list stays in
// this package so the dispatcher and help text cannot silently drift apart.
type Command string

const (
	CommandInit    Command = "init"
	CommandRemote  Command = "remote"
	CommandStatus  Command = "status"
	CommandDiff    Command = "diff"
	CommandPush    Command = "push"
	CommandPull    Command = "pull"
	CommandMerge   Command = "merge"
	CommandHome    Command = "home"
	CommandDoctor  Command = "doctor"
	CommandSecret  Command = "secret"
	CommandVersion Command = "version"
	CommandHelp    Command = "help"
)

// COMMANDS is the frozen top-level command order shown by --help.
var COMMANDS = []Command{
	CommandInit,
	CommandRemote,
	CommandStatus,
	CommandDiff,
	CommandPush,
	CommandPull,
	CommandMerge,
	CommandHome,
	CommandDoctor,
	CommandSecret,
	CommandVersion,
	CommandHelp,
}

// ParsedArgs separates the command word from its arguments.  Flag parsing is
// performed after this split so a command can reject every flag it does not
// own, matching node:util parseArgs({ strict: true }) in the TS CLI.
type ParsedArgs struct {
	Command Command
	Rest    []string
}

// SplitCommand recognizes a command from argv after the executable name.
// --help and -h are aliases for the top-level help command.
func SplitCommand(argv []string) ParsedArgs {
	if len(argv) == 0 {
		return ParsedArgs{}
	}
	first := argv[0]
	if first == "--help" || first == "-h" {
		return ParsedArgs{Command: CommandHelp, Rest: append([]string(nil), argv[1:]...)}
	}
	if first == "--version" {
		return ParsedArgs{Command: CommandVersion, Rest: append([]string(nil), argv[1:]...)}
	}
	for _, command := range COMMANDS {
		if string(command) == first {
			return ParsedArgs{Command: command, Rest: append([]string(nil), argv[1:]...)}
		}
	}
	return ParsedArgs{Rest: append([]string(nil), argv...)}
}

// USAGE is the top-level CLI help text. Keep this stable: scripts and the
// release smoke test use `homer --help` as the first post-install check.
const USAGE = `homer — dotfiles for humans and their AI agents

用法:
  homer <command> [options]

命令:
  init      扫描 adapter 并生成 homer.json + store 快照
  remote    配置 origin remote（不自动推送）
  status    显示本地/仓库之间的漂移概览
  diff      显示漂移的详细差异
  push      密钥扫描后推送本地快照到 store 并提交（+ 推送远端）
  pull      拉取远端快照，备份后应用到工具目录
  merge     逐项裁决本地/远端冲突
  home      新机器一键归位：clone 配置仓库 → 应用配置 → 解密密钥 → doctor
  doctor    八项体检（配置 / 仓库 / 远端 / adapter / age / state / 占位符残留）
  secret    密钥投递：keygen | push | pull | list
  version   打印 homer 版本（开发构建显示 dev）

全局选项:
  --home <dir>  homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --version     打印版本
  -h, --help    显示本帮助

退出码:
  0  成功（包括 help；漂移是信息而不是错误）
  1  用法错误或命令失败

示例:
  homer init
  homer status --json
  homer diff --category settings
  homer push --yes
  homer pull --yes
  homer merge --accept-remote
  homer home <repo-url> --yes
  homer doctor --json
  homer secret keygen
`

// CommandOptions is the shared parsed flag shape.  parseOptions accepts the
// union of known command flags, then validateCommandOptions enforces each
// command's strict allow-list.
type CommandOptions struct {
	Home         string
	JSON         bool
	All          bool
	Help         bool
	Yes          bool
	NoPush       bool
	AcceptLocal  bool
	AcceptRemote bool
	Offline      bool
	Verbose      bool
	Force        bool
	Adapters     []string
	Adapter      string
	Category     string
	Mode         string
	Remote       string
	Positionals  []string
}

type argumentError struct{ message string }

func (e *argumentError) Error() string { return e.message }

func usageArgumentError(message string) error {
	return &argumentError{message: message}
}

func isFlag(arg string) bool { return strings.HasPrefix(arg, "-") }

func splitLongFlag(arg string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(arg, "--") {
		return arg, "", false
	}
	if index := strings.IndexByte(arg, '='); index >= 0 {
		return arg[:index], arg[index+1:], true
	}
	return arg, "", false
}

func takeOptionValue(args []string, index *int, name string, inline string, hasInline bool) (string, error) {
	if hasInline {
		if inline == "" {
			return "", usageArgumentError(fmt.Sprintf("选项 %s 需要一个值", name))
		}
		return inline, nil
	}
	if *index+1 >= len(args) {
		return "", usageArgumentError(fmt.Sprintf("选项 %s 缺少值", name))
	}
	*index = *index + 1
	value := args[*index]
	if value == "" || isFlag(value) {
		return "", usageArgumentError(fmt.Sprintf("选项 %s 需要一个值", name))
	}
	return value, nil
}

func appendAdapters(options *CommandOptions, value string) {
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			options.Adapters = append(options.Adapters, item)
		}
	}
}

// parseOptions implements the strict, command-local flag surface used by Run.
// Long options accept both `--name value` and `--name=value`; booleans reject
// inline values, and unknown options are errors rather than positionals.
func parseOptions(command Command, args []string, allowPositionals bool) (CommandOptions, error) {
	var options CommandOptions
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			if !allowPositionals && index+1 < len(args) {
				return options, usageArgumentError(fmt.Sprintf("命令 %s 不接受位置参数", command))
			}
			options.Positionals = append(options.Positionals, args[index+1:]...)
			break
		}
		if arg == "-h" || arg == "--help" {
			options.Help = true
			continue
		}
		if !isFlag(arg) {
			if !allowPositionals {
				return options, usageArgumentError(fmt.Sprintf("命令 %s 不接受位置参数: %s", command, arg))
			}
			options.Positionals = append(options.Positionals, arg)
			continue
		}

		name, inline, hasInline := splitLongFlag(arg)
		switch name {
		case "--home":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Home = value
		case "--json":
			if hasInline {
				return options, usageArgumentError("选项 --json 不接受值")
			}
			options.JSON = true
		case "--all":
			if hasInline {
				return options, usageArgumentError("选项 --all 不接受值")
			}
			options.All = true
		case "--yes":
			if hasInline {
				return options, usageArgumentError("选项 --yes 不接受值")
			}
			options.Yes = true
		case "--no-push":
			if hasInline {
				return options, usageArgumentError("选项 --no-push 不接受值")
			}
			options.NoPush = true
		case "--accept-local":
			if hasInline {
				return options, usageArgumentError("选项 --accept-local 不接受值")
			}
			options.AcceptLocal = true
		case "--accept-remote":
			if hasInline {
				return options, usageArgumentError("选项 --accept-remote 不接受值")
			}
			options.AcceptRemote = true
		case "--offline":
			if hasInline {
				return options, usageArgumentError("选项 --offline 不接受值")
			}
			options.Offline = true
		case "--verbose", "-v":
			if hasInline {
				return options, usageArgumentError(fmt.Sprintf("选项 %s 不接受值", name))
			}
			options.Verbose = true
		case "--force":
			if hasInline {
				return options, usageArgumentError("选项 --force 不接受值")
			}
			options.Force = true
		case "--remote":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Remote = value
		case "--adapters":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			appendAdapters(&options, value)
		case "--adapter":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Adapter = value
		case "--category":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Category = value
		case "--mode":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Mode = value
		default:
			return options, usageArgumentError(fmt.Sprintf("未知选项: %s", arg))
		}
	}
	return options, nil
}

func commandUsage(command Command) string {
	switch command {
	case CommandInit:
		return "用法: homer init [options]\n\n扫描 adapter，生成 homer.json + store 快照。\n\n选项: --home <dir> --adapters <ids> --all --force --remote <url> --json -h, --help"
	case CommandRemote:
		return "用法: homer remote <url> [options]\n\n配置 origin，不自动推送。\n\n选项: --home <dir> --json -h, --help"
	case CommandStatus:
		return "用法: homer status [options]\n\n显示本地 / store / git remote 漂移概览。\n\n选项: --home <dir> --json --verbose, -v -h, --help"
	case CommandDiff:
		return "用法: homer diff [options]\n\n显示键级与行级差异。\n\n选项: --home <dir> --adapter <id> --category <name> -h, --help"
	case CommandPush:
		return "用法: homer push [options]\n\n选项: --home <dir> --json --yes --no-push -h, --help"
	case CommandPull:
		return "用法: homer pull [options]\n\n选项: --home <dir> --json --yes -h, --help"
	case CommandMerge:
		return "用法: homer merge [options]\n\n选项: --home <dir> --json --accept-local --accept-remote -h, --help"
	case CommandHome:
		return "用法: homer home <repo-url> [options]\n\n首次对接模式: --mode pull|merge|skip；--yes 默认 merge。\n\n选项: --home <dir> --mode <mode> --yes --json -h, --help"
	case CommandDoctor:
		return "用法: homer doctor [options]\n\n八项体检。\n\n选项: --home <dir> --offline --json -h, --help"
	case CommandSecret:
		return "用法: homer secret <keygen|push|pull|list> [options]\n\n选项: --home <dir> --yes --no-push --json -h, --help"
	case CommandVersion:
		return "用法: homer version"
	default:
		return fmt.Sprintf("用法: homer %s [options]", command)
	}
}
