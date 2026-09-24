package orderedjson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestGoldenMergeParseSerializeFileRoundTrip(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	goldenDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "golden", "merge")
	files, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	if err != nil {
		t.Fatalf("glob golden vectors: %v", err)
	}
	sort.Strings(files)
	if len(files) != 32 {
		t.Fatalf("golden merge vector count = %d, want 32", len(files))
	}

	for _, file := range files {
		file := file
		t.Run(filepath.Base(file), func(t *testing.T) {
			input, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("read golden vector: %v", err)
			}
			value, err := Parse(input)
			if err != nil {
				t.Fatalf("parse golden vector: %v", err)
			}
			if got := SerializeFile(value); string(got) != string(input) {
				t.Fatalf("parse→serialize changed bytes (-want +got):\n%s", diffBytes(input, got))
			}
			if got := Serialize(value); string(got) != strings.TrimSuffix(string(input), "\n") {
				t.Fatalf("Serialize must be SerializeFile without its final newline")
			}
		})
	}
}

func TestSerializeJavaScriptStringEscapes(t *testing.T) {
	lineSeparator := string(rune(0x2028))
	paragraphSeparator := string(rune(0x2029))
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "html punctuation is not escaped",
			input: `{"value":"<>&"}`,
			want:  "{\n  \"value\": \"<>&\"\n}",
		},
		{
			name:  "unicode and supplementary characters stay UTF-8",
			input: `{"value":"café 😀"}`,
			want:  "{\n  \"value\": \"café 😀\"\n}",
		},
		{
			name:  "line and paragraph separators are not escaped",
			input: `{"value":"` + lineSeparator + paragraphSeparator + `"}`,
			want:  "{\n  \"value\": \"" + lineSeparator + paragraphSeparator + "\"\n}",
		},
		{
			name:  "short controls and lower-case unicode escapes",
			input: `{"value":"\"\\\/\b\f\n\r\t\u0000\u0001\u001f"}`,
			want:  "{\n  \"value\": \"\\\"\\\\/\\b\\f\\n\\r\\t\\u0000\\u0001\\u001f\"\n}",
		},
		{
			name:  "surrogate pair becomes its JavaScript character",
			input: `{"value":"\ud83d\ude00"}`,
			want:  "{\n  \"value\": \"😀\"\n}",
		},
		{
			name:  "lone surrogate remains an escape",
			input: `{"value":"\ud800"}`,
			want:  "{\n  \"value\": \"\\ud800\"\n}",
		},
		{
			name:  "slash is not unnecessarily escaped",
			input: `{"value":"a\/b"}`,
			want:  "{\n  \"value\": \"a/b\"\n}",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := Parse([]byte(test.input))
			if err != nil {
				t.Fatalf("Parse(%q): %v", test.input, err)
			}
			if got := string(Serialize(value)); got != test.want {
				t.Fatalf("Serialize mismatch\nwant: %q\n got: %q", test.want, got)
			}
		})
	}
}

func TestSerializeJavaScriptNumbers(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "integer", input: `1`, want: `1`},
		{name: "fraction trailing zero", input: `1.20`, want: `1.2`},
		{name: "exponent to integer", input: `1e3`, want: `1000`},
		{name: "negative zero", input: `-0`, want: `0`},
		{name: "small decimal threshold", input: `1e-6`, want: `0.000001`},
		{name: "small exponent threshold", input: `1e-7`, want: `1e-7`},
		{name: "large decimal threshold", input: `1e20`, want: `100000000000000000000`},
		{name: "large exponent threshold", input: `1e21`, want: `1e+21`},
		{name: "JavaScript integer rounding", input: `12345678901234567890`, want: `12345678901234567000`},
		{name: "underflow is zero", input: `1e-400`, want: `0`},
		{name: "overflow is JSON null", input: `1e400`, want: `null`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := Parse([]byte(test.input))
			if err != nil {
				t.Fatalf("Parse(%q): %v", test.input, err)
			}
			if got := string(Serialize(value)); got != test.want {
				t.Fatalf("Serialize mismatch\nwant: %q\n got: %q", test.want, got)
			}
		})
	}
}

func TestObjectOrderAndDuplicateMembers(t *testing.T) {
	value, err := Parse([]byte(`{"z":1,"a":2,"z":3}`))
	if err != nil {
		t.Fatal(err)
	}
	object, ok := value.(*Object)
	if !ok {
		t.Fatalf("Parse returned %T, want *Object", value)
	}
	if want := []string{"z", "a"}; !reflect.DeepEqual(object.Keys, want) {
		t.Fatalf("Keys = %#v, want %#v", object.Keys, want)
	}
	if got := object.M["z"]; !DeepEqual(got, json.Number("3")) {
		t.Fatalf("duplicate key value = %#v, want 3", got)
	}
	want := "{\n  \"z\": 3,\n  \"a\": 2\n}"
	if got := string(Serialize(value)); got != want {
		t.Fatalf("serialized object = %q, want %q", got, want)
	}
}

