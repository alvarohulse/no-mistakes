package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/kunchenguid/no-mistakes/internal/eval"
)

const (
	evalBoxWidth                = 79
	evalBarWidth                = 20
	evalCompositionContentWidth = evalBoxWidth - 4
	minCompositionRepoWidth     = 8
	compositionSeparator        = " · "
)

func renderEvalSetsDashboard(summaries []eval.SetSummary) string {
	byName := map[string]eval.SetSummary{}
	for _, summary := range summaries {
		byName[summary.Name] = summary
	}
	diversified := byName["diversified"]

	var lines []string
	lines = append(lines, "", "  Diversified holdout (official gold-only set)")
	capDetail := fmt.Sprintf("pins %d · cap %d", diversified.PinCount, diversified.Cap)
	if diversified.Cap == 0 {
		capDetail = fmt.Sprintf("pins %d · cap none (one gold case per stratum)", diversified.PinCount)
	}
	lines = append(lines, metricStatsLine("Cases", strconv.Itoa(diversified.Cases), capDetail))
	goldFindings := diversified.TruePositive + diversified.FalseNegative + diversified.FalsePositive
	lines = append(lines, metricStatsLine("Gold findings", strconv.Itoa(goldFindings), fmt.Sprintf("across %d gold case(s)", diversified.GoldCases)))
	if goldFindings > 0 {
		lines = append(lines, "")
		lines = append(lines, evalConfusionMatrixLines(diversified.TruePositive, diversified.FalseNegative, diversified.FalsePositive)...)
	}
	lines = append(lines, "", "  Self-score: the recorded reviews scored against their own gold")
	if diversified.SelfScore.Labeled == 0 {
		lines = append(lines, "    unlabeled / pending (no finding-level gold yet)")
	} else {
		lines = append(lines, evalScoreLines(diversified.SelfScore)...)
	}
	if len(diversified.Composition) > 0 {
		lines = append(lines, "", "  Composition")
		lines = append(lines, compositionLines(diversified.Composition)...)
	}

	lines = append(lines, "", "  Other sets")
	for _, name := range []string{"all", "labeled", "tune"} {
		summary, ok := byName[name]
		if !ok {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %-8s %4d case(s) · %d gold · %d unlabeled / pending · %d queued",
			summary.Name, summary.Cases, summary.GoldCases, summary.Unlabeled, summary.QueuedFindings))
	}
	for _, summary := range summaries {
		if summary.Warning != "" {
			lines = append(lines, "", sYellow.Render("  ⚠ "+summary.Warning))
		}
	}
	lines = append(lines, "", sDim.Render("  local-only: cases, gold, and scores never leave this machine"), "")
	return renderTitledBox(" eval case sets ", evalBoxWidth, lines)
}

func compositionLines(rows []eval.CompositionRow) []string {
	widest := 0
	countPrefixWidth := 0
	for _, row := range rows {
		if width := lipgloss.Width(compositionStrata(row)); width > widest {
			widest = width
		}
		if width := lipgloss.Width(fmt.Sprintf("  %4d  ", row.Cases)); width > countPrefixWidth {
			countPrefixWidth = width
		}
	}
	compositionWidth := evalCompositionContentWidth - countPrefixWidth
	repoWidth := compositionWidth - widest - lipgloss.Width(compositionSeparator)
	if repoWidth < minCompositionRepoWidth {
		repoWidth = minCompositionRepoWidth
	}
	names := fitRepoNames(rows, repoWidth)
	column := 0
	for _, name := range names {
		if width := lipgloss.Width(name); width > column {
			column = width
		}
	}
	strataWidth := compositionWidth - column - lipgloss.Width(compositionSeparator)
	if strataWidth < 1 {
		strataWidth = 1
	}
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		padded := names[i] + strings.Repeat(" ", column-lipgloss.Width(names[i]))
		strata := truncateStatsLine(compositionStrata(row), strataWidth)
		lines = append(lines, fmt.Sprintf("  %4d  %s", row.Cases, padded+compositionSeparator+strata))
	}
	return lines
}

func compositionStrata(row eval.CompositionRow) string {
	return strings.Join([]string{row.Language, row.Size, row.Severity, row.FindingType}, compositionSeparator)
}

func fitRepoNames(rows []eval.CompositionRow, width int) []string {
	names := make([]string, 0, len(rows))
	shorten := false
	for _, row := range rows {
		if lipgloss.Width(row.Repo) > width {
			shorten = true
		}
	}
	for _, row := range rows {
		name := row.Repo
		if shorten {
			if slash := strings.LastIndex(name, "/"); slash >= 0 && slash+1 < len(name) {
				name = name[slash+1:]
			}
		}
		names = append(names, truncateStatsLine(name, width))
	}
	return names
}

