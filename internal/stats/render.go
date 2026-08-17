package stats

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pricing"
)

var csvHeader = []string{
	"schema_version", "repo_id", "run_id", "record_type", "entity_id", "section", "group", "metric",
	"value", "unit", "reported", "eligible", "complete", "basis", "reason", "json_path",
}

// reportFact is the shared projection seam for human-readable text and
// long-form CSV. JSON remains normative; every JSON leaf (including null and
// empty collection facts) produces exactly one reportFact with its JSON path.
type reportFact struct {
	SchemaVersion int
	RepoID        string
	RunID         string
	RecordType    string
	EntityID      string
	Section       string
	Group         string
	Metric        string
	Value         string
	Present       bool
	Unit          string
	Reported      int
	Eligible      int
	Complete      bool
	Basis         string
	Reason        string
	JSONPath      string
}

func RenderText(report *Report) string {
	return renderText(report, false)
}

// RenderDetailedText keeps the legacy --agents intent while using the same
// Report envelope as every other format.
func RenderDetailedText(report *Report) string {
	return renderText(report, true)
}

func renderText(report *Report, forceDetails bool) string {
	if report == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "stats report: %d run(s), %d step(s), %d repair(s), %d agent invocation(s)\n", report.Runs.Count, len(report.Steps), len(report.Repairs), len(report.Agents))
	detailed := forceDetails || report.Scope.RunID != "" || len(report.Scope.RepoIDs) > 0 || report.Scope.Since != nil || report.Scope.Until != nil ||
		len(report.Scope.Steps) > 0 || len(report.Scope.Agents) > 0 || len(report.Scope.Models) > 0 || len(report.Scope.Purposes) > 0 || len(report.Scope.Statuses) > 0
	if !detailed {
		statuses := make([]string, 0, len(report.Runs.ByStatus))
		for status, count := range report.Runs.ByStatus {
			statuses = append(statuses, fmt.Sprintf("%s=%d", status, count))
		}
		sort.Strings(statuses)
		fmt.Fprintf(&out, "runs by status: %s\n", textValue(strings.Join(statuses, ", ")))
		for _, total := range namedCostTotals(report.Costs.Totals) {
			fmt.Fprintf(&out, "cost total/%s value_usd=%s coverage=%d/%d complete=%t reason=%s\n",
				total.name, textFloat(total.total.ValueUSD), total.total.Coverage.Reported, total.total.Coverage.Eligible, total.total.Complete, textValue(strings.Join(total.total.Reasons, ";")))
		}
		renderMetricsText(&out, "metric total", report.Metrics.Totals)
		if len(report.DataErrors) > 0 {
			fmt.Fprintf(&out, "data errors: %d (use --format json or csv for complete details)\n", len(report.DataErrors))
		}
		return out.String()
	}
	for _, run := range report.Runs.Items {
		fmt.Fprintf(&out, "run %s repo=%s status=%s created_at=%d parked_ms=%s rich_data_retained=%t\n", run.ID, run.RepoID, run.Status, run.CreatedAt, textInt64(run.ParkedMS), run.RichDataRetained)
	}
	for _, skip := range report.Skips {
		fmt.Fprintf(&out, "skip %s/%s source=%s\n", skip.RunID, skip.Receipt.Step, skip.Receipt.Source)
	}
	for _, repair := range report.Repairs {
		fmt.Fprintf(&out, "repair %s/%s/%d trigger=%s selection=%s failure_fingerprint=%s result=%s duration_ms=%d\n",
			repair.RunID, repair.Step, repair.Round, repair.Trigger, textString(repair.SelectionSource), textString(repair.RepairFailureFingerprint), textString(repair.RepairResult), repair.DurationMS)
	}
	for _, step := range report.Steps {
		fmt.Fprintf(&out, "step %s/%s status=%s rounds=%d duration_ms=%s\n", step.RunID, step.Step.Name, step.Step.Status, len(step.Step.Rounds), textInt64(step.Step.DurationMS))
		for _, command := range step.Step.Commands {
			runnerName := "—"
			if command.Runner != nil && command.Runner.Executable != "" {
				runnerName = command.Runner.Executable
			}
			fmt.Fprintf(&out, "command %s/%s/%d/%d outcome=%s exit_code=%s source=%s runner=%s\n",
				step.RunID, step.Step.Name, command.Round, command.Sequence, command.Outcome, textInt(command.ExitCode), textValue(command.CommandSource), runnerName)
		}
	}
	for _, agent := range report.Agents {
		invocation := agent.Invocation
		fmt.Fprintf(&out, "agent %s/%s step=%s round=%d purpose=%s harness=%s invoked_via=%s model=%s provider=%s status=%s session=%s duration_ms=%d delta_input_tokens=%s raw_input_tokens=%s cache_write_tokens=%s subprocess_wait_ms=%s roundtrips=%s tool_calls=%s test_lint_calls=%s workload=%s/%s nested_agents=%s\n",
			agent.RunID, invocation.ID, invocation.Step, invocation.Round, invocation.Purpose, invocation.Agent, invocation.InvocationMode, textString(invocation.Model), textString(invocation.Provider), invocation.ExitStatus, invocation.SessionMode, invocation.DurationMS,
			textInt(invocation.DeltaUsage.InputTokens), textInt(invocation.RawUsage.InputTokens), textInt(invocation.RawUsage.CacheWriteTokens), textInt64(invocation.Activity.SubprocessWaitMS), textInt(invocation.Activity.ModelRoundtrips), textInt(invocation.Activity.ToolCalls), textInt(invocation.Activity.ToolTestLintCalls), textInt(invocation.Activity.WorkloadFiles), textInt(invocation.Activity.WorkloadLines), textNestedAgents(invocation))
	}
	for _, record := range report.Metrics.Items {
		renderMetricsText(&out, "metric "+record.RunID, record.Metrics)
	}
	for _, item := range report.Costs.Items {
		for _, class := range namedCostEstimates(item.Classes) {
			fmt.Fprintf(&out, "cost %s/%s/%s value_usd=%s coverage=%d/%d complete=%t reason=%s catalog=%s profile=%s\n",
				item.RunID, item.InvocationID, class.name, textFloat(class.estimate.ValueUSD), class.estimate.Coverage.Reported, class.estimate.Coverage.Eligible,
				class.estimate.Complete, textValue(class.estimate.Reason), provenanceCatalog(class.estimate.Provenance), provenanceProfile(class.estimate.Provenance))
		}
	}
	for _, total := range namedCostTotals(report.Costs.Totals) {
		fmt.Fprintf(&out, "cost total/%s value_usd=%s coverage=%d/%d complete=%t reason=%s\n",
			total.name, textFloat(total.total.ValueUSD), total.total.Coverage.Reported, total.total.Coverage.Eligible, total.total.Complete, textValue(strings.Join(total.total.Reasons, ";")))
	}
	for _, dataError := range report.DataErrors {
		fmt.Fprintf(&out, "data_error run=%s code=%s detail=%s\n", dataError.RunID, dataError.Code, strconv.Quote(dataError.Detail))
	}
	return out.String()
}

