package shellenv

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseLoginPATHIgnoresProfileNoise(t *testing.T) {
	got := parseLoginPATH("some profile banner\n__HOMER_PATH__/home/linuxbrew/.linuxbrew/bin:/usr/bin\n")
	if got != "/home/linuxbrew/.linuxbrew/bin:/usr/bin" {
		t.Fatalf("PATH = %q", got)
	}
	if parseLoginPATH("no marker") != "" {
		t.Fatal("missing marker should yield an empty PATH")
	}
}

func TestCommandPATHFindsBrewOutsideTheAgentPath(t *testing.T) {
	home := "/home/user"
	brew := "/home/linuxbrew/.linuxbrew/bin"
	local := filepath.Join(home, ".local", "bin")
	exists := func(dir string) bool {
		return dir == brew || dir == local || dir == "/usr/bin" || dir == "/bin"
	}
	got := commandPATH("/usr/bin:/bin", home, "", exists)
	want := strings.Join([]string{brew, local, "/usr/bin", "/bin"}, string(os.PathListSeparator))
	if got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if again := commandPATH(want, home, "", exists); again != want {
		t.Fatalf("PATH duplicated: %q", again)
	}
}

func TestLookInPathUsesGivenPATH(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := LookIn("pi", dir)
	if err != nil || got != bin {
		t.Fatalf("LookIn = %q, %v", got, err)
	}
	if _, err := LookIn("pi", "/nonexistent"); err == nil {
		t.Fatal("expected a missing command")
	}
}

func TestPathCachesLoginPATHWithinTTL(t *testing.T) {
	setPathTestEnvironment(t)
	loginPath := t.TempDir()
	calls := 0
	setTestLoginPATH(t, time.Minute, func() string {
		calls++
		return loginPath
	})

	current := time.Unix(1_800_000_000, 0)
	loginPATHNow = func() time.Time { return current }
	first := Path()
	second := Path()

	if calls != 1 {
		t.Fatalf("login PATH reads = %d, want 1 within the TTL", calls)
	}
	if !hasPathEntry(first, loginPath) || !hasPathEntry(second, loginPath) {
		t.Fatalf("Path results = %q and %q, want login PATH %q", first, second, loginPath)
	}
}

func TestPathRereadsLoginPATHAfterTTLExpires(t *testing.T) {
	setPathTestEnvironment(t)
	firstLoginPath := t.TempDir()
	secondLoginPath := t.TempDir()
	calls := 0
	const ttl = 30 * time.Second
	setTestLoginPATH(t, ttl, func() string {
		calls++
		if calls == 1 {
			return firstLoginPath
		}
		return secondLoginPath
	})

	current := time.Unix(1_800_000_000, 0)
	loginPATHNow = func() time.Time { return current }
	first := Path()
	current = current.Add(ttl + time.Nanosecond)
	second := Path()

	if calls != 2 {
		t.Fatalf("login PATH reads after expiry = %d, want 2", calls)
	}
	if !hasPathEntry(first, firstLoginPath) || !hasPathEntry(second, secondLoginPath) {
		t.Fatalf("Path results = %q and %q, want login PATHs %q and %q", first, second, firstLoginPath, secondLoginPath)
	}
}

func TestPathInvalidateImmediatelyRereadsLoginPATH(t *testing.T) {
	setPathTestEnvironment(t)
	firstLoginPath := t.TempDir()
	secondLoginPath := t.TempDir()
	currentLoginPath := firstLoginPath
	calls := 0
	setTestLoginPATH(t, time.Minute, func() string {
		calls++
		return currentLoginPath
	})

	first := Path()
	currentLoginPath = secondLoginPath
	stillCached := Path()
	Invalidate()
	refreshed := Path()

	if calls != 2 {
		t.Fatalf("login PATH reads after Invalidate = %d, want 2", calls)
	}
	if !hasPathEntry(first, firstLoginPath) || !hasPathEntry(stillCached, firstLoginPath) || !hasPathEntry(refreshed, secondLoginPath) {
		t.Fatalf("Path results = %q, %q, %q; want cached %q then refreshed %q", first, stillCached, refreshed, firstLoginPath, secondLoginPath)
	}
}

func TestPathConcurrentCallsShareLoginPATHRead(t *testing.T) {
	setPathTestEnvironment(t)
	loginPath := t.TempDir()
	const goroutines = 64
	var calls atomic.Int64
	readerStarted := make(chan struct{})
	releaseReader := make(chan struct{})
	// Disable TTL caching so this assertion specifically exercises the
	// singleflight behavior rather than relying on a completed cache entry.
	setTestLoginPATH(t, 0, func() string {
		if calls.Add(1) == 1 {
			close(readerStarted)
		}
		<-releaseReader
		return loginPath
	})

	start := make(chan struct{})
	ready := make(chan struct{}, goroutines)
	results := make(chan string, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			<-start
			ready <- struct{}{}
			results <- Path()
		}()
	}
	close(start)
	<-readerStarted
	for range goroutines {
		<-ready
	}
	// Keep the first login shell blocked long enough for the other callers to
	// join its in-flight read, including on a single-core test runner.
	time.Sleep(100 * time.Millisecond)
	close(releaseReader)
	wg.Wait()
	close(results)

	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent login PATH reads = %d, want 1", got)
	}
	for result := range results {
		if !hasPathEntry(result, loginPath) {
			t.Errorf("concurrent Path result = %q, want login PATH %q", result, loginPath)
		}
	}
}

func TestPathDoesNotCacheFailedOrEmptyLoginPATH(t *testing.T) {
	setPathTestEnvironment(t)
	fallbackPath := filepath.SplitList(os.Getenv("PATH"))[0]
	loginPath := t.TempDir()
	calls := 0
	setTestLoginPATH(t, time.Minute, func() string {
		calls++
		if calls == 1 {
			return "" // readLoginPATH uses an empty value to report failures.
		}
		return loginPath
	})

	first := Path()
	second := Path()
	if calls != 2 {
		t.Fatalf("login PATH reads after an empty/failed result = %d, want 2", calls)
	}
	if !hasPathEntry(first, fallbackPath) || !hasPathEntry(second, loginPath) {
		t.Fatalf("Path results = %q and %q, want fallback %q then successful login PATH %q", first, second, fallbackPath, loginPath)
	}
}

func TestPathWithNilReadLoginPATHUsesProcessEnvironment(t *testing.T) {
	setPathTestEnvironment(t)
	fallbackPath := filepath.SplitList(os.Getenv("PATH"))[0]
	calls := 0
	setTestLoginPATH(t, time.Minute, func() string {
		calls++
		return t.TempDir()
	})
	ReadLoginPATH = nil

	got := Path()
	if calls != 0 || !hasPathEntry(got, fallbackPath) {
		t.Fatalf("Path() = %q, login PATH reads = %d; want process PATH %q without a login shell", got, calls, fallbackPath)
	}
}

func setPathTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("PATH", t.TempDir())
}

func setTestLoginPATH(t *testing.T, ttl time.Duration, read func() string) {
	t.Helper()
	previousRead := ReadLoginPATH
	previousTTL := LoginPATHTTL
	previousNow := loginPATHNow
	Invalidate()
	ReadLoginPATH = read
	LoginPATHTTL = ttl
	loginPATHNow = time.Now
	t.Cleanup(func() {
		ReadLoginPATH = previousRead
		LoginPATHTTL = previousTTL
		loginPATHNow = previousNow
		Invalidate()
	})
}

func hasPathEntry(path, entry string) bool {
	for _, candidate := range filepath.SplitList(path) {
		if candidate == entry {
			return true
		}
	}
	return false
}
