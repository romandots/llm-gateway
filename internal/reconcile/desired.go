// Package reconcile turns the declarative configuration into an idempotent
// plan of Admin API calls and applies it.
package reconcile

import (
	"fmt"
	"sort"

	"github.com/romandots/llm-gateway/internal/config"
	"github.com/romandots/llm-gateway/internal/litellm"
)

// Environment variables holding the vendor keys inside the proxy container.
// The control plane never sees their values: it only references them.
var providerKeyEnv = map[string]string{
	config.ProviderAnthropic: "os.environ/ANTHROPIC_API_KEY",
	config.ProviderOpenAI:    "os.environ/OPENAI_API_KEY",
}

// GroupName is the LiteLLM model group serving a target. The primary target of
// an alias owns the alias name itself; every fallback gets its own group so
// that the order in models.yaml becomes a deterministic routing priority
// instead of load balancing inside one group.
func GroupName(alias string, index int) string {
	if index == 0 {
		return alias
	}
	return fmt.Sprintf("%s--fallback-%d", alias, index)
}

// DeploymentID is the stable control-plane identifier of a deployment. It is
// derived from the alias and position, so reconcile can match configuration
// against proxy state without keeping any local database.
func DeploymentID(alias string, index int) string {
	return fmt.Sprintf("gw-%s-%d", alias, index)
}

// IsFallbackGroup reports whether a model group name denotes a fallback.
func IsFallbackGroup(group string) bool {
	_, ok := splitFallback(group)
	return ok
}

// AliasOfGroup maps a model group back to the alias a consumer asked for.
func AliasOfGroup(group string) string {
	if alias, ok := splitFallback(group); ok {
		return alias
	}
	return group
}

func splitFallback(group string) (string, bool) {
	const marker = "--fallback-"
	idx := len(group) - 1
	for ; idx >= 0; idx-- {
		if group[idx] < '0' || group[idx] > '9' {
			break
		}
	}
	if idx == len(group)-1 || idx < len(marker)-1 {
		return "", false
	}
	head := group[:idx+1]
	if len(head) < len(marker) || head[len(head)-len(marker):] != marker {
		return "", false
	}
	return head[:len(head)-len(marker)], true
}

// DesiredDeployments renders every alias target as a LiteLLM deployment, in a
// stable order.
func DesiredDeployments(cfg *config.Config) []litellm.Deployment {
	var out []litellm.Deployment
	for _, aliasName := range cfg.AliasNames() {
		alias := cfg.Models.Aliases[aliasName]
		for index, target := range alias.Targets {
			out = append(out, deployment(aliasName, alias, index, target))
		}
	}
	return out
}

func deployment(aliasName string, alias config.Alias, index int, target config.Target) litellm.Deployment {
	params := map[string]any{
		"model": target.Provider + "/" + target.Model,
	}
	if env, ok := providerKeyEnv[target.Provider]; ok {
		params["api_key"] = env
	}
	for key, value := range target.Params {
		params[key] = value
	}

	role := "primary"
	if index > 0 {
		role = "fallback"
	}

	info := map[string]any{
		"id":   DeploymentID(aliasName, index),
		"mode": alias.Mode,
		"gw": map[string]any{
			"alias":             aliasName,
			"role":              role,
			"priority":          index,
			"description":       alias.Description,
			"context_window":    alias.ContextWindow,
			"max_output_tokens": alias.MaxOutputTokens,
			"capabilities": map[string]any{
				"streaming":   alias.Capabilities.Streaming,
				"tools":       alias.Capabilities.Tools,
				"json_schema": alias.Capabilities.JSONSchema,
				"vision":      alias.Capabilities.Vision,
				"embeddings":  alias.Capabilities.Embeddings,
			},
			"managed_by": litellm.ManagedByValue,
		},
	}

	return litellm.Deployment{
		ModelName:     GroupName(aliasName, index),
		LiteLLMParams: params,
		ModelInfo:     info,
	}
}

// Fallbacks returns the fallback chains implied by the target order of every
// alias: alias -> ordered list of fallback model groups.
func Fallbacks(cfg *config.Config) []map[string][]string {
	out := []map[string][]string{}
	if !cfg.Proxy.Fallback.Enabled {
		return out
	}

	for _, aliasName := range cfg.AliasNames() {
		alias := cfg.Models.Aliases[aliasName]
		if len(alias.Targets) < 2 {
			continue
		}
		chain := make([]string, 0, len(alias.Targets)-1)
		for index := 1; index < len(alias.Targets); index++ {
			chain = append(chain, GroupName(aliasName, index))
		}
		out = append(out, map[string][]string{aliasName: chain})
	}
	return out
}

// desiredKey is the attribute set a consumer's virtual key must have.
type desiredKey struct {
	Consumer       string
	Aliases        []string
	MaxBudget      float64
	BudgetDuration string
	RPM            int
	TPM            int
	Metadata       map[string]any
}

// DesiredKeyFor renders the key attributes of one consumer.
func DesiredKeyFor(name string, consumer config.Consumer) desiredKey {
	aliases := append([]string(nil), consumer.Aliases...)
	sort.Strings(aliases)

	return desiredKey{
		Consumer:       name,
		Aliases:        aliases,
		MaxBudget:      consumer.Budget.AmountUSD,
		BudgetDuration: config.BudgetDuration(consumer.Budget.Period),
		RPM:            consumer.Limits.RPM,
		TPM:            consumer.Limits.TPM,
		Metadata: map[string]any{
			litellm.MetadataConsumer:  name,
			litellm.MetadataOwner:     consumer.Owner,
			litellm.MetadataManagedBy: litellm.ManagedByValue,
		},
	}
}

// GenerateRequest turns desired key attributes into a create call.
func (d desiredKey) GenerateRequest(secret string) litellm.GenerateKeyRequest {
	return litellm.GenerateKeyRequest{
		Key:            secret,
		KeyAlias:       d.Consumer,
		Models:         d.Aliases,
		MaxBudget:      d.MaxBudget,
		BudgetDuration: d.BudgetDuration,
		RPMLimit:       d.RPM,
		TPMLimit:       d.TPM,
		Metadata:       d.Metadata,
	}
}

// UpdateRequest turns desired key attributes into an update call for the key
// identified by token.
func (d desiredKey) UpdateRequest(token string) litellm.UpdateKeyRequest {
	budget := d.MaxBudget
	rpm, tpm := d.RPM, d.TPM
	return litellm.UpdateKeyRequest{
		Key:            token,
		Models:         d.Aliases,
		MaxBudget:      &budget,
		BudgetDuration: d.BudgetDuration,
		RPMLimit:       &rpm,
		TPMLimit:       &tpm,
		Metadata:       d.Metadata,
	}
}
