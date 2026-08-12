package main

import (
	"fmt"
	"strings"

	"github.com/romandots/llm-gateway/internal/litellm"
	"github.com/romandots/llm-gateway/internal/reconcile"
	"github.com/romandots/llm-gateway/internal/report"
	"github.com/spf13/cobra"
)

func newModelsCommand() *cobra.Command {
	var local bool

	cmd := &cobra.Command{
		Use:   "models",
		Short: "List aliases and the vendor models serving them today",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			var live []litellm.Deployment
			if !local {
				client, err := newClient()
				if err != nil {
					return err
				}
				if live, err = client.ListDeployments(cmd.Context()); err != nil {
					return err
				}
				if live == nil {
					live = []litellm.Deployment{}
				}
			}

			rows := reconcile.AliasRows(cfg, live)
			table := report.Table{
				Headers: []string{"alias", "mode", "primary", "fallbacks", "capabilities", "proxy"},
				Empty:   "no aliases defined",
			}
			for _, row := range rows {
				table.Rows = append(table.Rows, []string{
					row.Alias,
					row.Mode,
					row.Primary,
					orDash(strings.Join(row.Fallbacks, ", ")),
					row.Capabilities,
					row.Live,
				})
			}
			return report.Emit(cmd.OutOrStdout(), opts.output, table, rows)
		},
	}

	cmd.Flags().BoolVar(&local, "local", false, "read the configuration only, without asking the proxy")
	return cmd
}

func newHealthCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check the proxy, its dependencies and every configured provider",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient()
			if err != nil {
				return err
			}

			rows, healthy := reconcile.Health(cmd.Context(), client)
			table := report.Table{Headers: []string{"component", "status", "detail"}}
			for _, row := range rows {
				table.Rows = append(table.Rows, []string{row.Component, row.Status, row.Detail})
			}
			if err := report.Emit(cmd.OutOrStdout(), opts.output, table, map[string]any{
				"healthy": healthy,
				"checks":  rows,
			}); err != nil {
				return err
			}
			if !healthy {
				return fmt.Errorf("some checks failed")
			}
			return nil
		},
	}
}
