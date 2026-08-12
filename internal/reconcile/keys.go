package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/romandots/llm-gateway/internal/config"
	"github.com/romandots/llm-gateway/internal/litellm"
)

// DeprecatedSuffix marks keys inside their revocation grace window. They keep
// working until they expire, but reconcile ignores them and the consumer's
// name is free for a new key.
const DeprecatedSuffix = "-deprecated"

// ErrKeyExists is returned when a consumer already holds an active key.
var ErrKeyExists = fmt.Errorf("consumer already has an active key")

// ErrNoKey is returned when an operation needs an existing key and there is none.
var ErrNoKey = fmt.Errorf("consumer has no active key")

// IssueKey creates a virtual key for a consumer and returns the secret. This
// is the only place the secret exists outside the proxy, and it is never
// written to disk.
func IssueKey(ctx context.Context, api litellm.API, cfg *config.Config, consumerName string) (string, error) {
	consumer, ok := cfg.Consumers.Consumers[consumerName]
	if !ok {
		return "", fmt.Errorf("consumer %q is not defined in %s", consumerName, config.ConsumersFileName)
	}

	existing, err := activeKey(ctx, api, consumerName)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return "", fmt.Errorf("%w: revoke it first or use `gwctl key rotate %s`", ErrKeyExists, consumerName)
	}

	secret, err := litellm.NewSecret(consumerName)
	if err != nil {
		return "", err
	}
	want := DesiredKeyFor(consumerName, consumer)
	if _, err := api.GenerateKey(ctx, want.GenerateRequest(secret)); err != nil {
		return "", fmt.Errorf("generate key for %s: %w", consumerName, err)
	}
	return secret, nil
}

// RevokeKey removes a consumer's key. With grace == 0 the key stops working
// immediately; otherwise it is marked deprecated and expires after grace.
func RevokeKey(ctx context.Context, api litellm.API, consumerName string, grace time.Duration) error {
	key, err := activeKey(ctx, api, consumerName)
	if err != nil {
		return err
	}
	if key == nil {
		return fmt.Errorf("%w: %s", ErrNoKey, consumerName)
	}
	return deprecate(ctx, api, *key, grace)
}

func deprecate(ctx context.Context, api litellm.API, key litellm.Key, grace time.Duration) error {
	if grace <= 0 {
		if err := api.DeleteKeys(ctx, []string{key.Token}); err != nil {
			return fmt.Errorf("delete key of %s: %w", key.Consumer(), err)
		}
		return nil
	}

	keys, err := api.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("list keys: %w", err)
	}
	metadata := map[string]any{}
	for name, value := range key.Metadata {
		metadata[name] = value
	}
	metadata[litellm.MetadataDeprecated] = true
	metadata[litellm.MetadataConsumer] = key.Consumer()

	update := litellm.UpdateKeyRequest{
		Key: key.Token,
		// Renaming frees the consumer's alias so a replacement key can be
		// issued while the old one is still serving traffic.
		KeyAlias: freeDeprecatedAlias(keys, key.Consumer()),
		Duration: formatDuration(grace),
		Metadata: metadata,
	}
	if err := api.UpdateKey(ctx, update); err != nil {
		return fmt.Errorf("deprecate key of %s: %w", key.Consumer(), err)
	}
	return nil
}

// RotateKey issues a new key and puts the old one into a grace window. It is a
// composition of revoke and issue, not a separate mechanism.
func RotateKey(ctx context.Context, api litellm.API, cfg *config.Config, consumerName string, grace time.Duration) (string, error) {
	if _, ok := cfg.Consumers.Consumers[consumerName]; !ok {
		return "", fmt.Errorf("consumer %q is not defined in %s", consumerName, config.ConsumersFileName)
	}
	key, err := activeKey(ctx, api, consumerName)
	if err != nil {
		return "", err
	}
	if key == nil {
		return "", fmt.Errorf("%w: %s (use `gwctl key issue %s`)", ErrNoKey, consumerName, consumerName)
	}
	// The old key is deprecated first: if issuing the new one fails, the
	// consumer is left with a working (if renamed) key rather than none.
	if err := deprecate(ctx, api, *key, grace); err != nil {
		return "", err
	}
	return IssueKey(ctx, api, cfg, consumerName)
}

func activeKey(ctx context.Context, api litellm.API, consumerName string) (*litellm.Key, error) {
	keys, err := api.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	for _, key := range keys {
		if key.ManagedByGwctl() && !key.Deprecated() && key.Consumer() == consumerName {
			found := key
			return &found, nil
		}
	}
	return nil, nil
}

// freeDeprecatedAlias picks an unused alias for a key being retired.
func freeDeprecatedAlias(keys []litellm.Key, consumerName string) string {
	taken := map[string]bool{}
	for _, key := range keys {
		taken[key.KeyAlias] = true
	}
	candidate := consumerName + DeprecatedSuffix
	for suffix := 2; taken[candidate]; suffix++ {
		candidate = fmt.Sprintf("%s%s-%d", consumerName, DeprecatedSuffix, suffix)
	}
	return candidate
}

// formatDuration renders a grace window in the syntax LiteLLM expects for key
// expiry (30s, 45m, 24h, 7d).
func formatDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// KeyRow is one line of `gwctl key list`.
type KeyRow struct {
	Consumer  string   `json:"consumer"`
	KeyName   string   `json:"key_name"`
	Aliases   []string `json:"aliases"`
	Budget    string   `json:"budget"`
	Spend     float64  `json:"spend_usd"`
	Limits    string   `json:"limits"`
	Status    string   `json:"status"`
	IssuedAt  string   `json:"issued_at"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

// ListKeys returns every key the control plane manages, newest state first by
// consumer name.
func ListKeys(ctx context.Context, api litellm.API) ([]KeyRow, error) {
	keys, err := api.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}

	rows := make([]KeyRow, 0, len(keys))
	for _, key := range keys {
		if !key.ManagedByGwctl() {
			continue
		}
		rows = append(rows, KeyRow{
			Consumer:  key.Consumer(),
			KeyName:   key.KeyName,
			Aliases:   key.Models,
			Budget:    budgetLabel(key),
			Spend:     key.Spend,
			Limits:    limitsLabel(key),
			Status:    statusLabel(key),
			IssuedAt:  shortTime(key.CreatedAt),
			ExpiresAt: shortTime(key.Expires),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Consumer != rows[j].Consumer {
			return rows[i].Consumer < rows[j].Consumer
		}
		return rows[i].Status < rows[j].Status
	})
	return rows, nil
}

func budgetLabel(key litellm.Key) string {
	if key.MaxBudget == nil {
		return "unlimited"
	}
	period := key.BudgetDuration
	if period == "" {
		period = "total"
	}
	return fmt.Sprintf("%.2f/%s", *key.MaxBudget, period)
}

func limitsLabel(key litellm.Key) string {
	rpm, tpm := "-", "-"
	if key.RPMLimit != nil {
		rpm = fmt.Sprint(*key.RPMLimit)
	}
	if key.TPMLimit != nil {
		tpm = fmt.Sprint(*key.TPMLimit)
	}
	return fmt.Sprintf("%s rpm / %s tpm", rpm, tpm)
}

func statusLabel(key litellm.Key) string {
	switch {
	case key.Blocked:
		return "blocked"
	case key.Deprecated():
		return "deprecated"
	default:
		return "active"
	}
}

func shortTime(value string) string {
	if value == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format("2006-01-02 15:04")
		}
	}
	if idx := strings.IndexByte(value, 'T'); idx > 0 {
		return value[:idx]
	}
	return value
}
