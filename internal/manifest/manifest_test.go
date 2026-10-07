package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/homer-cli/internal/core"
	"github.com/zzjcool/homer-cli/internal/shellenv"
)

type fakePort struct {
	output    []byte
	outputErr error
	applyIDs  []string
	applyErrs map[string]error
}

func (fake *fakePort) Output(string) ([]byte, error) {
	return fake.output, fake.outputErr
}

func (fake *fakePort) Apply(_ string, id string) error {
	fake.applyIDs = append(fake.applyIDs, id)
	return fake.applyErrs[id]
}

func TestParseIDsBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "blank lines", in: "\n \t\n", want: nil},
		{name: "crlf and whitespace", in: "  one\r\n\ttwo \r\n one\n", want: []string{"one", "two"}},
		{name: "preserve order and dedupe", in: "z\na\nz\nB\na\n", want: []string{"z", "a", "B"}},
		{name: "non-id text remains for apply validation", in: "has space\nsemi;colon\n", want: []string{"has space", "semi;colon"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := ParseIDs([]byte(test.in)); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ParseIDs(%q) = %#v, want %#v", test.in, got, test.want)
			}
		})
	}
}

func TestManifestContentRoundTrip(t *testing.T) {
	ids := []string{"pub.one", "pub.two"}
	if got, want := ContentOf(ids), "pub.one\npub.two\n"; got != want {
		t.Fatalf("ContentOf = %q, want %q", got, want)
	}
	if got := IDsOf(ContentOf(ids)); !reflect.DeepEqual(got, ids) {
		t.Fatalf("IDsOf(ContentOf(ids)) = %#v, want %#v", got, ids)
	}
	if got := VirtualFileName("extensions"); got != "extensions.manifest.txt" {
		t.Fatalf("VirtualFileName = %q", got)
	}
}

