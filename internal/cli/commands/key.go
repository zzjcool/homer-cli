package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zzjcool/homer-cli/internal/keyring"
	"golang.org/x/term"
)

const KEY_USAGE = `用法: homer key <list|create|encrypt|unlock|passwd> [options]

口令包住一把数据密钥，文件用这把数据密钥加密。密钥项随配置同步到其他节点。
口令和拆开后的数据密钥不写入配置。

子命令:
  list     列出密钥项和文件（不含口令、不含明文）
  create   新建一项密钥
  encrypt  用已有密钥加密一个本地文件
  unlock   用口令解开这项密钥下的全部文件，写回原路径
  passwd   更换口令（只重写信封，不重加密文件）

选项:
  --home <dir>           homer 工作区
  --id <id>              密钥 id
  --name <name>          显示名称（create；缺省等于 id）
  --file <id>            文件 id（encrypt）
  --path <path>          文件路径，以 ~ 或 / 开头（encrypt）
  --password <secret>    口令（非交互时必填；会出现在进程参数里）
  --new-password <secret> 新口令（passwd）
  --json
  -h, --help

口令至少 8 位。交互终端里省略 --password 时会在终端询问。`

// RunKeyArgs is the homer key entry point.
func RunKeyArgs(args []string, out, errOut io.Writer) int {
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	if len(args) == 0 {
		fmt.Fprintln(errOut, "homer key: 缺少子命令（list | create | encrypt | unlock | passwd）")
		fmt.Fprintln(errOut)
		fmt.Fprintln(errOut, KEY_USAGE)
		return 1
	}
	action := args[0]
	if action == "-h" || action == "--help" {
		fmt.Fprintln(out, KEY_USAGE)
		return 0
	}
	flags, err := parseKeyFlags(args[1:])
	if err != nil {
		fmt.Fprintln(errOut, err.Error())
		return 1
	}
	if flags.help {
		fmt.Fprintln(out, KEY_USAGE)
		return 0
	}
	cmd := keyring.Command{
		Action:      action,
		ID:          flags.id,
		Name:        flags.name,
		File:        flags.file,
		Path:        flags.path,
		Password:    flags.password,
		NewPassword: flags.newPassword,
	}
	if cmd.Password == "" && action != "list" {
		password, err := promptPassword("口令: ")
		if err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		cmd.Password = password
	}
	if action == "passwd" && cmd.NewPassword == "" {
		password, err := promptPassword("新口令: ")
		if err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		cmd.NewPassword = password
	}
	result := keyring.Apply(flags.home, cmd)
	if flags.json {
		encoded, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintln(errOut, err.Error())
			return 1
		}
		fmt.Fprintln(out, string(encoded))
	} else if result.OK {
		fmt.Fprintln(out, renderKeyResult(result))
	} else {
		fmt.Fprintln(errOut, strings.Join(result.Errors, "\n"))
	}
	if result.OK {
		return 0
	}
	return 1
}

type keyFlags struct {
	home, id, name, file, path, password, newPassword string
	json, help                                        bool
}

func parseKeyFlags(args []string) (keyFlags, error) {
	var flags keyFlags
	for i := 0; i < len(args); i++ {
		arg := args[i]
		need := func() (string, error) {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", fmt.Errorf("选项 %s 缺少取值", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "--json":
			flags.json = true
		case arg == "-h" || arg == "--help":
			flags.help = true
		case arg == "--home":
			value, err := need()
			if err != nil {
				return flags, err
			}
			flags.home = value
		case arg == "--id":
			value, err := need()
			if err != nil {
				return flags, err
			}
			flags.id = value
		case arg == "--name":
			value, err := need()
			if err != nil {
				return flags, err
			}
			flags.name = value
		case arg == "--file":
			value, err := need()
			if err != nil {
				return flags, err
			}
			flags.file = value
		case arg == "--path":
			value, err := need()
			if err != nil {
				return flags, err
			}
			flags.path = value
		case arg == "--password":
			value, err := need()
			if err != nil {
				return flags, err
			}
			flags.password = value
		case arg == "--new-password":
			value, err := need()
			if err != nil {
				return flags, err
			}
			flags.newPassword = value
		default:
			return flags, fmt.Errorf("未知选项: %s", arg)
		}
	}
	return flags, nil
}

func promptPassword(label string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("非交互环境请传入 --password")
	}
	fmt.Fprint(os.Stderr, label)
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func renderKeyResult(result keyring.Result) string {
	switch result.Status {
	case "created":
		return "已创建密钥 " + result.Keys[0].ID
	case "encrypted":
		return "已加密"
	case "unlocked":
		return "已写回 " + strings.Join(result.Written, ", ")
	case "rotated":
		return "已更换口令"
	case "listed":
		if len(result.Keys) == 0 {
			return "还没有密钥"
		}
		lines := make([]string, 0)
		for _, key := range result.Keys {
			lines = append(lines, key.ID+"  "+key.Name)
			for _, file := range key.Files {
				lines = append(lines, "  "+file.ID+"  "+file.Destination)
			}
		}
		return strings.Join(lines, "\n")
	default:
		return result.Status
	}
}
