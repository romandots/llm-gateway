package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/romandots/llm-gateway/internal/reconcile"
	"github.com/romandots/llm-gateway/internal/report"
	"github.com/spf13/cobra"
)

func newKeyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Issue, list, revoke and rotate consumer keys",
	}
	cmd.AddCommand(newKeyIssueCommand(), newKeyListCommand(), newKeyRevokeCommand(), newKeyRotateCommand())
	return cmd
}

func newKeyIssueCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "issue <consumer>",
		Short: "Issue a key for a consumer and print the secret once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			client, err := newClient()
			if err != nil {
				return err
			}

			secret, err := reconcile.IssueKey(cmd.Context(), client, cfg, args[0])
			if err != nil {
				return err
			}
			printSecret(cmd, args[0], secret, cfg.Consumers.Consumers[args[0]].Aliases)
			return nil
		},
	}
}

func newKeyListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the keys the gateway manages",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClient()
			if err != nil {
				return err
			}
			rows, err := reconcile.ListKeys(cmd.Context(), client)
			if err != nil {
				return err
			}

			table := report.Table{
				Headers: []string{"consumer", "key", "aliases", "budget", "spend", "limits", "status", "issued", "expires"},
				Empty:   "no keys issued yet",
			}
			for _, row := range rows {
				table.Rows = append(table.Rows, []string{
					row.Consumer,
					row.KeyName,
					strings.Join(row.Aliases, ","),
					row.Budget,
					report.Money(row.Spend),
					row.Limits,
					row.Status,
					row.IssuedAt,
					orDash(row.ExpiresAt),
				})
			}
			return report.Emit(cmd.OutOrStdout(), opts.output, table, rows)
		},
	}
}

func newKeyRevokeCommand() *cobra.Command {
	var grace time.Duration

	cmd := &cobra.Command{
		Use:   "revoke <consumer>",
		Short: "Revoke a consumer's key, immediately or after a grace period",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient()
			if err != nil {
				return err
			}

			question := fmt.Sprintf("Revoke the key of %q immediately? Requests will start failing with 401.", args[0])
			if grace > 0 {
				question = fmt.Sprintf("Revoke the key of %q in %s?", args[0], grace)
			}
			if err := confirm(question); err != nil {
				return err
			}
			if err := reconcile.RevokeKey(cmd.Context(), client, args[0], grace); err != nil {
				return err
			}

			if grace > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "key of %s marked deprecated, stops working in %s\n", args[0], grace)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "key of %s revoked\n", args[0])
			}
			return nil
		},
	}

	cmd.Flags().DurationVar(&grace, "grace", 0, "keep the old key working for this long (e.g. 24h)")
	return cmd
}

func newKeyRotateCommand() *cobra.Command {
	var grace time.Duration

	cmd := &cobra.Command{
		Use:   "rotate <consumer>",
		Short: "Issue a new key and retire the old one after a grace period",
		Long: `Composition of revoke and issue on top of the proxy's own key expiry: the
old key keeps working for --grace, which is what stops a rotation from taking
production down on a Sunday.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			client, err := newClient()
			if err != nil {
				return err
			}

			question := fmt.Sprintf("Rotate the key of %q? The old key stops working in %s.", args[0], grace)
			if grace <= 0 {
				question = fmt.Sprintf("Rotate the key of %q? The old key stops working immediately.", args[0])
			}
			if err := confirm(question); err != nil {
				return err
			}

			secret, err := reconcile.RotateKey(cmd.Context(), client, cfg, args[0], grace)
			if err != nil {
				return err
			}
			printSecret(cmd, args[0], secret, cfg.Consumers.Consumers[args[0]].Aliases)
			if grace > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "the previous key keeps working for %s\n", grace)
			}
			return nil
		},
	}

	cmd.Flags().DurationVar(&grace, "grace", 24*time.Hour, "keep the previous key working for this long")
	return cmd
}

// printSecret shows the key exactly once. It is not stored anywhere: the proxy
// keeps only a hash, and gwctl keeps nothing at all.
func printSecret(cmd *cobra.Command, consumer, secret string, aliases []string) {
	out := cmd.OutOrStdout()
	if opts.output == report.FormatJSON {
		_ = report.WriteJSON(out, map[string]any{
			"consumer": consumer,
			"key":      secret,
			"aliases":  aliases,
		})
		return
	}
	fmt.Fprintf(out, "consumer: %s\naliases:  %s\nkey:      %s\n\n", consumer, strings.Join(aliases, ", "), secret)
	fmt.Fprintln(out, "This is the only time the key is shown. Store it in the consumer's secret store now.")
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
