package reconcile_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/romandots/llm-gateway/internal/config"
	"github.com/romandots/llm-gateway/internal/litellm"
	"github.com/romandots/llm-gateway/internal/reconcile"
)

const models = `
version: 1
aliases:
  balanced:
    description: "workhorse"
    mode: chat
    capabilities:
      streaming: true
      tools: true
      json_schema: native
      vision: false
      embeddings: false
    context_window: 1000000
    max_output_tokens: 128000
    targets:
      - provider: anthropic
        model: claude-sonnet-5
      - provider: openai
        model: gpt-5
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

const consumers = `
version: 1
consumers:
  my-bot:
    description: "bot"
    owner: "roman"
    aliases: [balanced]
    budget:
      amount_usd: 5
      period: daily
    limits:
      rpm: 60
      tpm: 100000
`

const proxy = `
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

// setup writes a config tree and returns the loaded config plus the repo root.
func setup(t *testing.T) (*config.Config, string) {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		config.ModelsFileName:    models,
		config.ConsumersFileName: consumers,
		config.ProxyFileName:     proxy,
	} {
		if err := os.WriteFile(filepath.Join(configDir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if issues := cfg.Validate(); issues.HasErrors() {
		t.Fatalf("fixture is invalid: %v", issues)
	}
	return cfg, root
}

func TestGroupNamesEncodePriority(t *testing.T) {
	if got := reconcile.GroupName("balanced", 0); got != "balanced" {
		t.Fatalf("the primary target owns the alias itself, got %q", got)
	}
	fallback := reconcile.GroupName("balanced", 1)
	if fallback != "balanced--fallback-1" {
		t.Fatalf("unexpected fallback group %q", fallback)
	}
	if !reconcile.IsFallbackGroup(fallback) || reconcile.IsFallbackGroup("balanced") {
		t.Fatal("fallback groups must be distinguishable from aliases")
	}
	if got := reconcile.AliasOfGroup(fallback); got != "balanced" {
		t.Fatalf("AliasOfGroup(%q) = %q", fallback, got)
	}
	if got := reconcile.AliasOfGroup("balanced"); got != "balanced" {
		t.Fatalf("AliasOfGroup(alias) must be the alias, got %q", got)
	}
	// A name that merely looks similar is not a fallback group.
	if reconcile.IsFallbackGroup("balanced--fallback-") || reconcile.IsFallbackGroup("fallback-1") {
		t.Fatal("malformed names must not be read as fallback groups")
	}
}

func TestDesiredDeployments(t *testing.T) {
	cfg, _ := setup(t)
	deployments := reconcile.DesiredDeployments(cfg)

	if len(deployments) != 3 {
		t.Fatalf("expected 3 deployments (2 targets + 1 embedding), got %d", len(deployments))
	}

	primary := deployments[0]
	if primary.ModelName != "balanced" || primary.LiteLLMParams["model"] != "anthropic/claude-sonnet-5" {
		t.Fatalf("unexpected primary deployment: %+v", primary)
	}
	// The vendor key is referenced, never copied into the control plane.
	if primary.LiteLLMParams["api_key"] != "os.environ/ANTHROPIC_API_KEY" {
		t.Fatalf("vendor key must be an env reference, got %v", primary.LiteLLMParams["api_key"])
	}
	if primary.ID() != "gw-balanced-0" {
		t.Fatalf("unexpected deployment id %q", primary.ID())
	}

	gw, ok := primary.ModelInfo["gw"].(map[string]any)
	if !ok || gw["role"] != "primary" || gw["managed_by"] != litellm.ManagedByValue {
		t.Fatalf("unexpected model_info: %+v", primary.ModelInfo)
	}
	capabilities, ok := gw["capabilities"].(map[string]any)
	if !ok || capabilities["json_schema"] != config.JSONSchemaNative {
		t.Fatalf("capabilities not published to the proxy: %+v", gw)
	}

	if deployments[1].ModelName != "balanced--fallback-1" {
		t.Fatalf("second target must become a fallback group: %+v", deployments[1])
	}
	if deployments[2].ModelInfo["mode"] != config.ModeEmbedding {
		t.Fatalf("embedding alias must be declared as such: %+v", deployments[2].ModelInfo)
	}
}

func TestFallbacksFollowTargetOrder(t *testing.T) {
	cfg, _ := setup(t)

	fallbacks := reconcile.Fallbacks(cfg)
	if len(fallbacks) != 1 {
		t.Fatalf("only balanced has a second target: %+v", fallbacks)
	}
	if chain := fallbacks[0]["balanced"]; len(chain) != 1 || chain[0] != "balanced--fallback-1" {
		t.Fatalf("unexpected fallback chain: %+v", fallbacks[0])
	}

	// An alias without a fallback (embeddings, reasoning) must not get one.
	if _, ok := fallbacks[0]["embed-fast"]; ok {
		t.Fatal("embed-fast must have no fallback")
	}

	cfg.Proxy.Fallback.Enabled = false
	if got := reconcile.Fallbacks(cfg); len(got) != 0 {
		t.Fatalf("fallback disabled, got %+v", got)
	}
}

func TestRenderProxyConfigIsDeterministic(t *testing.T) {
	cfg, _ := setup(t)

	first, err := reconcile.RenderProxyConfig(cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := reconcile.RenderProxyConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("rendering twice must produce identical bytes, otherwise apply is never idempotent")
	}

	text := string(first)
	for _, want := range []string{
		"turn_off_message_logging: true",
		"timeout: 30",
		"balanced--fallback-1",
		"store_model_in_db: true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered config is missing %q", want)
		}
	}
	if strings.Contains(text, "ANTHROPIC_API_KEY: sk-") {
		t.Error("rendered config must not contain a secret")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	cfg, root := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()
	options := reconcile.Options{RepoRoot: root}

	plan, err := reconcile.Build(ctx, fake, cfg, options)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if plan.Empty() {
		t.Fatal("the first plan on an empty proxy must not be empty")
	}
	if err := reconcile.Apply(ctx, fake, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	second, err := reconcile.Build(ctx, fake, cfg, options)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !second.Empty() {
		t.Fatalf("applying twice must be a no-op, got:\n%s", second.String())
	}
	if got := second.String(); !strings.Contains(got, "key.missing") {
		t.Fatalf("a consumer without a key must stay visible: %q", got)
	}

	calls := len(fake.Calls)
	if err := reconcile.Apply(ctx, fake, second); err != nil {
		t.Fatalf("apply of an empty plan: %v", err)
	}
	if len(fake.Calls) != calls {
		t.Fatalf("an empty plan must not call the proxy, new calls: %v", fake.Calls[calls:])
	}
}

func TestBuildDetectsDrift(t *testing.T) {
	cfg, root := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()
	options := reconcile.Options{RepoRoot: root}

	plan, err := reconcile.Build(ctx, fake, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile.Apply(ctx, fake, plan); err != nil {
		t.Fatal(err)
	}

	// Someone repointed the alias in the proxy UI: reconcile must pull it back.
	fake.Deployments[0].LiteLLMParams["model"] = "anthropic/claude-haiku-4-5"

	drifted, err := reconcile.Build(ctx, fake, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	if drifted.Empty() {
		t.Fatal("expected the repointed deployment to show up as drift")
	}
	if got := drifted.String(); !strings.Contains(got, "claude-haiku-4-5 -> anthropic/claude-sonnet-5") {
		t.Fatalf("drift should name both sides, got:\n%s", got)
	}

	if err := reconcile.Apply(ctx, fake, drifted); err != nil {
		t.Fatal(err)
	}
	if fake.Deployments[0].LiteLLMParams["model"] != "anthropic/claude-sonnet-5" {
		t.Fatalf("drift not corrected: %+v", fake.Deployments[0].LiteLLMParams)
	}
}

func TestBuildIgnoresMaskedAPIKey(t *testing.T) {
	cfg, root := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()
	options := reconcile.Options{RepoRoot: root}

	plan, _ := reconcile.Build(ctx, fake, cfg, options)
	if err := reconcile.Apply(ctx, fake, plan); err != nil {
		t.Fatal(err)
	}

	// The proxy returns the vendor key masked. Comparing it would produce a
	// plan that never converges.
	for i := range fake.Deployments {
		fake.Deployments[i].LiteLLMParams["api_key"] = "sk-ant-...4f2a"
	}

	second, err := reconcile.Build(ctx, fake, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Empty() {
		t.Fatalf("a masked api_key must not read as drift:\n%s", second.String())
	}
}

func TestBuildRemovesStaleDeploymentsAndLeavesForeignOnesAlone(t *testing.T) {
	cfg, root := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()
	options := reconcile.Options{RepoRoot: root}

	plan, _ := reconcile.Build(ctx, fake, cfg, options)
	if err := reconcile.Apply(ctx, fake, plan); err != nil {
		t.Fatal(err)
	}

	// A deployment left over from an alias that has been removed.
	fake.Deployments = append(fake.Deployments, litellm.Deployment{
		ModelName:     "retired",
		LiteLLMParams: map[string]any{"model": "openai/gpt-4o"},
		ModelInfo: map[string]any{
			"id": "gw-retired-0",
			"gw": map[string]any{"managed_by": litellm.ManagedByValue, "alias": "retired"},
		},
	})
	// A deployment somebody added by hand in the LiteLLM UI.
	fake.Deployments = append(fake.Deployments, litellm.Deployment{
		ModelName:     "hand-made",
		LiteLLMParams: map[string]any{"model": "openai/gpt-4o"},
		ModelInfo:     map[string]any{"id": "manual-1"},
	})

	next, err := reconcile.Build(ctx, fake, cfg, options)
	if err != nil {
		t.Fatal(err)
	}

	deletes := 0
	for _, action := range next.Actions {
		if action.Kind == reconcile.KindDeleteModel {
			deletes++
			if !strings.Contains(action.Name, "gw-retired-0") {
				t.Errorf("unexpected deletion of %q", action.Name)
			}
		}
	}
	if deletes != 1 {
		t.Fatalf("exactly the stale managed deployment must be deleted, got %d", deletes)
	}
	if !containsNotice(next.Notices, "unmanaged deployment") {
		t.Fatalf("an unmanaged deployment should be reported, notices: %v", next.Notices)
	}
}

func TestKeyAttributeDriftIsPlanned(t *testing.T) {
	cfg, root := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()

	secret, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !litellm.SecretPattern.MatchString(secret) {
		t.Fatalf("issued key does not follow the contract: %q", secret)
	}

	budget := 99.0
	fake.Keys[0].MaxBudget = &budget
	fake.Keys[0].Models = []string{"balanced", "smart"}

	plan, err := reconcile.Build(ctx, fake, cfg, reconcile.Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile.Apply(ctx, fake, plan); err != nil {
		t.Fatal(err)
	}

	if *fake.Keys[0].MaxBudget != 5 {
		t.Fatalf("budget not brought back to the configuration: %v", *fake.Keys[0].MaxBudget)
	}
	if len(fake.Keys[0].Models) != 1 || fake.Keys[0].Models[0] != "balanced" {
		t.Fatalf("alias whitelist not corrected: %v", fake.Keys[0].Models)
	}
}

func TestOrphanedKeysAreOnlyRevokedWhenAsked(t *testing.T) {
	cfg, root := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()

	if _, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot"); err != nil {
		t.Fatal(err)
	}
	delete(cfg.Consumers.Consumers, "my-bot")

	plan, err := reconcile.Build(ctx, fake, cfg, reconcile.Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.Actions {
		if action.Kind == reconcile.KindRevokeKey {
			t.Fatal("a key must never be revoked without --prune-keys")
		}
	}
	if !containsNotice(plan.Notices, "has no entry in consumers.yaml") {
		t.Fatalf("expected a notice about the orphaned key, got %v", plan.Notices)
	}

	pruning, err := reconcile.Build(ctx, fake, cfg, reconcile.Options{RepoRoot: root, PruneKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcile.Apply(ctx, fake, pruning); err != nil {
		t.Fatal(err)
	}
	if len(fake.Keys) != 0 {
		t.Fatalf("key not revoked: %+v", fake.Keys)
	}
}

func TestIssueKeyRefusesDuplicatesAndUnknownConsumers(t *testing.T) {
	cfg, _ := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()

	if _, err := reconcile.IssueKey(ctx, fake, cfg, "nobody"); err == nil {
		t.Fatal("expected an error for a consumer that is not configured")
	}
	if _, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot"); err != nil {
		t.Fatal(err)
	}
	_, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot")
	if !errors.Is(err, reconcile.ErrKeyExists) {
		t.Fatalf("expected ErrKeyExists, got %v", err)
	}
}

func TestRevokeImmediatelyAndWithGrace(t *testing.T) {
	cfg, _ := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()

	if err := reconcile.RevokeKey(ctx, fake, "my-bot", 0); !errors.Is(err, reconcile.ErrNoKey) {
		t.Fatalf("expected ErrNoKey, got %v", err)
	}

	if _, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot"); err != nil {
		t.Fatal(err)
	}
	if err := reconcile.RevokeKey(ctx, fake, "my-bot", 24*time.Hour); err != nil {
		t.Fatalf("revoke with grace: %v", err)
	}

	if len(fake.Keys) != 1 {
		t.Fatalf("a key inside its grace window must keep existing: %+v", fake.Keys)
	}
	retired := fake.Keys[0]
	if !retired.Deprecated() {
		t.Fatal("the retired key must be marked deprecated")
	}
	if retired.KeyAlias != "my-bot"+reconcile.DeprecatedSuffix {
		t.Fatalf("the consumer's alias must be freed, got %q", retired.KeyAlias)
	}
	if retired.Expires != "in 1d" {
		t.Fatalf("grace window not applied: %q", retired.Expires)
	}

	// Now an immediate revoke of a freshly issued key.
	if _, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot"); err != nil {
		t.Fatal(err)
	}
	if err := reconcile.RevokeKey(ctx, fake, "my-bot", 0); err != nil {
		t.Fatal(err)
	}
	for _, key := range fake.Keys {
		if key.ManagedByGwctl() && !key.Deprecated() {
			t.Fatalf("active key survived an immediate revoke: %+v", key)
		}
	}
}

func TestRotateKeepsTheOldKeyAlive(t *testing.T) {
	cfg, _ := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()

	if _, err := reconcile.RotateKey(ctx, fake, cfg, "my-bot", time.Hour); !errors.Is(err, reconcile.ErrNoKey) {
		t.Fatalf("rotating without a key should fail with ErrNoKey, got %v", err)
	}

	first, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot")
	if err != nil {
		t.Fatal(err)
	}
	second, err := reconcile.RotateKey(ctx, fake, cfg, "my-bot", 24*time.Hour)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if first == second {
		t.Fatal("rotation must produce a new secret")
	}
	if len(fake.Keys) != 2 {
		t.Fatalf("both keys must exist during the grace window: %+v", fake.Keys)
	}

	rows, err := reconcile.ListKeys(ctx, fake)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]int{}
	for _, row := range rows {
		statuses[row.Status]++
		if strings.Contains(row.KeyName, second) {
			t.Fatal("key list must never contain the secret itself")
		}
	}
	if statuses["active"] != 1 || statuses["deprecated"] != 1 {
		t.Fatalf("unexpected key statuses: %v", statuses)
	}
}

func TestRotateReusesDeprecatedAliasesWithoutClashing(t *testing.T) {
	cfg, _ := setup(t)
	fake := litellm.NewFake()
	ctx := context.Background()

	if _, err := reconcile.IssueKey(ctx, fake, cfg, "my-bot"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := reconcile.RotateKey(ctx, fake, cfg, "my-bot", time.Hour); err != nil {
			t.Fatalf("rotation %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	for _, key := range fake.Keys {
		if seen[key.KeyAlias] {
			t.Fatalf("duplicate key alias %q", key.KeyAlias)
		}
		seen[key.KeyAlias] = true
	}
}

func TestListKeysSkipsForeignKeys(t *testing.T) {
	fake := litellm.NewFake()
	fake.Keys = []litellm.Key{{KeyAlias: "made-in-the-ui"}}

	rows, err := reconcile.ListKeys(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("gwctl only reports the keys it manages, got %+v", rows)
	}
}

func TestAggregateSpend(t *testing.T) {
	keys := []litellm.Key{{
		Token:    "hash-bot",
		KeyAlias: "my-bot",
		Metadata: map[string]any{litellm.MetadataManagedBy: litellm.ManagedByValue, litellm.MetadataConsumer: "my-bot"},
	}}
	logs := []litellm.SpendLog{
		{APIKey: "hash-bot", Model: "claude-sonnet-5", ModelGroup: "balanced", Spend: 1.5, PromptTokens: 1000, CompletionTokens: 100},
		{APIKey: "hash-bot", Model: "gpt-5", ModelGroup: "balanced--fallback-1", Spend: 0.5, PromptTokens: 500, CompletionTokens: 50},
		{APIKey: "unknown", Model: "gpt-5", ModelGroup: "smart", Spend: 3, PromptTokens: 10, CompletionTokens: 1},
	}

	byConsumer, err := reconcile.AggregateSpend(logs, reconcile.ByConsumer, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(byConsumer) != 2 || byConsumer[0].Group != "(unknown key)" {
		t.Fatalf("rows must be sorted by cost: %+v", byConsumer)
	}
	bot := findRow(t, byConsumer, "my-bot")
	if bot.Requests != 2 || bot.CostUSD != 2 || bot.TokensIn != 1500 {
		t.Fatalf("unexpected totals: %+v", bot)
	}
	if bot.Fallbacks != 1 {
		t.Fatalf("one request was served by a fallback, got %d", bot.Fallbacks)
	}

	byAlias, err := reconcile.AggregateSpend(logs, reconcile.ByAlias, keys)
	if err != nil {
		t.Fatal(err)
	}
	// Both the primary and its fallback are reported under the alias the
	// consumer actually asked for.
	balanced := findRow(t, byAlias, "balanced")
	if balanced.Requests != 2 {
		t.Fatalf("fallback traffic must be attributed to the alias: %+v", balanced)
	}

	byModel, err := reconcile.AggregateSpend(logs, reconcile.ByModel, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel) != 2 {
		t.Fatalf("expected one row per vendor model: %+v", byModel)
	}

	total := reconcile.TotalSpend(byConsumer)
	if total.CostUSD != 5 || total.Requests != 3 {
		t.Fatalf("unexpected total: %+v", total)
	}

	if _, err := reconcile.AggregateSpend(logs, "phase-of-the-moon", keys); err == nil {
		t.Fatal("expected an error for an unknown grouping")
	}
}

func TestAliasRowsReportProxyState(t *testing.T) {
	cfg, _ := setup(t)

	offline := reconcile.AliasRows(cfg, nil)
	if len(offline) != 2 || offline[0].Live != "not checked" {
		t.Fatalf("without proxy state nothing should be claimed: %+v", offline)
	}
	if offline[0].Primary != "anthropic/claude-sonnet-5" || len(offline[0].Fallbacks) != 1 {
		t.Fatalf("unexpected mapping: %+v", offline[0])
	}
	if !strings.Contains(offline[0].Capabilities, "json:native") {
		t.Fatalf("capabilities not rendered: %q", offline[0].Capabilities)
	}

	partial := reconcile.AliasRows(cfg, reconcile.DesiredDeployments(cfg)[:1])
	if partial[0].Live != "1/2 target(s) missing in proxy" {
		t.Fatalf("a half-applied alias must be visible: %q", partial[0].Live)
	}

	full := reconcile.AliasRows(cfg, reconcile.DesiredDeployments(cfg))
	for _, row := range full {
		if row.Live != "in sync" {
			t.Fatalf("alias %s reported as %q", row.Alias, row.Live)
		}
	}
}

// stubChecker plays the proxy for `gwctl health`.
type stubChecker struct {
	liveness  error
	readiness map[string]any
	readyErr  error
	health    litellm.HealthReport
	healthErr error
}

func (s stubChecker) Liveness(context.Context) error { return s.liveness }
func (s stubChecker) Readiness(context.Context) (map[string]any, error) {
	return s.readiness, s.readyErr
}
func (s stubChecker) ProviderHealth(context.Context) (litellm.HealthReport, error) {
	return s.health, s.healthErr
}

func TestHealth(t *testing.T) {
	ctx := context.Background()

	rows, healthy := reconcile.Health(ctx, stubChecker{
		readiness: map[string]any{"status": "connected", "db": "connected", "cache": "connected"},
		health: litellm.HealthReport{
			Healthy: []map[string]any{{"model": "anthropic/claude-sonnet-5"}},
		},
	})
	if !healthy {
		t.Fatalf("everything is up, got %+v", rows)
	}
	if len(rows) != 5 {
		t.Fatalf("expected proxy, readiness, db, cache and one model: %+v", rows)
	}

	_, healthy = reconcile.Health(ctx, stubChecker{
		readiness: map[string]any{"db": "connected", "cache": "unreachable"},
		health:    litellm.HealthReport{Unhealthy: []map[string]any{{"model": "openai/gpt-5", "error": "401 unauthorized"}}},
	})
	if healthy {
		t.Fatal("a dead provider and a dead cache must not report healthy")
	}

	rows, healthy = reconcile.Health(ctx, stubChecker{liveness: errors.New("connection refused")})
	if healthy || len(rows) != 1 {
		t.Fatalf("a dead proxy short-circuits the report: %+v", rows)
	}

	rows, healthy = reconcile.Health(ctx, stubChecker{
		readyErr:  errors.New("db down"),
		healthErr: errors.New("timeout"),
	})
	if healthy {
		t.Fatal("failed probes must report unhealthy")
	}
	if len(rows) != 3 {
		t.Fatalf("expected proxy, readiness failure and provider failure: %+v", rows)
	}
}

func TestBuildProxyConfigWritesTheFile(t *testing.T) {
	cfg, root := setup(t)

	plan, err := reconcile.BuildProxyConfig(cfg, reconcile.Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		t.Fatal("the generated config does not exist yet, it must be planned")
	}
	// No proxy is needed to render the file: without it the stack cannot boot.
	if err := reconcile.Apply(context.Background(), nil, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(root, reconcile.GeneratedConfigPath))
	if err != nil {
		t.Fatalf("generated file missing: %v", err)
	}
	if !strings.Contains(string(written), "DO NOT EDIT BY HAND") {
		t.Error("the generated file must say it is generated")
	}

	again, err := reconcile.BuildProxyConfig(cfg, reconcile.Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("rendering an unchanged config must be a no-op:\n%s", again.String())
	}
}

func TestPlanStringAndEmptiness(t *testing.T) {
	empty := &reconcile.Plan{}
	if !empty.Empty() || empty.String() != "no changes" {
		t.Fatalf("unexpected empty plan rendering: %q", empty.String())
	}

	plan := &reconcile.Plan{Actions: []reconcile.Action{
		{Kind: reconcile.KindCreateModel, Name: "balanced", Changes: []string{"new deployment"}},
	}}
	rendered := plan.String()
	if !strings.Contains(rendered, "create") || !strings.Contains(rendered, "new deployment") {
		t.Fatalf("unexpected plan rendering: %q", rendered)
	}
}

func TestApplyRejectsUnknownAction(t *testing.T) {
	plan := &reconcile.Plan{Actions: []reconcile.Action{{Kind: "nonsense", Name: "x"}}}
	if err := reconcile.Apply(context.Background(), litellm.NewFake(), plan); err == nil {
		t.Fatal("expected an error for an unknown action kind")
	}
}

func TestBuildSurfacesProxyErrors(t *testing.T) {
	cfg, root := setup(t)
	fake := litellm.NewFake()
	fake.FailOn["create-deployment:gw-balanced-0"] = errors.New("proxy exploded")

	plan, err := reconcile.Build(context.Background(), fake, cfg, reconcile.Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	err = reconcile.Apply(context.Background(), fake, plan)
	if err == nil || !strings.Contains(err.Error(), "proxy exploded") {
		t.Fatalf("apply must fail loudly, got %v", err)
	}
}

func findRow(t *testing.T, rows []reconcile.SpendRow, group string) reconcile.SpendRow {
	t.Helper()

	for _, row := range rows {
		if row.Group == group {
			return row
		}
	}
	t.Fatalf("no row for %q in %+v", group, rows)
	return reconcile.SpendRow{}
}

func containsNotice(notices []string, substring string) bool {
	for _, notice := range notices {
		if strings.Contains(notice, substring) {
			return true
		}
	}
	return false
}
