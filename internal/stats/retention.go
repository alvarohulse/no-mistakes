package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pricing"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	MetricReceiptSchemaVersion = 2
	RichRunRetentionAge        = 14 * 24 * time.Hour
	RichRunRetentionFloor      = 50
)

// MetricReceipt is the long-lived, content-free subset of one terminal run.
// Its explicit DTO prevents later additions to the rich audit from silently
// retaining run branches/heads, session keys, paths, or prose.
type MetricReceipt struct {
	SchemaVersion       int                `json:"schema_version"`
	ArchivedAt          int64              `json:"archived_at"`
	Run                 MetricRun          `json:"run"`
	PullRequest         bool               `json:"pull_request"`
	Steps               []MetricStep       `json:"steps"`
	Invocations         []MetricInvocation `json:"invocations"`
	Metrics             Metrics            `json:"metrics"`
	Costs               CostTotals         `json:"costs"`
	IntegrityErrorCount int                `json:"integrity_error_count"`
}

type MetricRun struct {
	ID                 string          `json:"id"`
	RepoID             string          `json:"repo_id"`
	Status             types.RunStatus `json:"status"`
	CreatedAt          int64           `json:"created_at"`
	UpdatedAt          int64           `json:"updated_at"`
	ParkedMS           *int64          `json:"parked_ms"`
	NoMistakesVersion  *string         `json:"no_mistakes_version"`
	NoMistakesBuildSHA *string         `json:"no_mistakes_build_sha"`
}

type MetricStep struct {
	ID               string            `json:"id"`
	Name             types.StepName    `json:"name"`
	Order            int               `json:"order"`
	Status           types.StepStatus  `json:"status"`
	SkipSource       *types.SkipSource `json:"skip_source"`
	ExitCode         *int              `json:"exit_code"`
	DurationMS       *int64            `json:"duration_ms"`
	StartedAt        *int64            `json:"started_at"`
	CompletedAt      *int64            `json:"completed_at"`
	Commands         []CommandReceipt  `json:"commands"`
	Rounds           []Round           `json:"rounds"`
	ReportedFindings int               `json:"reported_findings"`
	FixedFindings    int               `json:"fixed_findings"`
}

type MetricInvocation struct {
	ID                   string                    `json:"id"`
	Step                 types.StepName            `json:"step"`
	Round                int                       `json:"round"`
	Purpose              string                    `json:"purpose"`
	Agent                string                    `json:"agent"`
	InvocationMode       types.AgentInvocationMode `json:"invocation_mode"`
	NestedAgentsReported bool                      `json:"nested_agents_reported"`
	NestedAgentCount     *int                      `json:"nested_agent_count"`
	Model                *string                   `json:"model"`
	Provider             *string                   `json:"provider"`
	SessionMode          string                    `json:"session_mode"`
	FallbackReason       *string                   `json:"fallback_reason"`
	StartedAt            int64                     `json:"started_at"`
	CompletedAt          int64                     `json:"completed_at"`
	DurationMS           int64                     `json:"duration_ms"`
	ExitStatus           string                    `json:"exit_status"`
	FailureCategory      *string                   `json:"failure_category"`
	RawUsage             TokenMeters               `json:"raw_usage"`
	DeltaUsage           TokenMeters               `json:"delta_usage"`
	ReportedCostUSD      *float64                  `json:"reported_cost_usd"`
	Costs                pricing.CostClasses       `json:"costs"`
	Activity             Activity                  `json:"activity"`
}

// PruneRichRunData archives and removes terminal unpinned runs outside the
// required age and newest-run floors. beforeDelete owns filesystem artifacts;
// any cleanup failure leaves the rich database row untouched.
func PruneRichRunData(database *db.DB, now time.Time, retentionAge time.Duration, keepNewestTerminal int, beforeDelete func(string) error) (int, error) {
	if database == nil {
		return 0, fmt.Errorf("prune rich run data: database is nil")
	}
	if retentionAge <= 0 {
		return 0, nil
	}
	if retentionAge < RichRunRetentionAge {
		retentionAge = RichRunRetentionAge
	}
	if keepNewestTerminal < RichRunRetentionFloor {
		keepNewestTerminal = RichRunRetentionFloor
	}
	candidates, err := database.ListRunRetentionCandidates(now.Add(-retentionAge).Unix(), keepNewestTerminal)
	if err != nil {
		return 0, err
	}
	pruned := 0
	var failures []error
	for _, runID := range candidates {
		_, record, err := BuildMetricReceipt(database, runID, now)
		if err != nil {
			failures = append(failures, fmt.Errorf("archive run %s metrics: %w", runID, err))
			continue
		}
		archived, err := database.ArchiveRunWithMetricReceipt(record, true, func() error {
			if beforeDelete == nil {
				return nil
			}
			return beforeDelete(runID)
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("archive run %s: %w", runID, err))
			continue
		}
		if archived {
			pruned++
		}
	}
	return pruned, errors.Join(failures...)
}