type namedCostEstimate struct {
	name     string
	estimate pricing.CostEstimate
}

func namedCostEstimates(costs pricing.CostClasses) []namedCostEstimate {
	return []namedCostEstimate{
		{name: "harness_reported", estimate: costs.HarnessReported},
		{name: "api_list_estimate", estimate: costs.APIListEstimate},
		{name: "harness_adjusted_estimate", estimate: costs.HarnessAdjustedEstimate},
	}
}

type namedCostTotal struct {
	name  string
	total CostTotal
}

func namedCostTotals(costs CostTotals) []namedCostTotal {
	return []namedCostTotal{
		{name: "harness_reported", total: costs.HarnessReported},
		{name: "api_list_estimate", total: costs.APIListEstimate},
		{name: "harness_adjusted_estimate", total: costs.HarnessAdjustedEstimate},
	}
}

func textString(value *string) string {
	if value == nil || *value == "" {
		return "—"
	}
	return *value
}

func textValue(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func textInt(value *int) string {
	if value == nil {
		return "—"
	}
	return strconv.Itoa(*value)
}

func textInt64(value *int64) string {
	if value == nil {
		return "—"
	}
	return strconv.FormatInt(*value, 10)
}

func textFloat(value *float64) string {
	if value == nil {
		return "—"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func provenanceCatalog(value pricing.Provenance) string {
	if value.CatalogVersion == 0 && value.CatalogSHA256 == "" {
		return "—"
	}
	return fmt.Sprintf("v%d@%s", value.CatalogVersion, value.CatalogSHA256)
}

func provenanceProfile(value pricing.Provenance) string {
	if value.ProfileID == "" {
		return "—"
	}
	return fmt.Sprintf("%s/v%d", value.ProfileID, value.ProfileVersion)
}

func textNestedAgents(invocation Invocation) string {
	if !invocation.NestedAgentsReported {
		return "—"
	}
	if len(invocation.NestedAgents) == 0 {
		if invocation.NestedAgentCount != nil {
			return strconv.Itoa(*invocation.NestedAgentCount)
		}
		return "0"
	}
	values := make([]string, 0, len(invocation.NestedAgents))
	for _, observation := range invocation.NestedAgents {
		values = append(values, fmt.Sprintf("%s(%s)", observation.Identity, observation.InvocationMode))
	}
	return strings.Join(values, ",")
}

func renderMetricsText(out *strings.Builder, prefix string, metrics Metrics) {
	rows := []struct {
		name      string
		value     string
		coverage  Coverage
		integrity *string
	}{
		{name: "delta_input_tokens", value: textInt64(metrics.DeltaInputTokens.Value), coverage: metrics.DeltaInputTokens.Coverage, integrity: metrics.DeltaInputTokens.IntegrityError},
		{name: "delta_output_tokens", value: textInt64(metrics.DeltaOutputTokens.Value), coverage: metrics.DeltaOutputTokens.Coverage, integrity: metrics.DeltaOutputTokens.IntegrityError},
		{name: "delta_cache_read_tokens", value: textInt64(metrics.DeltaCacheReadTokens.Value), coverage: metrics.DeltaCacheReadTokens.Coverage, integrity: metrics.DeltaCacheReadTokens.IntegrityError},
		{name: "delta_cache_write_tokens", value: textInt64(metrics.DeltaCacheWriteTokens.Value), coverage: metrics.DeltaCacheWriteTokens.Coverage, integrity: metrics.DeltaCacheWriteTokens.IntegrityError},
		{name: "reported_cost_usd", value: textFloat(metrics.ReportedCostUSD.Value), coverage: metrics.ReportedCostUSD.Coverage, integrity: metrics.ReportedCostUSD.IntegrityError},
	}
	for _, row := range rows {
		fmt.Fprintf(out, "%s/%s value=%s coverage=%d/%d integrity=%s\n", prefix, row.name, row.value, row.coverage.Reported, row.coverage.Total, textString(row.integrity))
	}
}

func RenderCSV(report *Report) (string, error) {
	if report == nil {
		return "", fmt.Errorf("render stats CSV: report is nil")
	}
	facts, err := reportFacts(report)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	w := csv.NewWriter(&buffer)
	if err := w.Write(csvHeader); err != nil {
		return "", fmt.Errorf("write stats CSV header: %w", err)
	}
	for _, fact := range facts {
		value := fact.Value
		if !fact.Present {
			value = ""
		}
		_ = w.Write([]string{
			strconv.Itoa(fact.SchemaVersion), fact.RepoID, fact.RunID, fact.RecordType, fact.EntityID,
			fact.Section, fact.Group, fact.Metric, value, fact.Unit,
			strconv.Itoa(fact.Reported), strconv.Itoa(fact.Eligible), strconv.FormatBool(fact.Complete),
			fact.Basis, fact.Reason, fact.JSONPath,
		})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("write stats CSV: %w", err)
	}
	return buffer.String(), nil
}

func reportFacts(report *Report) ([]reportFact, error) {
	encoded, err := report.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	var envelope any
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode canonical stats report for projection: %w", err)
	}
	repoByRun := make(map[string]string, len(report.Runs.Items))
	for _, run := range report.Runs.Items {
		repoByRun[run.ID] = run.RepoID
	}
	var facts []reportFact
	walkReportJSON(report, repoByRun, nil, envelope, &facts)
	return facts, nil
}

func walkReportJSON(report *Report, repoByRun map[string]string, path []string, value any, facts *[]reportFact) {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			appendReportFact(report, repoByRun, path, "{}", true, facts)
			return
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			walkReportJSON(report, repoByRun, appendPath(path, key), typed[key], facts)
		}
	case []any:
		if len(typed) == 0 {
			appendReportFact(report, repoByRun, path, "[]", true, facts)
			return
		}
		for index, item := range typed {
			walkReportJSON(report, repoByRun, appendPath(path, strconv.Itoa(index)), item, facts)
		}
	case nil:
		appendReportFact(report, repoByRun, path, "", false, facts)
	case string:
		appendReportFact(report, repoByRun, path, typed, true, facts)
	case json.Number:
		appendReportFact(report, repoByRun, path, typed.String(), true, facts)
	case bool:
		appendReportFact(report, repoByRun, path, strconv.FormatBool(typed), true, facts)
	default:
		appendReportFact(report, repoByRun, path, fmt.Sprint(typed), true, facts)
	}
}

