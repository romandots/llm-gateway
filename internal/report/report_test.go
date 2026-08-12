package report_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/romandots/llm-gateway/internal/report"
)

func TestTableRendersAlignedColumns(t *testing.T) {
	table := report.Table{
		Headers: []string{"consumer", "cost usd"},
		Rows: [][]string{
			{"tansultant-reactivation", "18.42"},
			{"my-telegram-bot", "2.11"},
		},
	}

	var buf bytes.Buffer
	if err := table.Render(&buf); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected a header and two rows, got %q", buf.String())
	}
	if !strings.HasPrefix(lines[0], "CONSUMER") {
		t.Errorf("headers should be upper-cased: %q", lines[0])
	}
	// The cost column has to start at the same offset on every line, or the
	// operator cannot scan it.
	if strings.Index(lines[1], "18.42") != strings.Index(lines[2], "2.11") {
		t.Errorf("columns are not aligned:\n%s", buf.String())
	}
}

func TestTableWithoutRowsExplainsItself(t *testing.T) {
	var buf bytes.Buffer
	if err := (report.Table{Empty: "no keys issued yet"}).Render(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "no keys issued yet" {
		t.Fatalf("unexpected output %q", buf.String())
	}

	buf.Reset()
	if err := (report.Table{}).Render(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nothing to show") {
		t.Fatalf("unexpected default: %q", buf.String())
	}
}

func TestEmitSwitchesOnFormat(t *testing.T) {
	table := report.Table{Headers: []string{"a"}, Rows: [][]string{{"1"}}}
	value := map[string]int{"a": 1}

	var buf bytes.Buffer
	if err := report.Emit(&buf, report.FormatJSON, table, value); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"a": 1`) {
		t.Fatalf("expected JSON, got %q", buf.String())
	}

	buf.Reset()
	if err := report.Emit(&buf, report.FormatTable, table, value); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "A") {
		t.Fatalf("expected a table, got %q", buf.String())
	}

	buf.Reset()
	if err := report.Emit(&buf, "", table, value); err != nil {
		t.Fatalf("an empty format must default to a table: %v", err)
	}
	if err := report.Emit(&buf, "yaml", table, value); err == nil {
		t.Fatal("expected an error for an unsupported format")
	}
}

func TestValidFormat(t *testing.T) {
	if !report.ValidFormat(report.FormatTable) || !report.ValidFormat(report.FormatJSON) {
		t.Fatal("the two supported formats must validate")
	}
	if report.ValidFormat("csv") {
		t.Fatal("csv is not supported")
	}
}

func TestJSONDoesNotEscapeHTML(t *testing.T) {
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, map[string]string{"model": "a<b&c"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "a<b&c") {
		t.Fatalf("values must survive verbatim: %q", buf.String())
	}
}

func TestMoneyAndTokens(t *testing.T) {
	if got := report.Money(18.4237); got != "18.42" {
		t.Errorf("Money = %q", got)
	}
	cases := map[int]string{
		999:       "999",
		8_200_000: "8.2M",
		1_500:     "1.5K",
		0:         "0",
	}
	for count, want := range cases {
		if got := report.Tokens(count); got != want {
			t.Errorf("Tokens(%d) = %q, want %q", count, got, want)
		}
	}
}
