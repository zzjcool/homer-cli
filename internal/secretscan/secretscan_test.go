package secretscan

import (
	"reflect"
	"testing"

	"github.com/zzjcool/homer-cli/internal/core"
)

func secretShape(parts ...string) string {
	result := ""
	for _, part := range parts {
		result += part
	}
	return result
}

func secretBody(n int) string {
	const alphabet = "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8S9t0"
	result := ""
	for len(result) < n {
		result += alphabet
	}
	return result[:n]
}

func detectorHits(value string) []string {
	result := make([]string, 0)
	for _, detector := range SECRET_PATTERNS {
		if detector.Regex.MatchString(value) {
			result = append(result, detector.ID)
		}
	}
	return result
}

func TestSecretPatternsHaveFrozenOrderAndExamples(t *testing.T) {
	wantIDs := []string{
		"anthropic-api-key", "openai-api-key", "github-classic-token", "github-fine-grained-pat",
		"aws-access-key-id", "slack-token", "google-api-key", "gitlab-pat", "npm-token",
		"stripe-live-key", "private-key-block", "generic-secret-assignment", "age-secret-key",
	}
	gotIDs := make([]string, 0, len(SECRET_PATTERNS))
	for _, detector := range SECRET_PATTERNS {
		gotIDs = append(gotIDs, detector.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("pattern IDs = %v, want %v", gotIDs, wantIDs)
	}

	body := secretBody(40)
	positives := map[string]string{
		"anthropic-api-key":         secretShape("sk-ant-", "api03-", body),
		"openai-api-key":            secretShape("sk-", "proj-", body),
		"github-classic-token":      secretShape("ghp_", body),
		"github-fine-grained-pat":   secretShape("github_pat_", body),
		"aws-access-key-id":         "AKIAIOSFODNN7EXAMPLE",
		"slack-token":               secretShape("xox", "b-123456789012-", body),
		"google-api-key":            secretShape("AIza", body[:35]),
		"gitlab-pat":                secretShape("glpat-", body),
		"npm-token":                 secretShape("npm_", body[:36]),
		"stripe-live-key":           secretShape("sk_", "live_", body),
		"private-key-block":         "-----BEGIN RSA PRIVATE KEY-----",
		"generic-secret-assignment": secretShape(`"api_key": "`, body[:24], `"`),
		"age-secret-key":            secretShape("AGE-SECRET-KEY-", "1", body),
	}
	negatives := map[string][]string{
		"anthropic-api-key":         {"sk-ant-short", "sk-proj-" + body},
		"openai-api-key":            {"sk-ant-" + body, "sk-short"},
		"github-classic-token":      {"ghx_" + body, "ghp_short"},
		"github-fine-grained-pat":   {"github_pat_short", "github_ent_" + body},
		"aws-access-key-id":         {"AKIA1234", "AKIBIOSFODNN7EXAMPLE"},
		"slack-token":               {"xoxz-" + body, "xoxb-short"},
		"google-api-key":            {"AIzaShort", "AIzb" + body},
		"gitlab-pat":                {"glpat-short", "glpat-" + body[:19]},
		"npm-token":                 {"npm_short", "npmx_" + body},
		"stripe-live-key":           {"sk_test_" + body, "pk_live_" + body},
		"private-key-block":         {"-----BEGIN PUBLIC KEY-----", "-----BEGIN CERTIFICATE-----"},
		"generic-secret-assignment": {`"api_key": "__REQUIRED__"`, `"token": "short"`},
		"age-secret-key":            {"AGE-SECRET-KEY-short", "AGE-PUBLIC-KEY-1" + body},
	}
	for _, detector := range SECRET_PATTERNS {
		detector := detector
		t.Run(detector.ID, func(t *testing.T) {
			positive := positives[detector.ID]
			if positive == "" || !detector.Regex.MatchString(positive) {
				t.Fatalf("positive does not match %s: %q", detector.ID, positive)
			}
			if len(negatives[detector.ID]) < 2 {
				t.Fatalf("fewer than two negatives for %s", detector.ID)
			}
			for _, negative := range negatives[detector.ID] {
				if detector.Regex.MatchString(negative) {
					t.Errorf("negative matches %s: %q", detector.ID, negative)
				}
			}
		})
	}
	if hits := detectorHits(`"api_key": "__REQUIRED__"`); len(hits) != 0 {
		t.Fatalf("placeholder hits = %v", hits)
	}
	if hits := detectorHits("age-secret-key-" + body); len(hits) != 1 || hits[0] != "age-secret-key" {
		t.Fatalf("lower-case age key hits = %v", hits)
	}
}

func TestScanContentLineNumbersFirstMatchAndExcerpt(t *testing.T) {
	anthropic := secretShape("sk-ant-", "api03-", "AbCdEfGhIjKlMnOpQrStUvWx")
	content := "clean\r\n  token: \"" + anthropic + "\"\r\n" + anthropic + " and " + anthropic
	findings := ScanContent(content, "pi/settings/settings.json")
	if len(findings) != 2 {
		t.Fatalf("findings = %#v", findings)
	}
	if findings[0].Line != 2 || findings[0].PatternID != "anthropic-api-key" {
		t.Fatalf("first finding = %#v", findings[0])
	}
	if findings[0].Path != "pi/settings/settings.json" || findings[0].Excerpt == "" || findings[0].Excerpt == content {
		t.Fatalf("finding excerpt/path = %#v", findings[0])
	}
	if containsPlaintext(findings[0].Excerpt, anthropic) || !containsPlaintext(findings[0].Excerpt, anthropic[:4]) || !containsPlaintext(findings[0].Excerpt, anthropic[len(anthropic)-4:]) {
		t.Fatalf("secret not correctly masked: %q", findings[0].Excerpt)
	}
	if findings[1].Line != 3 {
		t.Fatalf("second line = %d", findings[1].Line)
	}
	multi := ScanContent(anthropic+" and ghp_"+secretBody(40), "x")
	if len(multi) != 1 || multi[0].PatternID != "anthropic-api-key" {
		t.Fatalf("first-match semantics = %#v", multi)
	}
	if MaskSecret("abcdefgh") != "********" || MaskSecret("123456789") != "1234*6789" {
		t.Fatal("mask boundary changed")
	}
}

func containsPlaintext(value, secret string) bool {
	for i := 0; i+len(secret) <= len(value); i++ {
		if value[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}

func TestScanSnapshotsAndIgnoreGlobs(t *testing.T) {
	anthropic := secretShape("sk-ant-", "api03-", "AbCdEfGhIjKlMnOpQrStUvWx")
	github := "ghp_" + secretBody(40)
	snapshots := []core.AdapterSnapshot{
		{AdapterID: "pi", Categories: []core.CategorySnapshot{{
			AdapterID: "pi", Category: "settings", Mode: core.SyncModeMirror,
			Files: core.SnapshotFiles{"settings.json": {Content: anthropic}},
		}, {AdapterID: "pi", Category: "skills", Mode: core.SyncModeMirror,
			Files: core.SnapshotFiles{"z/SKILL.md": {Content: github}, "a/SKILL.md": {Content: github}},
		}}},
	}
	findings := ScanSnapshots(snapshots)
	if len(findings) != 3 || findings[1].Path != "pi/skills/a/SKILL.md" || findings[2].Path != "pi/skills/z/SKILL.md" {
		t.Fatalf("snapshot findings = %#v", findings)
	}
	ignored := FilterIgnored(findings, []string{"pi/skills/"})
	if len(ignored) != 1 || ignored[0].Path != "pi/settings/settings.json" {
		t.Fatalf("ignored findings = %#v", ignored)
	}
	if ignoredAgain := FilterIgnored(findings, []string{"pi/*/*.json"}); len(ignoredAgain) != 2 {
		t.Fatalf("single-star glob result = %#v", ignoredAgain)
	}
	if kept := FilterIgnored(findings, []string{"pi/*"}); len(kept) != len(findings) {
		t.Fatalf("single-star crossed slash: %#v", kept)
	}
}
