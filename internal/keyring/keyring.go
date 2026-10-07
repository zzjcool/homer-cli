// Package keyring stores password-wrapped data keys and the files they
// encrypt. A key record syncs with the rest of the configuration. The
// password and the unwrapped data key exist only in the memory of the
// operation that uses them.
package keyring

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"filippo.io/age"

	"github.com/zzjcool/homer-cli/internal/adapter/keys"
	"github.com/zzjcool/homer-cli/internal/agecrypto"
	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/gens"
)

const (
	envelopeHeader    = "homer-envelope-v1\n"
	fileHeader        = "homer-file-v1\n"
	defaultWorkFactor = 18
	maxPlaintext      = 1 << 20
	minPasswordLength = 8
)

// Command is one keyring operation. Password fields are inputs only and are
// never copied into Result.
type Command struct {
	Action      string `json:"action"`
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	File        string `json:"file,omitempty"`
	Path        string `json:"path,omitempty"`
	Password    string `json:"password,omitempty"`
	NewPassword string `json:"newPassword,omitempty"`
	WorkFactor  int    `json:"-"`
}

// FileInfo is the public description of one encrypted file.
type FileInfo struct {
	ID          string `json:"id"`
	Destination string `json:"destination"`
}

// Summary is safe to return to a client. It has no key material.
type Summary struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Files []FileInfo `json:"files"`
}

// Result is the JSON report for CLI and the console.
type Result struct {
	OK      bool      `json:"ok"`
	Status  string    `json:"status,omitempty"`
	Keys    []Summary `json:"keys,omitempty"`
	Written []string  `json:"written,omitempty"`
	Errors  []string  `json:"errors,omitempty"`
}

type manifest struct {
	ID    string     `json:"id"`
	Name  string     `json:"name"`
	Files []FileInfo `json:"files"`
}

// Apply runs a command against the homer home. Creating a key also registers
// the keys adapter so a later push/pull carries the keyring.
var applyMu sync.Mutex

func Apply(homerHome string, cmd Command) Result {
	applyMu.Lock()
	defer applyMu.Unlock()
	paths := resolvePaths(homerHome)
	switch cmd.Action {
	case "list":
		return list(paths)
	case "create":
		return create(paths, cmd)
	case "encrypt":
		return encrypt(paths, cmd)
	case "unlock":
		return unlock(paths, cmd)
	case "passwd":
		return passwd(paths, cmd)
	default:
		return fail("bad-action", "未知的密钥操作")
	}
}

