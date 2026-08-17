package cli

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	runstats "github.com/kunchenguid/no-mistakes/internal/stats"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

const (
	statsBoxWidth     = 61
	statsContentWidth = statsBoxWidth - 4
	statsBarWidth     = 30
	statsRepoBarWidth = 10
)

func newStatsCmd() *cobra.Command {
	var agents bool
	var runID string
	var format string
	var repoSelectors []string
	var currentRepo bool
	var sinceValue string
	var untilValue string
	var stepValues []string
	var agentValues []string
	var modelValues []string
	var purposeValues []string
	var statusValues []string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show historical no-mistakes usage stats",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("stats", func() error {
				now := time.Now().UTC()
				format = strings.ToLower(strings.TrimSpace(format))
				if format == "" {
					format = "text"
				}
				if format != "text" && format != "json" && format != "csv" {
					return fmt.Errorf("unsupported stats format %q (expected text, json, or csv)", format)
				}
				_, database, err := openResources()
				if err != nil {
					return err
				}
				defer database.Close()
				if currentRepo && len(repoSelectors) > 0 {
					return fmt.Errorf("stats --current-repo cannot be combined with --repo")
				}
				if runID != "" && (currentRepo || len(repoSelectors) > 0 || strings.TrimSpace(sinceValue) != "" || strings.TrimSpace(untilValue) != "" || len(statusValues) > 0) {
					return fmt.Errorf("stats --run cannot be combined with repository, time, or status selectors")
				}
				repoIDs, err := resolveStatsRepoIDs(database, repoSelectors, currentRepo)
				if err != nil {
					return err
				}
				since, err := parseStatsSince(sinceValue, now)
				if err != nil {
					return err
				}
				until, err := parseStatsUntil(untilValue)
				if err != nil {
					return err
				}
				steps, err := parseStatsSteps(stepValues)
				if err != nil {
					return err
				}
				statuses, err := parseStatsStatuses(statusValues)
				if err != nil {
					return err
				}
				query := runstats.Query{
					RunID: runID, RepoIDs: repoIDs, Since: since, Until: until, Steps: steps,
					Agents: agentValues, Models: modelValues, Purposes: purposeValues, Statuses: statuses,
				}
				report, err := runstats.BuildReport(database, query, now)
				if err != nil {
					return err
				}
				switch format {
				case "json":
					encoded, err := report.CanonicalJSON()
					if err != nil {
						return err
					}
					fmt.Fprintln(cmd.OutOrStdout(), encoded)
				case "csv":
					encoded, err := runstats.RenderCSV(report)
					if err != nil {
						return err
					}
					fmt.Fprint(cmd.OutOrStdout(), encoded)
				default:
					if agents || report.Scope.RunID != "" {
						fmt.Fprint(cmd.OutOrStdout(), runstats.RenderDetailedText(report))
					} else {
						fmt.Fprintln(cmd.OutOrStdout(), renderStatsDashboard(reportDashboardStats(report.Dashboard)))
						if reportScopeIsUnfiltered(report.Scope) && len(report.DataErrors) > 0 {
							fmt.Fprintf(cmd.OutOrStdout(), "data errors: %d (use --format json or csv for complete details)\n", len(report.DataErrors))
						} else if !reportScopeIsUnfiltered(report.Scope) {
							fmt.Fprintln(cmd.OutOrStdout())
							fmt.Fprint(cmd.OutOrStdout(), runstats.RenderText(report))
						}
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&agents, "agents", false, "show per-purpose agent aggregates followed by invocation details")
	cmd.Flags().StringVar(&runID, "run", "", "show one run's steps, agent receipts, and parked time (implies --agents)")
	cmd.Flags().StringArrayVar(&repoSelectors, "repo", nil, "select an exact repository ID or registered path (repeatable)")
	cmd.Flags().BoolVar(&currentRepo, "current-repo", false, "select the registered repository containing the current directory")
	cmd.Flags().StringVar(&sinceValue, "since", "", "include runs created on or after an RFC3339 time or duration ago")
	cmd.Flags().StringVar(&untilValue, "until", "", "exclude runs created on or after an RFC3339 time")
	cmd.Flags().StringArrayVar(&stepValues, "step", nil, "select a pipeline step (repeatable)")
	cmd.Flags().StringArrayVar(&agentValues, "agent", nil, "select an agent harness (repeatable)")
	cmd.Flags().StringArrayVar(&modelValues, "model", nil, "select an exact model identity (repeatable)")
	cmd.Flags().StringArrayVar(&purposeValues, "purpose", nil, "select an invocation purpose (repeatable)")
	cmd.Flags().StringArrayVar(&statusValues, "status", nil, "select a run status (repeatable)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text, json, or csv")
	return cmd
}

func reportScopeIsUnfiltered(scope runstats.ReportScope) bool {
	return scope.RunID == "" && len(scope.RepoIDs) == 0 && scope.Since == nil && scope.Until == nil && len(scope.Steps) == 0 &&
		len(scope.Agents) == 0 && len(scope.Models) == 0 && len(scope.Purposes) == 0 && len(scope.Statuses) == 0
}

func reportDashboardStats(dashboard runstats.Dashboard) *db.Stats {
	result := &db.Stats{
		TotalRepos: dashboard.TotalRepos, TotalRuns: dashboard.TotalRuns, RescueRuns: dashboard.RescueRuns,
		ReportedFindings: dashboard.ReportedFindings, FixedFindings: dashboard.FixedFindings,
		StepStats: []db.StepStats{}, RepoStats: []db.RepoStats{},
	}
	for _, step := range dashboard.Steps {
		result.StepStats = append(result.StepStats, db.StepStats{StepName: step.Step, ReportedFindings: step.ReportedFindings, FixedFindings: step.FixedFindings})
	}
	for _, repo := range dashboard.Repositories {
		result.RepoStats = append(result.RepoStats, db.RepoStats{
			RepoID: repo.RepoID, WorkingPath: repo.DisplayName, Runs: repo.Runs, RescueRuns: repo.RescueRuns,
			ReportedFindings: repo.ReportedFindings, FixedFindings: repo.FixedFindings,
		})
	}
	return result
}

func resolveStatsRepoIDs(database *db.DB, selectors []string, current bool) ([]string, error) {
	if current {
		repo, err := findRepo(database)
		if err != nil {
			return nil, err
		}
		return []string{repo.ID}, nil
	}
	result := make([]string, 0, len(selectors))
	seen := make(map[string]bool, len(selectors))
	for _, selector := range selectors {
		selector = strings.TrimSpace(selector)
		if selector == "" {
			return nil, fmt.Errorf("stats --repo cannot be empty")
		}
		repo, err := database.GetRepo(selector)
		if err != nil {
			return nil, err
		}
		if repo == nil {
			repo, err = database.GetRepoByPath(selector)
			if err != nil {
				return nil, err
			}
		}
		if repo == nil {
			archived, err := database.HasRunMetricReceiptsForRepo(selector)
			if err != nil {
				return nil, err
			}
			if !archived {
				return nil, fmt.Errorf("repository %q is not registered and has no archived metrics", selector)
			}
			if !seen[selector] {
				seen[selector] = true
				result = append(result, selector)
			}
			continue
		}
		if !seen[repo.ID] {
			seen[repo.ID] = true
			result = append(result, repo.ID)
		}
	}
	return result, nil
}

func parseStatsSince(value string, now time.Time) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration <= 0 {
			return nil, fmt.Errorf("stats --since duration must be positive")
		}
		boundary := now.Add(-duration)
		return &boundary, nil
	}
	boundary, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("parse stats --since %q as duration or RFC3339: %w", value, err)
	}
	boundary = boundary.UTC()
	return &boundary, nil
}

