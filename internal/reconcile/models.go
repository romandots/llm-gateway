package reconcile

import (
	"context"
	"fmt"
	"strings"

	"github.com/romandots/llm-gateway/internal/config"
	"github.com/romandots/llm-gateway/internal/litellm"
)

// AliasRow is one line of `gwctl models`: an alias and where it points today.
type AliasRow struct {
	Alias        string   `json:"alias"`
	Mode         string   `json:"mode"`
	Primary      string   `json:"primary"`
	Fallbacks    []string `json:"fallbacks"`
	Capabilities string   `json:"capabilities"`
	Live         string   `json:"live"`
}

// AliasRows renders the alias taxonomy. When live deployments are supplied,
// each alias is checked against what the proxy actually serves, so a stale
// proxy is visible instead of being taken on faith from the config file.
func AliasRows(cfg *config.Config, live []litellm.Deployment) []AliasRow {
	liveByID := map[string]litellm.Deployment{}
	for _, deployment := range live {
		if id := deployment.ID(); id != "" {
			liveByID[id] = deployment
		}
	}

	rows := make([]AliasRow, 0, len(cfg.Models.Aliases))
	for _, name := range cfg.AliasNames() {
		alias := cfg.Models.Aliases[name]
		row := AliasRow{
			Alias:        name,
			Mode:         alias.Mode,
			Capabilities: capabilityLabel(alias.Capabilities),
			Fallbacks:    []string{},
		}

		missing := 0
		for index, target := range alias.Targets {
			vendor := target.Provider + "/" + target.Model
			if index == 0 {
				row.Primary = vendor
			} else {
				row.Fallbacks = append(row.Fallbacks, vendor)
			}
			if _, ok := liveByID[DeploymentID(name, index)]; !ok {
				missing++
			}
		}

		switch {
		case live == nil:
			row.Live = "not checked"
		case missing == 0:
			row.Live = "in sync"
		default:
			row.Live = fmt.Sprintf("%d/%d target(s) missing in proxy", missing, len(alias.Targets))
		}
		rows = append(rows, row)
	}
	return rows
}

func capabilityLabel(caps config.Capabilities) string {
	var parts []string
	if caps.Streaming {
		parts = append(parts, "stream")
	}
	if caps.Tools {
		parts = append(parts, "tools")
	}
	if caps.Vision {
		parts = append(parts, "vision")
	}
	if caps.Embeddings {
		parts = append(parts, "embeddings")
	}
	if caps.JSONSchema != "" && caps.JSONSchema != config.JSONSchemaUnsupported {
		parts = append(parts, "json:"+caps.JSONSchema)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

// HealthRow is one line of `gwctl health`.
type HealthRow struct {
	Component string `json:"component"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
}

// Health status values.
const (
	StatusOK     = "ok"
	StatusFailed = "failed"
	StatusUnknwn = "unknown"
)

// HealthChecker is the subset of the proxy client `gwctl health` needs.
type HealthChecker interface {
	Liveness(ctx context.Context) error
	Readiness(ctx context.Context) (map[string]any, error)
	ProviderHealth(ctx context.Context) (litellm.HealthReport, error)
}

// Health probes the proxy, its dependencies and every configured provider
// deployment.
func Health(ctx context.Context, checker HealthChecker) ([]HealthRow, bool) {
	rows := []HealthRow{}
	healthy := true

	if err := checker.Liveness(ctx); err != nil {
		rows = append(rows, HealthRow{Component: "proxy", Status: StatusFailed, Detail: err.Error()})
		// Nothing else can be probed through a proxy that does not answer.
		return rows, false
	}
	rows = append(rows, HealthRow{Component: "proxy", Status: StatusOK, Detail: "liveness"})

	readiness, err := checker.Readiness(ctx)
	if err != nil {
		rows = append(rows, HealthRow{Component: "readiness", Status: StatusFailed, Detail: err.Error()})
		healthy = false
	} else {
		for _, row := range readinessRows(readiness) {
			if row.Status == StatusFailed {
				healthy = false
			}
			rows = append(rows, row)
		}
	}

	report, err := checker.ProviderHealth(ctx)
	if err != nil {
		rows = append(rows, HealthRow{Component: "providers", Status: StatusUnknwn, Detail: err.Error()})
		return rows, false
	}
	for _, endpoint := range report.Healthy {
		rows = append(rows, HealthRow{Component: "model " + endpointName(endpoint), Status: StatusOK})
	}
	for _, endpoint := range report.Unhealthy {
		healthy = false
		rows = append(rows, HealthRow{
			Component: "model " + endpointName(endpoint),
			Status:    StatusFailed,
			Detail:    endpointError(endpoint),
		})
	}
	return rows, healthy
}

func readinessRows(readiness map[string]any) []HealthRow {
	rows := []HealthRow{}

	// Fixed order, from the dependency that fails hardest to the summary.
	for _, key := range []string{"db", "cache", "status"} {
		if _, ok := readiness[key]; !ok {
			continue
		}
		switch key {
		case "db", "cache":
			value := fmt.Sprint(readiness[key])
			status := StatusOK
			if !strings.Contains(strings.ToLower(value), "connected") && value != "true" {
				status = StatusFailed
			}
			rows = append(rows, HealthRow{Component: componentName(key), Status: status, Detail: value})
		case "status":
			value := fmt.Sprint(readiness[key])
			status := StatusOK
			if value != "connected" && value != "healthy" {
				status = StatusFailed
			}
			rows = append(rows, HealthRow{Component: "readiness", Status: status, Detail: value})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, HealthRow{Component: "readiness", Status: StatusOK, Detail: "no details reported"})
	}
	return rows
}

func componentName(key string) string {
	if key == "db" {
		return "postgres"
	}
	return "redis"
}

func endpointName(endpoint map[string]any) string {
	for _, key := range []string{"model", "model_name", "litellm_model_name"} {
		if value, ok := endpoint[key].(string); ok && value != "" {
			return value
		}
	}
	return "(unnamed)"
}

func endpointError(endpoint map[string]any) string {
	if value, ok := endpoint["error"].(string); ok && value != "" {
		if len(value) > 160 {
			return value[:160] + "…"
		}
		return value
	}
	return "unhealthy"
}
