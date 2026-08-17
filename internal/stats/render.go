package stats

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
)

var csvHeader = []string{
	"schema_version", "repo_id", "run_id", "record_type", "entity_id", "section", "group", "metric",
	"value", "unit", "reported", "eligible", "complete", "basis", "reason",
}

func RenderText(report *Report) string {
	if report == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "stats scope: %d run(s), %d step(s), %d repair(s), %d agent invocation(s)\n", report.Runs.Count, len(report.Steps), len(report.Repairs), len(report.Agents))
	for _, run := range report.Runs.Items {
		fmt.Fprintf(&out, "run %s repo=%s status=%s created=%d\n", run.ID, run.RepoID, run.Status, run.CreatedAt)
	}
	for _, repair := range report.Repairs {
		fmt.Fprintf(&out, "repair %s/%s/%d trigger=%s selection=%s duration_ms=%d\n", repair.RunID, repair.Step, repair.Round, repair.Trigger, optionalString(repair.SelectionSource), repair.DurationMS)
	}
	for _, step := range report.Steps {
		fmt.Fprintf(&out, "step %s/%s status=%s rounds=%d duration_ms=%s\n", step.RunID, step.Step.Name, step.Step.Status, len(step.Step.Rounds), optionalInt64(step.Step.DurationMS))
	}
	for _, invocation := range report.Agents {
		fmt.Fprintf(&out, "agent %s/%s step=%s purpose=%s harness=%s model=%s provider=%s status=%s\n",
			invocation.RunID, invocation.Invocation.ID, invocation.Invocation.Step, invocation.Invocation.Purpose, invocation.Invocation.Agent,
			optionalString(invocation.Invocation.Model), optionalString(invocation.Invocation.Provider), invocation.Invocation.ExitStatus)
	}
	for _, cost := range orderedCostTotals(report.Costs.Totals) {
		fmt.Fprintf(&out, "cost %s value_usd=%s coverage=%d/%d complete=%t reason=%s\n", cost.name, optionalFloat(cost.total.ValueUSD), cost.total.Coverage.Reported, cost.total.Coverage.Eligible, cost.total.Complete, strings.Join(cost.total.Reasons, ","))
	}
	for _, dataError := range report.DataErrors {
		fmt.Fprintf(&out, "data_error run=%s code=%s detail=%s\n", dataError.RunID, dataError.Code, dataError.Detail)
	}
	return out.String()
}

func RenderCSV(report *Report) (string, error) {
	if report == nil {
		return "", fmt.Errorf("render stats CSV: report is nil")
	}
	var buffer bytes.Buffer
	w := csv.NewWriter(&buffer)
	if err := w.Write(csvHeader); err != nil {
		return "", fmt.Errorf("write stats CSV header: %w", err)
	}
	repoByRun := make(map[string]string, len(report.Runs.Items))
	for _, run := range report.Runs.Items {
		repoByRun[run.ID] = run.RepoID
		writeCSVFact(w, report.SchemaVersion, run.RepoID, run.ID, "run", run.ID, "runs", "identity", "status", string(run.Status), "", "", "", "", "runs.status", "")
		writeCSVFact(w, report.SchemaVersion, run.RepoID, run.ID, "run", run.ID, "runs", "identity", "created_at", strconv.FormatInt(run.CreatedAt, 10), "unix_seconds", "1", "1", "true", "runs.created_at", "")
	}
	for _, repair := range report.Repairs {
		entityID := fmt.Sprintf("%s:%d", repair.StepID, repair.Round)
		writeCSVFact(w, report.SchemaVersion, repoByRun[repair.RunID], repair.RunID, "repair", entityID, "repairs", string(repair.Step), "trigger", repair.Trigger, "", "", "", "", "step_rounds.trigger_type", "")
		writeCSVFact(w, report.SchemaVersion, repoByRun[repair.RunID], repair.RunID, "repair", entityID, "repairs", string(repair.Step), "duration", strconv.FormatInt(repair.DurationMS, 10), "milliseconds", "1", "1", "true", "step_rounds.duration_ms", "")
	}
	for _, step := range report.Steps {
		writeCSVFact(w, report.SchemaVersion, repoByRun[step.RunID], step.RunID, "step", step.Step.ID, "steps", string(step.Step.Name), "status", string(step.Step.Status), "", "", "", "", "step_results.status", "")
		writeNullableInt64Fact(w, report.SchemaVersion, repoByRun[step.RunID], step.RunID, "step", step.Step.ID, "steps", string(step.Step.Name), "duration", step.Step.DurationMS, "milliseconds", "step_results.duration_ms")
	}
	for _, agent := range report.Agents {
		invocation := agent.Invocation
		writeCSVFact(w, report.SchemaVersion, repoByRun[agent.RunID], agent.RunID, "agent", invocation.ID, "agents", invocation.Agent, "purpose", invocation.Purpose, "", "", "", "", "agent_invocations.purpose", "")
		writeCSVFact(w, report.SchemaVersion, repoByRun[agent.RunID], agent.RunID, "agent", invocation.ID, "agents", invocation.Agent, "model", optionalString(invocation.Model), "", boolCount(invocation.Model != nil), "1", strconv.FormatBool(invocation.Model != nil), "agent_invocations.model", missingReason(invocation.Model != nil))
		writeNullableIntFact(w, report.SchemaVersion, repoByRun[agent.RunID], agent.RunID, "agent", invocation.ID, "agents", invocation.Agent, "delta_input_tokens", invocation.DeltaUsage.InputTokens, "tokens", "agent_invocations.delta_input_tokens")
		writeNullableIntFact(w, report.SchemaVersion, repoByRun[agent.RunID], agent.RunID, "agent", invocation.ID, "agents", invocation.Agent, "delta_output_tokens", invocation.DeltaUsage.OutputTokens, "tokens", "agent_invocations.delta_output_tokens")
	}
	for _, cost := range orderedCostTotals(report.Costs.Totals) {
		writeCSVFact(w, report.SchemaVersion, "", "", "cost", "total", "costs", cost.name, "usd", optionalFloatCSV(cost.total.ValueUSD), "usd",
			strconv.Itoa(cost.total.Coverage.Reported), strconv.Itoa(cost.total.Coverage.Eligible), strconv.FormatBool(cost.total.Complete), cost.total.Basis, costReason(cost.total))
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("write stats CSV: %w", err)
	}
	return buffer.String(), nil
}

