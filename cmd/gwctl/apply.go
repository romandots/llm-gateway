package main

import (
	"context"
	"fmt"
	"io"

	"github.com/romandots/llm-gateway/internal/config"
	"github.com/romandots/llm-gateway/internal/reconcile"
	"github.com/romandots/llm-gateway/internal/report"
	"github.com/spf13/cobra"
)

func newValidateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration locally, without touching the network",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(opts.configDir)
			if err != nil {
				return err
			}

			issues := cfg.Validate()
			issues = append(issues, config.CheckRepo(opts.root)...)

			rows := make([][]string, 0, len(issues))
			for _, issue := range issues {
				rows = append(rows, []string{issue.Severity, issue.File, issue.Path, issue.Message})
			}
			table := report.Table{
				Headers: []string{"severity", "file", "path", "message"},
				Rows:    rows,
				Empty:   fmt.Sprintf("ok: %d aliases, %d consumers, no problems found", len(cfg.Models.Aliases), len(cfg.Consumers.Consumers)),
			}
			if err := report.Emit(cmd.OutOrStdout(), opts.output, table, issues); err != nil {
				return err
			}
			if issues.HasErrors() {
				return fmt.Errorf("configuration is invalid")
			}
			return nil
		},
	}
}

func newApplyCommand() *cobra.Command {
	var dryRun, pruneKeys, renderOnly bool

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Bring the proxy in line with the configuration",
		Long: `Reconciles model deployments, proxy settings and key attributes with
config/*.yaml. The operation is idempotent: running it twice in a row reports
"no changes" the second time.

apply never prints or stores key secrets. Consumers without a key are reported,
not silently provisioned — issuing a key is an explicit gwctl key issue.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := buildPlan(cmd.Context(), pruneKeys, renderOnly)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if err := printPlan(out, plan); err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintln(out, "\ndry run: nothing was changed")
				return nil
			}
			if plan.Empty() {
				return nil
			}
			if renderOnly {
				if err := reconcile.Apply(cmd.Context(), nil, plan); err != nil {
					return err
				}
				fmt.Fprintf(out, "\nwrote %s\n", reconcile.GeneratedConfigPath)
				return nil
			}
			if err := confirm(fmt.Sprintf("\nApply %d change(s) to %s?", executableCount(plan), opts.endpoint)); err != nil {
				return err
			}

			client, err := newClient()
			if err != nil {
				return err
			}
			if err := reconcile.Apply(cmd.Context(), client, plan); err != nil {
				return err
			}
			fmt.Fprintf(out, "\napplied %d change(s)\n", executableCount(plan))
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan without changing anything")
	cmd.Flags().BoolVar(&pruneKeys, "prune-keys", false, "also revoke keys of consumers removed from consumers.yaml")
	cmd.Flags().BoolVar(&renderOnly, "render-only", false,
		"only regenerate "+reconcile.GeneratedConfigPath+", without talking to the proxy (used to bootstrap the stack)")
	return cmd
}

func newDiffCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "diff",
		Short: "Show what differs between the configuration and the proxy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			plan, err := buildPlan(cmd.Context(), false, false)
			if err != nil {
				return err
			}
			return printPlan(cmd.OutOrStdout(), plan)
		},
	}
}

func buildPlan(ctx context.Context, pruneKeys, renderOnly bool) (*reconcile.Plan, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	options := reconcile.Options{RepoRoot: opts.root, PruneKeys: pruneKeys}

	if renderOnly {
		return reconcile.BuildProxyConfig(cfg, options)
	}
	client, err := newClient()
	if err != nil {
		return nil, err
	}
	return reconcile.Build(ctx, client, cfg, options)
}

func printPlan(out io.Writer, plan *reconcile.Plan) error {
	if opts.output == report.FormatJSON {
		return report.WriteJSON(out, plan)
	}
	// "no changes" refers to what apply would do. Informational entries, such as
	// a consumer still waiting for its key, are printed after it rather than
	// instead of it.
	if plan.Empty() {
		fmt.Fprintln(out, "no changes")
	}
	if len(plan.Actions) > 0 {
		fmt.Fprintln(out, plan.String())
	}
	for _, notice := range plan.Notices {
		fmt.Fprintf(out, "\nnote: %s\n", notice)
	}
	return nil
}

func executableCount(plan *reconcile.Plan) int {
	count := 0
	for _, action := range plan.Actions {
		if action.Executable() {
			count++
		}
	}
	return count
}
