package litellm_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/romandots/llm-gateway/internal/litellm"
)

func TestNewSecretFollowsTheContract(t *testing.T) {
	secret, err := litellm.NewSecret("my-telegram-bot")
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}

	if !strings.HasPrefix(secret, "sk-gw-my-telegram-bot-") {
		t.Fatalf("secret must carry the consumer name: %q", secret)
	}
	if !litellm.SecretPattern.MatchString(secret) {
		t.Fatalf("secret does not match the contract pattern: %q", secret)
	}
	// The random tail must be long enough that the key cannot be guessed.
	tail := strings.TrimPrefix(secret, "sk-gw-my-telegram-bot-")
	if len(tail) < 32 {
		t.Fatalf("random part is only %d characters", len(tail))
	}

	other, err := litellm.NewSecret("my-telegram-bot")
	if err != nil {
		t.Fatal(err)
	}
	if other == secret {
		t.Fatal("two issued keys must never be equal")
	}
}

func TestNewSecretRequiresConsumer(t *testing.T) {
	if _, err := litellm.NewSecret(""); err == nil {
		t.Fatal("expected an error for an empty consumer name")
	}
}

func TestMaskSecret(t *testing.T) {
	secret := "sk-gw-bot-abcdefghijklmnop"
	masked := litellm.MaskSecret(secret)

	if strings.Contains(masked, "abcdefghij") {
		t.Fatalf("masked key still discloses the secret: %q", masked)
	}
	if !strings.HasSuffix(masked, "mnop") {
		t.Fatalf("masked key should keep a recognizable tail: %q", masked)
	}
	if got := litellm.MaskSecret("short"); got != "…" {
		t.Fatalf("MaskSecret(short) = %q", got)
	}
}

func TestKeyHelpers(t *testing.T) {
	managed := litellm.Key{
		KeyAlias: "bot",
		Metadata: map[string]any{
			litellm.MetadataManagedBy: litellm.ManagedByValue,
			litellm.MetadataConsumer:  "my-bot",
		},
	}
	if !managed.ManagedByGwctl() || managed.Consumer() != "my-bot" || managed.Deprecated() {
		t.Fatalf("unexpected key state: %+v", managed)
	}

	foreign := litellm.Key{KeyAlias: "made-in-the-ui"}
	if foreign.ManagedByGwctl() || foreign.Consumer() != "made-in-the-ui" {
		t.Fatalf("keys without metadata must not be treated as managed: %+v", foreign)
	}

	deprecated := litellm.Key{Metadata: map[string]any{litellm.MetadataDeprecated: true}}
	if !deprecated.Deprecated() {
		t.Fatal("deprecated flag not read")
	}
}

func TestDeploymentID(t *testing.T) {
	if got := (litellm.Deployment{}).ID(); got != "" {
		t.Fatalf("expected empty id, got %q", got)
	}
	deployment := litellm.Deployment{ModelInfo: map[string]any{"id": "gw-balanced-0"}}
	if got := deployment.ID(); got != "gw-balanced-0" {
		t.Fatalf("ID() = %q", got)
	}
}

// testServer returns a client wired to a handler, so the transport, headers
// and error decoding are exercised for real.
func testServer(t *testing.T, handler http.HandlerFunc) *litellm.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return litellm.New(server.URL, "sk-master-test", 5*time.Second)
}

func TestClientSendsMasterKeyAndDecodes(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-master-test" {
			t.Errorf("missing master key, got %q", got)
		}
		if r.URL.Path != "/model/info" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"model_name":"balanced","litellm_params":{"model":"anthropic/claude-sonnet-5"},"model_info":{"id":"gw-balanced-0"}}]}`))
	})

	deployments, err := client.ListDeployments(context.Background())
	if err != nil {
		t.Fatalf("ListDeployments: %v", err)
	}
	if len(deployments) != 1 || deployments[0].ID() != "gw-balanced-0" {
		t.Fatalf("unexpected deployments: %+v", deployments)
	}
}

func TestClientSurfacesAPIErrors(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model group not found"}}`))
	})

	err := client.CreateDeployment(context.Background(), litellm.Deployment{ModelName: "x"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "model group not found") {
		t.Fatalf("error message lost: %v", err)
	}

	var apiErr *litellm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %T", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.NotFound() {
		t.Fatalf("unexpected error details: %+v", apiErr)
	}
}

func TestClientRecognisesMissingEndpoint(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	})

	err := client.DeleteDeployment(context.Background(), "gw-x-0")
	var apiErr *litellm.APIError
	if !errors.As(err, &apiErr) || !apiErr.NotFound() {
		t.Fatalf("expected a not-found APIError, got %v", err)
	}
	if !strings.Contains(err.Error(), "Not Found") {
		t.Fatalf("detail not surfaced: %v", err)
	}
}