// ArchiveRepoRuns preserves every terminal run before its repository record is
// explicitly removed. Active runs fail closed rather than becoming incomplete
// immutable receipts.
func ArchiveRepoRuns(database *db.DB, repoID string, now time.Time, beforeDelete func(string) error) (int, error) {
	runs, err := database.GetRunsByRepo(repoID)
	if err != nil {
		return 0, err
	}
	for _, run := range runs {
		if !isTerminalStatus(run.Status) {
			return 0, fmt.Errorf("repository %s has active run %s", repoID, run.ID)
		}
	}
	archivedCount := 0
	for _, run := range runs {
		_, record, err := BuildMetricReceipt(database, run.ID, now)
		if err != nil {
			return archivedCount, err
		}
		archived, err := database.ArchiveRunWithMetricReceipt(record, false, func() error {
			if beforeDelete == nil {
				return nil
			}
			return beforeDelete(run.ID)
		})
		if err != nil {
			return archivedCount, err
		}
		if archived {
			archivedCount++
		}
	}
	return archivedCount, nil
}

func BuildMetricReceipt(database *db.DB, runID string, archivedAt time.Time) (*MetricReceipt, db.RunMetricReceipt, error) {
	audit, err := BuildRunAudit(database, runID)
	if err != nil {
		return nil, db.RunMetricReceipt{}, err
	}
	run, err := database.GetRun(runID)
	if err != nil {
		return nil, db.RunMetricReceipt{}, err
	}
	if run == nil {
		return nil, db.RunMetricReceipt{}, fmt.Errorf("run %q not found", runID)
	}
	invocationRows, err := database.GetAgentInvocationsByRun(runID)
	if err != nil {
		return nil, db.RunMetricReceipt{}, err
	}
	receipt := &MetricReceipt{
		SchemaVersion: MetricReceiptSchemaVersion,
		ArchivedAt:    archivedAt.UTC().Unix(),
		Run: MetricRun{
			ID: run.ID, RepoID: run.RepoID, Status: run.Status, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
			ParkedMS: cloneInt64(audit.Run.ParkedMS), NoMistakesVersion: cloneString(run.NoMistakesVersion), NoMistakesBuildSHA: cloneString(run.NoMistakesBuildSHA),
		},
		PullRequest:         run.PRURL != nil && *run.PRURL != "",
		Steps:               []MetricStep{},
		Invocations:         []MetricInvocation{},
		Metrics:             audit.Metrics,
		Costs:               audit.Costs,
		IntegrityErrorCount: len(audit.IntegrityErrors),
	}
	stepStats := make([]db.StepStats, 0, len(audit.Steps))
	for _, step := range audit.Steps {
		stored, err := database.GetStepResult(step.ID)
		if err != nil {
			return nil, db.RunMetricReceipt{}, err
		}
		findingStats := db.StepStats{StepName: step.Name}
		if stored != nil {
			findingStats, err = database.StepFindingStats(stored)
			if err != nil {
				return nil, db.RunMetricReceipt{}, err
			}
		}
		stepStats = append(stepStats, findingStats)
		receipt.Steps = append(receipt.Steps, MetricStep{
			ID: step.ID, Name: step.Name, Order: step.Order, Status: step.Status, SkipSource: cloneSkipSource(step.SkipSource),
			ExitCode: cloneInt(step.ExitCode), DurationMS: cloneInt64(step.DurationMS), StartedAt: cloneInt64(step.StartedAt), CompletedAt: cloneInt64(step.CompletedAt),
			Commands: cloneCommandReceipts(step.Commands), Rounds: cloneRounds(step.Rounds),
			ReportedFindings: findingStats.ReportedFindings, FixedFindings: findingStats.FixedFindings,
		})
	}
	for _, invocation := range audit.Invocations {
		receipt.Invocations = append(receipt.Invocations, metricInvocation(invocation))
	}
	receipt.Steps = nonNilMetricSteps(receipt.Steps)
	receipt.Invocations = nonNilMetricInvocations(receipt.Invocations)
	payload, err := json.Marshal(receipt)
	if err != nil {
		return nil, db.RunMetricReceipt{}, fmt.Errorf("encode run metric receipt: %w", err)
	}
	reported, fixed := 0, 0
	for _, step := range stepStats {
		reported += step.ReportedFindings
		fixed += step.FixedFindings
	}
	record := db.RunMetricReceipt{
		RunID: run.ID, RepoID: run.RepoID, RunCreatedAt: run.CreatedAt, RunStatus: run.Status,
		SchemaVersion: MetricReceiptSchemaVersion, PayloadJSON: string(payload), ArchivedAt: receipt.ArchivedAt,
		PullRequest: receipt.PullRequest, ReportedFindings: reported, FixedFindings: fixed,
		StepStats: stepStats, AgentAggregates: buildReceiptAgentAggregates(invocationRows),
	}
	return receipt, record, nil
}

