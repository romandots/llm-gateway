package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/romandots/llm-gateway/internal/config"
	"github.com/romandots/llm-gateway/internal/litellm"
)

// Kind of a planned change.
type Kind string

// The change kinds a plan can contain.
const (
	KindWriteConfig Kind = "config.write"
	KindCreateModel Kind = "model.create"
	KindUpdateModel Kind = "model.update"
	KindDeleteModel Kind = "model.delete"
	KindUpdateKey   Kind = "key.update"
	KindRevokeKey   Kind = "key.revoke"
	KindMissingKey  Kind = "key.missing"
)

// Action is one step of a plan.
type Action struct {
	Kind    Kind     `json:"kind"`
	Name    string   `json:"name"`
	Changes []string `json:"changes,omitempty"`

	deployment *litellm.Deployment
	keyUpdate  *litellm.UpdateKeyRequest
	ref        string
	contents   []byte
	path       string
}

// Executable reports whether `gwctl apply` can carry the action out. Missing
// keys are reported but never created by apply: the secret can only be shown
// once, so issuing it is an explicit `gwctl key issue`.
func (a Action) Executable() bool {
	return a.Kind != KindMissingKey
}

// String renders the action for the plan output.
func (a Action) String() string {
	verb := map[Kind]string{
		KindWriteConfig: "write",
		KindCreateModel: "create",
		KindUpdateModel: "update",
		KindDeleteModel: "delete",
		KindUpdateKey:   "update",
		KindRevokeKey:   "revoke",

		KindMissingKey: "missing",
	}[a.Kind]

	line := fmt.Sprintf("%-8s %-14s %s", verb, string(a.Kind), a.Name)
	if len(a.Changes) > 0 {
		line += "\n" + indent(strings.Join(a.Changes, "\n"), "             ")
	}
	return line
}

