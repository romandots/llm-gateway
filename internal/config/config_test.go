package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/romandots/llm-gateway/internal/config"
)

const validModels = `
version: 1
aliases:
  cheap-fast:
    description: "cheap and fast"
    mode: chat
    capabilities:
      streaming: true
      tools: true
      json_schema: emulated
      vision: false
      embeddings: false
    context_window: 200000
    max_output_tokens: 64000
    targets:
      - provider: anthropic
        model: claude-haiku-4-5
      - provider: openai
        model: gpt-5-mini
  embed-fast:
    description: "embeddings"
    mode: embedding
    capabilities:
      streaming: false
      tools: false
      json_schema: unsupported
      vision: false
      embeddings: true
    context_window: 8191
    targets:
      - provider: openai
        model: text-embedding-3-small
`

const validConsumers = `
version: 1
consumers:
  my-bot:
    description: "bot"
    owner: "roman"
    aliases: [cheap-fast]
    budget:
      amount_usd: 1
      period: daily
    limits:
      rpm: 30
      tpm: 50000
`

const validProxy = `
version: 1
request:
  timeout_seconds: 30
  num_retries: 2
  retry_after_seconds: 1
fallback:
  enabled: true
  trigger_on: [429, 500, timeout]
logging:
  request_bodies: false
  response_bodies: false
  metadata: true
cache:
  enabled: false
`

