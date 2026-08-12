// Package litellm is a thin client for the LiteLLM proxy Admin API. It covers
// only what the control plane needs: model deployments, virtual keys, router
// settings, health and spend logs.
package litellm

import "context"

// Deployment is one entry of the proxy's model_list. Several deployments may
// share a ModelName; LiteLLM then treats them as one model group.
type Deployment struct {
	ModelName     string         `json:"model_name"`
	LiteLLMParams map[string]any `json:"litellm_params"`
	ModelInfo     map[string]any `json:"model_info,omitempty"`
}

// ID returns the deployment identifier assigned by the control plane, or ""
// for a deployment the gateway does not manage.
func (d Deployment) ID() string {
	if d.ModelInfo == nil {
		return ""
	}
	id, _ := d.ModelInfo["id"].(string)
	return id
}

// Key is a virtual key as returned by the proxy. The secret itself is never
// part of it: Token is the hash stored by LiteLLM, KeyName a masked preview.
type Key struct {
	Token          string         `json:"token"`
	KeyName        string         `json:"key_name"`
	KeyAlias       string         `json:"key_alias"`
	Models         []string       `json:"models"`
	Spend          float64        `json:"spend"`
	MaxBudget      *float64       `json:"max_budget"`
	BudgetDuration string         `json:"budget_duration"`
	BudgetResetAt  string         `json:"budget_reset_at"`
	RPMLimit       *int           `json:"rpm_limit"`
	TPMLimit       *int           `json:"tpm_limit"`
	Expires        string         `json:"expires"`
	CreatedAt      string         `json:"created_at"`
	Blocked        bool           `json:"blocked"`
	Metadata       map[string]any `json:"metadata"`
}

// Consumer returns the consumer this key was issued for.
func (k Key) Consumer() string {
	if k.Metadata != nil {
		if name, ok := k.Metadata[MetadataConsumer].(string); ok && name != "" {
			return name
		}
	}
	return k.KeyAlias
}

// ManagedByGwctl reports whether the control plane owns this key. Keys created
// by hand through the LiteLLM UI are left alone by reconcile.
func (k Key) ManagedByGwctl() bool {
	if k.Metadata == nil {
		return false
	}
	owner, _ := k.Metadata[MetadataManagedBy].(string)
	return owner == ManagedByValue
}

// Deprecated reports whether the key is inside its revocation grace window.
func (k Key) Deprecated() bool {
	if k.Metadata == nil {
		return false
	}
	deprecated, _ := k.Metadata[MetadataDeprecated].(bool)
	return deprecated
}

// Metadata keys written by gwctl onto every managed virtual key.
const (
	MetadataConsumer   = "consumer"
	MetadataOwner      = "owner"
	MetadataManagedBy  = "managed_by"
	MetadataDeprecated = "deprecated"
	ManagedByValue     = "gwctl"
)

// GenerateKeyRequest creates a virtual key. Key carries the pre-generated
// secret so that it follows the sk-gw-<consumer>-<random> contract instead of
// LiteLLM's own format.
type GenerateKeyRequest struct {
	Key            string         `json:"key,omitempty"`
	KeyAlias       string         `json:"key_alias"`
	Models         []string       `json:"models"`
	MaxBudget      float64        `json:"max_budget"`
	BudgetDuration string         `json:"budget_duration"`
	RPMLimit       int            `json:"rpm_limit"`
	TPMLimit       int            `json:"tpm_limit"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// UpdateKeyRequest changes the attributes of an existing key. Key is the token
// hash from Key.Token — the secret is not needed and not stored anywhere.
type UpdateKeyRequest struct {
	Key            string         `json:"key"`
	KeyAlias       string         `json:"key_alias,omitempty"`
	Models         []string       `json:"models,omitempty"`
	MaxBudget      *float64       `json:"max_budget,omitempty"`
	BudgetDuration string         `json:"budget_duration,omitempty"`
	RPMLimit       *int           `json:"rpm_limit,omitempty"`
	TPMLimit       *int           `json:"tpm_limit,omitempty"`
	Duration       string         `json:"duration,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// GeneratedKey is the one and only moment the secret exists outside the proxy.
type GeneratedKey struct {
	Key      string `json:"key"`
	KeyName  string `json:"key_name"`
	KeyAlias string `json:"key_alias"`
	Expires  string `json:"expires"`
}

// SpendLog is one accounted request. Bodies are never part of it.
type SpendLog struct {
	RequestID        string  `json:"request_id"`
	APIKey           string  `json:"api_key"`
	Model            string  `json:"model"`
	ModelGroup       string  `json:"model_group"`
	Spend            float64 `json:"spend"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	StartTime        string  `json:"startTime"`
	CallType         string  `json:"call_type"`
}

// HealthReport is the aggregated result of the proxy's provider health check.
type HealthReport struct {
	Healthy   []map[string]any `json:"healthy_endpoints"`
	Unhealthy []map[string]any `json:"unhealthy_endpoints"`
}

// API is the surface reconcile depends on. It exists so plans can be built and
// applied against a fake in tests.
type API interface {
	ListDeployments(ctx context.Context) ([]Deployment, error)
	CreateDeployment(ctx context.Context, d Deployment) error
	UpdateDeployment(ctx context.Context, d Deployment) error
	DeleteDeployment(ctx context.Context, id string) error

	ListKeys(ctx context.Context) ([]Key, error)
	GenerateKey(ctx context.Context, req GenerateKeyRequest) (GeneratedKey, error)
	UpdateKey(ctx context.Context, req UpdateKeyRequest) error
	DeleteKeys(ctx context.Context, tokens []string) error
}
