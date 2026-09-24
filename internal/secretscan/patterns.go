package secretscan

import "regexp"

// SecretPattern is one ordered secret detector. Matching order is semantic:
// scanContent reports the first matching detector on each line.
type SecretPattern struct {
	ID          string
	Description string
	Regex       *regexp.Regexp

	// Lower-case aliases keep package-local migration tests source-compatible
	// with the TypeScript-shaped fixture terminology without creating a second
	// compiled expression.
	id          string
	description string
	regex       *regexp.Regexp
}

func pattern(id, description, expression string, flags ...string) SecretPattern {
	if len(flags) > 0 && flags[0] == "i" {
		expression = "(?i)" + expression
	}
	compiled := regexp.MustCompile(expression)
	return SecretPattern{
		ID: id, Description: description, Regex: compiled,
		id: id, description: description, regex: compiled,
	}
}

// openAIExpression is the regular-language expansion of the JS negative
// lookahead `sk-(?!ant-)`. Go's regexp is RE2 and intentionally has no
// look-around support, so the only forbidden body prefix is excluded by
// splitting the first three body characters into finite alternatives.
const openAIExpression = `sk-([A-Zb-z0-9_-][A-Za-z0-9_-]{19,}|a([A-MO-Za-mo-z0-9_-][A-Za-z0-9_-]{18,}|n([A-Za-su-z0-9_-][A-Za-z0-9_-]{17,}|t[A-Za-z0-9_][A-Za-z0-9_-]{16,})))`

// SECRET_PATTERNS is the frozen TS order: twelve M2 detectors followed by
// AGE-SECRET-KEY as the M3 addition.
var SECRET_PATTERNS = []SecretPattern{
	pattern("anthropic-api-key", "Anthropic API key (sk-ant-)", `sk-ant-[A-Za-z0-9_-]{20,}`),
	pattern("openai-api-key", "OpenAI API key (sk-)", openAIExpression),
	pattern("github-classic-token", "GitHub token (classic, ghp_/gho_/ghu_/ghs_/ghr_)", `gh[pousr]_[A-Za-z0-9]{36,}`),
	pattern("github-fine-grained-pat", "GitHub fine-grained personal access token (github_pat_)", `github_pat_[A-Za-z0-9_]{22,}`),
	pattern("aws-access-key-id", "AWS access key ID (AKIA)", `AKIA[0-9A-Z]{16}`),
	pattern("slack-token", "Slack token (xox?-...)", `xox[baprsce]-[A-Za-z0-9-]{10,}`),
	pattern("google-api-key", "Google API key (AIza)", `AIza[0-9A-Za-z_-]{35}`),
	pattern("gitlab-pat", "GitLab personal access token (glpat-)", `glpat-[A-Za-z0-9_-]{20,}`),
	pattern("npm-token", "npm access token (npm_)", `npm_[A-Za-z0-9]{36}`),
	pattern("stripe-live-key", "Stripe live secret/restricted key (sk_live_/rk_live_)", `[sr]k_live_[A-Za-z0-9]{20,}`),
	pattern("private-key-block", "PEM private key block header", `-----BEGIN (RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY( BLOCK)?-----`),
	pattern("generic-secret-assignment", "保守通用密钥赋值（api_key/secret/token/password = \"...\"）", `(api[_-]?key|secret|token|password)["']?\s*[:=]\s*["'][A-Za-z0-9+/_-]{20,}["']`, "i"),
	pattern("age-secret-key", "age X25519 私钥（AGE-SECRET-KEY-）", `AGE-SECRET-KEY-[a-z0-9]{20,}`, "i"),
}

// SecretPatterns returns the frozen detector slice. Callers must treat the
// entries as immutable; regexp values are safe for concurrent read use.
func SecretPatterns() []SecretPattern { return SECRET_PATTERNS }
