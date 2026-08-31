package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestEvalSetsDashboardShowsMatrixRepositoryAndSelfScore(t *testing.T) {
	summary := eval.SetSummary{
		Name: "diversified", Cases: 2, GoldCases: 2, PinCount: 2, Cap: 8,
		TruePositive: 1, FalseNegative: 1, FalsePositive: 1,
		Composition: []eval.CompositionRow{{Repo: "owner/repo", Language: "go", Size: "small", Severity: "error", FindingType: "error/auto-fix", Cases: 2}},
		SelfScore:   eval.EvaluationSummary{Labeled: 2, TruePositive: 1, FalseNegative: 1, FalsePositive: 1, FalsePositiveGold: 1},
	}
	out := renderEvalSetsDashboard([]eval.SetSummary{summary})
	for _, want := range []string{"Diversified holdout", "Confusion matrix", "TP      1", "FN      1", "FP      1", "TN      -", "Self-score", "owner/repo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard = %q, want %q", out, want)
		}
	}
}

func TestEvalCompositionFitsTheDashboardBox(t *testing.T) {
	rows := []eval.CompositionRow{
		{Repo: "a-very-long-organization-name/no-mistakes", Language: "javascript", Size: "medium", Severity: "warning", FindingType: "blocking-correctness-defect/requires-human-review", Cases: 9999},
		{Repo: "group/very-long-subgroup/actual-repo", Language: "typescript", Size: "large", Severity: "error", FindingType: "error/ask-user", Cases: 10000},
	}
	lines := compositionLines(rows)
	if len(lines) != len(rows) {
		t.Fatalf("compositionLines returned %d line(s), want %d", len(lines), len(rows))
	}
	for _, line := range lines {
		if width := lipgloss.Width(line); width > evalBoxWidth-4 {
			t.Fatalf("composition line %q is %d wide, want at most %d", line, width, evalBoxWidth-4)
		}
	}
	if !strings.Contains(lines[0], "no-mista") || !strings.Contains(lines[1], "actual-r") {
		t.Fatalf("lines = %q, want each repository identity retained", lines)
	}
}

func TestEvalRunSummaryLeavesUnknownUsageExplicit(t *testing.T) {
	session := eval.Session{Candidate: "historical+candidate", Set: "labeled", Cohort: "old-cohort", Repeats: 1}
	evaluations := []eval.Evaluation{{Candidate: session.Candidate, Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1, DurationMS: 1000}}
	out := renderEvalRunSummary(session, evaluations, 1)
	if !strings.Contains(out, "Tokens") || !strings.Contains(out, "unknown (not reported for every replay)") {
		t.Fatalf("summary = %q, want explicit unknown token usage", out)
	}
	if !strings.Contains(out, "historical+candidate") {
		t.Fatalf("summary = %q, want historical candidate identity preserved", out)
	}
}

func TestEvalDisplayCommandsDoNotCreatePipelineDatabase(t *testing.T) {
	for _, command := range [][]string{{"eval", "sets"}, {"eval", "report"}} {
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("NM_HOME", root)
			chdir(t, t.TempDir())
			if out, err := executeCmd(command...); err != nil {
				t.Fatalf("%v: %v\n%s", command, err, out)
			}
			p, err := paths.New()
			if err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if _, err := os.Stat(p.DB() + suffix); !os.IsNotExist(err) {
					t.Fatalf("%v created pipeline database %q: %v", command, p.DB()+suffix, err)
				}
			}
		})
	}
}