func appendPath(path []string, segment string) []string {
	result := make([]string, len(path)+1)
	copy(result, path)
	result[len(path)] = segment
	return result
}

func appendReportFact(report *Report, repoByRun map[string]string, path []string, value string, present bool, facts *[]reportFact) {
	fact := factContext(report, repoByRun, path)
	fact.Value = value
	fact.Present = present
	fact.Reported = 1
	fact.Eligible = 1
	fact.Complete = true
	fact.Basis = "report_json"
	if !present {
		fact.Reported = 0
		fact.Complete = false
		fact.Reason = "not_reported"
	}
	fact.Unit = factUnit(path)
	applyCostMetadata(report, path, &fact)
	applyMetricMetadata(report, path, &fact)
	*facts = append(*facts, fact)
}

func factContext(report *Report, repoByRun map[string]string, path []string) reportFact {
	fact := reportFact{SchemaVersion: report.SchemaVersion, RecordType: "report", EntityID: "total", Section: "report", Group: "envelope", JSONPath: jsonPointer(path)}
	fact.Metric = strings.Join(path, ".")
	if len(path) == 0 {
		fact.Metric = "value"
		return fact
	}
	fact.Section = path[0]
	switch path[0] {
	case "scope":
		fact.RecordType, fact.EntityID, fact.Group = "scope", "selection", "filters"
		fact.Metric = relativeMetric(path, 1)
	case "runs":
		if len(path) >= 3 && path[1] == "items" {
			if index, ok := pathIndex(path, 2, len(report.Runs.Items)); ok {
				run := report.Runs.Items[index]
				fact.RepoID, fact.RunID = run.RepoID, run.ID
				fact.RecordType, fact.EntityID, fact.Group = "run", run.ID, "identity"
				fact.Metric = relativeMetric(path, 3)
			}
		} else {
			fact.RecordType, fact.EntityID, fact.Group = "run_summary", "total", "aggregate"
			fact.Metric = relativeMetric(path, 1)
		}
	case "repairs":
		if index, ok := pathIndex(path, 1, len(report.Repairs)); ok {
			repair := report.Repairs[index]
			fact.RunID, fact.RepoID = repair.RunID, repoByRun[repair.RunID]
			fact.RecordType, fact.EntityID, fact.Group = "repair", fmt.Sprintf("%s:%d", repair.StepID, repair.Round), string(repair.Step)
			fact.Metric = relativeMetric(path, 2)
		}
	case "skip_receipts":
		if index, ok := pathIndex(path, 1, len(report.Skips)); ok {
			record := report.Skips[index]
			fact.RunID, fact.RepoID = record.RunID, repoByRun[record.RunID]
			fact.RecordType, fact.EntityID, fact.Group = "skip", record.RunID+":"+string(record.Receipt.Step), string(record.Receipt.Step)
			root := 2
			if len(path) >= 3 && path[2] == "receipt" {
				root = 3
			}
			fact.Metric = relativeMetric(path, root)
		}
	case "steps":
		if index, ok := pathIndex(path, 1, len(report.Steps)); ok {
			record := report.Steps[index]
			fact.RunID, fact.RepoID = record.RunID, repoByRun[record.RunID]
			fact.RecordType, fact.EntityID, fact.Group = "step", record.Step.ID, string(record.Step.Name)
			root := 2
			if len(path) >= 3 && path[2] == "step" {
				root = 3
			}
			if len(path) >= 5 && path[2] == "step" && path[3] == "commands" {
				if commandIndex, ok := pathIndex(path, 4, len(record.Step.Commands)); ok {
					command := record.Step.Commands[commandIndex]
					fact.RecordType = "command"
					fact.EntityID = fmt.Sprintf("%s:%d:%d", record.Step.ID, command.Round, command.Sequence)
					root = 5
				}
			}
			if len(path) >= 5 && path[2] == "step" && path[3] == "rounds" {
				if roundIndex, ok := pathIndex(path, 4, len(record.Step.Rounds)); ok {
					round := record.Step.Rounds[roundIndex]
					fact.RecordType = "round"
					fact.EntityID = fmt.Sprintf("%s:%d", record.Step.ID, round.Number)
					root = 5
				}
			}
			fact.Metric = relativeMetric(path, root)
		}
	case "agents":
		if index, ok := pathIndex(path, 1, len(report.Agents)); ok {
			record := report.Agents[index]
			fact.RunID, fact.RepoID = record.RunID, repoByRun[record.RunID]
			fact.RecordType, fact.EntityID, fact.Group = "agent", record.Invocation.ID, record.Invocation.Agent
			root := 2
			if len(path) >= 3 && path[2] == "invocation" {
				root = 3
			}
			fact.Metric = relativeMetric(path, root)
		}
	case "costs":
		fact.RecordType, fact.Group = "cost", "aggregate"
		if len(path) >= 3 && path[1] == "totals" {
			fact.EntityID = "total:" + path[2]
			fact.Group = path[2]
			fact.Metric = relativeMetric(path, 3)
		} else if len(path) >= 3 && path[1] == "items" {
			if index, ok := pathIndex(path, 2, len(report.Costs.Items)); ok {
				record := report.Costs.Items[index]
				fact.RunID, fact.RepoID = record.RunID, repoByRun[record.RunID]
				fact.EntityID = record.InvocationID
				fact.Metric = relativeMetric(path, 3)
				if len(path) >= 5 && path[3] == "classes" {
					fact.EntityID += ":" + path[4]
					fact.Group = path[4]
					fact.Metric = relativeMetric(path, 5)
				}
			}
		}
	case "metrics":
		fact.RecordType, fact.Group = "metric", "aggregate"
		if len(path) >= 3 && path[1] == "totals" {
			fact.EntityID = "total:" + path[2]
			fact.Group = path[2]
			fact.Metric = relativeMetric(path, 3)
		} else if len(path) >= 3 && path[1] == "items" {
			if index, ok := pathIndex(path, 2, len(report.Metrics.Items)); ok {
				record := report.Metrics.Items[index]
				fact.RunID, fact.RepoID = record.RunID, repoByRun[record.RunID]
				fact.EntityID = record.RunID
				fact.Metric = relativeMetric(path, 3)
				if len(path) >= 5 && path[3] == "metrics" {
					fact.EntityID += ":" + path[4]
					fact.Group = path[4]
					fact.Metric = relativeMetric(path, 5)
				}
			}
		}
	case "data_errors":
		if index, ok := pathIndex(path, 1, len(report.DataErrors)); ok {
			record := report.DataErrors[index]
			fact.RunID, fact.RepoID = record.RunID, repoByRun[record.RunID]
			fact.RecordType, fact.EntityID, fact.Group = "data_error", fmt.Sprintf("%s:%d", record.RunID, index), record.Code
			fact.Metric = relativeMetric(path, 2)
		}
	}
	return fact
}