func list(paths core.HomerPaths) Result {
	// The console's main list is the center. A machine's 收取 publishes
	// the keyring into the current generation; that copy is what other
	// nodes sync. The hub's own ~/.homer/keyring is only the keys created
	// on this process, so it is merged in and does not hide the center.
	merged := map[string]Summary{}
	order := []string{}
	add := func(items []Summary) {
		for _, item := range items {
			if _, ok := merged[item.ID]; !ok {
				order = append(order, item.ID)
			}
			merged[item.ID] = item
		}
	}
	if local, ok := readAll(localItems(paths)); ok {
		add(local)
	}
	// Center is applied second so a collected key replaces a stale local copy.
	if center, ok := readAll(centerItems(paths)); ok {
		add(center)
	}
	keys := make([]Summary, 0, len(order))
	for _, id := range order {
		keys = append(keys, merged[id])
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return Result{OK: true, Status: "listed", Keys: keys}
}

func create(paths core.HomerPaths, cmd Command) Result {
	id := strings.TrimSpace(cmd.ID)
	name := strings.TrimSpace(cmd.Name)
	if name == "" {
		name = id
	}
	if !validID(id) {
		return fail("invalid", "密钥 id 需要以字母或数字开头，其余只能是字母、数字、点、下划线或横线")
	}
	if err := checkPassword(cmd.Password); err != nil {
		return fail("invalid", err.Error())
	}
	if _, err := findKeyDir(paths, id); err == nil {
		return fail("exists", "密钥已存在: "+id)
	}
	if err := ensureAdapter(paths); err != nil {
		return fail("error", err.Error())
	}
	identity := agecrypto.GenerateIdentity()
	envelope, err := sealIdentity(identity.SecretKey, cmd.Password, work(cmd.WorkFactor))
	if err != nil {
		return fail("error", err.Error())
	}
	doc := manifest{ID: id, Name: name, Files: []FileInfo{}}
	if err := writeKey(paths, doc, envelope, nil); err != nil {
		return fail("error", err.Error())
	}
	return Result{OK: true, Status: "created", Keys: []Summary{summaryOf(doc)}}
}

func encrypt(paths core.HomerPaths, cmd Command) Result {
	id := strings.TrimSpace(cmd.ID)
	fileID := strings.TrimSpace(cmd.File)
	destination := strings.TrimSpace(cmd.Path)
	if !validID(id) || !validID(fileID) {
		return fail("invalid", "密钥 id 和文件 id 都要是合法名称")
	}
	if !validDestination(destination) {
		return fail("invalid", "路径要以 ~ 或 / 开头")
	}
	if err := checkPassword(cmd.Password); err != nil {
		return fail("invalid", err.Error())
	}
	doc, envelope, blobs, err := readKey(paths, id)
	if err != nil {
		return fail("missing", err.Error())
	}
	secret, err := openIdentity(envelope, cmd.Password)
	if err != nil {
		return fail("bad-password", "口令不正确")
	}
	identity, err := age.ParseX25519Identity(secret)
	if err != nil {
		return fail("error", "数据密钥无法使用")
	}
	source := core.ExpandHome(destination)
	plaintext, err := os.ReadFile(source)
	if err != nil {
		return fail("missing", "读不到要加密的文件: "+destination)
	}
	if len(plaintext) > maxPlaintext {
		return fail("invalid", "文件超过 1MB")
	}
	ciphertext, err := encryptToRecipient(identity.Recipient().String(), plaintext)
	if err != nil {
		return fail("error", err.Error())
	}
	blobs[fileID] = ciphertext
	doc.Files = upsertFile(doc.Files, FileInfo{ID: fileID, Destination: destination})
	if err := writeKey(paths, doc, envelope, blobs); err != nil {
		return fail("error", err.Error())
	}
	return Result{OK: true, Status: "encrypted", Keys: []Summary{summaryOf(doc)}}
}

func unlock(paths core.HomerPaths, cmd Command) Result {
	id := strings.TrimSpace(cmd.ID)
	if err := checkPassword(cmd.Password); err != nil {
		return fail("invalid", err.Error())
	}
	doc, envelope, blobs, err := readKey(paths, id)
	if err != nil {
		return fail("missing", err.Error())
	}
	if len(doc.Files) == 0 {
		return fail("empty", "这项密钥下面还没有文件")
	}
	secret, err := openIdentity(envelope, cmd.Password)
	if err != nil {
		return fail("bad-password", "口令不正确")
	}
	identity, err := age.ParseX25519Identity(secret)
	if err != nil {
		return fail("error", "数据密钥无法使用")
	}
	written := make([]string, 0, len(doc.Files))
	for _, file := range doc.Files {
		blob, ok := blobs[file.ID]
		if !ok {
			return fail("missing", "缺少文件密文: "+file.ID)
		}
		plaintext, err := decryptWithIdentity(identity, blob)
		if err != nil {
			return fail("error", "无法解开 "+file.ID)
		}
		target := core.ExpandHome(file.Destination)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fail("error", err.Error())
		}
		if err := writePrivate(target, plaintext); err != nil {
			return fail("error", err.Error())
		}
		written = append(written, file.Destination)
	}
	return Result{OK: true, Status: "unlocked", Written: written, Keys: []Summary{summaryOf(doc)}}
}

