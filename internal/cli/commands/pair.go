package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/orderedjson"
	"github.com/zzjcool/homer-cli/internal/pair"
)

// PairOptions controls the serve (empty Addr) or join (non-empty Addr) side
// of the pairing command.
type PairOptions struct {
	HomerHome string
	Addr      string
	Yes       bool
	JSON      bool
}

// PairDeps contains the command's protocol injection points. A custom
// transport is the test seam that bypasses the optional tailcat executable;
// the Age and UI seams keep the all-or-nothing pipeline deterministic in
// tests.
type PairDeps struct {
	UI        PromptPort
	Transport pair.PairTransport
	Age       agecrypto.AgeCryptoPort
	OnAddr    func(addr string)
}

type PairPeerSummary struct {
	Hostname  string `json:"hostname"`
	Recipient string `json:"recipient"`
}

type PairServeStatus string

const (
	PairServeStatusNoTailcat  PairServeStatus = "no-tailcat"
	PairServeStatusNoConfig   PairServeStatus = "no-config"
	PairServeStatusNoSource   PairServeStatus = "missing-source"
	PairServeStatusAborted    PairServeStatus = "aborted"
	PairServeStatusPeerFailed PairServeStatus = "peer-failed"
	PairServeStatusPaired     PairServeStatus = "paired"
	PairServeStatusError      PairServeStatus = "error"
)

type PairServeReport struct {
	OK             bool             `json:"ok"`
	Status         PairServeStatus  `json:"status"`
	Addr           string           `json:"addr,omitempty"`
	Peer           *PairPeerSummary `json:"peer,omitempty"`
	Sent           []string         `json:"sent"`
	RecipientAdded bool             `json:"recipientAdded"`
	PairedCount    int              `json:"pairedCount"`
	Warnings       []string         `json:"warnings"`
	Errors         []string         `json:"errors"`
}

