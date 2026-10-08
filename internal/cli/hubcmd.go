package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zzjcool/homer-cli/internal/hub"
	"github.com/zzjcool/homer-cli/internal/hubclient"
)

// hubRequested reports whether this invocation should talk to a hub.
// Like Docker, the client stays local until DOCKER_HOST/--host is set.
// Here that switch is HOMER_HOST or --host. Unset keeps the historical
// on-disk commands so existing machines and tests are unchanged.
func hubRequested(options CommandOptions) bool {
	return strings.TrimSpace(options.Host) != "" || strings.TrimSpace(os.Getenv("HOMER_HOST")) != ""
}

func hubClient(options CommandOptions) *hubclient.Client {
	return hubclient.New(hubclient.Endpoint(options.Host), hubclient.Token(options.Token, options.Home))
}

func agentTarget(options CommandOptions) (string, error) {
	if id := strings.TrimSpace(options.ID); id != "" {
		return id, nil
	}
	return hubclient.LocalAgentID(options.Home)
}

func runHubPS(options CommandOptions, out, errOut io.Writer) int {
	client := hubClient(options)
	body, code, err := client.Do(http.MethodGet, "/api/agents", nil, 20*time.Second)
	if err != nil {
		writeLine(errOut, "homer ps: "+err.Error())
		return 1
	}
	if code >= 300 {
		writeLine(errOut, "homer ps: 中心返回 "+http.StatusText(code))
		writeLine(errOut, strings.TrimSpace(string(body)))
		return 1
	}
	if options.JSON {
		writeLine(out, string(body))
		return 0
	}
	var payload struct {
		Agents []struct {
			AgentID  string    `json:"agentId"`
			Hostname string    `json:"hostname"`
			Version  string    `json:"version"`
			Stale    bool      `json:"stale"`
			LastSeen time.Time `json:"lastSeen"`
			Tools    []psTool  `json:"tools"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeLine(errOut, "homer ps: 中心返回的内容无法读取")
		return 1
	}
	if len(payload.Agents) == 0 {
		writeLine(out, "中心上还没有机器。")
		return 0
	}
	writeLine(out, "机器\t状态\t版本\t上次心跳\t应用")
	for _, agent := range payload.Agents {
		name := agent.Hostname
		if name == "" {
			name = agent.AgentID
		}
		state := "在线"
		if agent.Stale {
			state = "离线"
		}
		version := agent.Version
		if version == "" {
			version = "未上报"
		}
		seen := "—"
		if !agent.LastSeen.IsZero() {
			seen = agent.LastSeen.Local().Format("01-02 15:04:05")
		}
		writeLine(out, strings.Join([]string{name, state, version, seen, psToolsText(agent.Tools)}, "\t"))
	}
	return 0
}

// psTool is one program (pi, herdr, opencode...) on a machine, as the hub
// reports it.
type psTool struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Version  string `json:"version"`
	Latest   string `json:"latest"`
	Outdated bool   `json:"outdated"`
}

// psToolsText is the one cell that lists a machine's programs with their
// versions, saying so when one has fallen behind the fleet.
func psToolsText(tools []psTool) string {
	if len(tools) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(tools))
	for _, tool := range tools {
		name := tool.Label
		if name == "" {
			name = tool.ID
		}
		version := tool.Version
		if version == "" {
			version = "版本未知"
		}
		text := name + " " + version
		if tool.Outdated && tool.Latest != "" {
			text += "（落后，最新 " + tool.Latest + "）"
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, ", ")
}

func runHubStatus(options CommandOptions, out, errOut io.Writer) int {
	id, err := agentTarget(options)
	if err != nil {
		writeLine(errOut, "homer status: "+err.Error())
		return 1
	}
	client := hubClient(options)
	body, code, err := client.Do(http.MethodPost, "/api/agents/"+url.PathEscape(id)+"/status", map[string]any{}, 90*time.Second)
	if err != nil {
		writeLine(errOut, "homer status: "+err.Error())
		return 1
	}
	if code >= 300 {
		writeLine(errOut, fmt.Sprintf("homer status: 中心返回 %s", strings.TrimSpace(string(body))))
		return 1
	}
	if options.JSON {
		writeLine(out, string(body))
		return 0
	}
	text := renderHubStatus(client.BaseURL, id, body)
	if text != "" {
		writeLine(out, text)
	}
	return 0
}

func renderHubStatus(host, id string, body []byte) string {
	var payload struct {
		Report struct {
			Adapters []struct {
				ID        string `json:"id"`
				Push      int    `json:"push"`
				Pull      int    `json:"pull"`
				Conflicts int    `json:"conflicts"`
			} `json:"adapters"`
			Errors []string `json:"errors"`
		} `json:"report"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return strings.TrimSpace(string(body))
	}
	lines := []string{"中心: " + host, "机器: " + id}
	for _, adapter := range payload.Report.Adapters {
		if adapter.Push == 0 && adapter.Pull == 0 && adapter.Conflicts == 0 {
			continue
		}
		parts := []string{adapter.ID}
		if adapter.Push > 0 {
			parts = append(parts, fmt.Sprintf("%d 项未收取", adapter.Push))
		}
		if adapter.Pull > 0 {
			parts = append(parts, fmt.Sprintf("%d 项待下发", adapter.Pull))
		}
		if adapter.Conflicts > 0 {
			parts = append(parts, fmt.Sprintf("%d 项冲突", adapter.Conflicts))
		}
		lines = append(lines, strings.Join(parts, "  "))
	}
	if len(lines) == 2 {
		lines = append(lines, "已对齐")
	}
	for _, line := range payload.Report.Errors {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func runHubDiff(options CommandOptions, out, errOut io.Writer) int {
	id, err := agentTarget(options)
	if err != nil {
		writeLine(errOut, "homer diff: "+err.Error())
		return 1
	}
	query := url.Values{}
	if options.Adapter != "" {
		query.Set("adapter", options.Adapter)
	}
	if options.Category != "" {
		query.Set("category", options.Category)
	}
	path := "/api/agents/" + url.PathEscape(id) + "/diff"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	client := hubClient(options)
	body, code, err := client.Do(http.MethodPost, path, map[string]any{}, 90*time.Second)
	if err != nil {
		writeLine(errOut, "homer diff: "+err.Error())
		return 1
	}
	if code >= 300 {
		writeLine(errOut, "homer diff: "+strings.TrimSpace(string(body)))
		return 1
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeLine(out, strings.TrimSpace(string(body)))
		return 0
	}
	if payload.Text != "" {
		writeLine(out, payload.Text)
	}
	return 0
}

func runToken(options CommandOptions, out, errOut io.Writer) int {
	paths := ResolveHomerPaths(options.Home)
	token, created, err := hub.CreateHubToken(paths.Home, options.Force)
	if err != nil {
		writeLine(errOut, "homer token: "+err.Error())
		return 1
	}
	writeLine(out, token)
	if created {
		writeLine(errOut, "已写入 "+paths.Home+"/keys/hub-token。")
		writeLine(errOut, "正在运行的 homer serve 要重启后才会改用这一串。")
	} else if options.Force {
		writeLine(errOut, "令牌没有更换。")
	} else {
		writeLine(errOut, "这是现有的中心令牌。要换新的: homer token --force")
	}
	writeLine(errOut, "在其他机器上操作这个中心: export HOMER_HOST=<中心地址> HOMER_HUB_TOKEN=<上面这一行>")
	return 0
}

func runHubJoin(options CommandOptions, out, errOut io.Writer) int {
	client := hubClient(options)
	body, code, err := client.Do(http.MethodGet, "/api/auth/join", nil, 20*time.Second)
	if err != nil {
		writeLine(errOut, "homer join: "+err.Error())
		return 1
	}
	if code >= 300 {
		writeLine(errOut, "homer join: "+strings.TrimSpace(string(body)))
		return 1
	}
	var payload struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || strings.TrimSpace(payload.Command) == "" {
		writeLine(errOut, "homer join: 中心没有返回接入命令")
		return 1
	}
	writeLine(out, payload.Command)
	return 0
}

func runHubUpgrade(options CommandOptions, out, errOut io.Writer) int {
	id := strings.TrimSpace(options.ID)
	if id == "" {
		writeLine(errOut, "homer upgrade: 更新另一台机器需要 --id")
		return 1
	}
	client := hubClient(options)
	body, code, err := client.Do(http.MethodPost, "/api/agents/"+url.PathEscape(id)+"/upgrade", map[string]any{}, 4*time.Minute)
	if err != nil {
		writeLine(errOut, "homer upgrade: "+err.Error())
		return 1
	}
	if options.JSON {
		writeLine(out, string(body))
	}
	var report struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	_ = json.Unmarshal(body, &report)
	if code >= 300 || !report.OK {
		text := strings.TrimSpace(report.Note)
		if text == "" {
			text = strings.TrimSpace(string(body))
		}
		writeLine(errOut, "homer upgrade: "+text)
		return 1
	}
	if !options.JSON {
		writeLine(out, "已让 "+id+" 换成中心上的程序，agent 会自己重启")
	}
	return 0
}

func runHubResolve(options CommandOptions, out, errOut io.Writer) int {
	choice := "center"
	verb := "以中心为准"
	if options.AcceptLocal {
		choice = "local"
		verb = "以这台机器为准"
	}
	client := hubClient(options)
	if !options.Yes {
		writeLine(errOut, fmt.Sprintf("homer resolve: 这次会%s。中心是 %s。加上 --yes 才会执行。", verb, client.BaseURL))
		return 1
	}
	id, err := agentTarget(options)
	if err != nil {
		writeLine(errOut, "homer resolve: "+err.Error())
		return 1
	}
	body := map[string]any{}
	if len(options.Adapters) > 0 {
		body["adapters"] = options.Adapters
	}
	path := "/api/resolve?choice=" + url.QueryEscape(choice) + "&agent=" + url.QueryEscape(id) + "&confirm=true"
	payload, code, err := client.Do(http.MethodPost, path, body, 12*time.Minute)
	if err != nil {
		writeLine(errOut, "homer resolve: "+err.Error())
		return 1
	}
	if options.JSON {
		writeLine(out, string(payload))
	}
	var report struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(payload, &report)
	if code >= 300 || !report.OK {
		text := strings.TrimSpace(string(payload))
		if len(report.Errors) > 0 {
			text = strings.Join(report.Errors, "\n")
		}
		writeLine(errOut, "homer resolve: 没有完成")
		if text != "" {
			writeLine(errOut, text)
		}
		return 1
	}
	if !options.JSON {
		writeLine(out, "已在 "+id+" 上"+verb)
	}
	return 0
}

func runHubPush(options CommandOptions, out, errOut io.Writer) int {
	return runHubWrite("push", "收取", "/api/sync?direction=collect&agent=%s&confirm=true", options, out, errOut)
}

func runHubPull(options CommandOptions, out, errOut io.Writer) int {
	return runHubWrite("pull", "下发", "/api/agents/%s/pull?confirm=true", options, out, errOut)
}

func runHubWrite(command, verb, pathFmt string, options CommandOptions, out, errOut io.Writer) int {
	client := hubClient(options)
	if !options.Yes {
		writeLine(errOut, fmt.Sprintf("homer %s: 这次会%s。中心是 %s。加上 --yes 才会执行。", command, verb, client.BaseURL))
		return 1
	}
	id, err := agentTarget(options)
	if err != nil {
		writeLine(errOut, "homer "+command+": "+err.Error())
		return 1
	}
	body := map[string]any{}
	if len(options.Adapters) > 0 {
		body["adapters"] = options.Adapters
	}
	path := fmt.Sprintf(pathFmt, url.PathEscape(id))
	payload, code, err := client.Do(http.MethodPost, path, body, 12*time.Minute)
	if err != nil {
		writeLine(errOut, "homer "+command+": "+err.Error())
		return 1
	}
	if options.JSON {
		writeLine(out, string(payload))
	}
	var report struct {
		OK     bool     `json:"ok"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(payload, &report)
	if code >= 300 || !report.OK {
		if !options.JSON {
			text := strings.TrimSpace(string(payload))
			if len(report.Errors) > 0 {
				text = strings.Join(report.Errors, "\n")
			}
			writeLine(errOut, "homer "+command+": "+verb+"没有完成")
			if text != "" {
				writeLine(errOut, text)
			}
		}
		return 1
	}
	if !options.JSON {
		writeLine(out, fmt.Sprintf("已向 %s %s %s", client.BaseURL, verb, id))
	}
	return 0
}