func pathIndex(path []string, position, length int) (int, bool) {
	if position >= len(path) {
		return 0, false
	}
	index, err := strconv.Atoi(path[position])
	return index, err == nil && index >= 0 && index < length
}

func relativeMetric(path []string, root int) string {
	if root >= len(path) {
		return "value"
	}
	return strings.Join(path[root:], ".")
}

func jsonPointer(path []string) string {
	if len(path) == 0 {
		return ""
	}
	escaped := make([]string, len(path))
	for index, segment := range path {
		escaped[index] = strings.ReplaceAll(strings.ReplaceAll(segment, "~", "~0"), "/", "~1")
	}
	return "/" + strings.Join(escaped, "/")
}

func factUnit(path []string) string {
	if len(path) == 0 {
		return ""
	}
	name := path[len(path)-1]
	if name == "value" && len(path) > 1 {
		name = path[len(path)-2]
	}
	switch {
	case name == "value_usd" || name == "reported_cost_usd":
		return "usd"
	case strings.HasSuffix(name, "_tokens"):
		return "tokens"
	case strings.HasSuffix(name, "_ms"):
		return "milliseconds"
	default:
		return ""
	}
}

func applyMetricMetadata(report *Report, path []string, fact *reportFact) {
	if len(path) == 0 || path[len(path)-1] != "value" {
		return
	}
	metric, ok := reportMetricAt(report, path)
	if !ok {
		return
	}
	fact.Reported = metric.reported
	fact.Eligible = metric.total
	fact.Complete = metric.present && metric.integrityError == ""
	fact.Basis = "canonical_delta_invocation_meters"
	switch {
	case metric.integrityError != "":
		fact.Reason = metric.integrityError
	case !metric.present && metric.total == 0:
		fact.Reason = "not_reported"
	case !metric.present:
		fact.Reason = "partial_coverage"
	default:
		fact.Reason = ""
	}
}