// writeConfig lays out a config directory, letting a test override one file.
func writeConfig(t *testing.T, models, consumers, proxy string) string {
	t.Helper()

	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		config.ModelsFileName:    models,
		config.ConsumersFileName: consumers,
		config.ProxyFileName:     proxy,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(configDir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func load(t *testing.T, models, consumers, proxy string) *config.Config {
	t.Helper()

	cfg, err := config.Load(filepath.Join(writeConfig(t, models, consumers, proxy), "config"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func TestLoadValidConfiguration(t *testing.T) {
	cfg := load(t, validModels, validConsumers, validProxy)

	if issues := cfg.Validate(); issues.HasErrors() {
		t.Fatalf("expected a valid configuration, got %v", issues)
	}
	if got := cfg.AliasNames(); len(got) != 2 || got[0] != "cheap-fast" || got[1] != "embed-fast" {
		t.Fatalf("alias names not sorted: %v", got)
	}
	if got := cfg.ConsumerNames(); len(got) != 1 || got[0] != "my-bot" {
		t.Fatalf("unexpected consumers: %v", got)
	}
	alias := cfg.Models.Aliases["cheap-fast"]
	if len(alias.Targets) != 2 || alias.Targets[0].Model != "claude-haiku-4-5" {
		t.Fatalf("target order not preserved: %+v", alias.Targets)
	}
	if !cfg.Models.Aliases["embed-fast"].IsEmbedding() {
		t.Fatal("embed-fast should be an embedding alias")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	models := strings.Replace(validModels, "    context_window: 200000", "    contxt_window: 200000", 1)

	_, err := config.Load(filepath.Join(writeConfig(t, models, validConsumers, validProxy), "config"))
	if err == nil || !strings.Contains(err.Error(), "contxt_window") {
		t.Fatalf("expected a strict-parsing error naming the typo, got %v", err)
	}
}

func TestLoadRejectsDuplicateConsumer(t *testing.T) {
	consumers := validConsumers + `
  my-bot:
    description: "the same name again"
    owner: "roman"
    aliases: [cheap-fast]
    budget:
      amount_usd: 9
      period: daily
    limits:
      rpm: 1
      tpm: 1
`

	_, err := config.Load(filepath.Join(writeConfig(t, validModels, consumers, validProxy), "config"))
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("expected a duplicate key error, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "my-bot") {
		t.Fatalf("error should name the duplicated consumer: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := writeConfig(t, validModels, validConsumers, validProxy)
	if err := os.Remove(filepath.Join(dir, "config", config.ProxyFileName)); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Load(filepath.Join(dir, "config")); err == nil {
		t.Fatal("expected an error for a missing proxy.yaml")
	}
}

func TestValidateCatchesUnknownAlias(t *testing.T) {
	consumers := strings.Replace(validConsumers, "aliases: [cheap-fast]", "aliases: [cheap-fast, smrt]", 1)
	cfg := load(t, validModels, consumers, validProxy)

	assertIssue(t, cfg.Validate(), "consumers.my-bot.aliases[1]", "unknown alias")
}

func TestValidateCatchesMissingTargets(t *testing.T) {
	models := strings.Replace(validModels, `    targets:
      - provider: anthropic
        model: claude-haiku-4-5
      - provider: openai
        model: gpt-5-mini`, "    targets: []", 1)
	cfg := load(t, models, validConsumers, validProxy)

	assertIssue(t, cfg.Validate(), "aliases.cheap-fast.targets", "at least one target")
}

func TestValidateCatchesInvalidBudgetPeriod(t *testing.T) {
	consumers := strings.Replace(validConsumers, "period: daily", "period: fortnightly", 1)
	cfg := load(t, validModels, consumers, validProxy)

	assertIssue(t, cfg.Validate(), "consumers.my-bot.budget.period", "invalid period")
}

func TestValidateCatchesVendorModelPlaceholder(t *testing.T) {
	models := strings.Replace(validModels, "model: gpt-5-mini", "model: <уточняется при реализации>", 1)
	cfg := load(t, models, validConsumers, validProxy)

	assertIssue(t, cfg.Validate(), "aliases.cheap-fast.targets[1].model", "placeholder")
}

func TestValidateCatchesTimeoutAboveContract(t *testing.T) {
	proxy := strings.Replace(validProxy, "timeout_seconds: 30", "timeout_seconds: 120", 1)
	cfg := load(t, validModels, validConsumers, proxy)

	assertIssue(t, cfg.Validate(), "request.timeout_seconds", "public contract")
}

func TestValidateRejectsFallbackOn400(t *testing.T) {
	proxy := strings.Replace(validProxy, "trigger_on: [429, 500, timeout]", `trigger_on: ["400", 500]`, 1)
	cfg := load(t, validModels, validConsumers, proxy)

	assertIssue(t, cfg.Validate(), "fallback.trigger_on[0]", "400 must not trigger fallback")
}

func TestValidateRejectsVendorishAliasName(t *testing.T) {
	models := strings.Replace(validModels, "  cheap-fast:", "  claude-fast:", 1)
	consumers := strings.Replace(validConsumers, "aliases: [cheap-fast]", "aliases: [claude-fast]", 1)
	cfg := load(t, models, consumers, validProxy)

	assertIssue(t, cfg.Validate(), "aliases.claude-fast", "vendor model name")
}

func TestValidateRequiresStreamingAndToolsOnChatAliases(t *testing.T) {
	models := strings.Replace(validModels, `      streaming: true
      tools: true
      json_schema: emulated`, `      streaming: false
      tools: false
      json_schema: emulated`, 1)
	cfg := load(t, models, validConsumers, validProxy)

	issues := cfg.Validate()
	assertIssue(t, issues, "aliases.cheap-fast.capabilities.streaming", "mandatory")
	assertIssue(t, issues, "aliases.cheap-fast.capabilities.tools", "mandatory")
}

func TestValidateRejectsAnthropicEmbeddings(t *testing.T) {
	models := strings.Replace(validModels, `      - provider: openai
        model: text-embedding-3-small`, `      - provider: anthropic
        model: claude-embed`, 1)
	cfg := load(t, models, validConsumers, validProxy)

	assertIssue(t, cfg.Validate(), "aliases.embed-fast.targets[0].provider", "no embeddings API")
}

func TestValidateWarnsAboutBodyLogging(t *testing.T) {
	proxy := strings.Replace(validProxy, "request_bodies: false", "request_bodies: true", 1)
	cfg := load(t, validModels, validConsumers, proxy)

	issues := cfg.Validate()
	if issues.HasErrors() {
		t.Fatalf("body logging is allowed, only warned about: %v", issues)
	}
	assertIssue(t, issues, "logging", "conversations")
}

func TestBudgetDuration(t *testing.T) {
	cases := map[string]string{
		config.PeriodDaily:   "1d",
		config.PeriodWeekly:  "7d",
		config.PeriodMonthly: "30d",
		"yearly":             "",
	}
	for period, want := range cases {
		if got := config.BudgetDuration(period); got != want {
			t.Errorf("BudgetDuration(%q) = %q, want %q", period, got, want)
		}
	}
}

func TestCheckRepoFindsSecretsAndMissingGitignore(t *testing.T) {
	root := writeConfig(t, validModels, validConsumers, validProxy)

	// No .gitignore yet: committing deploy/.env would be one `git add` away.
	assertIssue(t, config.CheckRepo(root), ".gitignore", ".env must be ignored")

	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("bin/\n.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if issues := config.CheckRepo(root); issues.HasErrors() {
		t.Fatalf("expected a clean repository, got %v", issues)
	}

	leaked := validConsumers + "\n# key: sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA\n"
	if err := os.WriteFile(filepath.Join(root, "config", config.ConsumersFileName), []byte(leaked), 0o644); err != nil {
		t.Fatal(err)
	}
	assertIssue(t, config.CheckRepo(root), config.ConsumersFileName, "looks like a secret")
}

func TestCheckRepoWithoutConfigDirectory(t *testing.T) {
	if issues := config.CheckRepo(t.TempDir()); !issues.HasErrors() {
		t.Fatal("expected an error when config/ does not exist")
	}
}

func TestIssueString(t *testing.T) {
	issue := config.Issue{Severity: config.SeverityError, File: "models.yaml", Path: "aliases", Message: "boom"}
	if got := issue.String(); !strings.Contains(got, "ERROR") || !strings.Contains(got, "boom") {
		t.Fatalf("unexpected rendering: %q", got)
	}
}

// assertIssue fails unless some issue located at the given path (or file, for
// findings that are about a whole file) mentions substring.
func assertIssue(t *testing.T, issues config.Issues, location, substring string) {
	t.Helper()

	for _, issue := range issues {
		if issue.Path != location && issue.File != location {
			continue
		}
		if strings.Contains(issue.Message, substring) {
			return
		}
	}
	t.Fatalf("no issue at %q mentioning %q; got %v", location, substring, issues)
}