func TestJavaScriptNumericPropertyOrder(t *testing.T) {
	value, err := Parse([]byte(`{"10":"ten","2":"two","1":"one","01":"leading","a":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"1\": \"one\",\n  \"2\": \"two\",\n  \"10\": \"ten\",\n  \"01\": \"leading\",\n  \"a\": \"a\"\n}"
	if got := string(Serialize(value)); got != want {
		t.Fatalf("serialized object = %q, want %q", got, want)
	}
}

func TestSerializeFileAddsExactlyOneNewline(t *testing.T) {
	value, err := Parse([]byte(`{"a":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(SerializeFile(value)); got != "{\n  \"a\": true\n}\n" {
		t.Fatalf("SerializeFile = %q", got)
	}
}

func TestParseRejectsInvalidJSON(t *testing.T) {
	tests := []string{
		"",
		" ",
		"{",
		"}",
		`{"a":}`,
		`{"a":1,}`,
		`[1,]`,
		`[1 2]`,
		`01`,
		`-`,
		`1.`,
		`1e`,
		`true false`,
		`"unterminated`,
		`"bad\q"`,
		"\"bad\x01\"",
		"\"bad\xff\"",
	}
	for _, input := range tests {
		t.Run(strings.ReplaceAll(input, "\n", "\\n"), func(t *testing.T) {
			if value, err := Parse([]byte(input)); err == nil {
				t.Fatalf("Parse(%q) = %#v, want error", input, value)
			}
		})
	}
}

func TestDeepEqualJSONSemantics(t *testing.T) {
	parse := func(t *testing.T, input string) Value {
		t.Helper()
		value, err := Parse([]byte(input))
		if err != nil {
			t.Fatalf("Parse(%q): %v", input, err)
		}
		return value
	}

	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "object key order is not semantic", a: `{"a":1,"b":[true,null]}`, b: `{"b":[true,null],"a":1}`, want: true},
		{name: "number spelling is not semantic", a: `1.0`, b: `1e0`, want: true},
		{name: "negative zero is distinct", a: `-0`, b: `0`, want: false},
		{name: "array order is semantic", a: `[1,2]`, b: `[2,1]`, want: false},
		{name: "nested values", a: `{"n":{"x":1}}`, b: `{"n":{"x":1}}`, want: true},
		{name: "different types", a: `1`, b: `"1"`, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DeepEqual(parse(t, test.a), parse(t, test.b)); got != test.want {
				t.Fatalf("DeepEqual(%s, %s) = %v, want %v", test.a, test.b, got, test.want)
			}
		})
	}
}

func TestStripTopKeysIsTopLevelAndDeepCopy(t *testing.T) {
	value, err := Parse([]byte(`{"secret":"remove","keep":{"nested":"secret","list":[{"x":1}]},"other":true}`))
	if err != nil {
		t.Fatal(err)
	}
	stripped := StripTopKeys(value, []string{"secret", "missing"})
	object := stripped.(*Object)
	if got, exists := object.M["secret"]; exists || got != nil {
		t.Fatalf("top-level secret survived: %#v", got)
	}
	if _, exists := object.M["keep"]; !exists {
		t.Fatal("keep was removed")
	}
	if got := string(Serialize(stripped)); got != "{\n  \"keep\": {\n    \"nested\": \"secret\",\n    \"list\": [\n      {\n        \"x\": 1\n      }\n    ]\n  },\n  \"other\": true\n}" {
		t.Fatalf("StripTopKeys output = %q", got)
	}

	original := value.(*Object)
	original.M["keep"].(*Object).M["nested"] = "changed"
	if got := string(Serialize(stripped)); !strings.Contains(got, `"nested": "secret"`) {
		t.Fatalf("stripped value aliases original nested object: %q", got)
	}

	array := StripTopKeys([]Value{value, json.Number("1")}, []string{"secret"}).([]Value)
	if len(array) != 2 || !DeepEqual(array[1], json.Number("1")) {
		t.Fatalf("non-object deep copy changed array: %#v", array)
	}
}

func TestSerializeHandBuiltObjectAndNilValues(t *testing.T) {
	value := &Object{
		Keys: []string{"b", "a"},
		M: map[string]Value{
			"a": nil,
			"b": []Value{},
		},
	}
	want := "{\n  \"b\": [],\n  \"a\": null\n}"
	if got := string(Serialize(value)); got != want {
		t.Fatalf("Serialize(hand-built) = %q, want %q", got, want)
	}
}

func diffBytes(want, got []byte) string {
	// The golden assertion is primarily a byte comparison; keeping the helper
	// small makes failures readable without adding a third-party diff package.
	if len(want) == len(got) {
		for i := range want {
			if want[i] != got[i] {
				return "first differing byte at " + strconv.Itoa(i)
			}
		}
	}
	return "want=" + string(want) + "\ngot=" + string(got)
}