func evalConfusionMatrixLines(truePositive, falseNegative, falsePositive int) []string {
	const labelWidth = 18
	const cellWidth = 14
	const notAnIssue = "not an issue"
	cell := func(kind, value string) string { return fmt.Sprintf("%-2s %6s", kind, value) }
	rule := strings.Repeat("─", labelWidth+cellWidth+len(notAnIssue))
	return []string{
		"  Confusion matrix (finding-level gold)",
		fmt.Sprintf("    %-*s%-*s%-*s", labelWidth, "", cellWidth, "real issue", cellWidth, notAnIssue),
		"    " + sDim.Render(rule),
		fmt.Sprintf("    %-*s%-*s%-*s", labelWidth, "review raised", cellWidth, cell("TP", strconv.Itoa(truePositive)), cellWidth, cell("FP", strconv.Itoa(falsePositive))),
		fmt.Sprintf("    %-*s%-*s%-*s", labelWidth, "review missed", cellWidth, cell("FN", strconv.Itoa(falseNegative)), cellWidth, cell("TN", "-")),
		sDim.Render("    TN is never counted: a correctly silent review leaves no gold"),
	}
}

func evalScoreLines(s eval.EvaluationSummary) []string {
	var lines []string
	trueIssues := s.TruePositive + s.FalseNegative
	if trueIssues == 0 {
		lines = append(lines, metricStatsLine("Recall", "-", "unavailable (no true-issue gold)"))
	} else {
		detail := progressBar(s.Recall(), evalBarWidth) + fmt.Sprintf("  %d/%d true issues", s.TruePositive, trueIssues)
		lines = append(lines, metricStatsLine("Recall", percent(s.Recall()), detail))
	}
	bounds := fmt.Sprintf("%s-%s", percent(s.PrecisionLower()), percent(s.Precision()))
	lines = append(lines, metricStatsLine("Precision", bounds, "pending counted as FP in the lower bound"))
	if s.HasFalsePositiveGold() {
		lines = append(lines, metricStatsLine("F1", percent(s.F1()), "headline (false-positive gold present)"))
	} else {
		lines = append(lines, metricStatsLine("F1", "-", "withheld (no false-positive gold)"))
	}
	if s.Pending > 0 {
		lines = append(lines, metricStatsLine("Pending", strconv.Itoa(s.Pending), "queued unmatched candidate finding(s)"))
	}
	return lines
}

func evalRunProgress(w io.Writer, evaluation eval.Evaluation, completed, total int) {
	progress := fmt.Sprintf("%*d/%d", len(strconv.Itoa(total)), completed, total)
	if evaluation.Status != "completed" {
		fmt.Fprintf(w, "  %s %s  %s repeat %d  failed: %s\n", sRed.Render("✗"), progress, evaluation.CaseID, evaluation.Repeat, evaluation.Error)
		return
	}
	fmt.Fprintf(w, "  %s %s  %s repeat %d  TP %d · FN %d · FP %d · pending %d  %s\n",
		sGreen.Render("✓"), progress, evaluation.CaseID, evaluation.Repeat,
		evaluation.TruePositive, evaluation.FalseNegative, evaluation.FalsePositive, evaluation.Pending, formatMS(evaluation.DurationMS))
}

func renderEvalRunSummary(session eval.Session, evaluations []eval.Evaluation, caseCount int) string {
	s := eval.SummarizeEvaluations(evaluations)
	lines := []string{""}
	lines = append(lines, metricStatsLine("Candidate", "", session.Candidate))
	lines = append(lines, metricStatsLine("Case set", "", fmt.Sprintf("%s · cohort %s", session.Set, session.Cohort)))
	lines = append(lines, metricStatsLine("Replays", strconv.Itoa(s.Total), fmt.Sprintf("%d case(s) x %d repeat(s) · %d failure(s)", caseCount, session.Repeats, s.Failures)))
	lines = append(lines, metricStatsLine("Labeled", strconv.Itoa(s.Labeled), "replay(s) of cases with finding-level gold"), "")
	if s.Labeled == 0 {
		lines = append(lines, "  unlabeled / pending (no finding-level gold in this set yet)")
	} else {
		lines = append(lines, evalScoreLines(s)...)
	}
	lines = append(lines, "")
	if s.Total > 0 && s.TokensReported == s.Total {
		avgTokens := float64(s.FreshInputTokens+s.OutputTokens) / float64(s.Total)
		lines = append(lines, metricStatsLine("Tokens", fmt.Sprintf("%.0f", avgTokens), "fresh-input + output per replay"))
	} else {
		lines = append(lines, metricStatsLine("Tokens", "-", "unknown (not reported for every replay)"))
	}
	if s.Total > 0 {
		lines = append(lines, metricStatsLine("Wall time", formatMS(s.DurationMS/int64(s.Total)), "average per replay"))
	}
	lines = append(lines, "")
	return renderTitledBox(" eval run ", evalBoxWidth, lines)
}

func renderTitledBox(eyebrow string, width int, lines []string) string {
	var b strings.Builder
	b.WriteString("╭─" + eyebrow + strings.Repeat("─", width-3-lipgloss.Width(eyebrow)) + "╮\n")
	for _, line := range lines {
		contentWidth := width - 4
		line = truncateStatsLine(line, contentWidth)
		b.WriteString("│ " + line + strings.Repeat(" ", contentWidth-lipgloss.Width(line)) + " │\n")
	}
	b.WriteString("╰" + strings.Repeat("─", width-2) + "╯")
	return b.String()
}
