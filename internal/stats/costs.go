package stats

import (
	"encoding/json"

	"github.com/kunchenguid/no-mistakes/internal/legacycost"
)

type CostTotals struct {
	HarnessReported         CostTotal `json:"harness_reported"`
	APIListEstimate         CostTotal `json:"api_list_estimate"`
	HarnessAdjustedEstimate CostTotal `json:"harness_adjusted_estimate"`
}

type CostTotal struct {
	ValueUSD   *float64                `json:"value_usd"`
	Coverage   legacycost.Coverage     `json:"coverage"`
	Complete   bool                    `json:"complete"`
	Basis      string                  `json:"basis"`
	Reasons    []string                `json:"reasons"`
	Provenance []legacycost.Provenance `json:"provenance"`
}

func buildCostTotals(invocations []Invocation) CostTotals {
	return CostTotals{
		HarnessReported: aggregateCost(invocations, func(costs legacycost.CostClasses) legacycost.CostEstimate {
			return costs.HarnessReported
		}),
		APIListEstimate: aggregateCost(invocations, func(costs legacycost.CostClasses) legacycost.CostEstimate {
			return costs.APIListEstimate
		}),
		HarnessAdjustedEstimate: aggregateCost(invocations, func(costs legacycost.CostClasses) legacycost.CostEstimate {
			return costs.HarnessAdjustedEstimate
		}),
	}
}

func aggregateCost(invocations []Invocation, selectCost func(legacycost.CostClasses) legacycost.CostEstimate) CostTotal {
	result := CostTotal{Complete: len(invocations) > 0, Reasons: []string{}, Provenance: []legacycost.Provenance{}}
	known := 0
	var sum float64
	seenReasons := make(map[string]bool)
	seenProvenance := make(map[string]bool)
	for _, invocation := range invocations {
		cost := selectCost(invocation.Costs)
		result.Coverage.Reported += cost.Coverage.Reported
		result.Coverage.Eligible += cost.Coverage.Eligible
		if result.Basis == "" {
			result.Basis = cost.Basis
		} else if cost.Basis != "" && cost.Basis != result.Basis {
			result.Basis = "mixed"
		}
		if cost.ValueUSD != nil {
			sum += *cost.ValueUSD
			known++
		}
		if !cost.Complete || cost.ValueUSD == nil {
			result.Complete = false
		}
		if cost.Reason != "" && !seenReasons[cost.Reason] {
			seenReasons[cost.Reason] = true
			result.Reasons = append(result.Reasons, cost.Reason)
		}
		encoded, _ := json.Marshal(cost.Provenance)
		key := string(encoded)
		if key != "{}" && !seenProvenance[key] {
			seenProvenance[key] = true
			result.Provenance = append(result.Provenance, cost.Provenance)
		}
	}
	if known > 0 {
		result.ValueUSD = &sum
	}
	return result
}
