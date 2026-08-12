package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Severity of a validation finding.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Issue is a single validation finding tied to a place in the config.
type Issue struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Path     string `json:"path"`
	Message  string `json:"message"`
}

func (i Issue) String() string {
	return fmt.Sprintf("%s: %s: %s: %s", strings.ToUpper(i.Severity), i.File, i.Path, i.Message)
}

// Issues is an ordered list of findings.
type Issues []Issue

// HasErrors reports whether any finding blocks apply.
func (is Issues) HasErrors() bool {
	for _, i := range is {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}

func (is *Issues) errf(file, path, format string, args ...any) {
	*is = append(*is, Issue{Severity: SeverityError, File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (is *Issues) warnf(file, path, format string, args ...any) {
	*is = append(*is, Issue{Severity: SeverityWarning, File: file, Path: path, Message: fmt.Sprintf(format, args...)})
}

var (
	aliasNameRe    = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	consumerNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	// Alias names must not leak vendor branding: an alias called `gpt-4` would
	// re-couple consumers to a vendor, which is exactly what the gateway exists
	// to prevent.
	vendorishRe = regexp.MustCompile(`(?i)(claude|gpt|opus|sonnet|haiku|anthropic|openai|o[0-9]-)`)

	validTriggers = map[string]bool{
		"429": true, "500": true, "502": true, "503": true, "504": true, "timeout": true,
	}
)

// MaxTimeoutSeconds is the hard ceiling from the public contract (SPEC §3.5):
// a consumer never waits longer than this, retries and fallback included.
const MaxTimeoutSeconds = 30

// Validate checks the configuration without touching the network.
func (c *Config) Validate() Issues {
	var issues Issues
	c.validateModels(&issues)
	c.validateConsumers(&issues)
	c.validateProxy(&issues)
	return issues
}

func (c *Config) validateModels(issues *Issues) {
	file := ModelsFileName

	if c.Models.Version != 1 {
		issues.errf(file, "version", "unsupported version %d, expected 1", c.Models.Version)
	}
	if len(c.Models.Aliases) == 0 {
		issues.errf(file, "aliases", "no aliases defined")
		return
	}

	for _, name := range c.AliasNames() {
		alias := c.Models.Aliases[name]
		path := "aliases." + name

		if !aliasNameRe.MatchString(name) {
			issues.errf(file, path, "invalid alias name, expected lowercase kebab-case")
		}
		if vendorishRe.MatchString(name) {
			issues.errf(file, path, "alias name looks like a vendor model name; aliases describe a capability class, not a vendor")
		}
		if strings.TrimSpace(alias.Description) == "" {
			issues.errf(file, path+".description", "description is required")
		}
		if alias.Mode != ModeChat && alias.Mode != ModeEmbedding {
			issues.errf(file, path+".mode", "invalid mode %q, expected %s or %s", alias.Mode, ModeChat, ModeEmbedding)
		}
		if alias.ContextWindow <= 0 {
			issues.errf(file, path+".context_window", "context_window must be positive")
		}

		c.validateCapabilities(issues, path, alias)
		c.validateTargets(issues, path, alias)
	}
}

func (c *Config) validateCapabilities(issues *Issues, path string, alias Alias) {
	file := ModelsFileName
	caps := alias.Capabilities

	if alias.IsEmbedding() {
		if !caps.embeddingsOnly() {
			issues.errf(file, path+".capabilities",
				"embedding alias must declare embeddings: true and streaming/tools/vision: false")
		}
		if alias.MaxOutputTokens != 0 {
			issues.errf(file, path+".max_output_tokens", "embedding alias must not declare max_output_tokens")
		}
		return
	}

	if alias.MaxOutputTokens <= 0 {
		issues.errf(file, path+".max_output_tokens", "max_output_tokens must be positive for a chat alias")
	}
	if caps.Embeddings {
		issues.errf(file, path+".capabilities.embeddings", "chat alias must not declare embeddings: true")
	}
	// Streaming and tools are mandatory for every chat alias (SPEC §3.7): a
	// consumer may assume both on any chat alias it is allowed to use.
	if !caps.Streaming {
		issues.errf(file, path+".capabilities.streaming", "streaming is mandatory for chat aliases")
	}
	if !caps.Tools {
		issues.errf(file, path+".capabilities.tools", "tools are mandatory for chat aliases")
	}
	switch caps.JSONSchema {
	case JSONSchemaNative, JSONSchemaEmulated, JSONSchemaUnsupported:
	default:
		issues.errf(file, path+".capabilities.json_schema",
			"invalid value %q, expected %s|%s|%s", caps.JSONSchema, JSONSchemaNative, JSONSchemaEmulated, JSONSchemaUnsupported)
	}
}

func (caps Capabilities) embeddingsOnly() bool {
	return caps.Embeddings && !caps.Streaming && !caps.Tools && !caps.Vision
}

func (c *Config) validateTargets(issues *Issues, path string, alias Alias) {
	file := ModelsFileName

	if len(alias.Targets) == 0 {
		issues.errf(file, path+".targets", "at least one target is required")
		return
	}

	seen := map[string]bool{}
	for i, target := range alias.Targets {
		tp := fmt.Sprintf("%s.targets[%d]", path, i)
		switch target.Provider {
		case ProviderAnthropic, ProviderOpenAI:
		default:
			issues.errf(file, tp+".provider", "unknown provider %q, expected %s or %s",
				target.Provider, ProviderAnthropic, ProviderOpenAI)
		}
		if strings.TrimSpace(target.Model) == "" {
			issues.errf(file, tp+".model", "model is required")
		}
		if strings.Contains(target.Model, "<") {
			issues.errf(file, tp+".model", "model %q is a placeholder, put a real vendor identifier here", target.Model)
		}
		key := target.Provider + "/" + target.Model
		if seen[key] {
			issues.errf(file, tp, "duplicate target %s", key)
		}
		seen[key] = true

		if alias.IsEmbedding() && target.Provider == ProviderAnthropic {
			issues.errf(file, tp+".provider", "anthropic has no embeddings API")
		}
	}
}

func (c *Config) validateConsumers(issues *Issues) {
	file := ConsumersFileName

	if c.Consumers.Version != 1 {
		issues.errf(file, "version", "unsupported version %d, expected 1", c.Consumers.Version)
	}
	if len(c.Consumers.Consumers) == 0 {
		issues.warnf(file, "consumers", "no consumers defined")
	}

	for _, name := range c.ConsumerNames() {
		consumer := c.Consumers.Consumers[name]
		path := "consumers." + name

		if !consumerNameRe.MatchString(name) {
			// The name becomes part of the key prefix sk-gw-<consumer>-<random>,
			// so it has to survive being copied around logs verbatim.
			issues.errf(file, path, "invalid consumer name, expected lowercase kebab-case")
		}
		if strings.TrimSpace(consumer.Owner) == "" {
			issues.errf(file, path+".owner", "owner is required")
		}
		if len(consumer.Aliases) == 0 {
			issues.errf(file, path+".aliases", "at least one alias is required")
		}

		seen := map[string]bool{}
		for i, alias := range consumer.Aliases {
			ap := fmt.Sprintf("%s.aliases[%d]", path, i)
			if _, ok := c.Models.Aliases[alias]; !ok {
				issues.errf(file, ap, "unknown alias %q, not defined in %s", alias, ModelsFileName)
			}
			if seen[alias] {
				issues.errf(file, ap, "duplicate alias %q", alias)
			}
			seen[alias] = true
		}

		if consumer.Budget.AmountUSD <= 0 {
			issues.errf(file, path+".budget.amount_usd", "budget must be positive")
		}
		if BudgetDuration(consumer.Budget.Period) == "" {
			issues.errf(file, path+".budget.period", "invalid period %q, expected %s|%s|%s",
				consumer.Budget.Period, PeriodDaily, PeriodWeekly, PeriodMonthly)
		}
		if consumer.Limits.RPM < 0 {
			issues.errf(file, path+".limits.rpm", "rpm must not be negative")
		}
		if consumer.Limits.TPM < 0 {
			issues.errf(file, path+".limits.tpm", "tpm must not be negative")
		}
		if consumer.Limits.RPM == 0 {
			issues.warnf(file, path+".limits.rpm", "no request rate limit set")
		}
		if consumer.Limits.TPM == 0 {
			issues.warnf(file, path+".limits.tpm", "no token rate limit set")
		}
	}
}

func (c *Config) validateProxy(issues *Issues) {
	file := ProxyFileName

	if c.Proxy.Version != 1 {
		issues.errf(file, "version", "unsupported version %d, expected 1", c.Proxy.Version)
	}
	req := c.Proxy.Request
	if req.TimeoutSeconds <= 0 {
		issues.errf(file, "request.timeout_seconds", "timeout must be positive")
	}
	if req.TimeoutSeconds > MaxTimeoutSeconds {
		issues.errf(file, "request.timeout_seconds",
			"timeout %ds exceeds the %ds promised by the public contract", req.TimeoutSeconds, MaxTimeoutSeconds)
	}
	if req.NumRetries < 0 {
		issues.errf(file, "request.num_retries", "num_retries must not be negative")
	}
	if req.RetryAfterSeconds < 0 {
		issues.errf(file, "request.retry_after_seconds", "retry_after_seconds must not be negative")
	}

	for i, trigger := range c.Proxy.Fallback.TriggerOn {
		if !validTriggers[trigger] {
			issues.errf(file, fmt.Sprintf("fallback.trigger_on[%d]", i),
				"invalid trigger %q, expected one of %s", trigger, sortedKeys(validTriggers))
		}
		if trigger == "400" {
			issues.errf(file, fmt.Sprintf("fallback.trigger_on[%d]", i),
				"400 must not trigger fallback: the request itself is wrong, another provider returns the same")
		}
	}

	if c.Proxy.Logging.RequestBodies || c.Proxy.Logging.ResponseBodies {
		issues.warnf(file, "logging",
			"body logging is enabled: every consumer's conversations will be written to disk on the VPS")
	}
	if !c.Proxy.Logging.Metadata {
		issues.warnf(file, "logging.metadata", "metadata logging is disabled: gwctl spend will report nothing")
	}
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// Secret-looking prefixes. Vendor keys and issued gateway keys must never be
// committed; `gwctl validate` is the last cheap place to catch it.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`sk-proj-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`sk-gw-[a-z0-9-]+-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`),
}

// CheckRepo looks for the two mistakes that put secrets into git: a key pasted
// into a config file, and a .env that is not ignored.
func CheckRepo(root string) Issues {
	var issues Issues

	configDir := filepath.Join(root, "config")
	entries, err := os.ReadDir(configDir)
	if err != nil {
		issues.errf("config/", "", "cannot read config directory: %v", err)
		return issues
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(configDir, entry.Name()))
		if err != nil {
			issues.errf(entry.Name(), "", "cannot read: %v", err)
			continue
		}
		for _, pattern := range secretPatterns {
			if match := pattern.Find(raw); match != nil {
				issues.errf(entry.Name(), "", "looks like a secret in git: %s…", string(match[:min(len(match), 12)]))
				break
			}
		}
	}

	gitignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		issues.errf(".gitignore", "", "missing: .env must be ignored")
		return issues
	}
	if !ignoresEnv(string(gitignore)) {
		issues.errf(".gitignore", "", "does not ignore .env; vendor keys would be committed")
	}
	if _, err := os.Stat(filepath.Join(root, "deploy", ".env")); err == nil {
		issues.warnf("deploy/.env", "", "present on this machine; make sure it is ignored and never committed")
	}

	return issues
}

func ignoresEnv(gitignore string) bool {
	for _, line := range strings.Split(gitignore, "\n") {
		switch strings.TrimSpace(line) {
		case ".env", "*.env", "**/.env", "deploy/.env":
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