func metricInvocation(invocation Invocation) MetricInvocation {
	count := invocation.NestedAgentCount
	if count == nil && invocation.NestedAgentsReported {
		value := len(invocation.NestedAgents)
		count = &value
	}
	return MetricInvocation{
		ID: invocation.ID, Step: invocation.Step, Round: invocation.Round, Purpose: invocation.Purpose, Agent: invocation.Agent,
		InvocationMode: invocation.InvocationMode, NestedAgentsReported: invocation.NestedAgentsReported, NestedAgentCount: cloneInt(count),
		Model: cloneString(invocation.Model), Provider: cloneString(invocation.Provider), SessionMode: invocation.SessionMode,
		FallbackReason: cloneString(invocation.FallbackReason), StartedAt: invocation.StartedAt, CompletedAt: invocation.CompletedAt,
		DurationMS: invocation.DurationMS, ExitStatus: invocation.ExitStatus, FailureCategory: cloneString(invocation.FailureCategory),
		RawUsage: cloneTokenMeters(invocation.RawUsage), DeltaUsage: cloneTokenMeters(invocation.DeltaUsage),
		ReportedCostUSD: cloneFloat64(invocation.ReportedCostUSD), Costs: invocation.Costs, Activity: invocation.Activity,
	}
}

func (receipt MetricReceipt) RunAudit() *RunAudit {
	audit := &RunAudit{
		SchemaVersion: SchemaVersion,
		Run: RunIdentity{
			ID: receipt.Run.ID, RepoID: receipt.Run.RepoID, Status: receipt.Run.Status,
			CreatedAt: receipt.Run.CreatedAt, UpdatedAt: receipt.Run.UpdatedAt, ParkedMS: cloneInt64(receipt.Run.ParkedMS),
			NoMistakesVersion: cloneString(receipt.Run.NoMistakesVersion), NoMistakesBuildSHA: cloneString(receipt.Run.NoMistakesBuildSHA),
			ConfigSources: []ConfigDigest{}, RichDataRetained: false,
		},
		Steps: []Step{}, SkipReceipts: []SkipReceipt{}, Invocations: []Invocation{}, Metrics: receipt.Metrics, Costs: receipt.Costs, IntegrityErrors: []string{},
	}
	if receipt.IntegrityErrorCount > 0 {
		audit.IntegrityErrors = []string{fmt.Sprintf("archived run recorded %d integrity errors before rich data was pruned", receipt.IntegrityErrorCount)}
	}
	for _, stored := range receipt.Steps {
		step := Step{
			ID: stored.ID, Name: stored.Name, Order: stored.Order, Status: stored.Status, SkipSource: cloneSkipSource(stored.SkipSource),
			ExitCode: cloneInt(stored.ExitCode), DurationMS: cloneInt64(stored.DurationMS), StartedAt: cloneInt64(stored.StartedAt), CompletedAt: cloneInt64(stored.CompletedAt),
			Commands: cloneCommandReceipts(stored.Commands), Rounds: cloneRounds(stored.Rounds),
		}
		audit.Steps = append(audit.Steps, step)
		if step.SkipSource != nil {
			audit.SkipReceipts = append(audit.SkipReceipts, SkipReceipt{Step: step.Name, Source: *step.SkipSource})
		}
	}
	for _, stored := range receipt.Invocations {
		audit.Invocations = append(audit.Invocations, Invocation{
			ID: stored.ID, Step: stored.Step, Round: stored.Round, Purpose: stored.Purpose, Agent: stored.Agent,
			InvocationMode: stored.InvocationMode, NestedAgentsReported: stored.NestedAgentsReported, NestedAgentCount: cloneInt(stored.NestedAgentCount), NestedAgents: []types.AgentObservation{},
			Model: cloneString(stored.Model), Provider: cloneString(stored.Provider), SessionMode: stored.SessionMode, SessionKey: "",
			FallbackReason: cloneString(stored.FallbackReason), StartedAt: stored.StartedAt, CompletedAt: stored.CompletedAt,
			DurationMS: stored.DurationMS, ExitStatus: stored.ExitStatus, FailureCategory: cloneString(stored.FailureCategory),
			RawUsage: cloneTokenMeters(stored.RawUsage), DeltaUsage: cloneTokenMeters(stored.DeltaUsage), ReportedCostUSD: cloneFloat64(stored.ReportedCostUSD),
			Costs: stored.Costs, Activity: stored.Activity,
		})
	}
	return audit
}

