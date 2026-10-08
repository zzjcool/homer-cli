package web

import (
	"os/exec"
	"regexp"
	"testing"
)

func TestCollectNDJSONChunkParser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; browser script syntax is checked in the UI toolchain")
	}
	html := string(staticIndex)
	parser := extractJSFunction(t, html, "splitNDJSONLines")
	merge := extractJSFunction(t, html, "mergeCollectChoiceList")
	precheck := extractJSFunction(t, html, "applyCollectPrecheckToChoices")
	preserve := extractJSFunction(t, html, "preserveCollectChoiceChecks")
	program := parser + "\n" + merge + "\n" + precheck + "\n" + preserve + `
const assert = require("node:assert/strict");
const expected = [
  { type: "adapter", adapter: { id: "pi", checked: true, enabled: true }, done: 1, total: 2 },
  { type: "adapter", adapter: { id: "opencode", checked: true, enabled: true }, done: 2, total: 2 },
  { type: "precheck", credentials: { pi: [{ name: "auth.json", exists: "present" }] }, secretHits: { pi: [{ path: "pi/files/token.json" }] } },
  { type: "keys", keys: [{ id: "pi", files: [] }] },
  { type: "done", hint: "finished", adapters: [
    { id: "pi", push: 2, checked: true, enabled: true,
      credentials: [{ name: "auth.json", exists: "present" }],
      secretHits: [{ path: "pi/files/token.json" }] },
    { id: "opencode", pull: 1, checked: true, enabled: true,
      credentials: [], secretHits: [] }
  ] }
];
const body = expected.map(event => JSON.stringify(event)).join("\n") + "\n";
function parseChunks(chunks) {
  let remainder = "";
  const events = [];
  for (let i = 0; i < chunks.length; i++) {
    const parsed = splitNDJSONLines(remainder, chunks[i], i === chunks.length - 1);
    remainder = parsed.remainder;
    parsed.lines.forEach(line => events.push(JSON.parse(line)));
  }
  assert.equal(remainder, "");
  return events;
}
function renderState(events) {
  const state = { choices: [], keys: [], precheck: null, hint: "", selected: { pi: false, opencode: true } };
  events.forEach(event => {
    if (event.type === "adapter") state.choices = mergeCollectChoiceList(state.choices, event.adapter, state.precheck);
    if (event.type === "precheck") {
      state.precheck = { credentials: event.credentials, secretHits: event.secretHits };
      state.choices = applyCollectPrecheckToChoices(state.choices, state.precheck);
    }
    if (event.type === "keys") state.keys = event.keys;
    if (event.type === "done") {
      state.choices = preserveCollectChoiceChecks(event.adapters, state.selected);
      state.hint = event.hint;
    }
  });
  return state;
}
const oneChunkEvents = parseChunks([body]);
assert.deepEqual(oneChunkEvents.map(event => event.type), ["adapter", "adapter", "precheck", "keys", "done"], "one network chunk may contain every ordered row");
const splitChunkEvents = parseChunks([
  body.slice(0, 28),
  body.slice(28, body.indexOf("\n", 29) + 1),
  body.slice(body.indexOf("\n", 29) + 1)
]);
assert.deepEqual(splitChunkEvents, oneChunkEvents, "half rows across chunks and multiple rows in a chunk preserve the same events");
const bufferedState = renderState(oneChunkEvents);
const incrementalState = renderState(splitChunkEvents);
assert.deepEqual(bufferedState, incrementalState, "buffered and incremental arrivals reconcile to the same final UI state");
assert.equal(bufferedState.choices[0].checked, false, "done reconciliation preserves a user-unchecked adapter");
assert.equal(bufferedState.choices[1].checked, true, "done reconciliation preserves a user-checked adapter");
assert.equal(bufferedState.choices[0].credentials[0].exists, "present");
assert.equal(bufferedState.choices[0].secretHits[0].path, "pi/files/token.json");
assert.equal(bufferedState.keys[0].id, "pi");
assert.equal(bufferedState.hint, "finished");
const finalLine = splitNDJSONLines("", '{"type":"done"}', true);
assert.deepEqual(finalLine.lines, ['{"type":"done"}'], "the final unterminated row is still consumed");
`
	command := exec.Command(node, "--input-type=commonjs", "-e", program)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("node NDJSON chunk test: %v\n%s", err, output)
	}
}

func extractJSFunction(t *testing.T, source, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?ms)^function ` + regexp.QuoteMeta(name) + `\(.*?^\}`)
	function := pattern.FindString(source)
	if function == "" {
		t.Fatalf("static console is missing JS function %s", name)
	}
	return function
}