type projectedMetricMetadata struct {
	reported       int
	total          int
	present        bool
	integrityError string
}

func reportMetricAt(report *Report, path []string) (projectedMetricMetadata, bool) {
	if len(path) == 4 && path[0] == "metrics" && path[1] == "totals" {
		return metricMetadata(report.Metrics.Totals, path[2])
	}
	if len(path) == 6 && path[0] == "metrics" && path[1] == "items" && path[3] == "metrics" {
		if index, ok := pathIndex(path, 2, len(report.Metrics.Items)); ok {
			return metricMetadata(report.Metrics.Items[index].Metrics, path[4])
		}
	}
	return projectedMetricMetadata{}, false
}

func metricMetadata(metrics Metrics, name string) (projectedMetricMetadata, bool) {
	fromInt := func(metric IntMetric) projectedMetricMetadata {
		return projectedMetricMetadata{reported: metric.Coverage.Reported, total: metric.Coverage.Total, present: metric.Value != nil, integrityError: stringOrEmpty(metric.IntegrityError)}
	}
	switch name {
	case "delta_input_tokens":
		return fromInt(metrics.DeltaInputTokens), true
	case "delta_output_tokens":
		return fromInt(metrics.DeltaOutputTokens), true
	case "delta_cache_read_tokens":
		return fromInt(metrics.DeltaCacheReadTokens), true
	case "delta_cache_write_tokens":
		return fromInt(metrics.DeltaCacheWriteTokens), true
	case "reported_cost_usd":
		return projectedMetricMetadata{reported: metrics.ReportedCostUSD.Coverage.Reported, total: metrics.ReportedCostUSD.Coverage.Total, present: metrics.ReportedCostUSD.Value != nil, integrityError: stringOrEmpty(metrics.ReportedCostUSD.IntegrityError)}, true
	default:
		return projectedMetricMetadata{}, false
	}
}