func decodeMetricReceipt(record *db.RunMetricReceipt) (*MetricReceipt, error) {
	if record == nil {
		return nil, nil
	}
	var receipt MetricReceipt
	if err := json.Unmarshal([]byte(record.PayloadJSON), &receipt); err != nil {
		return nil, fmt.Errorf("decode run metric receipt: %w", err)
	}
	if receipt.SchemaVersion != MetricReceiptSchemaVersion || receipt.Run.ID != record.RunID || receipt.Run.RepoID != record.RepoID || receipt.Run.Status != record.RunStatus || receipt.Run.CreatedAt != record.RunCreatedAt {
		return nil, fmt.Errorf("run metric receipt %q identity mismatch", record.RunID)
	}
	receipt.Steps = nonNilMetricSteps(receipt.Steps)
	receipt.Invocations = nonNilMetricInvocations(receipt.Invocations)
	return &receipt, nil
}

func buildReceiptAgentAggregates(invocations []db.AgentInvocation) []db.AgentInvocationAggregate {
	type accumulator struct {
		value    db.AgentInvocationAggregate
		complete map[string]bool
	}
	byPurpose := map[string]*accumulator{}
	for _, invocation := range invocations {
		item := byPurpose[invocation.Purpose]
		if item == nil {
			item = &accumulator{value: db.AgentInvocationAggregate{Purpose: invocation.Purpose}, complete: map[string]bool{
				"subprocess": true, "cache_write": true, "fresh_input": true, "reasoning": true, "roundtrips": true,
				"tools": true, "wait": true, "test_lint": true, "edit": true, "read": true, "git": true, "other": true,
			}}
			byPurpose[invocation.Purpose] = item
		}
		a := &item.value
		a.Count++
		a.TotalDurationMS += invocation.DurationMS
		switch invocation.SessionMode {
		case db.InvocationModeCold:
			a.Cold++
		case db.InvocationModeStarted:
			a.Started++
		case db.InvocationModeResumed:
			a.Resumed++
		case db.InvocationModeFallback:
			a.Fallback++
		}
		if invocation.ExitStatus != "ok" {
			a.Errors++
		}
		a.InputTokens += int64(invocation.InputTokens)
		a.OutputTokens += int64(invocation.OutputTokens)
		a.CacheReadTokens += int64(invocation.CacheReadTokens)
		mergeAggregateInt64(&a.SubprocessWaitMS, invocation.SubprocessWaitMS, item.complete, "subprocess")
		mergeAggregateInt(&a.CacheCreationTokens, invocation.CacheCreationTokens, item.complete, "cache_write")
		mergeAggregateInt(&a.FreshInputTokens, invocation.FreshInputTokens, item.complete, "fresh_input")
		mergeAggregateInt(&a.ReasoningTokens, invocation.ReasoningTokens, item.complete, "reasoning")
		mergeAggregateInt(&a.ModelRoundtrips, invocation.ModelRoundtrips, item.complete, "roundtrips")
		mergeAggregateInt(&a.ToolCalls, invocation.ToolCalls, item.complete, "tools")
		mergeAggregateInt(&a.ToolWaitCalls, invocation.ToolWaitCalls, item.complete, "wait")
		mergeAggregateInt(&a.ToolTestLintCalls, invocation.ToolTestLintCalls, item.complete, "test_lint")
		mergeAggregateInt(&a.ToolEditCalls, invocation.ToolEditCalls, item.complete, "edit")
		mergeAggregateInt(&a.ToolReadCalls, invocation.ToolReadCalls, item.complete, "read")
		mergeAggregateInt(&a.ToolGitCalls, invocation.ToolGitCalls, item.complete, "git")
		mergeAggregateInt(&a.ToolOtherCalls, invocation.ToolOtherCalls, item.complete, "other")
		if invocation.ModelRoundtrips != nil {
			a.MetricsRows++
		}
	}
	result := make([]db.AgentInvocationAggregate, 0, len(byPurpose))
	for _, item := range byPurpose {
		if item.value.Count > 0 {
			item.value.AvgDurationMS = item.value.TotalDurationMS / int64(item.value.Count)
		}
		result = append(result, item.value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TotalDurationMS != result[j].TotalDurationMS {
			return result[i].TotalDurationMS > result[j].TotalDurationMS
		}
		return result[i].Purpose < result[j].Purpose
	})
	return result
}