func TestValidID(t *testing.T) {
	valid := []string{"a", "A1", "pub.one", "pub_one", "pub-one", "x.y-z_1", "pub.one@1.2.3", "npm:pi-lens", "npm:pi-lens@4.3.0", "npm:@zzjcool/pi-herdr-subagents@0.9.0"}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
	invalid := []string{"", "-first", ".first", "_first", "has space", "semi;colon", "$(touch)", "a/b", "a\\b", "é", "npm:../x@1.0.0", "npm:pi-lens@$(touch)"}
	for _, id := range invalid {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}

func TestScanCategorySuccessAndProblems(t *testing.T) {
	port := &fakePort{output: []byte(" pub.two\r\n\n pub.one\n pub.two\n")}
	got, problems := ScanCategory("vscode", "extensions", core.CategoryConfig{Mode: core.SyncModeMirror, ListCmd: "code --list-extensions"}, port)
	if len(problems) != 0 {
		t.Fatalf("success problems = %#v", problems)
	}
	want := core.CategorySnapshot{
		AdapterID: "vscode",
		Category:  "extensions",
		Mode:      core.SyncModeMirror,
		Files: core.SnapshotFiles{
			// ContentOf sorts: snapshots are deterministic regardless of
			// listCmd output order.
			"extensions.manifest.txt": {Kind: "file", Content: "pub.one\npub.two\n"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}

	failure := &fakePort{outputErr: errors.New("exit status 7")}
	got, problems = ScanCategory("vscode", "extensions", core.CategoryConfig{Mode: core.SyncModeMirror, ListCmd: "code --list-extensions"}, failure)
	if len(got.Files) != 0 || len(problems) != 1 {
		t.Fatalf("failure result = snapshot %#v problems %#v", got, problems)
	}
	if problems[0].Command != "code --list-extensions" || problems[0].Message != "exit status 7" {
		t.Fatalf("problem = %#v", problems[0])
	}

	timeout := &fakePort{outputErr: contextDeadlineError{}}
	_, problems = ScanCategory("vscode", "extensions", core.CategoryConfig{ListCmd: "sleep 31"}, timeout)
	if len(problems) != 1 || problems[0].Message != "context deadline exceeded" {
		t.Fatalf("timeout problem = %#v", problems)
	}
}

// contextDeadlineError keeps the timeout test independent of an actual 30s
// process while presenting the same diagnostic text as context.DeadlineExceeded.
type contextDeadlineError struct{}

func (contextDeadlineError) Error() string { return "context deadline exceeded" }

func writeExecutable(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest-command.sh")
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultPortUsesMinimalEnvironment(t *testing.T) {
	previous := shellenv.ReadLoginPATH
	shellenv.ReadLoginPATH = func() string { return "" }
	t.Cleanup(func() { shellenv.ReadLoginPATH = previous })
	t.Setenv("HOMER_TEST_SECRET", "must-not-cross-boundary")
	t.Setenv("HOME", "/tmp/homer-manifest-home")
	t.Setenv("PATH", "/bin")
	script := writeExecutable(t, `#!/bin/sh
printf '%s\n' "${HOMER_TEST_SECRET-unset}"
printf '%s\n' "$HOME"
printf '%s\n' "$PATH"
`)
	output, err := DefaultPort().Output(script)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if strings.Contains(text, "must-not-cross-boundary") || !strings.Contains(text, "unset\n") {
		t.Fatalf("manifest command inherited HOMER_* environment: %q", text)
	}
	if !strings.Contains(text, "/tmp/homer-manifest-home\n") || !strings.Contains(text, "/bin\n") {
		t.Fatalf("minimal PATH/HOME environment missing: %q", text)
	}
}

func TestDefaultPortBoundsStdoutAndKillsProcessGroupOnTimeout(t *testing.T) {
	large := writeExecutable(t, "#!/bin/sh\n/usr/bin/dd if=/dev/zero bs=16777217 count=1 2>/dev/null\n")
	if _, err := DefaultPort().Output(large); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("large stdout error = %v, want 16 MiB limit", err)
	}

	slow := writeExecutable(t, "#!/bin/sh\n(/bin/sleep 30) &\n/bin/sleep 30\n")
	started := time.Now()
	_, err := run([]string{slow}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("process group was not killed promptly: %s", elapsed)
	}
}

func TestApplyTasksAllSuccessFailureAndInvalidIDs(t *testing.T) {
	port := &fakePort{applyErrs: map[string]error{"bad": errors.New("installer failed")}}
	tasks := []Task{{
		AdapterID: "vscode",
		Category:  "extensions",
		ApplyCmd:  "code --install-extension",
		IDs:       []string{"pub.two", "bad", "pub.two", "semi;colon", "pub.one"},
	}}
	got := ApplyTasks(tasks, port)
	if want := []string{"bad", "pub.one", "pub.two"}; !reflect.DeepEqual(port.applyIDs, want) {
		t.Fatalf("applied IDs = %#v, want %#v", port.applyIDs, want)
	}
	if want := []string{"vscode/extensions:pub.one", "vscode/extensions:pub.two"}; !reflect.DeepEqual(got.Installed, want) {
		t.Fatalf("installed = %#v, want %#v", got.Installed, want)
	}
	if len(got.Failed) != 2 || got.Failed[0].ID != "bad" || got.Failed[1].ID != "semi;colon" {
		t.Fatalf("failed = %#v", got.Failed)
	}
	if got.Failed[0].Message != "installer failed" || got.Failed[1].Message != "invalid manifest ID" {
		t.Fatalf("failure messages = %#v", got.Failed)
	}
}

func TestApplyTasksContinuesAfterFailure(t *testing.T) {
	port := &fakePort{applyErrs: map[string]error{"pub.one": errors.New("nope")}}
	result := ApplyTasks([]Task{{AdapterID: "a", Category: "c", ApplyCmd: "install", IDs: []string{"pub.one", "pub.two"}}}, port)
	if !reflect.DeepEqual(port.applyIDs, []string{"pub.one", "pub.two"}) {
		t.Fatalf("apply stopped after failure: %#v", port.applyIDs)
	}
	if !reflect.DeepEqual(result.Installed, []string{"a/c:pub.two"}) {
		t.Fatalf("installed = %#v", result.Installed)
	}
	if len(result.Failed) != 1 || result.Failed[0].ID != "pub.one" {
		t.Fatalf("failed = %#v", result.Failed)
	}
}

// Manifest categories may declare their own ID line pattern: package
// managers print noise (headers, install paths) around the IDs, and IDs
// themselves carry registry prefixes (npm:) or scopes (@org/) that the
// default pattern rejects. A category-level pattern filters listCmd
// output to exactly the installable IDs.
func TestVersionedListCommandAndPinnedNpmVersion(t *testing.T) {
	if got := VersionedListCommand("code --list-extensions"); got != "code --list-extensions --show-versions" {
		t.Fatalf("vscode list = %q", got)
	}
	if got := VersionedListCommand("code --list-extensions --show-versions"); got != "code --list-extensions --show-versions" {
		t.Fatalf("vscode list already versioned = %q", got)
	}
	if got := VersionedListCommand("pi list"); got != "pi list" {
		t.Fatalf("pi list = %q", got)
	}

	modules := t.TempDir()
	writePackageJSON(t, modules, "pi-lens", "4.3.0")
	writePackageJSON(t, modules, "@zzjcool/pi-herdr-subagents", "0.9.0")
	snapshot := core.CategorySnapshot{
		Category: "packages",
		Files: core.SnapshotFiles{
			VirtualFileName("packages"): {Kind: "file", Content: "npm:pi-lens\nnpm:@zzjcool/pi-herdr-subagents\nnpm:missing@1.2.3\n"},
		},
	}
	PinNpmVersions(modules, snapshot)
	got := strings.Split(strings.TrimSuffix(snapshot.Files[VirtualFileName("packages")].Content, "\n"), "\n")
	want := []string{"npm:@zzjcool/pi-herdr-subagents@0.9.0", "npm:missing@1.2.3", "npm:pi-lens@4.3.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned = %#v, want %#v", got, want)
	}
}

func writePackageJSON(t *testing.T, modules, name, version string) {
	t.Helper()
	dir := filepath.Join(modules, filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"name":"` + strings.ReplaceAll(name, `"`, "") + `","version":"` + version + `"}`)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanCategoryWithIDPattern(t *testing.T) {
	port := &fakePort{output: []byte(`User packages:
  npm:pi-lens
    /home/u/.pi/agent/npm/node_modules/pi-lens
  npm:@zzjcool/pi-herdr-subagents
    /home/u/.pi/agent/npm/node_modules/@zzjcool/pi-herdr-subagents

Project packages:
  npm:local-tool
`)}
	cfg := core.CategoryConfig{
		Mode:      "mirror",
		Kind:      manifestKindPtr(),
		ListCmd:   "pi list",
		ApplyCmd:  "pi install",
		IDPattern: `^  (npm:[A-Za-z0-9@/._-]+)$`,
	}
	snapshot, problems := ScanCategoryWithPattern("pi", "packages", cfg, port, cfg.IDPattern)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	entry := snapshot.Files[VirtualFileName("packages")]
	got := strings.Split(strings.TrimSuffix(entry.Content, "\n"), "\n")
	// Sorted and filtered: headers and install paths are gone; user and
	// project packages share the same line shape, so both ride along
	// (they are machine-global installs either way).
	want := []string{"npm:@zzjcool/pi-herdr-subagents", "npm:local-tool", "npm:pi-lens"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v (sorted, filtered)", got, want)
	}
}

func TestParseIDsWithPattern(t *testing.T) {
	stdout := []byte(`User packages:
  npm:pi-lens
    /some/path/pi-lens
  npm:other
`)
	ids := ParseIDsWithPattern(stdout, `^  (npm:[a-z-]+)$`)
	if !reflect.DeepEqual(ids, []string{"npm:pi-lens", "npm:other"}) {
		t.Fatalf("ids = %v", ids)
	}
	// empty pattern keeps every non-empty line (legacy behavior).
	all := ParseIDsWithPattern(stdout, "")
	if len(all) != 4 {
		t.Fatalf("legacy ids = %v", all)
	}
}

func TestCommandEnvReadsLoginPATHEveryCall(t *testing.T) {
	previous := shellenv.ReadLoginPATH
	calls := 0
	shellenv.ReadLoginPATH = func() string {
		calls++
		return "/usr/bin:/bin"
	}
	t.Cleanup(func() { shellenv.ReadLoginPATH = previous })
	_ = minimalCommandEnv()
	_ = minimalCommandEnv()
	if calls != 2 {
		t.Fatalf("login PATH reads = %d, want 2", calls)
	}
}

func manifestKindPtr() *core.CategoryKind {
	kind := core.CategoryKindManifest
	return &kind
}