func (report PairServeReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

type PairJoinStatus string

const (
	PairJoinStatusNoTailcat     PairJoinStatus = "no-tailcat"
	PairJoinStatusNoConfig      PairJoinStatus = "no-config"
	PairJoinStatusNoIdentity    PairJoinStatus = "no-identity"
	PairJoinStatusDeclined      PairJoinStatus = "declined"
	PairJoinStatusUnknownSecret PairJoinStatus = "unknown-secret"
	PairJoinStatusUndecryptable PairJoinStatus = "undecryptable"
	PairJoinStatusPaired        PairJoinStatus = "paired"
	PairJoinStatusError         PairJoinStatus = "error"
)

type PairJoinReport struct {
	OK        bool             `json:"ok"`
	Status    PairJoinStatus   `json:"status"`
	Peer      *PairPeerSummary `json:"peer,omitempty"`
	Applied   []string         `json:"applied"`
	BackupDir string           `json:"backupDir,omitempty"`
	Warnings  []string         `json:"warnings"`
	Errors    []string         `json:"errors"`
}

func (report PairJoinReport) ExitCode() int {
	if report.OK {
		return 0
	}
	return 1
}

const (
	pairJoinAckUndeliveredWarning = "目标文件与 vault 已写入且验证通过，仅确认未送达 A；可重试 pair"
	pairServePeerWriteWarning     = "B 端可能已完成写入；可检查目标文件与 vault，或重试 pair"
)

const PAIR_USAGE = `用法: homer pair [options]           （机器 A：serve，输出一次性地址并等待）
      homer pair <tc-addr> [options]  （机器 B：join，完成配对后退出）

两台机器在线时的密钥快车道：tailcat 建立 WireGuard 加密管道（可选外部
依赖），传输内容全部为 age 密文——即使地址泄露，拿到的只是密文。

流程:
  1. 机器 A 运行 homer pair，把输出的一次性地址通过安全信道（当面/
     私密消息）交给机器 B；切勿粘贴到 git、issue 或群聊。
  2. 机器 B 运行 homer pair <tc-addr>；A 端确认对方 hostname 与
     recipient 指纹后放行（非 TTY 需 --yes）。
  3. A 将全部密钥以 age 多 recipient 密文（A 现有 recipients + B）发送；
     B 解密验证后写入 secrets.files 登记的目标路径（0600，覆盖前备份），
     并同步写入自己的 secrets/ vault。

前置: 两端已完成 homer init / homer home；B 端已 homer secret keygen；
     两端 PATH 中有 tailcat（官方 INSTALL:
     https://github.com/tailscale/tailcat，建议 v0.7.0+）。

选项:
  --home <dir>       homer 工作区（默认 $HOMER_HOME 或 ~/.homer）
  --yes              serve 端跳过对端确认（非 TTY 必须）；join 端无操作
  --json             输出机器可读 JSON
  -h, --help         显示本帮助

退出码: 配对成功 → 0；tailcat 缺失 / 用法错误 / 对端拒绝或传输失败 → 1。

安全: 地址即凭证（bearer secret）且进程结束即失效；无 tailcat 时请继续
使用 homer secret push / homer secret pull（age 密文走 git）。`

// RunPairServe implements the complete serve-side protocol. The only write
// before the confirmation gate is the transport's ephemeral session; config
// and state remain untouched when the local user refuses the peer.
func RunPairServe(options PairOptions, deps *PairDeps) PairServeReport {
	return runPairServe(context.Background(), options, deps, nil)
}

func runPairServe(ctx context.Context, options PairOptions, deps *PairDeps, output io.Writer) PairServeReport {
	report := newPairServeReport(PairServeStatusError)
	ctx = nonNilPairContext(ctx)
	if err := ctx.Err(); err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if !pairTailcatAvailable(deps) {
		return noTailcatServeReport()
	}

	paths := resolveCommandPaths(options.HomerHome)
	config, err := loadPairConfig(paths)
	if err != nil {
		return noConfigServeReport(err)
	}

	transport, err := pairTransportFor(deps)
	if err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}

	// Serve owns the whole session, not just the accept window. A deadline
	// here would kill a healthy DERP session after AcceptDeadline; the bounded
	// child context below is only for waiting for the first peer connection.
	serveContext := ctx
	server, err := transport.Serve(serveContext)
	if err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	defer func() { _ = server.Close() }()

	report.Addr = server.Addr()
	if report.Addr == "" {
		report.Errors = append(report.Errors, "pair transport returned an empty address")
		return report
	}
	if !options.JSON {
		if deps != nil && deps.OnAddr != nil {
			deps.OnAddr(report.Addr)
		} else if output != nil {
			_, _ = fmt.Fprintln(output, "🐈 pair 地址（一次性，仅带外交换，勿入 git/聊天记录）: "+report.Addr)
		} else {
			_, _ = fmt.Fprintln(os.Stdout, "🐈 pair 地址（一次性，仅带外交换，勿入 git/聊天记录）: "+report.Addr)
		}
	}

	acceptContext, acceptCancel := context.WithTimeout(ctx, pair.AcceptDeadline)
	conn, err := server.Accept(acceptContext)
	acceptCancel()
	if err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	defer func() { _ = conn.Close() }()

	// Accept only establishes the tailcat stream; it does not mean the peer
	// has sent hello yet. Keep this first read on the full accept window.
	frameType, payload, err := readPairFrameContext(ctx, conn, pair.AcceptDeadline)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			report.Errors = append(report.Errors, "等待对端 hello 超时")
		} else {
			report.Errors = append(report.Errors, safeError(err))
		}
		return report
	}
	if frameType == pair.FrameAbort || frameType == pair.FrameError {
		report.Status = PairServeStatusPeerFailed
		report.Errors = append(report.Errors, pairPeerFailureMessage("对端中止了配对", frameType, payload))
		return report
	}
	if frameType != pair.FrameHello {
		failAbort(ctx, conn, "expected peer hello", nil, nil)
		report.Status = PairServeStatusPeerFailed
		report.Errors = append(report.Errors, "对端未发送 hello")
		return report
	}
	hello, err := pair.DecodeHello(payload)
	if err != nil {
		failAbort(ctx, conn, "invalid peer hello", nil, nil)
		report.Status = PairServeStatusPeerFailed
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if err := pair.ValidateHello(hello); err != nil {
		failAbort(ctx, conn, safeError(err), nil, nil)
		report.Status = PairServeStatusPeerFailed
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	report.Peer = &PairPeerSummary{Hostname: hello.Hostname, Recipient: hello.Recipient}

	confirmed := options.Yes
	if !confirmed {
		confirmed = promptConfirmContext(ctx, pairUI(deps), pair.PeerDisplay(hello)+"\n是否接受此设备并发送密钥？", false)
	}
	if err := ctx.Err(); err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if !confirmed {
		if err := sendPairFrameContext(ctx, conn, pair.FrameDecline, pair.Decline{Reason: "本机用户拒绝了配对请求"}); err != nil {
			report.Warnings = append(report.Warnings, "拒绝通知未能发送给对端")
		}
		report.Status = PairServeStatusAborted
		report.Errors = append(report.Errors, "已拒绝对端配对请求；未修改 homer.json 或 state.json")
		return report
	}

	updatedConfig, recipientAdded := configWithRecipient(config, hello.Recipient)
	if err := core.SaveConfig(paths, updatedConfig); err != nil {
		failAbort(ctx, conn, "无法保存配对 recipient", &report.Errors, err)
		return report
	}
	report.RecipientAdded = recipientAdded
	config = &updatedConfig
	report.Warnings = appendRecipientWarning(report.Warnings)

	names := agecrypto.SecretNames(config)
	plaintexts := make(map[string][]byte, len(names))
	missing := make([]string, 0)
	for _, name := range names {
		destination, destinationErr := agecrypto.DestinationOf(config, name)
		if destinationErr != nil {
			missing = append(missing, fmt.Sprintf("%s → %s", name, safeError(destinationErr)))
			continue
		}
		plaintext, readErr := os.ReadFile(destination)
		if readErr != nil {
			missing = append(missing, fmt.Sprintf("%s → %s", name, destination))
			continue
		}
		plaintexts[name] = plaintext
	}
	if len(missing) > 0 {
		failAbort(ctx, conn, "源文件缺失", nil, nil)
		report.Status = PairServeStatusNoSource
		report.Errors = append(report.Errors, fmt.Sprintf("以下目标文件不可读（%d/%d），已中止且未发送任何密文：", len(missing), len(names)))
		report.Errors = append(report.Errors, missing...)
		return report
	}

	recipients := append([]string(nil), config.Secrets.Recipients...)
	ciphertexts := make(map[string][]byte, len(names))
	totalBytes := 0
	crypto := pairAge(deps)
	for _, name := range names {
		ciphertext, encryptErr := crypto.Encrypt(plaintexts[name], recipients)
		if encryptErr != nil {
			failAbort(ctx, conn, "无法生成 age 密文", nil, nil)
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %s", name, safeError(encryptErr)))
			return report
		}
		if !agecrypto.CiphertextLooksSafe(ciphertext, plaintexts[name]) {
			failAbort(ctx, conn, "age 密文完整性预检失败", nil, nil)
			report.Errors = append(report.Errors, fmt.Sprintf("%s: age 密文完整性预检失败", name))
			return report
		}
		if len(ciphertext) > pair.MaxFrameBytes || totalBytes > pair.MaxTotalBundleBytes-len(ciphertext) {
			failAbort(ctx, conn, "密钥 bundle 超过传输大小限制", nil, nil)
			report.Errors = append(report.Errors, "密钥 bundle 超过传输大小限制")
			return report
		}
		ciphertexts[name] = ciphertext
		totalBytes += len(ciphertext)
	}

	offer := pair.Offer{Version: pair.ProtocolVersion, Files: make([]pair.OfferFile, 0, len(names))}
	for _, name := range names {
		offer.Files = append(offer.Files, pair.OfferFile{Name: name, Size: len(ciphertexts[name])})
	}
	if err := sendPairFrameContext(ctx, conn, pair.FrameOffer, offer); err != nil {
		report.Status = PairServeStatusPeerFailed
		report.Warnings = append(report.Warnings, pairServePeerWriteWarning)
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	for _, name := range names {
		if err := writePairPayloadContext(ctx, conn, pair.FrameBlob, ciphertexts[name]); err != nil {
			report.Status = PairServeStatusPeerFailed
			report.Warnings = append(report.Warnings, pairServePeerWriteWarning)
			report.Errors = append(report.Errors, safeError(err))
			return report
		}
		report.Sent = append(report.Sent, name)
	}

	frameType, payload, err = readPairFrameContext(ctx, conn, pair.StepDeadline)
	if err != nil {
		report.Status = PairServeStatusPeerFailed
		report.Warnings = append(report.Warnings, pairServePeerWriteWarning)
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if frameType == pair.FrameAbort || frameType == pair.FrameError {
		report.Status = PairServeStatusPeerFailed
		report.Warnings = append(report.Warnings, pairServePeerWriteWarning)
		report.Errors = append(report.Errors, pairPeerFailureMessage("对端中止配对", frameType, payload))
		return report
	}
	if frameType != pair.FrameAck {
		report.Status = PairServeStatusPeerFailed
		report.Warnings = append(report.Warnings, pairServePeerWriteWarning)
		report.Errors = append(report.Errors, "对端未发送 ack")
		return report
	}
	ack, err := pair.DecodeAck(payload)
	if err != nil {
		report.Status = PairServeStatusPeerFailed
		report.Warnings = append(report.Warnings, pairServePeerWriteWarning)
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if !ack.OK {
		report.Status = PairServeStatusPeerFailed
		report.Warnings = append(report.Warnings, pairServePeerWriteWarning)
		if len(ack.Errors) > 0 {
			report.Errors = append(report.Errors, ack.Errors...)
		} else {
			report.Errors = append(report.Errors, "对端报告配对失败")
		}
		return report
	}

	state := core.LoadState(paths)
	state = state.AddPairedDevice(core.PairedDevice{
		Hostname:  hello.Hostname,
		Recipient: hello.Recipient,
		PairedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err := core.SaveState(paths, state); err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	report.PairedCount = len(state.Paired)
	report.Status = PairServeStatusPaired
	report.OK = true
	report.Warnings = append(report.Warnings, "提示: homer.json 已更新；如需 git 通道，请随后运行 `homer push`。")
	return report
}

// RunPairJoin implements the complete join-side protocol. It never writes a
// vault or destination until every offered blob has been received, decrypted,
// and passed the ciphertext self-check.
func RunPairJoin(options PairOptions, deps *PairDeps) PairJoinReport {
	return runPairJoin(context.Background(), options, deps, nil)
}

func runPairJoin(ctx context.Context, options PairOptions, deps *PairDeps, output io.Writer) PairJoinReport {
	report := newPairJoinReport(PairJoinStatusError)
	ctx = nonNilPairContext(ctx)
	if err := ctx.Err(); err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if !pairTailcatAvailable(deps) {
		return noTailcatJoinReport()
	}

	paths := resolveCommandPaths(options.HomerHome)
	config, err := loadPairConfig(paths)
	if err != nil {
		return noConfigJoinReport(err)
	}
	identity, identityOK := agecrypto.LoadIdentity(paths)
	if !identityOK {
		report.Status = PairJoinStatusNoIdentity
		report.Errors = append(report.Errors,
			fmt.Sprintf("未找到本机 age identity: %s", agecrypto.IdentityFilePath(paths)),
			"请先运行 `homer secret keygen` 生成本机私钥。",
		)
		return report
	}

	transport, err := pairTransportFor(deps)
	if err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	connectContext, connectCancel := context.WithTimeout(ctx, pair.StepDeadline)
	conn, err := transport.Connect(connectContext, options.Addr)
	connectCancel()
	if err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	defer func() { _ = conn.Close() }()

	hostname, err := os.Hostname()
	if err != nil {
		report.Errors = append(report.Errors, "无法读取本机 hostname")
		return report
	}
	hello := pair.PeerHello{Version: pair.ProtocolVersion, Hostname: hostname, Recipient: identity.Recipient}
	if err := pair.ValidateHello(hello); err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if err := sendPairFrameContext(ctx, conn, pair.FrameHello, hello); err != nil {
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	if !options.JSON && output != nil {
		_, _ = fmt.Fprintf(output, "等待对端确认…（最长 %ds）\n", int(pair.StepDeadline/time.Second))
	}

	frameType, payload, err := readPairFrameContext(ctx, conn, pair.StepDeadline)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			report.Errors = append(report.Errors, "等待确认超时")
		} else {
			report.Errors = append(report.Errors, safeError(err))
		}
		return report
	}
	switch frameType {
	case pair.FrameDecline:
		decline, decodeErr := pair.DecodeDecline(payload)
		report.Status = PairJoinStatusDeclined
		if decodeErr != nil || decline.Reason == "" {
			report.Errors = append(report.Errors, "serve 端拒绝了配对请求")
		} else {
			report.Errors = append(report.Errors, "serve 端拒绝了配对请求: "+decline.Reason)
		}
		return report
	case pair.FrameAbort, pair.FrameError:
		report.Errors = append(report.Errors, pairPeerFailureMessage("serve 端中止了配对", frameType, payload))
		return report
	case pair.FrameOffer:
		// Continue below.
	default:
		failAbort(ctx, conn, "expected offer", nil, nil)
		report.Errors = append(report.Errors, "serve 端未发送 offer")
		return report
	}

	offer, err := pair.DecodeOffer(payload)
	if err != nil {
		failAbort(ctx, conn, "invalid offer", &report.Errors, err)
		return report
	}
	unknown, offerErr := validatePairOffer(config, offer)
	if offerErr != nil {
		// If the offer shape is usable, consume the announced blobs before
		// sending Abort. This prevents a synchronous net.Pipe writer on the
		// serve side from being stranded while the join side reports failure.
		if offer.Version == pair.ProtocolVersion && offerFilesDrainable(offer) {
			_ = drainPairBlobsContext(ctx, conn, offer)
		}
		failAbort(ctx, conn, messageForPairError(offerErr), &report.Errors, offerErr)
		return report
	}
	if len(unknown) > 0 {
		if err := drainPairBlobsContext(ctx, conn, offer); err != nil {
			report.Errors = append(report.Errors, safeError(err))
			return report
		}
		failAbort(ctx, conn, "offer 包含本机未配置的密钥", nil, nil)
		report.Status = PairJoinStatusUnknownSecret
		report.Errors = append(report.Errors, "offer 包含本机未配置的密钥: "+strings.Join(unknown, ", "))
		return report
	}

	crypto := pairAge(deps)
	ciphertexts := make(map[string][]byte, len(offer.Files))
	plaintexts := make(map[string][]byte, len(offer.Files))
	failures := make([]string, 0)
	allBlobsRead := true
	for _, file := range offer.Files {
		frameType, payload, err := readPairFrameContext(ctx, conn, pair.StepDeadline)
		if err != nil {
			failures = append(failures, file.Name+": "+safeError(err))
			allBlobsRead = false
			break
		}
		if frameType == pair.FrameAbort || frameType == pair.FrameError {
			failures = append(failures, file.Name+": "+pairPeerFailureMessage("serve 端中止了配对", frameType, payload))
			allBlobsRead = false
			break
		}
		if frameType != pair.FrameBlob {
			failures = append(failures, file.Name+": blob frame type mismatch")
			allBlobsRead = false
			break
		}
		if len(payload) != file.Size {
			// The malformed blob has already been consumed, so continue
			// draining the remaining announced blobs. That preserves the
			// all-or-nothing boundary and lets the serve side observe a clean
			// Abort.
			failures = append(failures, file.Name+": blob size mismatch")
			continue
		}
		ciphertexts[file.Name] = append([]byte(nil), payload...)
		plaintext, decryptErr := crypto.Decrypt(payload, identity)
		if decryptErr != nil {
			failures = append(failures, file.Name+": "+safeError(decryptErr))
			continue
		}
		if !agecrypto.CiphertextLooksSafe(payload, plaintext) {
			failures = append(failures, file.Name+": age ciphertext integrity preflight failed")
			continue
		}
		plaintexts[file.Name] = plaintext
	}
	if len(failures) > 0 {
		if allBlobsRead {
			failAbort(ctx, conn, "密文无法用本机 identity 验证", nil, nil)
		}
		report.Status = PairJoinStatusUndecryptable
		report.Errors = append(report.Errors, fmt.Sprintf("以下密文无法验证（%d/%d），未写入任何 vault 或目标文件：", len(failures), len(offer.Files)))
		report.Errors = append(report.Errors, failures...)
		return report
	}

	names := make([]string, 0, len(offer.Files))
	for _, file := range offer.Files {
		names = append(names, file.Name)
	}
	backupDir, backupErr := backupSecretDestinations(paths, config, names)
	if backupErr != nil {
		failAbort(ctx, conn, "无法备份现有目标文件", nil, nil)
		report.Errors = append(report.Errors, safeError(backupErr))
		return report
	}
	report.BackupDir = backupDir
	for _, name := range names {
		if err := agecrypto.WriteVaultCiphertext(paths, name, ciphertexts[name]); err != nil {
			failAbort(ctx, conn, "无法写入 vault", nil, nil)
			report.Errors = append(report.Errors, safeError(err))
			return report
		}
		destination, destinationErr := agecrypto.DestinationOf(config, name)
		if destinationErr != nil {
			failAbort(ctx, conn, "无法解析目标文件", nil, nil)
			report.Errors = append(report.Errors, safeError(destinationErr))
			return report
		}
		if err := writeSecretDestination(destination, plaintexts[name]); err != nil {
			failAbort(ctx, conn, "无法写入目标文件", nil, nil)
			report.Errors = append(report.Errors, safeError(err))
			return report
		}
		report.Applied = append(report.Applied, name)
	}

	ack := pair.Ack{OK: true, Applied: append([]string(nil), report.Applied...), BackupDir: report.BackupDir}
	if err := sendPairFrameContext(ctx, conn, pair.FrameAck, ack); err != nil {
		report.Warnings = append(report.Warnings, pairJoinAckUndeliveredWarning)
		report.Errors = append(report.Errors, safeError(err))
		return report
	}
	report.Status = PairJoinStatusPaired
	report.OK = true
	return report
}

// ExecutePair dispatches on options.Addr and renders the final report. The
// actual transport/session handles are closed by the RunPair* defers, so a
// caller can safely use this synchronous entry point from the CLI.
func ExecutePair(options PairOptions, deps *PairDeps, out, errOut io.Writer) int {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if options.Addr == "" {
		report := runPairServe(ctx, options, deps, out)
		if options.JSON {
			_ = writeOrderedJSON(out, pairServeValue(report))
		} else {
			_, _ = fmt.Fprintln(out, RenderPairServeReport(report))
		}
		writePairPreflightErrors(report.Status == PairServeStatusNoTailcat || report.Status == PairServeStatusNoConfig, report.Errors, errOut)
		return report.ExitCode()
	}

	report := runPairJoin(ctx, options, deps, out)
	if options.JSON {
		_ = writeOrderedJSON(out, pairJoinValue(report))
	} else {
		_, _ = fmt.Fprintln(out, RenderPairJoinReport(report))
	}
	writePairPreflightErrors(report.Status == PairJoinStatusNoTailcat || report.Status == PairJoinStatusNoConfig, report.Errors, errOut)
	return report.ExitCode()
}

func RenderPairServeReport(report PairServeReport) string {
	lines := []string{fmt.Sprintf("homer pair: %s", report.Status)}
	if report.Addr != "" {
		lines = append(lines, "  地址: "+report.Addr)
	}
	if report.Peer != nil {
		hello := pair.PeerHello{Hostname: report.Peer.Hostname, Recipient: report.Peer.Recipient}
		lines = append(lines, "  对端: "+pair.PeerDisplay(hello))
	}
	lines = append(lines, fmt.Sprintf("  已发送: %d 个密钥", len(report.Sent)))
	if report.PairedCount > 0 {
		lines = append(lines, fmt.Sprintf("  已配对设备: %d", report.PairedCount))
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, err := range report.Errors {
		for _, line := range strings.Split(err, "\n") {
			if line != "" {
				lines = append(lines, "  ✗ "+line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func RenderPairJoinReport(report PairJoinReport) string {
	lines := []string{fmt.Sprintf("homer pair: %s", report.Status)}
	if report.Peer != nil {
		hello := pair.PeerHello{Hostname: report.Peer.Hostname, Recipient: report.Peer.Recipient}
		lines = append(lines, "  对端: "+pair.PeerDisplay(hello))
	}
	lines = append(lines, fmt.Sprintf("  已归位: %d 个密钥", len(report.Applied)))
	if report.BackupDir != "" {
		lines = append(lines, "  备份目录: "+report.BackupDir)
	}
	for _, warning := range report.Warnings {
		lines = append(lines, "  ⚠ "+warning)
	}
	for _, err := range report.Errors {
		for _, line := range strings.Split(err, "\n") {
			if line != "" {
				lines = append(lines, "  ✗ "+line)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func newPairServeReport(status PairServeStatus) PairServeReport {
	return PairServeReport{
		OK:       status == PairServeStatusPaired,
		Status:   status,
		Sent:     []string{},
		Warnings: []string{},
		Errors:   []string{},
	}
}

func newPairJoinReport(status PairJoinStatus) PairJoinReport {
	return PairJoinReport{
		OK:       status == PairJoinStatusPaired,
		Status:   status,
		Applied:  []string{},
		Warnings: []string{},
		Errors:   []string{},
	}
}

func noTailcatServeReport() PairServeReport {
	report := newPairServeReport(PairServeStatusNoTailcat)
	report.Errors = append(report.Errors, pair.TailcatInstallHint)
	return report
}

func noTailcatJoinReport() PairJoinReport {
	report := newPairJoinReport(PairJoinStatusNoTailcat)
	report.Errors = append(report.Errors, pair.TailcatInstallHint)
	return report
}

func noConfigServeReport(err error) PairServeReport {
	report := newPairServeReport(PairServeStatusNoConfig)
	report.Errors = append(report.Errors, safeError(err))
	return report
}

func noConfigJoinReport(err error) PairJoinReport {
	report := newPairJoinReport(PairJoinStatusNoConfig)
	report.Errors = append(report.Errors, safeError(err))
	return report
}

func pairTailcatAvailable(deps *PairDeps) bool {
	// An injected transport is the explicit test seam and does not require the
	// optional production binary. The P2 tailcat adapter supplies the default
	// transport when it is present in the full release branch.
	if deps != nil && deps.Transport != nil {
		return true
	}
	_, err := pair.DetectTailcat(nil)
	return err == nil
}

func pairTransportFor(deps *PairDeps) (pair.PairTransport, error) {
	if deps != nil && deps.Transport != nil {
		return deps.Transport, nil
	}
	// Production default: the P2 tailcat CLI adapter.
	transport, err := pair.NewTailcatTransport(pair.TailcatOptions{
		Stderr: os.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("tailcat transport: %w", err)
	}
	return transport, nil
}

func loadPairConfig(paths core.HomerPaths) (*core.HomerConfig, error) {
	config, err := core.LoadConfig(paths)
	if err != nil || config == nil {
		return nil, core.NewCliError(fmt.Sprintf("未找到 homer 配置: %s\n请先运行 `homer init` 生成 homer.json 与 store 快照。", paths.ConfigFile))
	}
	if config.Secrets == nil || len(config.Secrets.Files) == 0 {
		return nil, core.NewCliError("homer.json 的 secrets.files 为空：没有可配对的密钥。")
	}
	return config, nil
}

func pairUI(deps *PairDeps) PromptPort {
	if deps != nil && deps.UI != nil {
		return deps.UI
	}
	return nil
}

func pairAge(deps *PairDeps) agecrypto.AgeCryptoPort {
	if deps != nil && deps.Age != nil {
		return deps.Age
	}
	return agecrypto.NewAgeCryptoPort()
}

func configWithRecipient(config *core.HomerConfig, recipient string) (core.HomerConfig, bool) {
	updated := *config
	secrets := *config.Secrets
	secrets.Recipients = append([]string(nil), config.Secrets.Recipients...)
	for _, existing := range secrets.Recipients {
		if existing == recipient {
			updated.Secrets = &secrets
			return updated, false
		}
	}
	secrets.Recipients = append(secrets.Recipients, recipient)
	updated.Secrets = &secrets
	return updated, true
}

func appendRecipientWarning(warnings []string) []string {
	return append(warnings, "B recipient 已写入 homer.json；配对未完成时可保留或重试。")
}

type pairFrameResult struct {
	frameType uint8
	payload   []byte
	err       error
}

func nonNilPairContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func readPairFrameContext(parent context.Context, conn io.ReadWriteCloser, timeout time.Duration) (uint8, []byte, error) {
	if conn == nil {
		return 0, nil, errors.New("pair connection is nil")
	}
	result := make(chan pairFrameResult, 1)
	go func() {
		frameType, payload, err := pair.ReadFrame(conn, pair.MaxFrameBytes)
		result <- pairFrameResult{frameType: frameType, payload: payload, err: err}
	}()
	ctx, cancel := context.WithTimeout(nonNilPairContext(parent), timeout)
	defer cancel()
	select {
	case result := <-result:
		return result.frameType, result.payload, result.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func writePairFrameContext(ctx context.Context, conn io.ReadWriteCloser, frameType uint8, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writePairPayloadContext(ctx, conn, frameType, payload)
}

func writePairPayloadContext(parent context.Context, conn io.ReadWriteCloser, frameType uint8, payload []byte) error {
	if conn == nil {
		return errors.New("pair connection is nil")
	}
	result := make(chan error, 1)
	go func() { result <- pair.WriteFrame(conn, frameType, payload) }()
	ctx, cancel := context.WithTimeout(nonNilPairContext(parent), pair.StepDeadline)
	defer cancel()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sendPairFrameContext(ctx context.Context, conn io.ReadWriteCloser, frameType uint8, value any) error {
	return writePairFrameContext(ctx, conn, frameType, value)
}

func sendPairAbortContext(ctx context.Context, conn io.ReadWriteCloser, reason string) error {
	return sendPairFrameContext(ctx, conn, pair.FrameAbort, pair.Abort{Reason: reason})
}

// failAbort is the common three-step protocol failure path: best-effort abort,
// preserve the local cause, and let the caller's deferred Close tear down the
// transport. It deliberately does not replace a useful local error with a
// failed notification write.
func failAbort(ctx context.Context, conn io.ReadWriteCloser, reason string, errorsOut *[]string, cause error) {
	_ = sendPairAbortContext(ctx, conn, reason)
	if errorsOut != nil && cause != nil {
		*errorsOut = append(*errorsOut, safeError(cause))
	}
}

func pairPeerFailureMessage(prefix string, _ uint8, payload []byte) string {
	abort, err := pair.DecodeAbort(payload)
	if err != nil || strings.TrimSpace(abort.Reason) == "" {
		return prefix
	}
	return prefix + ": " + abort.Reason
}

func promptConfirmContext(ctx context.Context, value any, message string, fallback bool) bool {
	ctx = nonNilPairContext(ctx)
	if err := ctx.Err(); err != nil {
		return false
	}
	result := make(chan bool, 1)
	go func() { result <- promptConfirm(value, message, fallback) }()
	select {
	case confirmed := <-result:
		return confirmed
	case <-ctx.Done():
		return false
	}
}

func validatePairOffer(config *core.HomerConfig, offer pair.Offer) ([]string, error) {
	if offer.Version != pair.ProtocolVersion {
		return nil, errors.New("unsupported pair offer version")
	}
	if len(offer.Files) == 0 {
		return nil, errors.New("pair offer contains no files")
	}
	seen := make(map[string]struct{}, len(offer.Files))
	unknown := make([]string, 0)
	total := 0
	for _, file := range offer.Files {
		if !agecrypto.SecretNameValid(file.Name) {
			return nil, errors.New("pair offer contains an invalid secret name")
		}
		if _, exists := seen[file.Name]; exists {
			return nil, errors.New("pair offer contains duplicate secret names")
		}
		seen[file.Name] = struct{}{}
		if file.Size <= 0 || file.Size > pair.MaxFrameBytes || total > pair.MaxTotalBundleBytes-file.Size {
			return nil, errors.New("pair offer exceeds bundle size limit")
		}
		total += file.Size
		if config.Secrets == nil {
			unknown = append(unknown, file.Name)
			continue
		}
		if _, exists := config.Secrets.Files[file.Name]; !exists {
			unknown = append(unknown, file.Name)
		}
	}
	return unknown, nil
}

func offerFilesDrainable(offer pair.Offer) bool {
	if len(offer.Files) == 0 || offer.Version != pair.ProtocolVersion {
		return false
	}
	total := 0
	for _, file := range offer.Files {
		// Names and duplicate detection are handled by validatePairOffer. For
		// a malformed offer we still drain any safely sized frames so the
		// synchronous serve writer can observe the Abort instead of deadlocking
		// on net.Pipe.
		if file.Size < 0 || file.Size > pair.MaxFrameBytes || total > pair.MaxTotalBundleBytes-file.Size {
			return false
		}
		total += file.Size
	}
	return true
}

func drainPairBlobsContext(ctx context.Context, conn io.ReadWriteCloser, offer pair.Offer) error {
	for _, file := range offer.Files {
		frameType, payload, err := readPairFrameContext(ctx, conn, pair.StepDeadline)
		if err != nil {
			return err
		}
		if frameType == pair.FrameAbort || frameType == pair.FrameError {
			return errors.New(pairPeerFailureMessage("pair offer aborted by peer", frameType, payload))
		}
		if frameType != pair.FrameBlob || len(payload) != file.Size {
			return errors.New("pair offer blob does not match its declared size")
		}
	}
	return nil
}

func messageForPairError(err error) string {
	if err == nil {
		return "pair protocol aborted"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "pair protocol aborted"
	}
	return message
}

func writePairPreflightErrors(enabled bool, messages []string, out io.Writer) {
	if !enabled || out == nil {
		return
	}
	for _, message := range messages {
		for _, line := range strings.Split(message, "\n") {
			if strings.TrimSpace(line) != "" {
				_, _ = fmt.Fprintln(out, line)
			}
		}
	}
}

func pairServeValue(report PairServeReport) orderedjson.Value {
	keys := []string{"ok", "status"}
	values := map[string]orderedjson.Value{
		"ok":             report.OK,
		"status":         string(report.Status),
		"sent":           orderedStringArray(report.Sent),
		"recipientAdded": report.RecipientAdded,
		"pairedCount":    report.PairedCount,
		"warnings":       orderedStringArray(report.Warnings),
		"errors":         orderedStringArray(report.Errors),
	}
	if report.Addr != "" {
		keys = append(keys, "addr")
		values["addr"] = report.Addr
	}
	if report.Peer != nil {
		keys = append(keys, "peer")
		values["peer"] = pairPeerValue(report.Peer)
	}
	keys = append(keys, "sent", "recipientAdded", "pairedCount", "warnings", "errors")
	return &orderedjson.Object{Keys: keys, M: values}
}

func pairJoinValue(report PairJoinReport) orderedjson.Value {
	keys := []string{"ok", "status"}
	values := map[string]orderedjson.Value{
		"ok":       report.OK,
		"status":   string(report.Status),
		"applied":  orderedStringArray(report.Applied),
		"warnings": orderedStringArray(report.Warnings),
		"errors":   orderedStringArray(report.Errors),
	}
	if report.Peer != nil {
		keys = append(keys, "peer")
		values["peer"] = pairPeerValue(report.Peer)
	}
	if report.BackupDir != "" {
		keys = append(keys, "backupDir")
		values["backupDir"] = report.BackupDir
	}
	keys = append(keys, "applied", "warnings", "errors")
	return &orderedjson.Object{Keys: keys, M: values}
}

func pairPeerValue(peer *PairPeerSummary) orderedjson.Value {
	if peer == nil {
		return nil
	}
	return &orderedjson.Object{
		Keys: []string{"hostname", "recipient"},
		M: map[string]orderedjson.Value{
			"hostname":  peer.Hostname,
			"recipient": peer.Recipient,
		},
	}
}
