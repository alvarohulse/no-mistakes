package stats

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const ReportSchemaVersion = 9

type Query struct {
	RunID    string
	RepoIDs  []string
	Since    *time.Time
	Until    *time.Time
	Steps    []types.StepName
	Agents   []string
	Models   []string
	Purposes []string
	Statuses []types.RunStatus
}

type Report struct {
	SchemaVersion   int              `json:"schema_version"`
	GeneratedAt     string           `json:"generated_at"`
	Scope           ReportScope      `json:"scope"`
	Runs            ReportRuns       `json:"runs"`
	Skips           []ReportSkip     `json:"skip_receipts"`
	Repairs         []Repair         `json:"repairs"`
	Steps           []ReportStep     `json:"steps"`
	Agents          []ReportAgent    `json:"agents"`
	AgentAggregates []AgentAggregate `json:"agent_aggregates"`
	Metrics         ReportMetrics    `json:"metrics"`
	Dashboard       Dashboard        `json:"dashboard"`
	DataErrors      []DataError      `json:"data_errors"`
}

type ReportScope struct {
	RunID     string            `json:"run_id,omitempty"`
	RepoIDs   []string          `json:"repo_ids"`
	Since     *string           `json:"since"`
	Until     *string           `json:"until"`
	Steps     []types.StepName  `json:"steps"`
	Agents    []string          `json:"agents"`
	Models    []string          `json:"models"`
	Purposes  []string          `json:"purposes"`
	Statuses  []types.RunStatus `json:"statuses"`
	TimeBasis string            `json:"time_basis"`
}

type ReportRuns struct {
	Count    int                     `json:"count"`
	ByStatus map[types.RunStatus]int `json:"by_status"`
	Items    []RunIdentity           `json:"items"`
}

type ReportStep struct {
	RunID string `json:"run_id"`
	Step  Step   `json:"step"`
}

type ReportSkip struct {
	RunID   string      `json:"run_id"`
	Receipt SkipReceipt `json:"receipt"`
}

type Repair struct {
	RunID                    string         `json:"run_id"`
	StepID                   string         `json:"step_id"`
	Step                     types.StepName `json:"step"`
	Round                    int            `json:"round"`
	Trigger                  string         `json:"trigger"`
	SelectionSource          *string        `json:"selection_source"`
	RepairFailureFingerprint *string        `json:"repair_failure_fingerprint"`
	RepairResult             *string        `json:"repair_result"`
	DurationMS               int64          `json:"duration_ms"`
	CreatedAt                int64          `json:"created_at"`
}

type ReportAgent struct {
	RunID      string     `json:"run_id"`
	Invocation Invocation `json:"invocation"`
}

type AgentAggregate struct {
	Purpose           string                 `json:"purpose"`
	Count             int                    `json:"count"`
	TotalDurationMS   int64                  `json:"total_duration_ms"`
	AvgDurationMS     int64                  `json:"avg_duration_ms"`
	SubprocessWaitMS  *int64                 `json:"subprocess_wait_ms"`
	Cold              int                    `json:"cold"`
	Started           int                    `json:"started"`
	Resumed           int                    `json:"resumed"`
	Fallback          int                    `json:"fallback"`
	Errors            int                    `json:"errors"`
	InputTokens       *int64                 `json:"input_tokens"`
	OutputTokens      *int64                 `json:"output_tokens"`
	CacheReadTokens   *int64                 `json:"cache_read_tokens"`
	CacheWriteTokens  *int64                 `json:"cache_write_tokens"`
	FreshInputTokens  *int64                 `json:"fresh_input_tokens"`
	ReasoningTokens   *int64                 `json:"reasoning_tokens"`
	ModelRoundtrips   *int64                 `json:"model_roundtrips"`
	ToolCalls         *int64                 `json:"tool_calls"`
	ToolWaitCalls     *int64                 `json:"tool_wait_calls"`
	ToolTestLintCalls *int64                 `json:"tool_test_lint_calls"`
	ToolEditCalls     *int64                 `json:"tool_edit_calls"`
	ToolReadCalls     *int64                 `json:"tool_read_calls"`
	ToolGitCalls      *int64                 `json:"tool_git_calls"`
	ToolOtherCalls    *int64                 `json:"tool_other_calls"`
	MetricsRows       int                    `json:"metrics_rows"`
	Coverage          AgentAggregateCoverage `json:"coverage"`
}

