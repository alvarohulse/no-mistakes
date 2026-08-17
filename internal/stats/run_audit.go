package stats

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pricing"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const SchemaVersion = 2

type RunAudit struct {
	SchemaVersion   int           `json:"schema_version"`
	Run             RunIdentity   `json:"run"`
	Steps           []Step        `json:"steps"`
	SkipReceipts    []SkipReceipt `json:"skip_receipts"`
	Invocations     []Invocation  `json:"invocations"`
	Metrics         Metrics       `json:"metrics"`
	Costs           CostTotals    `json:"costs"`
	IntegrityErrors []string      `json:"integrity_errors"`
}

type RunIdentity struct {
	ID                 string                `json:"id"`
	RepoID             string                `json:"repo_id"`
	Branch             string                `json:"branch"`
	HeadSHA            string                `json:"head_sha"`
	BaseSHA            string                `json:"base_sha"`
	RefreshStrategy    types.RefreshStrategy `json:"refresh_strategy"`
	Status             types.RunStatus       `json:"status"`
	CreatedAt          int64                 `json:"created_at"`
	UpdatedAt          int64                 `json:"updated_at"`
	ParkedMS           *int64                `json:"parked_ms"`
	NoMistakesVersion  *string               `json:"no_mistakes_version"`
	NoMistakesBuildSHA *string               `json:"no_mistakes_build_sha"`
	PolicyDigest       *string               `json:"policy_digest"`
	ConfigSources      []ConfigDigest        `json:"config_sources"`
}

type ConfigDigest struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}

type Step struct {
	ID          string            `json:"id"`
	Name        types.StepName    `json:"name"`
	Order       int               `json:"order"`
	Status      types.StepStatus  `json:"status"`
	SkipSource  *types.SkipSource `json:"skip_source"`
	ExitCode    *int              `json:"exit_code"`
	DurationMS  *int64            `json:"duration_ms"`
	StartedAt   *int64            `json:"started_at"`
	CompletedAt *int64            `json:"completed_at"`
	Rounds      []Round           `json:"rounds"`
}

type Round struct {
	Number          int     `json:"number"`
	Trigger         string  `json:"trigger"`
	SelectionSource *string `json:"selection_source"`
	DurationMS      int64   `json:"duration_ms"`
	CreatedAt       int64   `json:"created_at"`
}

type SkipReceipt struct {
	Step   types.StepName   `json:"step"`
	Source types.SkipSource `json:"source"`
}