func indent(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// Plan is the ordered set of changes bringing the proxy in line with config.
type Plan struct {
	Actions []Action `json:"actions"`
	Notices []string `json:"notices,omitempty"`
}

// Empty reports whether there is nothing to do.
func (p *Plan) Empty() bool {
	for _, action := range p.Actions {
		if action.Executable() {
			return false
		}
	}
	return true
}

// String renders the plan the way `gwctl apply --dry-run` prints it.
func (p *Plan) String() string {
	if len(p.Actions) == 0 {
		return "no changes"
	}
	var sb strings.Builder
	for _, action := range p.Actions {
		sb.WriteString(action.String())
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Options control how a plan is built.
type Options struct {
	// RepoRoot is where the generated proxy configuration file lives.
	RepoRoot string
	// PruneKeys allows revoking keys of consumers that are no longer in the
	// configuration. Off by default: losing a key is not recoverable.
	PruneKeys bool
}

// Build compares the desired configuration with the live proxy and returns the
// plan that closes the gap.
func Build(ctx context.Context, api litellm.API, cfg *config.Config, opts Options) (*Plan, error) {
	plan := &Plan{}

	if err := planProxyConfig(plan, cfg, opts); err != nil {
		return nil, err
	}
	if err := planDeployments(ctx, plan, api, cfg); err != nil {
		return nil, err
	}
	if err := planKeys(ctx, plan, api, cfg, opts); err != nil {
		return nil, err
	}
	return plan, nil
}

// BuildProxyConfig plans only the generated proxy configuration file. It needs
// no proxy, which is what makes bootstrapping possible: the stack cannot start
// before the file exists, and the file cannot be rendered by a running proxy.
func BuildProxyConfig(cfg *config.Config, opts Options) (*Plan, error) {
	plan := &Plan{}
	if err := planProxyConfig(plan, cfg, opts); err != nil {
		return nil, err
	}
	return plan, nil
}

func planProxyConfig(plan *Plan, cfg *config.Config, opts Options) error {
	if opts.RepoRoot == "" {
		return nil
	}
	path := filepath.Join(opts.RepoRoot, GeneratedConfigPath)

	rendered, err := RenderProxyConfig(cfg)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", GeneratedConfigPath, err)
	}
	if string(current) == string(rendered) {
		return nil
	}

	change := "file differs from config/proxy.yaml"
	if len(current) == 0 {
		change = "file is missing"
	}
	plan.Actions = append(plan.Actions, Action{
		Kind:     KindWriteConfig,
		Name:     GeneratedConfigPath,
		Changes:  []string{change},
		contents: rendered,
		path:     path,
	})
	plan.Notices = append(plan.Notices,
		"proxy settings changed: restart the proxy (make restart) for them to take effect")
	return nil
}

func planDeployments(ctx context.Context, plan *Plan, api litellm.API, cfg *config.Config) error {
	actual, err := api.ListDeployments(ctx)
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}

	byID := map[string]litellm.Deployment{}
	for _, deployment := range actual {
		if id := deployment.ID(); id != "" {
			byID[id] = deployment
		}
	}

	desired := DesiredDeployments(cfg)
	keep := map[string]bool{}

	for _, want := range desired {
		id := want.ID()
		keep[id] = true

		current, exists := byID[id]
		if !exists {
			deployment := want
			plan.Actions = append(plan.Actions, Action{
				Kind:       KindCreateModel,
				Name:       fmt.Sprintf("%s (%s)", want.ModelName, want.LiteLLMParams["model"]),
				deployment: &deployment,
			})
			continue
		}
		if changes := deploymentDiff(current, want); len(changes) > 0 {
			deployment := want
			plan.Actions = append(plan.Actions, Action{
				Kind:       KindUpdateModel,
				Name:       want.ModelName,
				Changes:    changes,
				deployment: &deployment,
			})
		}
	}

	var stale []string
	for id, deployment := range byID {
		if keep[id] || !isManaged(deployment) {
			continue
		}
		stale = append(stale, id)
	}
	sort.Strings(stale)
	for _, id := range stale {
		plan.Actions = append(plan.Actions, Action{
			Kind:    KindDeleteModel,
			Name:    fmt.Sprintf("%s (%s)", byID[id].ModelName, id),
			Changes: []string{"no longer present in models.yaml"},
			ref:     id,
		})
	}

	// Deployments created by hand in the LiteLLM UI are left alone, but the
	// operator should know they exist: they can serve traffic outside the
	// contract.
	for _, deployment := range actual {
		if !isManaged(deployment) {
			plan.Notices = append(plan.Notices, fmt.Sprintf(
				"unmanaged deployment %q exists in the proxy but not in models.yaml; gwctl will not touch it",
				deployment.ModelName))
		}
	}
	return nil
}

func isManaged(d litellm.Deployment) bool {
	gw, ok := d.ModelInfo["gw"].(map[string]any)
	if !ok {
		return false
	}
	owner, _ := gw["managed_by"].(string)
	return owner == litellm.ManagedByValue
}

// deploymentDiff compares the fields the control plane owns. The proxy adds
// and masks fields of its own (ids, timestamps, the resolved api_key), and
// those must not show up as permanent drift.
func deploymentDiff(current, want litellm.Deployment) []string {
	var changes []string

	if current.ModelName != want.ModelName {
		changes = append(changes, fmt.Sprintf("model_name: %s -> %s", current.ModelName, want.ModelName))
	}
	for _, key := range sortedParamKeys(want.LiteLLMParams) {
		if key == "api_key" {
			// Returned masked by the proxy; comparing it would never converge.
			continue
		}
		wantValue := normalize(want.LiteLLMParams[key])
		currentValue := normalize(current.LiteLLMParams[key])
		if !reflect.DeepEqual(currentValue, wantValue) {
			changes = append(changes, fmt.Sprintf("litellm_params.%s: %s -> %s", key, render(currentValue), render(wantValue)))
		}
	}
	if !reflect.DeepEqual(normalize(current.ModelInfo["gw"]), normalize(want.ModelInfo["gw"])) {
		changes = append(changes, "model_info.gw: capability declaration changed")
	}
	if !reflect.DeepEqual(normalize(current.ModelInfo["mode"]), normalize(want.ModelInfo["mode"])) {
		changes = append(changes, fmt.Sprintf("model_info.mode: %s -> %s",
			render(normalize(current.ModelInfo["mode"])), render(normalize(want.ModelInfo["mode"]))))
	}
	return changes
}

func sortedParamKeys(params map[string]any) []string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// normalize puts a value through JSON so that values coming from the proxy and
// values built in Go compare equal (int vs float64, typed maps vs map[string]any).
func normalize(value any) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return value
	}
	return out
}