func parseStatsUntil(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	boundary, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("parse stats --until %q as RFC3339: %w", value, err)
	}
	boundary = boundary.UTC()
	return &boundary, nil
}

func parseStatsSteps(values []string) ([]types.StepName, error) {
	result := make([]types.StepName, 0, len(values))
	for _, value := range values {
		step := types.StepName(strings.ToLower(strings.TrimSpace(value))).Canonical()
		if step.Order() == 0 {
			return nil, fmt.Errorf("unsupported stats step %q", value)
		}
		result = append(result, step)
	}
	return result, nil
}

func parseStatsStatuses(values []string) ([]types.RunStatus, error) {
	result := make([]types.RunStatus, 0, len(values))
	for _, value := range values {
		status := types.RunStatus(strings.ToLower(strings.TrimSpace(value)))
		switch status {
		case types.RunPending, types.RunRunning, types.RunCompleted, types.RunFailed, types.RunCancelled:
			result = append(result, status)
		default:
			return nil, fmt.Errorf("unsupported stats run status %q", value)
		}
	}
	return result, nil
}

// renderAgentPerfReport prints the local performance telemetry: per-purpose
// invocation aggregates, or one run's per-invocation detail with its
// accumulated parked-at-gate time. This is read-only local evidence; none of
// it is sent to remote analytics.
func renderAgentPerfReport(w io.Writer, database *db.DB, runID string) error {
	if runID != "" {
		return renderRunAgentPerf(w, database, runID)
	}

	aggregates, err := database.AgentInvocationAggregates()
	if err != nil {
		return fmt.Errorf("agent invocation aggregates: %w", err)
	}
	if len(aggregates) == 0 {
		fmt.Fprintln(w, "no agent invocations recorded yet")
		return nil
	}

	// Table 1: session modes and token totals.
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PURPOSE\tCOUNT\tAVG\tTOTAL\tCOLD\tSTARTED\tRESUMED\tFALLBACK\tERRORS\tIN TOK\tOUT TOK\tCACHE READ TOK\tCACHE WRITE TOK\tFRESH IN TOK\tREASON TOK")
	for _, a := range aggregates {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
			a.Purpose, a.Count,
			formatMS(a.AvgDurationMS), formatMS(a.TotalDurationMS),
			a.Cold, a.Started, a.Resumed, a.Fallback, a.Errors,
			a.InputTokens, a.OutputTokens, a.CacheReadTokens, optInt64(a.CacheCreationTokens),
			optInt64(a.FreshInputTokens), optInt64(a.ReasoningTokens),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Table 2: subprocess-vs-model time and the bounded tool-call histogram.
	// METRICS is how many of COUNT rows carried activity metrics, so a zero can
	// be told apart from missing instrumentation (older rows, other adapters).
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PURPOSE\tMETRICS\tSUBPROC\tROUNDTRIPS\tTOOLS\tWAIT\tTEST/LINT\tEDIT\tREAD\tGIT\tOTHER")
	for _, a := range aggregates {
		metricsCov := fmt.Sprintf("%d/%d", a.MetricsRows, a.Count)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Purpose, metricsCov, optMS(a.SubprocessWaitMS),
			optInt64(a.ModelRoundtrips), optInt64(a.ToolCalls),
			optInt64(a.ToolWaitCalls), optInt64(a.ToolTestLintCalls), optInt64(a.ToolEditCalls), optInt64(a.ToolReadCalls), optInt64(a.ToolGitCalls), optInt64(a.ToolOtherCalls),
		)
	}
	return tw.Flush()
}