func TestClientKeyLifecycle(t *testing.T) {
	var generated litellm.GenerateKeyRequest

	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/generate":
			if err := json.NewDecoder(r.Body).Decode(&generated); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"key":"` + generated.Key + `","key_alias":"` + generated.KeyAlias + `"}`))
		case "/key/list":
			_, _ = w.Write([]byte(`{"keys":[{"token":"hash","key_alias":"bot","models":["cheap-fast"],"max_budget":1.5,"metadata":{"managed_by":"gwctl"}}]}`))
		case "/key/update", "/key/delete":
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	ctx := context.Background()
	result, err := client.GenerateKey(ctx, litellm.GenerateKeyRequest{Key: "sk-gw-bot-secret", KeyAlias: "bot"})
	if err != nil || result.Key != "sk-gw-bot-secret" {
		t.Fatalf("GenerateKey: %+v, %v", result, err)
	}

	keys, err := client.ListKeys(ctx)
	if err != nil || len(keys) != 1 || *keys[0].MaxBudget != 1.5 {
		t.Fatalf("ListKeys: %+v, %v", keys, err)
	}
	if err := client.UpdateKey(ctx, litellm.UpdateKeyRequest{Key: "hash"}); err != nil {
		t.Fatalf("UpdateKey: %v", err)
	}
	if err := client.DeleteKeys(ctx, []string{"hash"}); err != nil {
		t.Fatalf("DeleteKeys: %v", err)
	}
	// Deleting nothing must not produce a call at all.
	if err := client.DeleteKeys(ctx, nil); err != nil {
		t.Fatalf("DeleteKeys(nil): %v", err)
	}
}

func TestClientHealthAndSpend(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/liveness":
			_, _ = w.Write([]byte(`"I'm alive!"`))
		case "/health/readiness":
			_, _ = w.Write([]byte(`{"status":"connected","db":"connected","cache":"connected"}`))
		case "/health":
			_, _ = w.Write([]byte(`{"healthy_endpoints":[{"model":"anthropic/claude-sonnet-5"}],"unhealthy_endpoints":[]}`))
		case "/spend/logs":
			if r.URL.Query().Get("start_date") == "" || r.URL.Query().Get("end_date") == "" {
				t.Error("spend logs must be bounded by a date range")
			}
			_, _ = w.Write([]byte(`[{"request_id":"r1","api_key":"hash","model":"claude-sonnet-5","model_group":"balanced","spend":0.5,"prompt_tokens":10,"completion_tokens":5}]`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	})

	ctx := context.Background()
	if err := client.Liveness(ctx); err != nil {
		t.Fatalf("Liveness: %v", err)
	}
	readiness, err := client.Readiness(ctx)
	if err != nil || readiness["db"] != "connected" {
		t.Fatalf("Readiness: %+v, %v", readiness, err)
	}
	health, err := client.ProviderHealth(ctx)
	if err != nil || len(health.Healthy) != 1 {
		t.Fatalf("ProviderHealth: %+v, %v", health, err)
	}
	logs, err := client.SpendLogs(ctx, time.Now().Add(-24*time.Hour), time.Now())
	if err != nil || len(logs) != 1 || logs[0].Spend != 0.5 {
		t.Fatalf("SpendLogs: %+v, %v", logs, err)
	}
}

func TestFakeImplementsTheAPI(t *testing.T) {
	fake := litellm.NewFake()
	ctx := context.Background()

	deployment := litellm.Deployment{ModelName: "balanced", ModelInfo: map[string]any{"id": "gw-balanced-0"}}
	if err := fake.CreateDeployment(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	deployment.ModelName = "balanced-v2"
	if err := fake.UpdateDeployment(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	deployments, err := fake.ListDeployments(ctx)
	if err != nil || len(deployments) != 1 || deployments[0].ModelName != "balanced-v2" {
		t.Fatalf("unexpected state: %+v, %v", deployments, err)
	}
	if err := fake.DeleteDeployment(ctx, "gw-balanced-0"); err != nil {
		t.Fatal(err)
	}
	if deployments, _ := fake.ListDeployments(ctx); len(deployments) != 0 {
		t.Fatalf("deployment not removed: %+v", deployments)
	}

	if err := fake.UpdateDeployment(ctx, litellm.Deployment{ModelInfo: map[string]any{"id": "missing"}}); err == nil {
		t.Fatal("updating a missing deployment should fail")
	}
	if err := fake.UpdateKey(ctx, litellm.UpdateKeyRequest{Key: "missing"}); err == nil {
		t.Fatal("updating a missing key should fail")
	}
}