func applyCostMetadata(report *Report, path []string, fact *reportFact) {
	if len(path) == 0 || path[len(path)-1] != "value_usd" {
		return
	}
	if estimate, ok := costEstimateAt(report, path); ok {
		fact.Reported = estimate.Coverage.Reported
		fact.Eligible = estimate.Coverage.Eligible
		fact.Complete = estimate.Complete
		fact.Basis = estimate.Basis
		fact.Reason = estimate.Reason
		return
	}
	if total, ok := costTotalAt(report, path); ok {
		fact.Reported = total.Coverage.Reported
		fact.Eligible = total.Coverage.Eligible
		fact.Complete = total.Complete
		fact.Basis = total.Basis
		fact.Reason = strings.Join(total.Reasons, ";")
	}
}

func costEstimateAt(report *Report, path []string) (pricing.CostEstimate, bool) {
	if len(path) == 6 && path[0] == "agents" && path[2] == "invocation" && path[3] == "costs" {
		if index, valid := pathIndex(path, 1, len(report.Agents)); valid {
			return costEstimate(report.Agents[index].Invocation.Costs, path[4])
		}
	}
	if len(path) == 6 && path[0] == "costs" && path[1] == "items" && path[3] == "classes" {
		if index, valid := pathIndex(path, 2, len(report.Costs.Items)); valid {
			return costEstimate(report.Costs.Items[index].Classes, path[4])
		}
	}
	return pricing.CostEstimate{}, false
}

func costEstimate(classes pricing.CostClasses, name string) (pricing.CostEstimate, bool) {
	switch name {
	case "harness_reported":
		return classes.HarnessReported, true
	case "api_list_estimate":
		return classes.APIListEstimate, true
	case "harness_adjusted_estimate":
		return classes.HarnessAdjustedEstimate, true
	default:
		return pricing.CostEstimate{}, false
	}
}

func costTotalAt(report *Report, path []string) (CostTotal, bool) {
	if len(path) != 4 || path[0] != "costs" || path[1] != "totals" {
		return CostTotal{}, false
	}
	switch path[2] {
	case "harness_reported":
		return report.Costs.Totals.HarnessReported, true
	case "api_list_estimate":
		return report.Costs.Totals.APIListEstimate, true
	case "harness_adjusted_estimate":
		return report.Costs.Totals.HarnessAdjustedEstimate, true
	default:
		return CostTotal{}, false
	}
}