func passwd(paths core.HomerPaths, cmd Command) Result {
	id := strings.TrimSpace(cmd.ID)
	if err := checkPassword(cmd.Password); err != nil {
		return fail("invalid", err.Error())
	}
	if err := checkPassword(cmd.NewPassword); err != nil {
		return fail("invalid", "新口令至少 8 位")
	}
	doc, envelope, blobs, err := readKey(paths, id)
	if err != nil {
		return fail("missing", err.Error())
	}
	secret, err := openIdentity(envelope, cmd.Password)
	if err != nil {
		return fail("bad-password", "口令不正确")
	}
	rewrapped, err := sealIdentity(secret, cmd.NewPassword, work(cmd.WorkFactor))
	if err != nil {
		return fail("error", err.Error())
	}
	if err := writeKey(paths, doc, rewrapped, blobs); err != nil {
		return fail("error", err.Error())
	}
	return Result{OK: true, Status: "rotated", Keys: []Summary{summaryOf(doc)}}
}

func ensureAdapter(paths core.HomerPaths) error {
	config, err := core.LoadConfig(paths)
	if err != nil {
		return errors.New("请先运行 homer init")
	}
	if _, ok := config.Adapters[keys.AdapterID]; ok {
		return nil
	}
	if config.Adapters == nil {
		config.Adapters = map[string]core.AdapterConfig{}
	}
	config.Adapters[keys.AdapterID] = keys.DefaultAdapter
	return core.SaveConfig(paths, *config)
}

func readAll(items string) ([]Summary, bool) {
	entries, err := os.ReadDir(items)
	if err != nil {
		return nil, false
	}
	out := make([]Summary, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(items, entry.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var doc manifest
		if json.Unmarshal(raw, &doc) != nil || doc.ID == "" {
			continue
		}
		if doc.Files == nil {
			doc.Files = []FileInfo{}
		}
		out = append(out, summaryOf(doc))
	}
	return out, true
}

func readKey(paths core.HomerPaths, id string) (manifest, []byte, map[string][]byte, error) {
	if !validID(id) {
		return manifest{}, nil, nil, errors.New("密钥不存在")
	}
	dir, err := findKeyDir(paths, id)
	if err != nil {
		return manifest{}, nil, nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return manifest{}, nil, nil, errors.New("密钥不存在: " + id)
	}
	var doc manifest
	if err := json.Unmarshal(raw, &doc); err != nil {
		return manifest{}, nil, nil, errors.New("密钥清单损坏: " + id)
	}
	envelope, err := decodeFramed(filepath.Join(dir, "envelope.age"), envelopeHeader)
	if err != nil {
		return manifest{}, nil, nil, errors.New("密钥信封损坏: " + id)
	}
	blobs := map[string][]byte{}
	for _, file := range doc.Files {
		blob, err := decodeFramed(filepath.Join(dir, "files", file.ID+".age"), fileHeader)
		if err != nil {
			return manifest{}, nil, nil, errors.New("缺少文件密文: " + file.ID)
		}
		blobs[file.ID] = blob
	}
	return doc, envelope, blobs, nil
}

func writeKey(paths core.HomerPaths, doc manifest, envelope []byte, blobs map[string][]byte) error {
	if !validID(doc.ID) {
		return errors.New("非法的密钥 id")
	}
	dir := localKeyDir(paths, doc.ID)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := writePrivate(filepath.Join(dir, "manifest.json"), body); err != nil {
		return err
	}
	if err := writeFramed(filepath.Join(dir, "envelope.age"), envelopeHeader, envelope); err != nil {
		return err
	}
	for id, blob := range blobs {
		if !validID(id) {
			return errors.New("非法的文件 id")
		}
		if err := writeFramed(filepath.Join(dir, "files", id+".age"), fileHeader, blob); err != nil {
			return err
		}
	}
	return nil
}

func sealIdentity(secretKey, password string, workFactor int) ([]byte, error) {
	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		return nil, err
	}
	recipient.SetWorkFactor(workFactor)
	var buf bytes.Buffer
	writer, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(writer, secretKey+"\n"); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func openIdentity(envelope []byte, password string) (string, error) {
	identity, err := age.NewScryptIdentity(password)
	if err != nil {
		return "", err
	}
	reader, err := age.Decrypt(bytes.NewReader(envelope), identity)
	if err != nil {
		return "", err
	}
	plain, err := io.ReadAll(io.LimitReader(reader, 512))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(plain)), nil
}

