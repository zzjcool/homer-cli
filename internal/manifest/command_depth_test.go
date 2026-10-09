package manifest

import (
	"strconv"
	"strings"
	"testing"
)

// These tests guard against a manifest command that re-runs homer and thereby
// re-triggers the same scan without bound. That happened once with a test that
// pointed listCmd at its own test binary: the child inherited a scrubbed
// environment, never reached its "I am the child" branch, and forked until the
// session's pids cgroup was full.
//
// SAFETY RULE FOR THIS FILE: nothing here may execute os.Args[0] or
// os.Executable(). The checks read the environment that minimalCommandEnv()
// would hand to a child; they never start one.

func TestMinimalCommandEnvIncrementsDepth(t *testing.T) {
	t.Setenv(commandDepthEnv, "")
	if got := envValue(minimalCommandEnv(), commandDepthEnv); got != "1" {
		t.Fatalf("depth for a top-level command = %q, want 1", got)
	}
	t.Setenv(commandDepthEnv, "2")
	if got := envValue(minimalCommandEnv(), commandDepthEnv); got != "3" {
		t.Fatalf("depth nested under 2 = %q, want 3", got)
	}
}

func TestUnparseableDepthFailsClosed(t *testing.T) {
	for _, bad := range []string{"abc", "-1", "1.5", " "} {
		t.Setenv(commandDepthEnv, bad)
		if bad == " " {
			// whitespace only is "unset", not an attack on the guard
			if got := currentCommandDepth(); got != 0 {
				t.Fatalf("whitespace depth = %d, want 0", got)
			}
			continue
		}
		if got := currentCommandDepth(); got != maxCommandDepth {
			t.Fatalf("depth for %q = %d, want fail-closed %d", bad, got, maxCommandDepth)
		}
	}
}

func TestRunRefusesPastMaxDepth(t *testing.T) {
	t.Setenv(commandDepthEnv, strconv.Itoa(maxCommandDepth))
	// "true" is harmless; the point is that it must not even be started.
	out, err := run([]string{"true"}, 0)
	if err == nil {
		t.Fatalf("run past max depth succeeded with output %q; want a refusal", out)
	}
	if !strings.Contains(err.Error(), "嵌套过深") {
		t.Fatalf("refusal should explain the recursion, got: %v", err)
	}
}

func TestRunStillWorksBelowMaxDepth(t *testing.T) {
	t.Setenv(commandDepthEnv, strconv.Itoa(maxCommandDepth-1))
	if _, err := run([]string{"true"}, 5_000_000_000); err != nil {
		t.Fatalf("run below max depth failed: %v", err)
	}
}

func TestOnlyTestCounterVarsPassThrough(t *testing.T) {
	t.Setenv("HOMER_TEST_INSPECT_SCAN_COUNTER", "/tmp/x")
	t.Setenv("HOMER_HUB_TOKEN", "must-not-leak")
	t.Setenv("HOMER_TEST_SECRET", "must-not-leak")
	t.Setenv("SOME_SECRET", "must-not-leak")
	env := minimalCommandEnv()
	if envValue(env, "HOMER_TEST_INSPECT_SCAN_COUNTER") != "/tmp/x" {
		t.Fatal("HOMER_TEST_* counter should reach the child so tests can observe it")
	}
	for _, key := range []string{"HOMER_HUB_TOKEN", "HOMER_TEST_SECRET", "SOME_SECRET"} {
		if envValue(env, key) != "" {
			t.Fatalf("%s leaked into the manifest command environment", key)
		}
	}
}
