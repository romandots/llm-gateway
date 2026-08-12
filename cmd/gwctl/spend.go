package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/romandots/llm-gateway/internal/reconcile"
	"github.com/romandots/llm-gateway/internal/report"
	"github.com/spf13/cobra"
)

func newSpendCommand() *cobra.Command {
	var by, since string

	cmd := &cobra.Command{
		Use:   "spend",
		Short: "Report spend, tokens and fallbacks over a period",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !reconcile.ValidGrouping(by) {
				return fmt.Errorf("unknown --by %q, expected %s|%s|%s", by, reconcile.ByConsumer, reconcile.ByAlias, reconcile.ByModel)
			}
			window, err := parseSince(since)
			if err != nil {
				return err
			}
			client, err := newClient()
			if err != nil {
				return err
			}

			end := time.Now().UTC()
			start := end.Add(-window)

			logs, err := client.SpendLogs(cmd.Context(), start, end)
			if err != nil {
				return err
			}
			keys, err := client.ListKeys(cmd.Context())
			if err != nil {
				return err
			}
			rows, err := reconcile.AggregateSpend(logs, by, keys)
			if err != nil {
				return err
			}

			table := report.Table{
				Headers: []string{by, "requests", "tokens in", "tokens out", "cost usd", "fallbacks"},
				Empty:   fmt.Sprintf("no accounted requests since %s", start.Format("2006-01-02")),
			}
			for _, row := range rows {
				table.Rows = append(table.Rows, spendRow(row))
			}
			if len(rows) > 1 {
				table.Rows = append(table.Rows, spendRow(reconcile.TotalSpend(rows)))
			}

			return report.Emit(cmd.OutOrStdout(), opts.output, table, map[string]any{
				"since": start.Format(time.RFC3339),
				"until": end.Format(time.RFC3339),
				"by":    by,
				"rows":  rows,
			})
		},
	}

	cmd.Flags().StringVar(&by, "by", reconcile.ByConsumer, "group by consumer|alias|model")
	cmd.Flags().StringVar(&since, "since", "7d", "how far back to look: 30m, 12h, 7d, 4w")
	return cmd
}

func spendRow(row reconcile.SpendRow) []string {
	return []string{
		row.Group,
		strconv.Itoa(row.Requests),
		report.Tokens(row.TokensIn),
		report.Tokens(row.TokensOut),
		report.Money(row.CostUSD),
		strconv.Itoa(row.Fallbacks),
	}
}

var sinceRe = regexp.MustCompile(`^([0-9]+)([mhdw])$`)

// parseSince accepts the short window syntax used across gwctl and make. Go's
// time.ParseDuration has no day or week unit, and "--since 7d" is what an
// operator types.
func parseSince(value string) (time.Duration, error) {
	match := sinceRe.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, fmt.Errorf("invalid --since %q, expected a number followed by m, h, d or w (e.g. 7d)", value)
	}
	amount, err := strconv.Atoi(match[1])
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("invalid --since %q, the amount must be a positive number", value)
	}

	unit := map[string]time.Duration{
		"m": time.Minute,
		"h": time.Hour,
		"d": 24 * time.Hour,
		"w": 7 * 24 * time.Hour,
	}[match[2]]
	return time.Duration(amount) * unit, nil
}
