// Package report formats command output as either a human-readable table or
// JSON for scripts.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Output formats supported by every command.
const (
	FormatTable = "table"
	FormatJSON  = "json"
)

// ValidFormat reports whether the value is a supported --output format.
func ValidFormat(format string) bool {
	return format == FormatTable || format == FormatJSON
}

// Table is a rendered table: a header row and its rows, already stringified.
type Table struct {
	Headers []string
	Rows    [][]string
	// Empty is printed instead of an empty table.
	Empty string
}

// Render writes the table with aligned columns.
func (t Table) Render(w io.Writer) error {
	if len(t.Rows) == 0 {
		message := t.Empty
		if message == "" {
			message = "(nothing to show)"
		}
		_, err := fmt.Fprintln(w, message)
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	if len(t.Headers) > 0 {
		if _, err := fmt.Fprintln(tw, strings.Join(upper(t.Headers), "\t")); err != nil {
			return err
		}
	}
	for _, row := range t.Rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func upper(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.ToUpper(value)
	}
	return out
}

// WriteJSON renders a value as indented JSON.
func WriteJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}

// Emit writes either the table or the structured value, depending on format.
func Emit(w io.Writer, format string, table Table, value any) error {
	switch format {
	case FormatJSON:
		return WriteJSON(w, value)
	case FormatTable, "":
		return table.Render(w)
	default:
		return fmt.Errorf("unknown output format %q, expected %s or %s", format, FormatTable, FormatJSON)
	}
}

// Money renders a USD amount the way every table in gwctl shows it.
func Money(amount float64) string {
	return fmt.Sprintf("%.2f", amount)
}

// Tokens renders a token count compactly: 8.2M reads better than 8203441 in a
// column the operator scans for outliers.
func Tokens(count int) string {
	switch {
	case count >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(count)/1_000_000)
	case count >= 1_000:
		return fmt.Sprintf("%.1fK", float64(count)/1_000)
	default:
		return fmt.Sprint(count)
	}
}