func render(value any) string {
	if value == nil {
		return "(unset)"
	}
	if text, ok := value.(string); ok {
		return text
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

func planKeys(ctx context.Context, plan *Plan, api litellm.API, cfg *config.Config, opts Options) error {
	keys, err := api.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("list keys: %w", err)
	}

	active := map[string]litellm.Key{}
	for _, key := range keys {
		if !key.ManagedByGwctl() || key.Deprecated() {
			continue
		}
		active[key.Consumer()] = key
	}

	for _, name := range cfg.ConsumerNames() {
		consumer := cfg.Consumers.Consumers[name]
		want := DesiredKeyFor(name, consumer)

		current, exists := active[name]
		if !exists {
			plan.Actions = append(plan.Actions, Action{
				Kind:    KindMissingKey,
				Name:    name,
				Changes: []string{"no key issued yet; run: gwctl key issue " + name},
			})
			continue
		}
		if changes := keyDiff(current, want); len(changes) > 0 {
			update := want.UpdateRequest(current.Token)
			plan.Actions = append(plan.Actions, Action{
				Kind:      KindUpdateKey,
				Name:      name,
				Changes:   changes,
				keyUpdate: &update,
			})
		}
	}

	var orphans []string
	for name := range active {
		if _, ok := cfg.Consumers.Consumers[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	for _, name := range orphans {
		if !opts.PruneKeys {
			plan.Notices = append(plan.Notices, fmt.Sprintf(
				"key of consumer %q has no entry in consumers.yaml; revoke it with: gwctl key revoke %s", name, name))
			continue
		}
		plan.Actions = append(plan.Actions, Action{
			Kind:    KindRevokeKey,
			Name:    name,
			Changes: []string{"consumer removed from consumers.yaml"},
			ref:     active[name].Token,
		})
	}
	return nil
}

func keyDiff(current litellm.Key, want desiredKey) []string {
	var changes []string

	currentAliases := append([]string(nil), current.Models...)
	sort.Strings(currentAliases)
	if !reflect.DeepEqual(currentAliases, want.Aliases) {
		changes = append(changes, fmt.Sprintf("aliases: [%s] -> [%s]",
			strings.Join(currentAliases, " "), strings.Join(want.Aliases, " ")))
	}
	if current.MaxBudget == nil || *current.MaxBudget != want.MaxBudget {
		changes = append(changes, fmt.Sprintf("budget: %s -> %.2f USD", floatOrUnset(current.MaxBudget), want.MaxBudget))
	}
	if current.BudgetDuration != want.BudgetDuration {
		changes = append(changes, fmt.Sprintf("budget period: %s -> %s", orUnset(current.BudgetDuration), want.BudgetDuration))
	}
	if current.RPMLimit == nil || *current.RPMLimit != want.RPM {
		changes = append(changes, fmt.Sprintf("rpm: %s -> %d", intOrUnset(current.RPMLimit), want.RPM))
	}
	if current.TPMLimit == nil || *current.TPMLimit != want.TPM {
		changes = append(changes, fmt.Sprintf("tpm: %s -> %d", intOrUnset(current.TPMLimit), want.TPM))
	}
	if owner, _ := current.Metadata[litellm.MetadataOwner].(string); owner != want.Metadata[litellm.MetadataOwner] {
		changes = append(changes, fmt.Sprintf("owner: %s -> %v", orUnset(owner), want.Metadata[litellm.MetadataOwner]))
	}
	return changes
}

func orUnset(value string) string {
	if value == "" {
		return "(unset)"
	}
	return value
}

func floatOrUnset(value *float64) string {
	if value == nil {
		return "(unset)"
	}
	return fmt.Sprintf("%.2f", *value)
}

func intOrUnset(value *int) string {
	if value == nil {
		return "(unset)"
	}
	return fmt.Sprint(*value)
}

// Apply executes every executable action of the plan in order.
func Apply(ctx context.Context, api litellm.API, plan *Plan) error {
	for _, action := range plan.Actions {
		if err := applyAction(ctx, api, action); err != nil {
			return fmt.Errorf("%s %s: %w", action.Kind, action.Name, err)
		}
	}
	return nil
}

func applyAction(ctx context.Context, api litellm.API, action Action) error {
	switch action.Kind {
	case KindWriteConfig:
		if err := os.MkdirAll(filepath.Dir(action.path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(action.path, action.contents, 0o644)
	case KindCreateModel:
		return api.CreateDeployment(ctx, *action.deployment)
	case KindUpdateModel:
		return api.UpdateDeployment(ctx, *action.deployment)
	case KindDeleteModel:
		return api.DeleteDeployment(ctx, action.ref)
	case KindUpdateKey:
		return api.UpdateKey(ctx, *action.keyUpdate)
	case KindRevokeKey:
		return api.DeleteKeys(ctx, []string{action.ref})
	case KindMissingKey:
		return nil
	default:
		return fmt.Errorf("unknown action kind %q", action.Kind)
	}
}