type namedCostTotal struct {
	name  string
	total CostTotal
}

func orderedCostTotals(costs CostTotals) []namedCostTotal {
	return []namedCostTotal{
		{name: "harness_reported", total: costs.HarnessReported},
		{name: "api_list_estimate", total: costs.APIListEstimate},
		{name: "harness_adjusted_estimate", total: costs.HarnessAdjustedEstimate},
	}
}

func writeCSVFact(w *csv.Writer, schemaVersion int, repoID, runID, recordType, entityID, section, group, metric, value, unit, reported, eligible, complete, basis, reason string) {
	_ = w.Write([]string{strconv.Itoa(schemaVersion), repoID, runID, recordType, entityID, section, group, metric, value, unit, reported, eligible, complete, basis, reason})
}

func writeNullableIntFact(w *csv.Writer, schemaVersion int, repoID, runID, recordType, entityID, section, group, metric string, value *int, unit, basis string) {
	if value == nil {
		writeCSVFact(w, schemaVersion, repoID, runID, recordType, entityID, section, group, metric, "", unit, "0", "1", "false", basis, "not_reported")
		return
	}
	writeCSVFact(w, schemaVersion, repoID, runID, recordType, entityID, section, group, metric, strconv.Itoa(*value), unit, "1", "1", "true", basis, "")
}

func writeNullableInt64Fact(w *csv.Writer, schemaVersion int, repoID, runID, recordType, entityID, section, group, metric string, value *int64, unit, basis string) {
	if value == nil {
		writeCSVFact(w, schemaVersion, repoID, runID, recordType, entityID, section, group, metric, "", unit, "0", "1", "false", basis, "not_reported")
		return
	}
	writeCSVFact(w, schemaVersion, repoID, runID, recordType, entityID, section, group, metric, strconv.FormatInt(*value, 10), unit, "1", "1", "true", basis, "")
}

func optionalString(value *string) string {
	if value == nil || *value == "" {
		return "—"
	}
	return *value
}

func optionalInt64(value *int64) string {
	if value == nil {
		return "—"
	}
	return strconv.FormatInt(*value, 10)
}

func optionalFloat(value *float64) string {
	if value == nil {
		return "—"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func optionalFloatCSV(value *float64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func boolCount(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func missingReason(present bool) string {
	if present {
		return ""
	}
	return "not_reported"
}

func costReason(total CostTotal) string {
	if len(total.Reasons) > 0 {
		return strings.Join(total.Reasons, ";")
	}
	if total.ValueUSD == nil {
		return "not_reported"
	}
	return ""
}