type Invocation struct {
	ID                   string                    `json:"id"`
	Step                 types.StepName            `json:"step"`
	Round                int                       `json:"round"`
	Purpose              string                    `json:"purpose"`
	Agent                string                    `json:"agent"`
	InvocationMode       types.AgentInvocationMode `json:"invocation_mode"`
	NestedAgentsReported bool                      `json:"nested_agents_reported"`
	NestedAgents         []types.AgentObservation  `json:"nested_agents"`
	Model                *string                   `json:"model"`
	Provider             *string                   `json:"provider"`
	Review               *ReviewReceipt            `json:"review"`
	SessionMode          string                    `json:"session_mode"`
	SessionKey           string                    `json:"session_key"`
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

type TokenMeters struct {
	InputTokens      *int `json:"input_tokens"`
	OutputTokens     *int `json:"output_tokens"`
	CacheReadTokens  *int `json:"cache_read_tokens"`
	CacheWriteTokens *int `json:"cache_write_tokens"`
	FreshInputTokens *int `json:"fresh_input_tokens"`
	ReasoningTokens  *int `json:"reasoning_tokens"`
}

type Activity struct {
	SubprocessWaitMS  *int64 `json:"subprocess_wait_ms"`
	ModelRoundtrips   *int   `json:"model_roundtrips"`
	ToolCalls         *int   `json:"tool_calls"`
	ToolWaitCalls     *int   `json:"tool_wait_calls"`
	ToolTestLintCalls *int   `json:"tool_test_lint_calls"`
	ToolEditCalls     *int   `json:"tool_edit_calls"`
	ToolReadCalls     *int   `json:"tool_read_calls"`
	ToolGitCalls      *int   `json:"tool_git_calls"`
	ToolOtherCalls    *int   `json:"tool_other_calls"`
	WorkloadFiles     *int   `json:"workload_files"`
	WorkloadLines     *int   `json:"workload_lines"`
	FindingCount      *int   `json:"finding_count"`
}

type ReviewReceipt struct {
	CandidatePool []ReviewCandidate `json:"candidate_pool"`
	Selected      Route             `json:"selected"`
}

type ReviewCandidate struct {
	Agent    string `json:"agent"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Optional bool   `json:"optional"`
}

type Route struct {
	Agent    string  `json:"agent"`
	Model    *string `json:"model"`
	Provider *string `json:"provider"`
}

type Coverage struct {
	Reported int `json:"reported"`
	Total    int `json:"total"`
}

type IntMetric struct {
	Value          *int64   `json:"value"`
	Coverage       Coverage `json:"coverage"`
	IntegrityError *string  `json:"integrity_error"`
}

type FloatMetric struct {
	Value          *float64 `json:"value"`
	Coverage       Coverage `json:"coverage"`
	IntegrityError *string  `json:"integrity_error"`
}

type Metrics struct {
	InvocationCount       int         `json:"invocation_count"`
	DeltaInputTokens      IntMetric   `json:"delta_input_tokens"`
	DeltaOutputTokens     IntMetric   `json:"delta_output_tokens"`
	DeltaCacheReadTokens  IntMetric   `json:"delta_cache_read_tokens"`
	DeltaCacheWriteTokens IntMetric   `json:"delta_cache_write_tokens"`
	ReportedCostUSD       FloatMetric `json:"reported_cost_usd"`
}

func (a RunAudit) CanonicalJSON() (string, error) {
	encoded, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("encode run audit: %w", err)
	}
	return string(encoded), nil
}

func BuildRunAudit(database *db.DB, runID string) (*RunAudit, error) {
	if database == nil {
		return nil, fmt.Errorf("build run audit: database is nil")
	}
	run, err := database.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %q not found", runID)
	}
	parkedMS, err := database.GetRunParkedMS(runID)
	if err != nil {
		return nil, err
	}
	rows, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get run steps: %w", err)
	}
	invocationRows, err := database.GetAgentInvocationsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("get agent invocations: %w", err)
	}
	databaseTotals, err := database.GetAgentInvocationAuditTotals(runID)
	if err != nil {
		return nil, err
	}
	estimator, err := pricing.DefaultEstimator()
	if err != nil {
		return nil, fmt.Errorf("load pricing estimator: %w", err)
	}

	configSources, configErrors := configDigests(run.ConfigSources)
	audit := &RunAudit{
		SchemaVersion: SchemaVersion,
		Run: RunIdentity{
			ID: run.ID, RepoID: run.RepoID, Branch: run.Branch, HeadSHA: run.HeadSHA, BaseSHA: run.BaseSHA,
			RefreshStrategy: run.RefreshStrategy.OrDefault(), Status: run.Status,
			CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt, ParkedMS: cloneInt64(parkedMS),
			NoMistakesVersion: cloneString(run.NoMistakesVersion), NoMistakesBuildSHA: cloneString(run.NoMistakesBuildSHA),
			PolicyDigest: nonEmptyString(run.ResolvedPolicyDigest), ConfigSources: configSources,
		},
		Steps:           make([]Step, 0, len(rows)),
		SkipReceipts:    []SkipReceipt{},
		Invocations:     make([]Invocation, 0, len(invocationRows)),
		IntegrityErrors: append([]string{}, configErrors...),
	}

	policy, policyResolved, policyErrors := resolvedPolicyFacts(run.ResolvedPolicy, run.ResolvedPolicyDigest)
	audit.IntegrityErrors = append(audit.IntegrityErrors, policyErrors...)
	receiptSources := make(map[types.StepName]types.SkipSource, len(policy.SkipSources))
	for step, source := range policy.SkipSources {
		receiptSources[step] = source
	}
	for _, row := range rows {
		policySource, policySkipped := policy.SkipSources[row.StepName.Canonical()]
		step, errors := buildStep(database, row, policySource, policySkipped, policyResolved)
		audit.Steps = append(audit.Steps, step)
		audit.IntegrityErrors = append(audit.IntegrityErrors, errors...)
		if step.SkipSource != nil {
			receiptSources[step.Name.Canonical()] = *step.SkipSource
		}
	}
	audit.SkipReceipts = orderedSkipReceipts(receiptSources, policy.StepOrder)
	if policyResolved {
		audit.IntegrityErrors = append(audit.IntegrityErrors, reconcilePolicySteps(run.Status, policy.Steps, audit.Steps)...)
	}

	for _, row := range invocationRows {
		requireManagedReviewReceipt := policyResolved && policy.ManagedReviewReceipts
		invocation, errors := buildInvocation(row, estimator, policy.PricingProfiles[row.Agent], requireManagedReviewReceipt, policy.ReviewCandidates)
		audit.Invocations = append(audit.Invocations, invocation)
		audit.IntegrityErrors = append(audit.IntegrityErrors, errors...)
	}
	audit.Metrics, policyErrors = buildMetrics(audit.Invocations, databaseTotals)
	audit.Costs = buildCostTotals(audit.Invocations)
	audit.IntegrityErrors = append(audit.IntegrityErrors, policyErrors...)
	return audit, nil
}

func configDigests(sources []db.ConfigSource) ([]ConfigDigest, []string) {
	result := make([]ConfigDigest, 0, len(sources))
	var integrityErrors []string
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		result = append(result, ConfigDigest{Kind: source.Kind, Digest: source.Digest})
		decoded, err := hex.DecodeString(source.Digest)
		if strings.TrimSpace(source.Kind) == "" || err != nil || len(decoded) != sha256.Size {
			integrityErrors = append(integrityErrors, fmt.Sprintf("config source %q has an invalid digest", source.Kind))
		}
		if seen[source.Kind] {
			integrityErrors = append(integrityErrors, fmt.Sprintf("config source %q is duplicated", source.Kind))
		}
		seen[source.Kind] = true
	}
	return result, integrityErrors
}

func buildStep(database *db.DB, row *db.StepResult, policySource types.SkipSource, policySkipped, policyResolved bool) (Step, []string) {
	result := Step{
		ID: row.ID, Name: row.StepName.Canonical(), Order: row.StepOrder, Status: row.Status,
		ExitCode: cloneInt(row.ExitCode), DurationMS: cloneInt64(row.DurationMS),
		StartedAt: cloneInt64(row.StartedAt), CompletedAt: cloneInt64(row.CompletedAt),
		Rounds: []Round{},
	}
	rounds, err := database.GetRoundsByStep(row.ID)
	if err != nil {
		return result, []string{fmt.Sprintf("step %s rounds could not be read: %v", row.StepName, err)}
	}
	result.Rounds = make([]Round, 0, len(rounds))
	var integrityErrors []string
	for _, round := range rounds {
		result.Rounds = append(result.Rounds, Round{Number: round.Round, Trigger: round.Trigger, SelectionSource: cloneString(round.SelectionSource), DurationMS: round.DurationMS, CreatedAt: round.CreatedAt})
	}
	if row.SkipSource != nil {
		source := types.SkipSource(*row.SkipSource)
		if !source.Valid() {
			integrityErrors = append(integrityErrors, fmt.Sprintf("step %s has unsupported skip source %q", row.StepName, source))
		} else if row.Status != types.StepStatusSkipped {
			integrityErrors = append(integrityErrors, fmt.Sprintf("step %s has a skip source but status %s", row.StepName, row.Status))
		} else {
			result.SkipSource = &source
		}
	}
	if policyResolved && policySkipped {
		if row.Status != types.StepStatusSkipped {
			integrityErrors = append(integrityErrors, fmt.Sprintf("step %s is skipped in resolved policy but has status %s", row.StepName, row.Status))
		} else if result.SkipSource != nil && *result.SkipSource != policySource {
			integrityErrors = append(integrityErrors, fmt.Sprintf("step %s skip source differs from resolved policy", row.StepName))
		} else {
			source := policySource
			result.SkipSource = &source
		}
	} else if policyResolved && result.SkipSource != nil {
		integrityErrors = append(integrityErrors, fmt.Sprintf("step %s is skipped in stored results but not in resolved policy", row.StepName))
	}
	return result, integrityErrors
}

func buildInvocation(row db.AgentInvocation, estimator *pricing.Estimator, pricingProfileID string, requireManagedReviewReceipt bool, expectedReviewPool []ReviewCandidate) (Invocation, []string) {
	result := Invocation{
		ID: row.ID, Step: types.StepName(row.StepName).Canonical(), Round: row.Round, Purpose: row.Purpose, Agent: row.Agent,
		InvocationMode: row.InvocationMode, NestedAgentsReported: row.AgentObservationsReported,
		NestedAgents: append([]types.AgentObservation(nil), row.AgentObservations...),
		Model:        nonEmptyValue(row.Model), Provider: cloneString(row.ModelProvider),
		SessionMode: row.SessionMode, SessionKey: row.SessionKey, FallbackReason: cloneString(row.FallbackReason),
		StartedAt: row.StartedAt, CompletedAt: row.CompletedAt, DurationMS: row.DurationMS, ExitStatus: row.ExitStatus,
		FailureCategory: nonEmptyValue(row.FailureCategory), ReportedCostUSD: cloneFloat64(row.ReportedCostUSD),
		DeltaUsage: TokenMeters{
			InputTokens: cloneInt(row.DeltaInputTokens), OutputTokens: cloneInt(row.DeltaOutputTokens),
			CacheReadTokens: cloneInt(row.DeltaCacheReadTokens), CacheWriteTokens: cloneInt(row.DeltaCacheCreationTokens),
		},
		Activity: Activity{
			SubprocessWaitMS: cloneInt64(row.SubprocessWaitMS), ModelRoundtrips: cloneInt(row.ModelRoundtrips),
			ToolCalls: cloneInt(row.ToolCalls), ToolWaitCalls: cloneInt(row.ToolWaitCalls), ToolTestLintCalls: cloneInt(row.ToolTestLintCalls),
			ToolEditCalls: cloneInt(row.ToolEditCalls), ToolReadCalls: cloneInt(row.ToolReadCalls), ToolGitCalls: cloneInt(row.ToolGitCalls),
			ToolOtherCalls: cloneInt(row.ToolOtherCalls), WorkloadFiles: cloneInt(row.WorkloadFiles), WorkloadLines: cloneInt(row.WorkloadLines),
			FindingCount: cloneInt(row.FindingCount),
		},
	}
	result.NestedAgents = nonNilObservations(result.NestedAgents)
	if row.DeltaInputTokens != nil {
		result.RawUsage.InputTokens = intValue(row.InputTokens)
	}
	if row.DeltaOutputTokens != nil {
		result.RawUsage.OutputTokens = intValue(row.OutputTokens)
	}
	if row.DeltaCacheReadTokens != nil {
		result.RawUsage.CacheReadTokens = intValue(row.CacheReadTokens)
	}
	result.RawUsage.CacheWriteTokens = cloneInt(row.CacheCreationTokens)
	result.RawUsage.FreshInputTokens = cloneInt(row.FreshInputTokens)
	result.RawUsage.ReasoningTokens = cloneInt(row.ReasoningTokens)
	_, uncachedInput := agent.CanonicalInputMeters(row.Agent, row.DeltaInputTokens, row.DeltaCacheReadTokens, row.DeltaCacheCreationTokens)
	provider := ""
	if row.ModelProvider != nil {
		provider = *row.ModelProvider
	}
	result.Costs = estimator.Estimate(pricing.Observation{
		Harness: row.Agent, ProfileID: pricingProfileID, Provider: provider, Model: row.Model,
		StartedAt: time.Unix(row.StartedAt, 0).UTC(), ReportedCostUSD: row.ReportedCostUSD,
		Meters: pricing.TokenMeters{
			UncachedInputTokens: int64FromInt(uncachedInput),
			CacheReadTokens:     int64FromInt(row.DeltaCacheReadTokens),
			CacheWriteTokens:    int64FromInt(row.DeltaCacheCreationTokens),
			OutputTokens:        int64FromInt(row.DeltaOutputTokens),
		},
	})
	if row.ReviewCandidatePool != nil {
		candidates := make([]ReviewCandidate, 0, len(row.ReviewCandidatePool))
		for _, candidate := range row.ReviewCandidatePool {
			candidates = append(candidates, ReviewCandidate{Agent: candidate.Agent, Model: candidate.Model, Provider: candidate.Vendor, Optional: candidate.Optional})
		}
		result.Review = &ReviewReceipt{
			CandidatePool: candidates,
			Selected:      Route{Agent: row.Agent, Model: nonEmptyValue(row.Model), Provider: cloneString(row.ModelProvider)},
		}
	}
	return result, reviewReceiptErrors(result, requireManagedReviewReceipt, expectedReviewPool)
}

func int64FromInt(value *int) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}

func reviewReceiptErrors(invocation Invocation, requireManagedReviewReceipt bool, expectedReviewPool []ReviewCandidate) []string {
	if invocation.Review == nil {
		if requireManagedReviewReceipt && invocation.Purpose == "review" {
			return []string{fmt.Sprintf("managed review invocation %s has no candidate-pool receipt", invocation.ID)}
		}
		return nil
	}
	if requireManagedReviewReceipt && !sameReviewPool(invocation.Review.CandidatePool, expectedReviewPool) {
		return []string{fmt.Sprintf("managed review invocation %s candidate pool differs from resolved policy", invocation.ID)}
	}
	selectedModel := stringOrEmpty(invocation.Review.Selected.Model)
	selectedProvider := stringOrEmpty(invocation.Review.Selected.Provider)
	for _, candidate := range invocation.Review.CandidatePool {
		if candidate.Agent == invocation.Review.Selected.Agent && candidate.Model == selectedModel && candidate.Provider == selectedProvider {
			return nil
		}
	}
	return []string{fmt.Sprintf("review invocation %s selected route is absent from its candidate pool", invocation.ID)}
}

func sameReviewPool(actual, expected []ReviewCandidate) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return false
		}
	}
	return true
}

func nonNilObservations(values []types.AgentObservation) []types.AgentObservation {
	if values == nil {
		return []types.AgentObservation{}
	}
	return values
}

func nonEmptyString(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return cloneString(value)
}

func nonEmptyValue(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return stringValue(value)
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	return stringValue(*value)
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	return intValue(*value)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneFloat64(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func stringValue(value string) *string { return &value }
func intValue(value int) *int          { return &value }

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
