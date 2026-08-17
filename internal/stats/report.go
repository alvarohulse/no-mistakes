package stats

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pricing"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const ReportSchemaVersion = 2

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
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Scope         ReportScope   `json:"scope"`
	Runs          ReportRuns    `json:"runs"`
	Repairs       []Repair      `json:"repairs"`
	Steps         []ReportStep  `json:"steps"`
	Agents        []ReportAgent `json:"agents"`
	Costs         ReportCosts   `json:"costs"`
	DataErrors    []DataError   `json:"data_errors"`
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

type Repair struct {
	RunID           string         `json:"run_id"`
	StepID          string         `json:"step_id"`
	Step            types.StepName `json:"step"`
	Round           int            `json:"round"`
	Trigger         string         `json:"trigger"`
	SelectionSource *string        `json:"selection_source"`
	DurationMS      int64          `json:"duration_ms"`
	CreatedAt       int64          `json:"created_at"`
}

type ReportAgent struct {
	RunID      string     `json:"run_id"`
	Invocation Invocation `json:"invocation"`
}

type ReportCosts struct {
	Totals CostTotals   `json:"totals"`
	Items  []CostRecord `json:"items"`
}

type CostRecord struct {
	RunID        string              `json:"run_id"`
	InvocationID string              `json:"invocation_id"`
	Classes      pricing.CostClasses `json:"classes"`
}

type DataError struct {
	RunID  string `json:"run_id"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
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
		Repairs:       []Repair{}, Steps: []ReportStep{}, Agents: []ReportAgent{},
		Costs: ReportCosts{Items: []CostRecord{}}, DataErrors: []DataError{},
	}

	repos, err := database.GetRepos()
	if err != nil {
		return nil, fmt.Errorf("list repositories for stats: %w", err)
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
		for _, step := range steps {
			report.Steps = append(report.Steps, ReportStep{RunID: audit.Run.ID, Step: step})
			for _, round := range step.Rounds {
				if round.Trigger != "auto_fix" && round.Trigger != "user_fix" {
					continue
				}
				report.Repairs = append(report.Repairs, Repair{
					RunID: audit.Run.ID, StepID: step.ID, Step: step.Name, Round: round.Number, Trigger: round.Trigger,
					SelectionSource: round.SelectionSource, DurationMS: round.DurationMS, CreatedAt: round.CreatedAt,
				})
			}
		}
		for _, invocation := range invocations {
			report.Agents = append(report.Agents, ReportAgent{RunID: audit.Run.ID, Invocation: invocation})
			report.Costs.Items = append(report.Costs.Items, CostRecord{RunID: audit.Run.ID, InvocationID: invocation.ID, Classes: invocation.Costs})
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
	report.Costs.Totals = buildCostTotals(selectedInvocations)
	return report, nil
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
	return nil
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