func encryptToRecipient(recipient string, plaintext []byte) ([]byte, error) {
	parsed, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writer, err := age.Encrypt(&buf, parsed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(plaintext); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decryptWithIdentity(identity *age.X25519Identity, ciphertext []byte) ([]byte, error) {
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(reader, maxPlaintext+1))
}

func decodeFramed(path, header string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	if !strings.HasPrefix(text, header) {
		return nil, fmt.Errorf("bad header")
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(text, header)))
}

func writeFramed(path, header string, payload []byte) error {
	body := header + base64.StdEncoding.EncodeToString(payload) + "\n"
	return writePrivate(path, []byte(body))
}

func writePrivate(path string, body []byte) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}

func upsertFile(files []FileInfo, next FileInfo) []FileInfo {
	out := make([]FileInfo, 0, len(files)+1)
	replaced := false
	for _, file := range files {
		if file.ID == next.ID {
			out = append(out, next)
			replaced = true
			continue
		}
		out = append(out, file)
	}
	if !replaced {
		out = append(out, next)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func summaryOf(doc manifest) Summary {
	files := append([]FileInfo(nil), doc.Files...)
	if files == nil {
		files = []FileInfo{}
	}
	return Summary{ID: doc.ID, Name: doc.Name, Files: files}
}

func localItems(paths core.HomerPaths) string {
	return filepath.Join(directory(paths), "items")
}

func localKeyDir(paths core.HomerPaths, id string) string {
	return filepath.Join(localItems(paths), id)
}

// centerItems is the keyring inside the published generation. 收取 writes
// a machine's keyring there; the console list reads it back.
func centerItems(paths core.HomerPaths) string {
	head, ok := gens.New(paths.Home).Read()
	if !ok {
		return ""
	}
	return filepath.Join(head.StoreDir, keys.AdapterID, "items")
}

func findKeyDir(paths core.HomerPaths, id string) (string, error) {
	local := localKeyDir(paths, id)
	if fileExists(filepath.Join(local, "manifest.json")) {
		return local, nil
	}
	if center := centerItems(paths); center != "" {
		alt := filepath.Join(center, id)
		if fileExists(filepath.Join(alt, "manifest.json")) {
			return alt, nil
		}
	}
	return "", errors.New("密钥不存在: " + id)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func directory(paths core.HomerPaths) string {
	config, err := core.LoadConfig(paths)
	if err == nil {
		if adapter, ok := config.Adapters[keys.AdapterID]; ok && adapter.Root != "" {
			return core.ExpandHome(adapter.Root)
		}
	}
	return core.ExpandHome(keys.DefaultAdapter.Root)
}

func resolvePaths(home string) core.HomerPaths {
	if strings.TrimSpace(home) == "" {
		return core.GetHomerPaths(nil)
	}
	return core.GetHomerPaths(func(name string) string {
		if name == "HOMER_HOME" {
			return home
		}
		return os.Getenv(name)
	})
}

func validID(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || (i > 0 && (r == '.' || r == '_' || r == '-'))
		if !ok {
			return false
		}
	}
	return true
}

func validDestination(value string) bool {
	return strings.HasPrefix(value, "~/") || strings.HasPrefix(value, "/") || value == "~"
}

func checkPassword(password string) error {
	if len([]rune(password)) < minPasswordLength {
		return errors.New("口令至少 8 位")
	}
	return nil
}

func work(factor int) int {
	if factor == 0 {
		return defaultWorkFactor
	}
	return factor
}

func fail(status, message string) Result {
	return Result{Status: status, Errors: []string{message}}
}
