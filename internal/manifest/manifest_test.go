package manifest

import (
	"errors"
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
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
	valid := []string{"a", "A1", "pub.one", "pub_one", "pub-one", "x.y-z_1"}
	for _, id := range valid {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
	invalid := []string{"", "-first", ".first", "_first", "has space", "semi;colon", "$(touch)", "a/b", "a\\b", "é"}
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
			"extensions.manifest.txt": {Kind: "file", Content: "pub.two\npub.one\n"},
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
