package reconcile

import (
	"fmt"
	"sort"

	"github.com/romandots/llm-gateway/internal/litellm"
)

// Grouping dimensions supported by `gwctl spend --by`.
const (
	ByConsumer = "consumer"
	ByAlias    = "alias"
	ByModel    = "model"
)

// ValidGrouping reports whether the dimension is supported.
func ValidGrouping(by string) bool {
	return by == ByConsumer || by == ByAlias || by == ByModel
}

// SpendRow is one aggregated line of the spend report.
type SpendRow struct {
	Group     string  `json:"group"`
	Requests  int     `json:"requests"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	Fallbacks int     `json:"fallbacks"`
}

// AggregateSpend groups spend logs by the requested dimension. Keys are needed
// to translate the hashed api_key of a log line into a consumer name.
func AggregateSpend(logs []litellm.SpendLog, by string, keys []litellm.Key) ([]SpendRow, error) {
	if !ValidGrouping(by) {
		return nil, fmt.Errorf("unknown grouping %q, expected %s|%s|%s", by, ByConsumer, ByAlias, ByModel)
	}

	consumerOf := map[string]string{}
	for _, key := range keys {
		consumerOf[key.Token] = key.Consumer()
	}

	totals := map[string]*SpendRow{}
	for _, log := range logs {
		group := groupOf(log, by, consumerOf)
		row, ok := totals[group]
		if !ok {
			row = &SpendRow{Group: group}
			totals[group] = row
		}
		row.Requests++
		row.TokensIn += log.PromptTokens
		row.TokensOut += log.CompletionTokens
		row.CostUSD += log.Spend
		// A request served by a fallback deployment carries the fallback model
		// group; the alias the consumer asked for is its prefix.
		if IsFallbackGroup(log.ModelGroup) {
			row.Fallbacks++
		}
	}

	rows := make([]SpendRow, 0, len(totals))
	for _, row := range totals {
		rows = append(rows, *row)
	}
	// Most expensive first: the report exists to find where the money went.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		return rows[i].Group < rows[j].Group
	})
	return rows, nil
}

func groupOf(log litellm.SpendLog, by string, consumerOf map[string]string) string {
	switch by {
	case ByConsumer:
		if name, ok := consumerOf[log.APIKey]; ok && name != "" {
			return name
		}
		return "(unknown key)"
	case ByAlias:
		if log.ModelGroup == "" {
			return "(unknown alias)"
		}
		return AliasOfGroup(log.ModelGroup)
	default:
		if log.Model == "" {
			return "(unknown model)"
		}
		return log.Model
	}
}

// TotalSpend sums a report, for the trailing TOTAL line.
func TotalSpend(rows []SpendRow) SpendRow {
	total := SpendRow{Group: "TOTAL"}
	for _, row := range rows {
		total.Requests += row.Requests
		total.TokensIn += row.TokensIn
		total.TokensOut += row.TokensOut
		total.CostUSD += row.CostUSD
		total.Fallbacks += row.Fallbacks
	}
	return total
}