func renderRunAgentPerf(w io.Writer, database *db.DB, runID string) error {
	audit, err := runstats.BuildRunAudit(database, runID)
	if err != nil {
		return err
	}
	return renderRunAudit(w, audit)
}

func renderRunAudit(w io.Writer, audit *runstats.RunAudit) error {
	if audit == nil {
		return fmt.Errorf("run audit is nil")
	}
	fmt.Fprintf(w, "run %s (%s), parked at gates %s total\n", audit.Run.ID, audit.Run.Status, optMS(audit.Run.ParkedMS))
	fmt.Fprintf(w, "binary: %s (%s)\n", orUnknown(deref(audit.Run.NoMistakesVersion)), orUnknown(deref(audit.Run.NoMistakesBuildSHA)))
	fmt.Fprintf(w, "policy digest: %s\n", orUnknown(deref(audit.Run.PolicyDigest)))
	if len(audit.Run.ConfigSources) > 0 {
		sources := make([]string, 0, len(audit.Run.ConfigSources))
		for _, source := range audit.Run.ConfigSources {
			sources = append(sources, source.Kind+"@"+source.Digest)
		}
		fmt.Fprintf(w, "config sources: %s\n", strings.Join(sources, ", "))
	}
	if len(audit.Steps) > 0 {
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "STEP\tSTATUS\tSKIP SOURCE\tROUNDS\tDURATION")
		for _, step := range audit.Steps {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
				step.Name.DisplayName(audit.Run.RefreshStrategy), step.Status, optSkipSource(step.SkipSource), len(step.Rounds), optMS(step.DurationMS))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(audit.Invocations) == 0 {
		fmt.Fprintln(w, "no agent invocations recorded for this run")
		return renderAuditMetrics(w, audit.Metrics, audit.IntegrityErrors)
	}
	fmt.Fprintln(w, "\"-\" means the field was not reported for that invocation (unknown), which is distinct from a recorded 0.")

	// Table 1: session, timing split, activity, workload, and findings.
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tROUND\tPURPOSE\tAGENT\tINVOKED VIA\tNESTED AGENTS\tMODEL\tPROVIDER\tREVIEW ROUTE\tSESSION\tKEY\tDURATION\tMODEL TIME\tSUBPROC\tRT\tTOOLS (w/t/e/r/g/o)\tFIND\tWORK (f/l)\tFALLBACK\tEXIT")
	for _, inv := range audit.Invocations {
		exit := inv.ExitStatus
		if inv.FailureCategory != nil && *inv.FailureCategory != inv.ExitStatus {
			exit += "/" + *inv.FailureCategory
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			inv.Step.DisplayName(audit.Run.RefreshStrategy), inv.Round, inv.Purpose, inv.Agent, orUnknown(string(inv.InvocationMode)), formatAgentObservations(inv), orUnknown(deref(inv.Model)), orUnknown(deref(inv.Provider)), formatReviewReceipt(inv.Review),
			inv.SessionMode, inv.SessionKey,
			formatMS(inv.DurationMS), formatModelTime(inv), optMS(inv.Activity.SubprocessWaitMS),
			optInt(inv.Activity.ModelRoundtrips), formatToolHistogram(inv), optInt(inv.Activity.FindingCount),
			formatWorkload(inv), orUnknown(deref(inv.FallbackReason)), exit,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Table 2: per-round token deltas next to the raw (cumulative for resumed
	// sessions) counters, so a cumulative counter cannot be misread as per-round.
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STEP\tROUND\tPURPOSE\tSESSION\tΔ IN (round)\tΔ OUT\tΔ CACHE RD\tIN (raw)\tOUT (raw)\tCACHE RD (raw)\tCACHE WR\tFRESH IN\tREASON\tREPORTED COST")
	for _, inv := range audit.Invocations {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			inv.Step.DisplayName(audit.Run.RefreshStrategy), inv.Round, inv.Purpose, inv.SessionMode,
			optInt(inv.DeltaUsage.InputTokens), optInt(inv.DeltaUsage.OutputTokens), optInt(inv.DeltaUsage.CacheReadTokens),
			optInt(inv.RawUsage.InputTokens), optInt(inv.RawUsage.OutputTokens), optInt(inv.RawUsage.CacheReadTokens),
			optInt(inv.RawUsage.CacheWriteTokens), optInt(inv.RawUsage.FreshInputTokens), optInt(inv.RawUsage.ReasoningTokens), optFloat(inv.ReportedCostUSD),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return renderAuditMetrics(w, audit.Metrics, audit.IntegrityErrors)
}

func formatMS(ms int64) string {
	return time.Duration(ms * int64(time.Millisecond)).Round(100 * time.Millisecond).String()
}

// optInt renders a nullable count: "-" (unknown) when nil, else the number.
func optInt(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}

func optInt64(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

func optFloat(p *float64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

// optMS renders a nullable duration: "-" when nil, else a rounded duration.
func optMS(p *int64) string {
	if p == nil {
		return "-"
	}
	return formatMS(*p)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func orUnknown(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatModelTime shows model/reasoning wall-clock (duration minus subprocess
// wait). It is unknown when the invocation reported no subprocess-wait split.
func formatModelTime(inv runstats.Invocation) string {
	if inv.Activity.SubprocessWaitMS == nil {
		return "-"
	}
	return formatMS(agent.ModelTimeMS(inv.DurationMS, *inv.Activity.SubprocessWaitMS))
}

// formatToolHistogram renders "total w/t/e/r/g/o" or "-" when the invocation
// reported no activity metrics.
func formatToolHistogram(inv runstats.Invocation) string {
	if inv.Activity.ToolCalls == nil {
		return "-"
	}
	return fmt.Sprintf("%d %s/%s/%s/%s/%s/%s", *inv.Activity.ToolCalls,
		optInt(inv.Activity.ToolWaitCalls), optInt(inv.Activity.ToolTestLintCalls), optInt(inv.Activity.ToolEditCalls),
		optInt(inv.Activity.ToolReadCalls), optInt(inv.Activity.ToolGitCalls), optInt(inv.Activity.ToolOtherCalls))
}

// formatWorkload renders "files/lines" or "-" when unknown.
func formatWorkload(inv runstats.Invocation) string {
	if inv.Activity.WorkloadFiles == nil && inv.Activity.WorkloadLines == nil {
		return "-"
	}
	return fmt.Sprintf("%s/%s", optInt(inv.Activity.WorkloadFiles), optInt(inv.Activity.WorkloadLines))
}

func formatAgentObservations(inv runstats.Invocation) string {
	if !inv.NestedAgentsReported {
		return "-"
	}
	if len(inv.NestedAgents) == 0 {
		if inv.NestedAgentCount != nil && *inv.NestedAgentCount > 0 {
			return strconv.Itoa(*inv.NestedAgentCount)
		}
		return "none"
	}
	observations := make([]string, 0, len(inv.NestedAgents))
	for _, observation := range inv.NestedAgents {
		observations = append(observations, fmt.Sprintf("%s (%s)", observation.Identity, observation.InvocationMode))
	}
	return strings.Join(observations, ", ")
}

func optSkipSource(source *types.SkipSource) string {
	if source == nil {
		return "-"
	}
	return string(*source)
}

func formatReviewReceipt(receipt *runstats.ReviewReceipt) string {
	if receipt == nil {
		return "-"
	}
	return fmt.Sprintf("%d -> %s/%s", len(receipt.CandidatePool), receipt.Selected.Agent, orUnknown(deref(receipt.Selected.Model)))
}

func renderAuditMetrics(w io.Writer, metrics runstats.Metrics, integrityErrors []string) error {
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "METRIC\tVALUE\tCOVERAGE\tINTEGRITY")
	rows := []struct {
		name     string
		value    string
		coverage runstats.Coverage
		error    *string
	}{
		{name: "delta_input_tokens", value: optInt64(metrics.DeltaInputTokens.Value), coverage: metrics.DeltaInputTokens.Coverage, error: metrics.DeltaInputTokens.IntegrityError},
		{name: "delta_output_tokens", value: optInt64(metrics.DeltaOutputTokens.Value), coverage: metrics.DeltaOutputTokens.Coverage, error: metrics.DeltaOutputTokens.IntegrityError},
		{name: "delta_cache_read_tokens", value: optInt64(metrics.DeltaCacheReadTokens.Value), coverage: metrics.DeltaCacheReadTokens.Coverage, error: metrics.DeltaCacheReadTokens.IntegrityError},
		{name: "delta_cache_write_tokens", value: optInt64(metrics.DeltaCacheWriteTokens.Value), coverage: metrics.DeltaCacheWriteTokens.Coverage, error: metrics.DeltaCacheWriteTokens.IntegrityError},
		{name: "reported_cost_usd", value: optFloat(metrics.ReportedCostUSD.Value), coverage: metrics.ReportedCostUSD.Coverage, error: metrics.ReportedCostUSD.IntegrityError},
	}
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\n", row.name, row.value, row.coverage.Reported, row.coverage.Total, orUnknown(deref(row.error)))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(integrityErrors) > 0 {
		fmt.Fprintln(w, "integrity errors:")
		for _, integrityError := range integrityErrors {
			fmt.Fprintf(w, "- %s\n", integrityError)
		}
	}
	return nil
}

func renderStatsDashboard(stats *db.Stats) string {
	var lines []string
	lines = append(lines, "")
	lines = append(lines, centeredStatsBlock(strings.Split(banner, "\n"))...)
	lines = append(lines, "", "")

	rescueRate := ratio(stats.RescueRuns, stats.TotalRuns)
	fixRate := ratio(stats.FixedFindings, stats.ReportedFindings)
	repoDetail := "across all repos"
	if stats.TotalRepos > 0 {
		repoDetail = fmt.Sprintf("across %d repos", stats.TotalRepos)
	}
	lines = append(lines,
		metricStatsLine("Total changes", fmt.Sprintf("%d", stats.TotalRuns), repoDetail),
		metricStatsLine("Rescued changes", fmt.Sprintf("%d", stats.RescueRuns), "mistake caught + fixed"),
		metricStatsLine("Rescue rate", percent(rescueRate), progressBar(rescueRate, statsBarWidth)),
		"",
		"  Mistakes",
		metricStatsLine("Reported", fmt.Sprintf("%d", stats.ReportedFindings), progressBar(ratio(stats.ReportedFindings, stats.ReportedFindings), statsBarWidth)),
		metricStatsLine("Fixed", percent(fixRate), progressBar(fixRate, statsBarWidth)),
		"",
		"  Fixes by step",
	)

	maxStepFixes := maxStepFixedFindings(stats.StepStats)
	for _, step := range pipelineOrderedStepStats(stats.StepStats) {
		if step.FixedFindings == 0 {
			continue
		}
		lines = append(lines, metricStatsLine(string(step.StepName), fmt.Sprintf("%d", step.FixedFindings), progressBar(ratio(step.FixedFindings, maxStepFixes), statsBarWidth)))
	}

	lines = append(lines, "", "  Top repos")
	maxRepoFixes := maxRepoFixedFindings(stats.RepoStats)
	repoCount := 0
	for _, repo := range stats.RepoStats {
		if repo.Runs == 0 {
			continue
		}
		lines = append(lines, repoStatsLine(repo, maxRepoFixes))
		repoCount++
		if repoCount == 3 {
			break
		}
	}
	if repoCount == 0 {
		lines = append(lines, "  no runs yet")
	}
	lines = append(lines, "")

	return renderStatsBox(lines)
}

func renderStatsBox(lines []string) string {
	var b strings.Builder
	eyebrow := " git push no-mistakes "
	b.WriteString("╭─" + eyebrow + strings.Repeat("─", statsBoxWidth-3-lipgloss.Width(eyebrow)) + "╮\n")
	for _, line := range lines {
		b.WriteString(renderStatsBoxLine(line))
		b.WriteByte('\n')
	}
	b.WriteString("╰" + strings.Repeat("─", statsBoxWidth-2) + "╯")
	return b.String()
}

func renderStatsBoxLine(line string) string {
	width := lipgloss.Width(line)
	if width > statsContentWidth {
		line = truncateStatsLine(line, statsContentWidth)
		width = lipgloss.Width(line)
	}
	return "│ " + line + strings.Repeat(" ", statsContentWidth-width) + " │"
}

func centerStatsLine(line string) string {
	width := lipgloss.Width(line)
	if width >= statsContentWidth {
		return line
	}
	return strings.Repeat(" ", (statsContentWidth-width)/2) + line
}

func centeredStatsBlock(lines []string) []string {
	maxWidth := 0
	for _, line := range lines {
		if width := lipgloss.Width(line); width > maxWidth {
			maxWidth = width
		}
	}
	if maxWidth >= statsContentWidth {
		return lines
	}
	indent := strings.Repeat(" ", (statsContentWidth-maxWidth)/2)
	centered := make([]string, 0, len(lines))
	for _, line := range lines {
		centered = append(centered, indent+sCyan.Render(line))
	}
	return centered
}

func metricStatsLine(label, value, detail string) string {
	return fmt.Sprintf("  %-16s %5s   %s", label, value, detail)
}

func repoStatsLine(repo db.RepoStats, maxFixes int) string {
	name := truncateStatsLine(repo.DisplayName(), 16)
	return fmt.Sprintf("  %-16s %5d rescue %5d fixes   %s", name, repo.RescueRuns, repo.FixedFindings, progressBar(ratio(repo.FixedFindings, maxFixes), statsRepoBarWidth))
}

func progressBar(value float64, width int) string {
	if value < 0 {
		value = 0
	}
	if value > 1 {
		value = 1
	}
	filled := int(math.Round(value * float64(width)))
	if filled > width {
		filled = width
	}
	return sGreen.Render(strings.Repeat("█", filled)) + sDim.Render(strings.Repeat("░", width-filled))
}

func percent(value float64) string {
	return fmt.Sprintf("%d%%", int(math.Round(value*100)))
}

func ratio(value, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(value) / float64(total)
}

func maxStepFixedFindings(stats []db.StepStats) int {
	maxValue := 0
	for _, stat := range stats {
		if stat.FixedFindings > maxValue {
			maxValue = stat.FixedFindings
		}
	}
	return maxValue
}

func pipelineOrderedStepStats(stats []db.StepStats) []db.StepStats {
	byStep := make(map[types.StepName]db.StepStats, len(stats))
	for _, stat := range stats {
		byStep[stat.StepName] = stat
	}
	ordered := make([]db.StepStats, 0, len(stats))
	seen := make(map[types.StepName]bool, len(stats))
	for _, step := range types.AllSteps() {
		stat, ok := byStep[step]
		if !ok {
			continue
		}
		ordered = append(ordered, stat)
		seen[step] = true
	}
	for _, stat := range stats {
		if seen[stat.StepName] {
			continue
		}
		ordered = append(ordered, stat)
	}
	return ordered
}

func maxRepoFixedFindings(stats []db.RepoStats) int {
	maxValue := 0
	for _, stat := range stats {
		if stat.FixedFindings > maxValue {
			maxValue = stat.FixedFindings
		}
	}
	return maxValue
}

func truncateStatsLine(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes)
}
