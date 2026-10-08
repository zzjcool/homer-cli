package cli

import (
	"fmt"
	"strings"

	"github.com/zzjcool/homer-cli/internal/cli/commands"
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
	CommandKey     Command = "key"
	CommandPair    Command = "pair"
	CommandServe   Command = "serve"
	CommandAgent   Command = "agent"
	CommandPS      Command = "ps"
	CommandToken   Command = "token"
	CommandJoin    Command = "join"
	CommandResolve Command = "resolve"
	CommandVersion Command = "version"
	CommandUpgrade Command = "upgrade"
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
	CommandKey,
	CommandPair,
	CommandServe,
	CommandAgent,
	CommandPS,
	CommandToken,
	CommandJoin,
	CommandResolve,
	CommandVersion,
	CommandUpgrade,
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
  serve     启动中心：网页控制台和 API 一直在后台跑
  agent     把本机接入中心，并保持连接
  token     在中心机器上查看或生成中心令牌
  ps        列出中心上的机器，以及每台机器上 pi、herdr 等应用的版本
  join      打印接入新机器的命令（对应控制台「接入新机器」）
  status    查看漂移。设置了 HOMER_HOST 或 --host 时向中心查询，否则检查本机
  diff      查看差异。设置了中心地址时向中心查询，否则检查本机
  push      设置了中心地址时把这台机器收取到中心，否则写入本机存储
  pull      设置了中心地址时把中心内容下发到这台机器，否则应用到本机工具目录
  resolve   在这台机器上选择保留哪一边（以这台机器为准 / 以中心为准）
  upgrade   更新程序。不带 --id 更新本机；带 --id 更新那台机器
  key       口令密钥：list | create | encrypt | unlock | passwd
  version   打印 homer 版本

这些命令不走中心，控制台也没有对应按钮，日常和自动化不要用:
  init      扫描 adapter 并生成 homer.json + store 快照
  remote    配置 origin remote（不自动推送）
  merge     逐项裁决本地/远端冲突
  home      新机器一键归位：clone 配置仓库 → 应用配置 → 解密密钥 → doctor
  doctor    八项体检（配置 / 仓库 / 远端 / adapter / age / state / 占位符残留）
  secret    密钥投递：keygen | push | pull | list
  pair      在线配对另一台机器（tailcat 快车道，传输全程 age 密文）

全局选项:
  --home <dir>   homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --host <url>   中心地址（默认 $HOMER_HOST，否则 http://127.0.0.1:7760；也接受 tcp://host:port）
  --token <t>    中心令牌（默认 $HOMER_HUB_TOKEN，否则 keys/hub-token）
  --id <id>      要操作的机器（默认本机 agent.json）
  --version      打印版本
  -h, --help     显示本帮助

退出码:
  0  成功（包括 help；漂移是信息而不是错误）
  1  用法错误或命令失败

示例:
  homer token
  homer ps
  homer join
  homer status
  homer push --yes
  homer pull --yes
  homer resolve --accept-local --yes
  homer upgrade --id <机器>
  homer pair <tc-addr>
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
	// Hub-form options (serve/agent, plan §2.6): shared struct keeps the
	// strict command-local whitelist pattern.
	Addr      string
	Listen    string
	Connect   string
	Hub       string
	Advertise string
	Token     string
	ID        string
	ShowJoin  bool
	// Host is the hub this client talks to, like Docker's --host.
	Host string
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
		case "--addr":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Addr = value
		case "--listen":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Listen = value
		case "--connect":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Connect = value
		case "--hub":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Hub = value
		case "--advertise":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Advertise = value
		case "--token":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Token = value
		case "--id":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.ID = value
		case "--host", "-H":
			value, err := takeOptionValue(args, &index, name, inline, hasInline)
			if err != nil {
				return options, err
			}
			options.Host = value
		case "--show-join":
			options.ShowJoin = true
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
	case CommandServe:
		return "用法: homer serve [options]\n\n启动本地 hub：HTTP API + 网页控制台（浏览器访问）。\n\n选项:\n  --addr <addr>       监听地址（默认 127.0.0.1:7760）\n  --home <dir>        homer 工作区\n  --token <t>         hub 鉴权 token（非回环地址必须提供；未提供时自动生成并落盘 keys/hub-token）\n  --show-join         打印含 token 的机器接入命令后退出\n  -h, --help          显示本帮助"
	case CommandAgent:
		return "用法: homer agent [--listen <addr> | --connect <url>] [options]\n\n把本机作为 agent 接入 hub。无参数时用上次注册成功的持久化配置重启。\n\n选项:\n  --listen <addr>      监听地址，等 hub 直连采集（机器可达时用）\n  --connect <url>      主动拨出连接 hub（NAT 后机器用）\n\n选项:\n  --hub <url>          listen 模式注册用的 hub 地址\n  --advertise <url>    listen 模式自报的可达地址（0.0.0.0 监听时自动用唯一全局 IPv4）\n  --token <t>          hub token（默认读 HOMER_HUB_TOKEN）\n  --home <dir>         homer 工作区\n  --id <agentId>       覆盖默认 agent ID\n  -h, --help           显示本帮助"
	case CommandRemote:
		return "用法: homer remote <url> [options]\n\n配置 origin，不自动推送。\n\n选项: --home <dir> --json -h, --help"
	case CommandStatus:
		return "用法: homer status [options]\n\n向中心查询这台机器的漂移，和控制台用的是同一条路径。\n\n选项: --home <dir> --host <url> --id <agentId> --json --verbose, -v -h, --help"
	case CommandDiff:
		return "用法: homer diff [options]\n\n向中心查询这台机器的差异。\n\n选项: --home <dir> --host <url> --id <agentId> --adapter <id> --category <name> -h, --help"
	case CommandPush:
		return "用法: homer push [options]\n\n把这台机器的配置收取到中心。加上 --yes 才会执行。\n\n选项: --home <dir> --host <url> --id <agentId> --adapters <ids> --json --yes -h, --help"
	case CommandPull:
		return "用法: homer pull [options]\n\n把中心的配置下发到这台机器。加上 --yes 才会执行。\n\n选项: --home <dir> --host <url> --id <agentId> --adapters <ids> --json --yes -h, --help"
	case CommandPS:
		return "用法: homer ps [options]\n\n列出中心上的机器，以及每台机器上 pi、herdr 等应用的版本；落后于其他机器的会标出最新版本。\n\n选项: --home <dir> --host <url> --json -h, --help"
	case CommandToken:
		return "用法: homer token [options]\n\n在中心那台机器上查看中心令牌。文件不存在时会生成。--force 换成新的，正在运行的 homer serve 要重启后才使用新令牌。\n\n选项: --home <dir> --force -h, --help"
	case CommandJoin:
		return "用法: homer join [options]\n\n向中心要一条接入新机器的命令，和控制台「接入新机器」相同。\n\n选项: --home <dir> --host <url> --token <t> -h, --help"
	case CommandResolve:
		return "用法: homer resolve [options]\n\n在指定机器上选择保留哪一边。--accept-local 以这台机器为准，--accept-remote 以中心为准。加上 --yes 才会执行。\n\n选项: --home <dir> --host <url> --id <agentId> --adapters <ids> --accept-local --accept-remote --yes --json -h, --help"
	case CommandMerge:
		return "用法: homer merge [options]\n\n选项: --home <dir> --json --accept-local --accept-remote -h, --help"
	case CommandHome:
		return "用法: homer home <repo-url> [options]\n\n首次对接模式: --mode pull|merge|skip；--yes 默认 merge。\n\n选项: --home <dir> --mode <mode> --yes --json -h, --help"
	case CommandDoctor:
		return "用法: homer doctor [options]\n\n八项体检。\n\n选项: --home <dir> --offline --json -h, --help"
	case CommandSecret:
		return "用法: homer secret <keygen|push|pull|list> [options]\n\n选项: --home <dir> --yes --no-push --json -h, --help"
	case CommandKey:
		return commands.KEY_USAGE
	case CommandPair:
		lines := strings.Split(commands.PAIR_USAGE, "\n")
		if len(lines) >= 2 {
			return strings.Join(lines[:2], "\n")
		}
		return commands.PAIR_USAGE
	case CommandVersion:
		return "用法: homer version\n\n打印版本；联网时顺带检查新版本并在有更新时提示 homer upgrade"
	case CommandUpgrade:
		return "用法: homer upgrade [options]\n\n下载并替换当前程序。正在运行的 agent 会一起重启；没在跑的不会被拉起来。\n\n选项: --force 即使无新版本也重装 -h, --help"
	default:
		return fmt.Sprintf("用法: homer %s [options]", command)
	}
}