type AgentAggregateCoverage struct {
	SubprocessWaitMS  Coverage `json:"subprocess_wait_ms"`
	InputTokens       Coverage `json:"input_tokens"`
	OutputTokens      Coverage `json:"output_tokens"`
	CacheReadTokens   Coverage `json:"cache_read_tokens"`
	CacheWriteTokens  Coverage `json:"cache_write_tokens"`
	FreshInputTokens  Coverage `json:"fresh_input_tokens"`
	ReasoningTokens   Coverage `json:"reasoning_tokens"`
	ModelRoundtrips   Coverage `json:"model_roundtrips"`
	ToolCalls         Coverage `json:"tool_calls"`
	ToolWaitCalls     Coverage `json:"tool_wait_calls"`
	ToolTestLintCalls Coverage `json:"tool_test_lint_calls"`
	ToolEditCalls     Coverage `json:"tool_edit_calls"`
	ToolReadCalls     Coverage `json:"tool_read_calls"`
	ToolGitCalls      Coverage `json:"tool_git_calls"`
	ToolOtherCalls    Coverage `json:"tool_other_calls"`
}

type ReportMetrics struct {
	Totals Metrics        `json:"totals"`
	Items  []MetricRecord `json:"items"`
}

type MetricRecord struct {
	RunID   string  `json:"run_id"`
	Metrics Metrics `json:"metrics"`
}

