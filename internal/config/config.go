// Package config parses and validates the declarative gateway configuration
// stored in config/*.yaml. It never reads or writes secrets: the files it
// understands describe aliases, consumers and proxy behavior only.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Provider identifiers accepted in models.yaml.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// json_schema support levels declared per alias (see SPEC §3.6).
const (
	JSONSchemaNative      = "native"
	JSONSchemaEmulated    = "emulated"
	JSONSchemaUnsupported = "unsupported"
)

// Alias modes. Chat aliases are served by /v1/chat/completions, embedding
// aliases by /v1/embeddings.
const (
	ModeChat      = "chat"
	ModeEmbedding = "embedding"
)

// Budget periods accepted in consumers.yaml.
const (
	PeriodDaily   = "daily"
	PeriodWeekly  = "weekly"
	PeriodMonthly = "monthly"
)

// Config is the whole configuration tree loaded from a directory.
type Config struct {
	Dir       string
	Models    ModelsFile
	Consumers ConsumersFile
	Proxy     ProxyFile
}

// ModelsFile is config/models.yaml.
type ModelsFile struct {
	Version int              `yaml:"version"`
	Aliases map[string]Alias `yaml:"aliases"`
}

// Alias is a single capability class exposed to consumers.
type Alias struct {
	Description     string       `yaml:"description"`
	Mode            string       `yaml:"mode"`
	Capabilities    Capabilities `yaml:"capabilities"`
	ContextWindow   int          `yaml:"context_window"`
	MaxOutputTokens int          `yaml:"max_output_tokens"`
	Targets         []Target     `yaml:"targets"`
}

// Capabilities is the declared (not auto-detected) capability matrix of an
// alias. Smoke tests verify every declared flag against a real request.
type Capabilities struct {
	Streaming  bool   `yaml:"streaming"`
	Tools      bool   `yaml:"tools"`
	JSONSchema string `yaml:"json_schema"`
	Vision     bool   `yaml:"vision"`
	Embeddings bool   `yaml:"embeddings"`
}

// Target is one vendor deployment behind an alias. The order of targets in
// models.yaml is the routing priority: index 0 is primary, the rest are
// fallbacks tried in order.
type Target struct {
	Provider string         `yaml:"provider"`
	Model    string         `yaml:"model"`
	Params   map[string]any `yaml:"params"`
}

// ConsumersFile is config/consumers.yaml.
type ConsumersFile struct {
	Version   int                 `yaml:"version"`
	Consumers map[string]Consumer `yaml:"consumers"`
}

// Consumer is one downstream project holding a virtual key.
type Consumer struct {
	Description string   `yaml:"description"`
	Owner       string   `yaml:"owner"`
	Aliases     []string `yaml:"aliases"`
	Budget      Budget   `yaml:"budget"`
	Limits      Limits   `yaml:"limits"`
}

// Budget is the spend cap of a consumer for one period.
type Budget struct {
	AmountUSD float64 `yaml:"amount_usd"`
	Period    string  `yaml:"period"`
}

// Limits are per-key rate limits enforced by the proxy.
type Limits struct {
	RPM int `yaml:"rpm"`
	TPM int `yaml:"tpm"`
}

// ProxyFile is config/proxy.yaml.
type ProxyFile struct {
	Version  int             `yaml:"version"`
	Request  RequestSettings `yaml:"request"`
	Fallback FallbackSetting `yaml:"fallback"`
	Logging  LoggingSettings `yaml:"logging"`
	Cache    CacheSettings   `yaml:"cache"`
}

// RequestSettings control timeouts and internal retries.
type RequestSettings struct {
	TimeoutSeconds    int `yaml:"timeout_seconds"`
	NumRetries        int `yaml:"num_retries"`
	RetryAfterSeconds int `yaml:"retry_after_seconds"`
}

// FallbackSetting controls cross-provider fallback.
type FallbackSetting struct {
	Enabled   bool     `yaml:"enabled"`
	TriggerOn []string `yaml:"trigger_on"`
}

// LoggingSettings decide what ends up on disk. Request and response bodies
// must stay off: the VPS must not accumulate everyone's conversations.
type LoggingSettings struct {
	RequestBodies  bool `yaml:"request_bodies"`
	ResponseBodies bool `yaml:"response_bodies"`
	Metadata       bool `yaml:"metadata"`
}

// CacheSettings toggles the proxy response cache.
type CacheSettings struct {
	Enabled bool `yaml:"enabled"`
}

// File names inside the config directory.
const (
	ModelsFileName    = "models.yaml"
	ConsumersFileName = "consumers.yaml"
	ProxyFileName     = "proxy.yaml"
)

// Load reads and strictly parses all three configuration files from dir.
// Parse errors are returned as-is; semantic problems are reported by Validate.
func Load(dir string) (*Config, error) {
	cfg := &Config{Dir: dir}

	if err := decodeFile(filepath.Join(dir, ModelsFileName), &cfg.Models); err != nil {
		return nil, err
	}
	if err := decodeFile(filepath.Join(dir, ConsumersFileName), &cfg.Consumers); err != nil {
		return nil, err
	}
	if err := decodeFile(filepath.Join(dir, ProxyFileName), &cfg.Proxy); err != nil {
		return nil, err
	}

	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	for name, alias := range c.Models.Aliases {
		if alias.Mode == "" {
			if alias.Capabilities.Embeddings {
				alias.Mode = ModeEmbedding
			} else {
				alias.Mode = ModeChat
			}
		}
		if alias.Capabilities.JSONSchema == "" && alias.Mode == ModeChat {
			alias.Capabilities.JSONSchema = JSONSchemaUnsupported
		}
		c.Models.Aliases[name] = alias
	}
}

// decodeFile parses one YAML file with unknown fields and duplicate keys
// rejected. Both are silent-misconfiguration hazards: a typo in a key would
// otherwise be applied as "field absent".
func decodeFile(path string, out any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if dups := duplicateKeys(&doc, ""); len(dups) > 0 {
		return fmt.Errorf("%s: duplicate key %s", filepath.Base(path), dups[0])
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: file is empty", filepath.Base(path))
		}
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// duplicateKeys walks a YAML document and reports mapping keys defined twice
// under the same parent. yaml.v3 keeps the last one silently, which would let
// a second `consumers:` entry with the same name shadow the first.
func duplicateKeys(node *yaml.Node, path string) []string {
	var found []string
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			found = append(found, duplicateKeys(child, path)...)
		}
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			child := join(path, key)
			if seen[key] {
				found = append(found, fmt.Sprintf("%s (line %d)", child, node.Content[i].Line))
			}
			seen[key] = true
			found = append(found, duplicateKeys(node.Content[i+1], child)...)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			found = append(found, duplicateKeys(child, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return found
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// AliasNames returns alias names in stable order so that plans, tables and
// generated payloads never depend on Go map iteration order.
func (c *Config) AliasNames() []string {
	names := make([]string, 0, len(c.Models.Aliases))
	for name := range c.Models.Aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ConsumerNames returns consumer names in stable order.
func (c *Config) ConsumerNames() []string {
	names := make([]string, 0, len(c.Consumers.Consumers))
	for name := range c.Consumers.Consumers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BudgetDuration translates a config period into the LiteLLM budget_duration
// syntax.
func BudgetDuration(period string) string {
	switch period {
	case PeriodDaily:
		return "1d"
	case PeriodWeekly:
		return "7d"
	case PeriodMonthly:
		return "30d"
	default:
		return ""
	}
}

// IsEmbedding reports whether the alias serves /v1/embeddings.
func (a Alias) IsEmbedding() bool {
	return a.Mode == ModeEmbedding
}