func mergeAggregateInt64(total **int64, value *int64, complete map[string]bool, key string) {
	if value == nil {
		complete[key] = false
		*total = nil
		return
	}
	if !complete[key] {
		return
	}
	if *total == nil {
		zero := int64(0)
		*total = &zero
	}
	**total += *value
}

func mergeAggregateInt(total **int64, value *int, complete map[string]bool, key string) {
	if value == nil {
		complete[key] = false
		*total = nil
		return
	}
	converted := int64(*value)
	mergeAggregateInt64(total, &converted, complete, key)
}

func cloneTokenMeters(value TokenMeters) TokenMeters {
	return TokenMeters{
		InputTokens: cloneInt(value.InputTokens), OutputTokens: cloneInt(value.OutputTokens), CacheReadTokens: cloneInt(value.CacheReadTokens),
		CacheWriteTokens: cloneInt(value.CacheWriteTokens), FreshInputTokens: cloneInt(value.FreshInputTokens), ReasoningTokens: cloneInt(value.ReasoningTokens),
	}
}

func cloneCommandReceipts(values []CommandReceipt) []CommandReceipt {
	result := make([]CommandReceipt, 0, len(values))
	for _, value := range values {
		result = append(result, CommandReceipt{
			Round: value.Round, Sequence: value.Sequence, Outcome: value.Outcome,
			ExitCode: cloneInt(value.ExitCode), CommandSource: value.CommandSource,
			Runner: cloneRunnerProvenance(value.Runner),
		})
	}
	return result
}

func cloneRounds(values []Round) []Round {
	result := make([]Round, 0, len(values))
	for _, value := range values {
		result = append(result, Round{
			Number: value.Number, Trigger: value.Trigger, SelectionSource: cloneString(value.SelectionSource),
			RepairFailureFingerprint: cloneString(value.RepairFailureFingerprint), RepairResult: cloneString(value.RepairResult),
			DurationMS: value.DurationMS, CreatedAt: value.CreatedAt,
		})
	}
	return result
}

func cloneSkipSource(value *types.SkipSource) *types.SkipSource {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func nonNilMetricSteps(values []MetricStep) []MetricStep {
	if values == nil {
		return []MetricStep{}
	}
	for index := range values {
		values[index].Commands = cloneCommandReceipts(values[index].Commands)
		values[index].Rounds = cloneRounds(values[index].Rounds)
	}
	return values
}

func nonNilMetricInvocations(values []MetricInvocation) []MetricInvocation {
	if values == nil {
		return []MetricInvocation{}
	}
	return values
}

func isTerminalStatus(status types.RunStatus) bool {
	switch status {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}