type DataError struct {
	RunID  string `json:"run_id"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type Dashboard struct {
	TotalRepos       int             `json:"total_repos"`
	TotalRuns        int             `json:"total_runs"`
	RescueRuns       int             `json:"rescue_runs"`
	RescueRate       RatioMetric     `json:"rescue_rate"`
	ReportedFindings int             `json:"reported_findings"`
	FixedFindings    int             `json:"fixed_findings"`
	FixRate          RatioMetric     `json:"fix_rate"`
	Steps            []DashboardStep `json:"steps"`
	Repositories     []DashboardRepo `json:"repositories"`
}

type RatioMetric struct {
	Value       *float64 `json:"value"`
	Numerator   int      `json:"numerator"`
	Denominator int      `json:"denominator"`
}

type DashboardStep struct {
	Step             types.StepName `json:"step"`
	ReportedFindings int            `json:"reported_findings"`
	FixedFindings    int            `json:"fixed_findings"`
}

type DashboardRepo struct {
	RepoID           string `json:"repo_id"`
	DisplayName      string `json:"display_name"`
	Runs             int    `json:"runs"`
	RescueRuns       int    `json:"rescue_runs"`
	ReportedFindings int    `json:"reported_findings"`
	FixedFindings    int    `json:"fixed_findings"`
}

func (r Report) CanonicalJSON() (string, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode stats report: %w", err)
	}
	return string(encoded), nil
}

func BuildReport(database *db.DB, query Query, generatedAt time.Time) (*Report, error) {
	if database == nil {
		return nil, fmt.Errorf("build stats report: database is nil")
	}
	if err := query.validate(); err != nil {
		return nil, err
	}
	report := &Report{
		SchemaVersion: ReportSchemaVersion,
		GeneratedAt:   generatedAt.UTC().Format(time.RFC3339),
		Scope:         query.scope(),
		Runs:          ReportRuns{ByStatus: map[types.RunStatus]int{}, Items: []RunIdentity{}},
		Skips:         []ReportSkip{}, Repairs: []Repair{}, Steps: []ReportStep{}, Agents: []ReportAgent{},
		AgentAggregates: []AgentAggregate{}, Metrics: ReportMetrics{Items: []MetricRecord{}},
		Dashboard: Dashboard{Steps: []DashboardStep{}, Repositories: []DashboardRepo{}}, DataErrors: []DataError{},
	}

	repos, err := database.GetRepos()
	if err != nil {
		return nil, fmt.Errorf("list repositories for stats: %w", err)
	}
	repoDisplayNames := make(map[string]string, len(repos))
	for _, repo := range repos {
		repoDisplayNames[repo.ID] = dashboardRepoDisplayName(repo.WorkingPath, repo.ID)
	}
	repoFilter := stringSet(query.RepoIDs)
	stepFilter := stepSet(query.Steps)
	agentFilter := normalizedSet(query.Agents)
	modelFilter := normalizedSet(query.Models)
	purposeFilter := normalizedSet(query.Purposes)
	statusFilter := statusSet(query.Statuses)
	var audits []*RunAudit
	rawRunIDs := map[string]bool{}
	for _, repo := range repos {
		if len(repoFilter) > 0 && !repoFilter[repo.ID] {
			continue
		}
		runs, err := database.GetRunsByRepo(repo.ID)
		if err != nil {
			return nil, fmt.Errorf("list runs for repository %s: %w", repo.ID, err)
		}
		for _, run := range runs {
			audit, err := BuildRunAudit(database, run.ID)
			if err != nil {
				return nil, err
			}
			audits = append(audits, audit)
			rawRunIDs[run.ID] = true
		}
	}
	receiptRecords, err := database.GetRunMetricReceipts()
	if err != nil {
		return nil, err
	}
	for i := range receiptRecords {
		if rawRunIDs[receiptRecords[i].RunID] {
			continue
		}
		receipt, err := decodeMetricReceipt(&receiptRecords[i])
		if err != nil {
			return nil, err
		}
		audits = append(audits, receipt.RunAudit())
	}
	sort.Slice(audits, func(i, j int) bool {
		if audits[i].Run.CreatedAt != audits[j].Run.CreatedAt {
			return audits[i].Run.CreatedAt > audits[j].Run.CreatedAt
		}
		return audits[i].Run.ID > audits[j].Run.ID
	})
	childInvocationFilter := len(agentFilter) > 0 || len(modelFilter) > 0 || len(purposeFilter) > 0
	childFilter := len(stepFilter) > 0 || childInvocationFilter
	var selectedInvocations []Invocation
	var dashboardRuns []dashboardRun
	for _, audit := range audits {
		if query.RunID != "" && audit.Run.ID != query.RunID {
			continue
		}
		if len(repoFilter) > 0 && !repoFilter[audit.Run.RepoID] {
			continue
		}
		if query.Since != nil && audit.Run.CreatedAt < query.Since.Unix() {
			continue
		}
		if query.Until != nil && audit.Run.CreatedAt >= query.Until.Unix() {
			continue
		}
		if len(statusFilter) > 0 && !statusFilter[audit.Run.Status] {
			continue
		}
		invocations := filterInvocations(audit.Invocations, stepFilter, agentFilter, modelFilter, purposeFilter)
		steps := filterSteps(audit.Steps, stepFilter, invocations, childInvocationFilter)
		if childFilter && ((childInvocationFilter && len(invocations) == 0) || (!childInvocationFilter && len(steps) == 0)) {
			continue
		}
		report.Runs.Items = append(report.Runs.Items, audit.Run)
		report.Runs.ByStatus[audit.Run.Status]++
		dashboardRuns = append(dashboardRuns, dashboardRun{identity: audit.Run, steps: steps})
		selectedStepNames := make(map[types.StepName]bool, len(steps))
		for _, step := range steps {
			selectedStepNames[step.Name.Canonical()] = true
		}
		for _, receipt := range audit.SkipReceipts {
			if childFilter && !selectedStepNames[receipt.Step.Canonical()] {
				continue
			}
			report.Skips = append(report.Skips, ReportSkip{RunID: audit.Run.ID, Receipt: receipt})
		}
		metrics := audit.Metrics
		if childFilter {
			metrics = projectedMetrics(invocations)
		}
		report.Metrics.Items = append(report.Metrics.Items, MetricRecord{RunID: audit.Run.ID, Metrics: metrics})
		for _, step := range steps {
			report.Steps = append(report.Steps, ReportStep{RunID: audit.Run.ID, Step: step})
			for _, round := range step.Rounds {
				if round.Status != "" && round.Status != db.RoundStatusCompleted {
					continue
				}
				isFixRound := round.Trigger == "auto_fix" || round.Trigger == "user_fix"
				hasRepairDecision := round.RepairFailureFingerprint != nil || round.RepairResult != nil
				if !isFixRound && !hasRepairDecision {
					continue
				}
				report.Repairs = append(report.Repairs, Repair{
					RunID: audit.Run.ID, StepID: step.ID, Step: step.Name, Round: round.Number, Trigger: round.Trigger,
					SelectionSource: round.SelectionSource, RepairFailureFingerprint: round.RepairFailureFingerprint, RepairResult: round.RepairResult,
					DurationMS: round.DurationMS, CreatedAt: round.CreatedAt,
				})
			}
		}
		for _, invocation := range invocations {
			report.Agents = append(report.Agents, ReportAgent{RunID: audit.Run.ID, Invocation: invocation})
			selectedInvocations = append(selectedInvocations, invocation)
		}
		for _, integrityError := range audit.IntegrityErrors {
			report.DataErrors = append(report.DataErrors, DataError{RunID: audit.Run.ID, Code: "run_integrity_error", Detail: integrityError})
		}
	}
	if query.RunID != "" && len(report.Runs.Items) == 0 {
		return nil, fmt.Errorf("run %q not found in selected scope", query.RunID)
	}
	report.Runs.Count = len(report.Runs.Items)
	report.AgentAggregates = buildAgentAggregates(selectedInvocations)
	report.Metrics.Totals = projectedMetrics(selectedInvocations)
	report.Dashboard = buildDashboard(dashboardRuns, repoDisplayNames, query.unfiltered())
	return report, nil
}

type dashboardRun struct {
	identity RunIdentity
	steps    []Step
}

func buildDashboard(runs []dashboardRun, displayNames map[string]string, includeEmptyRepositories bool) Dashboard {
	dashboard := Dashboard{TotalRuns: len(runs), Steps: []DashboardStep{}, Repositories: []DashboardRepo{}}
	stepTotals := make(map[types.StepName]*DashboardStep)
	repoTotals := make(map[string]*DashboardRepo)
	if includeEmptyRepositories {
		for repoID, displayName := range displayNames {
			repoTotals[repoID] = &DashboardRepo{RepoID: repoID, DisplayName: displayName}
		}
	}
	for _, run := range runs {
		repo := repoTotals[run.identity.RepoID]
		if repo == nil {
			displayName := displayNames[run.identity.RepoID]
			if displayName == "" {
				displayName = run.identity.RepoID
			}
			repo = &DashboardRepo{RepoID: run.identity.RepoID, DisplayName: displayName}
			repoTotals[run.identity.RepoID] = repo
		}
		repo.Runs++
		runReported, runFixed := 0, 0
		for _, step := range run.steps {
			runReported += step.ReportedFindings
			runFixed += step.FixedFindings
			name := step.Name.Canonical()
			total := stepTotals[name]
			if total == nil {
				total = &DashboardStep{Step: name}
				stepTotals[name] = total
			}
			total.ReportedFindings += step.ReportedFindings
			total.FixedFindings += step.FixedFindings
		}
		dashboard.ReportedFindings += runReported
		dashboard.FixedFindings += runFixed
		repo.ReportedFindings += runReported
		repo.FixedFindings += runFixed
		if runReported > 0 && runFixed > 0 {
			dashboard.RescueRuns++
			repo.RescueRuns++
		}
	}
	dashboard.TotalRepos = len(repoTotals)
	dashboard.RescueRate = ratioMetric(dashboard.RescueRuns, dashboard.TotalRuns)
	dashboard.FixRate = ratioMetric(dashboard.FixedFindings, dashboard.ReportedFindings)
	seenSteps := make(map[types.StepName]bool, len(stepTotals))
	for _, name := range types.AllSteps() {
		if total := stepTotals[name]; total != nil {
			dashboard.Steps = append(dashboard.Steps, *total)
			seenSteps[name] = true
		}
	}
	var extraSteps []types.StepName
	for name := range stepTotals {
		if !seenSteps[name] {
			extraSteps = append(extraSteps, name)
		}
	}
	sort.Slice(extraSteps, func(i, j int) bool { return extraSteps[i] < extraSteps[j] })
	for _, name := range extraSteps {
		dashboard.Steps = append(dashboard.Steps, *stepTotals[name])
	}
	for _, repo := range repoTotals {
		dashboard.Repositories = append(dashboard.Repositories, *repo)
	}
	sort.Slice(dashboard.Repositories, func(i, j int) bool {
		a, b := dashboard.Repositories[i], dashboard.Repositories[j]
		if a.RescueRuns != b.RescueRuns {
			return a.RescueRuns > b.RescueRuns
		}
		if a.FixedFindings != b.FixedFindings {
			return a.FixedFindings > b.FixedFindings
		}
		if a.Runs != b.Runs {
			return a.Runs > b.Runs
		}
		if a.DisplayName != b.DisplayName {
			return a.DisplayName < b.DisplayName
		}
		return a.RepoID < b.RepoID
	})
	return dashboard
}

func ratioMetric(numerator, denominator int) RatioMetric {
	result := RatioMetric{Numerator: numerator, Denominator: denominator}
	if denominator > 0 {
		value := float64(numerator) / float64(denominator)
		result.Value = &value
	}
	return result
}

func dashboardRepoDisplayName(path, fallback string) string {
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return fallback
	}
	return name
}

func buildAgentAggregates(invocations []Invocation) []AgentAggregate {
	type accumulator struct {
		aggregate AgentAggregate
		complete  map[string]bool
		reported  map[string]int
	}
	byPurpose := make(map[string]*accumulator)
	for _, invocation := range invocations {
		item := byPurpose[invocation.Purpose]
		if item == nil {
			item = &accumulator{aggregate: AgentAggregate{Purpose: invocation.Purpose}, complete: map[string]bool{
				"subprocess": true, "input": true, "output": true, "cache_read": true, "cache_write": true, "fresh_input": true, "reasoning": true,
				"roundtrips": true, "tools": true, "wait": true, "test_lint": true, "edit": true, "read": true, "git": true, "other": true,
			}, reported: map[string]int{}}
			byPurpose[invocation.Purpose] = item
		}
		aggregate := &item.aggregate
		aggregate.Count++
		aggregate.TotalDurationMS += invocation.DurationMS
		switch invocation.SessionMode {
		case db.InvocationModeCold:
			aggregate.Cold++
		case db.InvocationModeStarted:
			aggregate.Started++
		case db.InvocationModeResumed:
			aggregate.Resumed++
		case db.InvocationModeFallback:
			aggregate.Fallback++
		}
		if invocation.ExitStatus != "ok" {
			aggregate.Errors++
		}
		mergeAggregateInt64(&aggregate.SubprocessWaitMS, invocation.Activity.SubprocessWaitMS, item.complete, "subprocess")
		recordAggregateCoverage(item.reported, "subprocess", invocation.Activity.SubprocessWaitMS != nil)
		mergeAggregateInt(&aggregate.InputTokens, invocation.DeltaUsage.InputTokens, item.complete, "input")
		recordAggregateCoverage(item.reported, "input", invocation.DeltaUsage.InputTokens != nil)
		mergeAggregateInt(&aggregate.OutputTokens, invocation.DeltaUsage.OutputTokens, item.complete, "output")
		recordAggregateCoverage(item.reported, "output", invocation.DeltaUsage.OutputTokens != nil)
		mergeAggregateInt(&aggregate.CacheReadTokens, invocation.DeltaUsage.CacheReadTokens, item.complete, "cache_read")
		recordAggregateCoverage(item.reported, "cache_read", invocation.DeltaUsage.CacheReadTokens != nil)
		mergeAggregateInt(&aggregate.CacheWriteTokens, invocation.DeltaUsage.CacheWriteTokens, item.complete, "cache_write")
		recordAggregateCoverage(item.reported, "cache_write", invocation.DeltaUsage.CacheWriteTokens != nil)
		freshInput := aggregateFreshInput(invocation)
		mergeAggregateInt(&aggregate.FreshInputTokens, freshInput, item.complete, "fresh_input")
		recordAggregateCoverage(item.reported, "fresh_input", freshInput != nil)
		reasoning := aggregateReasoning(invocation)
		mergeAggregateInt(&aggregate.ReasoningTokens, reasoning, item.complete, "reasoning")
		recordAggregateCoverage(item.reported, "reasoning", reasoning != nil)
		mergeAggregateInt(&aggregate.ModelRoundtrips, invocation.Activity.ModelRoundtrips, item.complete, "roundtrips")
		recordAggregateCoverage(item.reported, "roundtrips", invocation.Activity.ModelRoundtrips != nil)
		mergeAggregateInt(&aggregate.ToolCalls, invocation.Activity.ToolCalls, item.complete, "tools")
		recordAggregateCoverage(item.reported, "tools", invocation.Activity.ToolCalls != nil)
		mergeAggregateInt(&aggregate.ToolWaitCalls, invocation.Activity.ToolWaitCalls, item.complete, "wait")
		recordAggregateCoverage(item.reported, "wait", invocation.Activity.ToolWaitCalls != nil)
		mergeAggregateInt(&aggregate.ToolTestLintCalls, invocation.Activity.ToolTestLintCalls, item.complete, "test_lint")
		recordAggregateCoverage(item.reported, "test_lint", invocation.Activity.ToolTestLintCalls != nil)
		mergeAggregateInt(&aggregate.ToolEditCalls, invocation.Activity.ToolEditCalls, item.complete, "edit")
		recordAggregateCoverage(item.reported, "edit", invocation.Activity.ToolEditCalls != nil)
		mergeAggregateInt(&aggregate.ToolReadCalls, invocation.Activity.ToolReadCalls, item.complete, "read")
		recordAggregateCoverage(item.reported, "read", invocation.Activity.ToolReadCalls != nil)
		mergeAggregateInt(&aggregate.ToolGitCalls, invocation.Activity.ToolGitCalls, item.complete, "git")
		recordAggregateCoverage(item.reported, "git", invocation.Activity.ToolGitCalls != nil)
		mergeAggregateInt(&aggregate.ToolOtherCalls, invocation.Activity.ToolOtherCalls, item.complete, "other")
		recordAggregateCoverage(item.reported, "other", invocation.Activity.ToolOtherCalls != nil)
		if invocation.Activity.ModelRoundtrips != nil {
			aggregate.MetricsRows++
		}
	}
	result := make([]AgentAggregate, 0, len(byPurpose))
	for _, item := range byPurpose {
		if item.aggregate.Count > 0 {
			item.aggregate.AvgDurationMS = item.aggregate.TotalDurationMS / int64(item.aggregate.Count)
		}
		item.aggregate.Coverage = buildAgentAggregateCoverage(item.reported, item.aggregate.Count)
		result = append(result, item.aggregate)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TotalDurationMS != result[j].TotalDurationMS {
			return result[i].TotalDurationMS > result[j].TotalDurationMS
		}
		return result[i].Purpose < result[j].Purpose
	})
	return result
}

func aggregateFreshInput(invocation Invocation) *int {
	_, fresh := agent.CanonicalInputMeters(invocation.Agent, invocation.DeltaUsage.InputTokens, invocation.DeltaUsage.CacheReadTokens, invocation.DeltaUsage.CacheWriteTokens)
	return fresh
}

func aggregateReasoning(invocation Invocation) *int {
	if normalized(invocation.Agent) == "codex" && invocation.SessionMode == db.InvocationModeResumed {
		return nil
	}
	return invocation.RawUsage.ReasoningTokens
}

func recordAggregateCoverage(reported map[string]int, key string, present bool) {
	if present {
		reported[key]++
	}
}

func buildAgentAggregateCoverage(reported map[string]int, total int) AgentAggregateCoverage {
	coverage := func(key string) Coverage { return Coverage{Reported: reported[key], Total: total} }
	return AgentAggregateCoverage{
		SubprocessWaitMS: coverage("subprocess"), InputTokens: coverage("input"), OutputTokens: coverage("output"),
		CacheReadTokens: coverage("cache_read"), CacheWriteTokens: coverage("cache_write"), FreshInputTokens: coverage("fresh_input"),
		ReasoningTokens: coverage("reasoning"), ModelRoundtrips: coverage("roundtrips"), ToolCalls: coverage("tools"),
		ToolWaitCalls: coverage("wait"), ToolTestLintCalls: coverage("test_lint"), ToolEditCalls: coverage("edit"),
		ToolReadCalls: coverage("read"), ToolGitCalls: coverage("git"), ToolOtherCalls: coverage("other"),
	}
}

func projectedMetrics(invocations []Invocation) Metrics {
	observed := observedTotals(invocations)
	expected := db.AgentInvocationAuditTotals{
		Rows:               len(invocations),
		DeltaInputReported: observed.inputReported, DeltaInputSum: optionalInt64Sum(observed.inputReported, observed.inputSum),
		DeltaOutputReported: observed.outputReported, DeltaOutputSum: optionalInt64Sum(observed.outputReported, observed.outputSum),
		DeltaCacheReadReported: observed.cacheReadReported, DeltaCacheReadSum: optionalInt64Sum(observed.cacheReadReported, observed.cacheReadSum),
		DeltaCacheCreationReported: observed.cacheWriteReported, DeltaCacheCreationSum: optionalInt64Sum(observed.cacheWriteReported, observed.cacheWriteSum),
		ReportedCostReported: observed.costReported, ReportedCostSum: optionalFloat64Sum(observed.costReported, observed.costSum),
	}
	metrics, _ := buildMetrics(invocations, expected)
	return metrics
}

func optionalInt64Sum(reported int, value int64) *int64 {
	if reported == 0 {
		return nil
	}
	return &value
}

func optionalFloat64Sum(reported int, value float64) *float64 {
	if reported == 0 {
		return nil
	}
	return &value
}

func (q Query) validate() error {
	if q.Since != nil && q.Until != nil && !q.Since.Before(*q.Until) {
		return fmt.Errorf("stats time window must satisfy since < until")
	}
	for _, step := range q.Steps {
		if step.Canonical().Order() == 0 {
			return fmt.Errorf("unsupported stats step %q", step)
		}
	}
	for _, status := range q.Statuses {
		switch status {
		case types.RunPending, types.RunRunning, types.RunCompleted, types.RunFailed, types.RunCancelled:
		default:
			return fmt.Errorf("unsupported stats run status %q", status)
		}
	}
	for _, selectors := range []struct {
		name   string
		values []string
	}{
		{name: "repository", values: q.RepoIDs},
		{name: "agent", values: q.Agents},
		{name: "model", values: q.Models},
		{name: "purpose", values: q.Purposes},
	} {
		for _, value := range selectors.values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("stats %s selector cannot be empty", selectors.name)
			}
		}
	}
	return nil
}

func (q Query) unfiltered() bool {
	return strings.TrimSpace(q.RunID) == "" && len(q.RepoIDs) == 0 && q.Since == nil && q.Until == nil && len(q.Steps) == 0 &&
		len(q.Agents) == 0 && len(q.Models) == 0 && len(q.Purposes) == 0 && len(q.Statuses) == 0
}

func (q Query) scope() ReportScope {
	return ReportScope{
		RunID: strings.TrimSpace(q.RunID), RepoIDs: nonNilStrings(q.RepoIDs), Since: formatTime(q.Since), Until: formatTime(q.Until),
		Steps: nonNilSteps(q.Steps), Agents: nonNilStrings(q.Agents), Models: nonNilStrings(q.Models),
		Purposes: nonNilStrings(q.Purposes), Statuses: nonNilStatuses(q.Statuses), TimeBasis: "run_created_at",
	}
}

func filterInvocations(invocations []Invocation, steps map[types.StepName]bool, agents, models, purposes map[string]bool) []Invocation {
	result := make([]Invocation, 0, len(invocations))
	for _, invocation := range invocations {
		if len(steps) > 0 && !steps[invocation.Step.Canonical()] {
			continue
		}
		if len(agents) > 0 && !agents[normalized(invocation.Agent)] {
			continue
		}
		if len(models) > 0 && !models[normalized(stringOrEmpty(invocation.Model))] {
			continue
		}
		if len(purposes) > 0 && !purposes[normalized(invocation.Purpose)] {
			continue
		}
		result = append(result, invocation)
	}
	return result
}

func filterSteps(steps []Step, filter map[types.StepName]bool, invocations []Invocation, invocationFilter bool) []Step {
	invocationSteps := make(map[types.StepName]bool, len(invocations))
	for _, invocation := range invocations {
		invocationSteps[invocation.Step.Canonical()] = true
	}
	result := make([]Step, 0, len(steps))
	for _, step := range steps {
		name := step.Name.Canonical()
		if len(filter) > 0 && !filter[name] {
			continue
		}
		if invocationFilter && !invocationSteps[name] {
			continue
		}
		result = append(result, step)
	}
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result[value] = true
		}
	}
	return result
}

func normalizedSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if value = normalized(value); value != "" {
			result[value] = true
		}
	}
	return result
}

func normalized(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func stepSet(values []types.StepName) map[types.StepName]bool {
	result := make(map[types.StepName]bool, len(values))
	for _, value := range values {
		result[value.Canonical()] = true
	}
	return result
}

func statusSet(values []types.RunStatus) map[types.RunStatus]bool {
	result := make(map[types.RunStatus]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func formatTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return append([]string(nil), values...)
}

func nonNilSteps(values []types.StepName) []types.StepName {
	if values == nil {
		return []types.StepName{}
	}
	return append([]types.StepName(nil), values...)
}

func nonNilStatuses(values []types.RunStatus) []types.RunStatus {
	if values == nil {
		return []types.RunStatus{}
	}
	return append([]types.RunStatus(nil), values...)
}
