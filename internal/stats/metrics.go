package stats

import (
	"fmt"
	"math"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func buildMetrics(invocations []Invocation, expected db.AgentInvocationAuditTotals) (Metrics, []string) {
	observed := observedTotals(invocations)
	var integrityErrors []string
	rowCountMatches := expected.Rows == len(invocations)
	if !rowCountMatches {
		integrityErrors = append(integrityErrors, fmt.Sprintf("invocation row count mismatch: emitted %d, database aggregate %d", len(invocations), expected.Rows))
	}
	input, inputError := intMetric("delta_input_tokens", len(invocations), observed.inputReported, observed.inputSum, expected.DeltaInputReported, expected.DeltaInputSum, rowCountMatches)
	output, outputError := intMetric("delta_output_tokens", len(invocations), observed.outputReported, observed.outputSum, expected.DeltaOutputReported, expected.DeltaOutputSum, rowCountMatches)
	cacheRead, cacheReadError := intMetric("delta_cache_read_tokens", len(invocations), observed.cacheReadReported, observed.cacheReadSum, expected.DeltaCacheReadReported, expected.DeltaCacheReadSum, rowCountMatches)
	cacheWrite, cacheWriteError := intMetric("delta_cache_write_tokens", len(invocations), observed.cacheWriteReported, observed.cacheWriteSum, expected.DeltaCacheCreationReported, expected.DeltaCacheCreationSum, rowCountMatches)
	cost, costError := floatMetric("reported_cost_usd", len(invocations), observed.costReported, observed.costSum, expected.ReportedCostReported, expected.ReportedCostSum, rowCountMatches)
	for _, metricError := range []*string{inputError, outputError, cacheReadError, cacheWriteError, costError} {
		if metricError != nil {
			integrityErrors = append(integrityErrors, *metricError)
		}
	}
	return Metrics{
		InvocationCount: len(invocations), DeltaInputTokens: input, DeltaOutputTokens: output,
		DeltaCacheReadTokens: cacheRead, DeltaCacheWriteTokens: cacheWrite, ReportedCostUSD: cost,
	}, integrityErrors
}

type invocationTotals struct {
	inputReported, outputReported, cacheReadReported, cacheWriteReported, costReported int
	inputSum, outputSum, cacheReadSum, cacheWriteSum                                   int64
	costSum                                                                            float64
}

func observedTotals(invocations []Invocation) invocationTotals {
	var totals invocationTotals
	for _, invocation := range invocations {
		addIntMeter(invocation.DeltaUsage.InputTokens, &totals.inputReported, &totals.inputSum)
		addIntMeter(invocation.DeltaUsage.OutputTokens, &totals.outputReported, &totals.outputSum)
		addIntMeter(invocation.DeltaUsage.CacheReadTokens, &totals.cacheReadReported, &totals.cacheReadSum)
		addIntMeter(invocation.DeltaUsage.CacheWriteTokens, &totals.cacheWriteReported, &totals.cacheWriteSum)
		if invocation.ReportedCostUSD != nil {
			totals.costReported++
			totals.costSum += *invocation.ReportedCostUSD
		}
	}
	return totals
}

func addIntMeter(value *int, reported *int, sum *int64) {
	if value == nil {
		return
	}
	*reported++
	*sum += int64(*value)
}

func intMetric(name string, total, observedReported int, observedSum int64, expectedReported int, expectedSum *int64, rowCountMatches bool) (IntMetric, *string) {
	coverage := Coverage{Reported: observedReported, Total: total}
	var errors []string
	if !rowCountMatches {
		errors = append(errors, "invocation row count mismatch")
	}
	if observedReported != expectedReported || !sameIntSum(observedReported, observedSum, expectedSum) {
		errors = append(errors, fmt.Sprintf("%s aggregate mismatch", name))
	}
	if observedReported != total && total > 0 {
		errors = append(errors, fmt.Sprintf("%s has partial coverage %d/%d", name, observedReported, total))
	}
	errorMessage := joinedError(errors)
	metric := IntMetric{Coverage: coverage, IntegrityError: errorMessage}
	if total > 0 && observedReported == total && errorMessage == nil {
		value := observedSum
		metric.Value = &value
	}
	return metric, errorMessage
}

func floatMetric(name string, total, observedReported int, observedSum float64, expectedReported int, expectedSum *float64, rowCountMatches bool) (FloatMetric, *string) {
	coverage := Coverage{Reported: observedReported, Total: total}
	var errors []string
	if !rowCountMatches {
		errors = append(errors, "invocation row count mismatch")
	}
	if observedReported != expectedReported || !sameFloatSum(observedReported, observedSum, expectedSum) {
		errors = append(errors, fmt.Sprintf("%s aggregate mismatch", name))
	}
	if observedReported != total && total > 0 {
		errors = append(errors, fmt.Sprintf("%s has partial coverage %d/%d", name, observedReported, total))
	}
	errorMessage := joinedError(errors)
	metric := FloatMetric{Coverage: coverage, IntegrityError: errorMessage}
	if total > 0 && observedReported == total && errorMessage == nil {
		value := observedSum
		metric.Value = &value
	}
	return metric, errorMessage
}

func sameIntSum(reported int, observed int64, expected *int64) bool {
	if reported == 0 {
		return expected == nil
	}
	return expected != nil && observed == *expected
}

func sameFloatSum(reported int, observed float64, expected *float64) bool {
	if reported == 0 {
		return expected == nil
	}
	return expected != nil && math.Abs(observed-*expected) <= 1e-9
}

func joinedError(errors []string) *string {
	if len(errors) == 0 {
		return nil
	}
	value := strings.Join(errors, "; ")
	return &value
}
