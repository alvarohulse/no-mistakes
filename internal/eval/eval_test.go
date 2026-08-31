package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCaptureCreatesPortableReviewCaseWithoutRecordingRemoteURL(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("captured cases = %d, want 1", len(cases))
	}
	captured := cases[0]
	if captured.SourceRunID != run.ID || captured.SourceRoundID != reviewRound.ID {
		t.Fatalf("capture provenance = %#v, want run %q round %q", captured, run.ID, reviewRound.ID)
	}
	if !captured.Labels.HasGold() || len(captured.Labels.Findings) != 1 {
		t.Fatalf("gold labels = %#v, want one recorded user-fix finding", captured.Labels)
	}
	gold := captured.Labels.Findings[0]
	if gold.Kind != GoldTruePositive || gold.Source != goldSourceUserFix || gold.ID != "real-bug" || gold.Description != "bug" {
		t.Fatalf("true-positive gold = %#v, want recorded user-fix for real-bug", gold)
	}
	restored := filepath.Join(t.TempDir(), "restore.git")
	if err := git.InitBare(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := restoreCaseObjects(ctx, store.poolDir(captured.RepoFingerprint), restored, captured.ID); err != nil {
		t.Fatalf("case objects are not restorable: %v", err)
	}
	if got := mustGit(t, ctx, restored, "rev-parse", captured.ReviewedHeadSHA+"^{commit}"); got != captured.ReviewedHeadSHA {
		t.Fatalf("restored reviewed commit = %q, want %q", got, captured.ReviewedHeadSHA)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(captured.Dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestBytes), repo.UpstreamURL) || strings.Contains(string(manifestBytes), "secret-token") {
		t.Fatalf("manifest leaked source remote credential: %s", manifestBytes)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.TrustedConfigSHA == "" || manifest.ReviewedHeadSHA == "" {
		t.Fatalf("manifest did not pin replay inputs: %#v", manifest)
	}

	listed, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != captured.ID {
		t.Fatalf("registry cases = %#v, want captured case", listed)
	}
}

func TestCaptureWritesVersionedOwnerOnlyReplayConfig(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayConfigJSON, err := config.MarshalEvalReplayConfig(&config.Config{
		AgentPathOverride:       map[string]string{"claude": "/private/agent"},
		AgentArgsOverride:       map[string][]string{"claude": {"--token", "private-secret"}},
		IgnorePatterns:          []string{"generated/**"},
		ProcessTerminationGrace: 2 * time.Second,
		DisableProjectSettings:  true,
		Prompts:                 config.PromptConfig{Shared: "shared guidance", Review: "review guidance", Test: "private test prompt"},
		Review:                  config.Review{PathInstructions: []config.PathInstruction{{Path: "internal/**", Instructions: "review errors"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	secondRound, err := sourceDB.InsertReviewStepRoundWithReplayConfig(steps[0].ID, 2, "initial", &clean, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), replayConfigJSON, 20)
	if err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var captured Case
	for _, c := range cases {
		if c.SourceRoundID == secondRound.ID {
			captured = c
		}
	}
	if captured.ID == "" {
		t.Fatalf("new replay case missing from %#v", cases)
	}
	replayPath := filepath.Join(captured.Dir, "config", "replay.json")
	got, err := os.ReadFile(replayPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(replayConfigJSON) {
		t.Fatalf("replay config = %s, want %s", got, replayConfigJSON)
	}
	for _, secret := range []string{"/private/agent", "private-secret", "private test prompt"} {
		if strings.Contains(string(got), secret) {
			t.Fatalf("replay config leaked %q: %s", secret, got)
		}
	}
	for _, legacy := range []string{"global.yaml", "repo-config.yaml"} {
		if _, err := os.Stat(filepath.Join(captured.Dir, "config", legacy)); !os.IsNotExist(err) {
			t.Fatalf("new case retained legacy %s: %v", legacy, err)
		}
	}
	replayed, err := replayConfig(captured)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.IgnorePatterns) != 1 || replayed.IgnorePatterns[0] != "generated/**" || replayed.Prompts.Review != "review guidance" || !replayed.DisableProjectSettings {
		t.Fatalf("replayed config = %#v", replayed)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{
			p.EvalDir():                           0o700,
			captured.Dir:                          0o700,
			filepath.Join(captured.Dir, "config"): 0o700,
			replayPath:                            0o600,
			filepath.Join(p.EvalDir(), "registry.sqlite"): 0o600,
		} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != want {
				t.Errorf("%s permissions = %04o, want %04o", path, got, want)
			}
		}
	}
}

func TestReplayConfigRejectsCorruptVersionedConfigWithoutLegacyFallback(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "replay.json"), []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "global.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "repo-config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := replayConfig(Case{Dir: dir})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("replayConfig error = %v, want unsupported version", err)
	}
}

func TestCaptureRejectsReviewRoundBeforeGateDecision(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusAwaitingApproval); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err == nil || !strings.Contains(err.Error(), "no recorded gate decision") {
		t.Fatalf("capture error = %v, want missing gate decision", err)
	}
	cases, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 0 {
		t.Fatalf("premature capture registered %d cases", len(cases))
	}
}

func TestCaptureExplainsMissingConfigurationProvenance(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`
	if _, err := sourceDB.InsertReviewStepRound(steps[0].ID, 2, "legacy", &clean, nil, run.HeadSHA, 10); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = Capture(ctx, store, p, sourceDB, run.ID)
	if !errors.Is(err, ErrNoCapturableReview) {
		t.Fatalf("capture error = %v, want ErrNoCapturableReview", err)
	}
	if !strings.Contains(err.Error(), "eval.capture_provenance was off") {
		t.Fatalf("capture error = %q, want the disabled setting named as the likely cause", err)
	}
}

func TestCapturePinsConfigurationFromSourceReview(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	gateDir := p.RepoDir(run.RepoID)
	workDir := filepath.Join(p.Root(), "advance-main")
	mustGit(t, ctx, p.Root(), "clone", gateDir, workDir)
	mustGit(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustGit(t, ctx, workDir, "config", "user.name", "Eval Test")
	mustGit(t, ctx, workDir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("ignore_patterns: ['advanced-only']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", ".no-mistakes.yaml")
	t.Setenv("GIT_AUTHOR_DATE", time.Unix(reviewRound.CreatedAt+60, 0).Format(time.RFC3339))
	t.Setenv("GIT_COMMITTER_DATE", time.Unix(reviewRound.CreatedAt+60, 0).Format(time.RFC3339))
	mustGit(t, ctx, workDir, "commit", "-m", "advance trusted config")
	mustGit(t, ctx, workDir, "push", "origin", "main")

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].TrustedConfigSHA != run.BaseSHA {
		t.Fatalf("trusted config pin = %#v, want source-review SHA %s", cases, run.BaseSHA)
	}
}

func TestCapturePreservesFixRoundStartingHead(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "main.go"), []byte("package sample\n\nfunc Fixed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, repo.WorkingPath, "add", "main.go")
	mustGit(t, ctx, repo.WorkingPath, "commit", "-m", "fix review finding")
	mustGit(t, ctx, repo.WorkingPath, "push", "origin", "feature/eval")
	fixedSHA := mustGit(t, ctx, repo.WorkingPath, "rev-parse", "HEAD")
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[],"risk_level":"low","risk_rationale":"fixed","risk_scope":"source-or-external"}`
	secondRound, err := sourceDB.InsertReviewStepRoundWithProvenance(steps[0].ID, 2, "auto_fix", &clean, nil, fixedSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 25)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 || cases[1].SourceRoundID != secondRound.ID || cases[1].StartingHeadSHA != stringValue(firstRound.ReviewedHeadSHA) || cases[1].ReviewedHeadSHA != fixedSHA {
		t.Fatalf("captured fix-round provenance = %#v", cases)
	}
}

func TestReplayRestoresCaseIntoAnIsolatedWorktree(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "claude")
	const reply = `{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"clean"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"},"usage":{"input_tokens":12,"output_tokens":3}}
`
	var script string
	if runtime.GOOS == "windows" {
		fake += ".cmd"
		script = "@echo off\r\nmore >nul\r\necho " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\n[ \"$NM_HOME\" = \"" + p.Root() + "\" ] && touch \"" + p.Root() + "/shared-home-used\"\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}

	session, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test", Vendor: "test"}, Repeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" || len(evaluations) != 1 {
		t.Fatalf("replay = session %#v evaluations %#v", session, evaluations)
	}
	got := evaluations[0]
	if got.Status != "completed" || got.GoldCount != 1 || got.TruePositive != 0 || got.FalseNegative != 1 || got.Pending != 0 {
		t.Fatalf("replay outcome = %#v, want a completed miss of the true-positive gold", got)
	}
	if !got.TokensReported || got.FreshInputTokens != 12 || got.OutputTokens != 3 {
		t.Fatalf("replay metrics = %#v", got)
	}
	if strings.Contains(got.Error, p.Root()) {
		t.Fatalf("replay error leaked production root: %q", got.Error)
	}
	if _, err := os.Stat(filepath.Join(p.Root(), "shared-home-used")); !os.IsNotExist(err) {
		t.Fatalf("candidate used production NM_HOME: %v", err)
	}
	var reservations int
	if err := store.db.QueryRow(`SELECT count(*) FROM replay_case_reservations WHERE session_id = ?`, session.ID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Fatalf("completed replay retained %d case reservations", reservations)
	}
}

func TestBaselineForRoundIncludesOnlyCompleteReviewInvocationMetrics(t *testing.T) {
	input, output, cache, cacheCreation := 100, 20, 30, 0
	invocations := []db.AgentInvocation{
		{StepName: string(types.StepReview), Round: 2, Purpose: "review-fix", DurationMS: 900, DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cache},
		{StepName: string(types.StepReview), Round: 2, Purpose: "review", Agent: "codex", DurationMS: 100, DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cache, DeltaCacheCreationTokens: &cacheCreation},
	}
	baseline := baselineForRound(invocations, 2)
	if baseline.DurationMS != 100 || !baseline.TokensReported || baseline.InputTokens != 100 || baseline.OutputTokens != 20 || baseline.CacheReadTokens != 30 || baseline.FreshInputTokens != 70 {
		t.Fatalf("review baseline = %#v", baseline)
	}

	invocations = append(invocations, db.AgentInvocation{StepName: string(types.StepReview), Round: 2, Purpose: "review", DurationMS: 50})
	baseline = baselineForRound(invocations, 2)
	if baseline.DurationMS != 150 || baseline.TokensReported || baseline.InputTokens != 0 || baseline.OutputTokens != 0 || baseline.CacheReadTokens != 0 || baseline.FreshInputTokens != 0 {
		t.Fatalf("incomplete review baseline = %#v", baseline)
	}
}

func TestScoreCandidateMatchesSameFindingID(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{
		ID:          "error-handling",
		Kind:        GoldTruePositive,
		File:        "old.go",
		Description: "drops an HTTP error",
	}}}
	candidate := `{"findings":[{"id":"error-handling","file":"new.go","description":"drops a database error"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 1 || score.FalseNegative != 0 || score.Pending != 0 {
		t.Fatalf("score = %#v, want same finding ID matched", score)
	}
}

func TestScoreCandidateMatchesNormalizedFileAndDescription(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{ID: "review-1", Kind: GoldTruePositive, File: " internal/eval/score.go ", Description: "Drops   an HTTP Error"}}}
	candidate := `{"findings":[{"id":"different","file":"internal/eval/score.go","description":"drops an http error"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 1 || score.FalseNegative != 0 || score.Pending != 0 {
		t.Fatalf("score = %#v, want conservative file-and-description match", score)
	}
}

func TestScoreCandidateDoesNotMatchFindingsWithoutFiles(t *testing.T) {
	labels := Labels{Findings: []FindingGold{{ID: "gold", Kind: GoldTruePositive, Description: "drops an HTTP error"}}}
	candidate := `{"findings":[{"id":"candidate","description":"drops an http error"}]}`

	score := ScoreCandidate(labels, candidate)
	if score.TruePositive != 0 || score.FalseNegative != 1 || score.Pending != 1 {
		t.Fatalf("score = %#v, want file-less findings left unmatched", score)
	}
}

func TestSummarizeEvaluationsScoresFindingGoldAndLeavesUnmatchedPending(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{
		{CaseID: "fix-gold", Candidate: "claude+test", Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1},
		{CaseID: "fix-gold", Candidate: "claude+test", Status: "completed", HasFindingGold: true, GoldCount: 1, FalseNegative: 1},
		{CaseID: "approve-unlabeled", Candidate: "claude+test", Status: "completed", Pending: 2},
		{CaseID: "approve-unlabeled", Candidate: "claude+test", Status: "completed"},
	})

	if summary.Labeled != 2 || summary.TruePositive != 1 || summary.FalseNegative != 1 || summary.FalsePositive != 0 || summary.Pending != 2 {
		t.Fatalf("summary = %#v, want TP/FN gold plus queued unmatched findings", summary)
	}
	if got := summary.Recall(); got != 0.5 {
		t.Fatalf("recall = %v, want 0.5", got)
	}
}

func TestSummarizeEvaluationsKeepsExplicitInvalidOnlyScoresLabeled(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{{
		Candidate:      "claude+test",
		Status:         "completed",
		HasFindingGold: true,
		FalsePositive:  1,
	}})

	if summary.Labeled != 1 || summary.FalsePositive != 1 {
		t.Fatalf("summary = %#v, want explicit-invalid-only evaluation retained", summary)
	}
}

func TestFailedLabeledReplayCountsAsFalseNegativeAndBlocksFrontier(t *testing.T) {
	summary := SummarizeEvaluations([]Evaluation{
		{Candidate: "claude+test", Status: "completed", GoldCount: 1, TruePositive: 1},
		{Candidate: "claude+test", Status: "failed", GoldCount: 1, FalseNegative: 1},
	})
	if summary.Labeled != 2 || summary.TruePositive != 1 || summary.FalseNegative != 1 || summary.Recall() != 0.5 {
		t.Fatalf("summary = %#v, want failed labeled replay counted as a false-negative", summary)
	}
	cost := 10.0
	reports := []CandidateReport{{Cohort: "same", Summary: summary, AverageTokens: &cost}}
	markFrontier(reports)
	if reports[0].OnFrontier {
		t.Fatal("candidate with failed replay was marked frontier-eligible")
	}
}

func TestPersistEvaluationQueuesEveryUnexpectedCandidateFinding(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	c := writeSyntheticCase(t, store, syntheticCaseSpec{
		id: "candidate-findings", fingerprint: "repo", capturedAt: 1, changedLines: 10,
		changedFiles: []string{"main.go"},
	})
	c.Labels.QueuedCandidateFindings = 7
	if err := writeJSON(filepath.Join(c.Dir, "labels.json"), c.Labels); err != nil {
		t.Fatal(err)
	}
	labelsBefore, err := os.ReadFile(filepath.Join(c.Dir, "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.persistEvaluation(c, Evaluation{
		ID:           "evaluation",
		SessionID:    "session",
		CaseID:       c.ID,
		Candidate:    "claude+test",
		Repeat:       1,
		Status:       "completed",
		Pending:      3,
		FindingCount: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.persistEvaluation(c, Evaluation{
		ID:           "failed-evaluation",
		SessionID:    "session",
		CaseID:       c.ID,
		Candidate:    "claude+test",
		Repeat:       2,
		Status:       "failed",
		Pending:      5,
		FindingCount: 5,
	}); err != nil {
		t.Fatal(err)
	}

	queued, err := store.pendingFindingCounts()
	if err != nil {
		t.Fatal(err)
	}
	if queued[c.ID] != 3 {
		t.Fatalf("queued candidate findings = %d, want 3 from completed evaluations", queued[c.ID])
	}
	labelsAfter, err := os.ReadFile(filepath.Join(c.Dir, "labels.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(labelsBefore) != string(labelsAfter) {
		t.Fatalf("persisting evaluations changed labels.json:\nbefore: %s\nafter: %s", labelsBefore, labelsAfter)
	}
	summaries, err := InspectSets(store)
	if err != nil {
		t.Fatal(err)
	}
	if summaries[0].Name != "all" || summaries[0].QueuedFindings != 3 {
		t.Fatalf("all-set queued findings = %#v, want 3 from completed evaluations and no legacy counter", summaries[0])
	}
}

func TestBaselineForRoundDerivesFreshTokensFromPerRoundDeltas(t *testing.T) {
	deltaInput, deltaOutput, deltaCache, deltaCacheCreation := 100, 20, 40, 0
	cumulativeFresh := 900
	got := baselineForRound([]db.AgentInvocation{
		{
			StepName:                 string(types.StepReview),
			Round:                    2,
			Purpose:                  "review",
			Agent:                    "codex",
			DurationMS:               50,
			FreshInputTokens:         &cumulativeFresh,
			DeltaInputTokens:         &deltaInput,
			DeltaOutputTokens:        &deltaOutput,
			DeltaCacheReadTokens:     &deltaCache,
			DeltaCacheCreationTokens: &deltaCacheCreation,
		},
	}, 2)

	if !got.TokensReported || got.InputTokens != 100 || got.OutputTokens != 20 || got.CacheReadTokens != 40 || got.FreshInputTokens != 60 {
		t.Fatalf("baseline = %#v, want per-round token metrics", got)
	}
}

func TestBaselineForRoundUsesProviderSpecificCanonicalFreshInput(t *testing.T) {
	input, output, cacheRead, cacheCreation := 120, 30, 80, 5
	tests := []struct {
		agent     string
		wantFresh int64
	}{
		{agent: "claude", wantFresh: 120},
		{agent: "cursor", wantFresh: 120},
		{agent: "codex", wantFresh: 35},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			got := baselineForRound([]db.AgentInvocation{{
				StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: test.agent,
				DeltaInputTokens: &input, DeltaOutputTokens: &output,
				DeltaCacheReadTokens: &cacheRead, DeltaCacheCreationTokens: &cacheCreation,
			}}, 1)
			if !got.TokensReported || got.FreshInputTokens != test.wantFresh || got.OutputTokens != int64(output) {
				t.Fatalf("baseline = %#v, want fresh=%d output=%d", got, test.wantFresh, output)
			}
		})
	}

	got := baselineForRound([]db.AgentInvocation{{
		StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "codex",
		DeltaInputTokens: &input, DeltaOutputTokens: &output, DeltaCacheReadTokens: &cacheRead,
	}}, 1)
	if got.TokensReported {
		t.Fatalf("codex baseline without cache-creation evidence = %#v, want unreported", got)
	}
}

func TestApplyEvaluationUsageUsesProviderSpecificCanonicalFreshInput(t *testing.T) {
	tests := []struct {
		agent     string
		wantFresh int64
	}{
		{agent: "claude", wantFresh: 120},
		{agent: "cursor", wantFresh: 120},
		{agent: "codex", wantFresh: 35},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			result := &agent.Result{
				Usage: agent.TokenUsage{
					InputTokens: 120, OutputTokens: 30, CacheReadTokens: 80, CacheCreationTokens: 5,
					MeterPresenceReported: true, InputReported: true, OutputReported: true,
					CacheReadReported: true, CacheCreationReported: true,
				},
				CacheCreationReported: true,
			}
			var got Evaluation
			applyEvaluationUsage(&got, test.agent, result)
			if !got.TokensReported || got.FreshInputTokens != test.wantFresh || got.OutputTokens != 30 {
				t.Fatalf("evaluation = %#v, want fresh=%d output=30", got, test.wantFresh)
			}
		})
	}

	result := &agent.Result{Usage: agent.TokenUsage{
		InputTokens: 120, OutputTokens: 30, CacheReadTokens: 80,
		MeterPresenceReported: true, InputReported: true, OutputReported: true, CacheReadReported: true,
	}}
	var got Evaluation
	applyEvaluationUsage(&got, "codex", result)
	if got.TokensReported {
		t.Fatalf("codex evaluation without cache-creation evidence = %#v, want unreported", got)
	}
}

func TestSourceInvocationsPreserveBoundedTelemetry(t *testing.T) {
	cacheCreation, deltaCacheCreation, nested, toolCalls := 5, 3, 2, 7
	reportedCost := 0.42
	provider := "anthropic"
	got := sourceInvocationsFor([]db.AgentInvocation{{
		StepName: string(types.StepReview), Round: 1, Purpose: "review", Agent: "claude",
		InvocationMode:            types.AgentInvocationModeHarnessCLI,
		AgentObservations:         []types.AgentObservation{{Identity: "worker", InvocationMode: types.AgentInvocationModeSubagentTool}},
		AgentObservationsReported: true, NestedAgentCount: &nested, ModelProvider: &provider,
		CacheCreationTokens: &cacheCreation, DeltaCacheCreationTokens: &deltaCacheCreation,
		ReportedCostUSD: &reportedCost, ToolCalls: &toolCalls,
	}})
	if len(got) != 1 || got[0].InvocationMode != types.AgentInvocationModeHarnessCLI ||
		!got[0].AgentObservationsReported || got[0].NestedAgentCount == nil || *got[0].NestedAgentCount != nested ||
		got[0].ModelProvider == nil || *got[0].ModelProvider != provider ||
		got[0].DeltaCacheCreationTokens == nil || *got[0].DeltaCacheCreationTokens != deltaCacheCreation ||
		got[0].ReportedCostUSD == nil || *got[0].ReportedCostUSD != reportedCost ||
		got[0].ToolCalls == nil || *got[0].ToolCalls != toolCalls {
		t.Fatalf("source invocation = %#v, want bounded telemetry preserved", got)
	}
}

func TestConfidenceIntervalRequiresMultipleIndependentCases(t *testing.T) {
	rows := []Evaluation{{CaseID: "only", Candidate: "claude+test", Status: "completed", GoldCount: 1, TruePositive: 1}}
	if got := confidenceInterval("claude+test", rows); got != nil {
		t.Fatalf("single-case confidence interval = %#v, want unavailable", got)
	}
}

func TestConfidenceIntervalRepresentsUniformSampleUncertainty(t *testing.T) {
	rows := []Evaluation{
		{CaseID: "one", Status: "completed", GoldCount: 1, TruePositive: 1},
		{CaseID: "two", Status: "completed", GoldCount: 1, TruePositive: 1},
	}
	got := confidenceInterval("claude+test", rows)
	if got == nil || got.Lower <= 0 || got.Lower >= 1 || got.Upper < 0.999 {
		t.Fatalf("uniform-success confidence interval = %#v, want finite-sample uncertainty ending at 1", got)
	}
}

func TestConfidenceIntervalIncludesFailedLabeledReplays(t *testing.T) {
	rows := []Evaluation{
		{CaseID: "passed", Status: "completed", GoldCount: 1, TruePositive: 1},
		{CaseID: "failed", Status: "failed", GoldCount: 1, FalseNegative: 1},
	}
	got := confidenceInterval("claude+test", rows)
	if got == nil || got.Cases != 2 || got.Lower >= 0.5 || got.Upper <= 0.5 {
		t.Fatalf("confidence interval with scored failure = %#v, want interval around 50%% over two cases", got)
	}
}

func TestRenderReportNamesCaseLevelRecallRange(t *testing.T) {
	output := RenderReport([]CandidateReport{
		{
			Cohort:     "cohort",
			Summary:    EvaluationSummary{Candidate: "claude+test", Total: 2, Labeled: 2, TruePositive: 2},
			Confidence: &Interval{Lower: 0.34, Upper: 1, Cases: 2},
		},
	})
	if !strings.Contains(output, "case-level recall range: 34.0%-100.0% over 2 case(s)") {
		t.Fatalf("report recall range = %q", output)
	}
}

func TestRenderReportKeepsInvalidOnlyScoreWithoutClaimingRecall(t *testing.T) {
	cost := 10.0
	output := RenderReport([]CandidateReport{{
		Cohort:        "cohort",
		Summary:       EvaluationSummary{Candidate: "claude+test", Total: 1, Labeled: 1, FalsePositive: 1},
		AverageTokens: &cost,
	}})
	if !strings.Contains(output, "false-positive 1") || !strings.Contains(output, "recall: unavailable (no true-issue gold)") {
		t.Fatalf("invalid-only report = %q, want FP score with unavailable recall", output)
	}
	if strings.Contains(output, "0/0 gold issues") || strings.Contains(output, "recall-vs-cost frontier: true") {
		t.Fatalf("invalid-only report claims recall evidence: %q", output)
	}
}

func TestAverageTokensRequiresCompleteReplayCoverage(t *testing.T) {
	rows := []Evaluation{
		{TokensReported: true, FreshInputTokens: 10, OutputTokens: 2},
		{TokensReported: false},
	}
	if cost, ok := averageTokens(rows); ok {
		t.Fatalf("partial token cost = %v, want unknown", cost)
	}
}

func TestFrontierDoesNotCompareDifferentCohorts(t *testing.T) {
	cheap := 10.0
	expensive := 100.0
	reports := []CandidateReport{
		{Cohort: "a", Summary: EvaluationSummary{Labeled: 1, TruePositive: 1}, AverageTokens: &expensive},
		{Cohort: "b", Summary: EvaluationSummary{Labeled: 1, TruePositive: 1}, AverageTokens: &cheap},
	}
	markFrontier(reports)
	if !reports[0].OnFrontier || !reports[1].OnFrontier {
		t.Fatalf("different cohorts dominated each other: %#v", reports)
	}
}

func TestCaptureDoesNotLabelSkipOrApproveAsPass(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status types.StepStatus
	}{
		{name: "approve-with-findings", status: types.StepStatusCompleted},
		{name: "skip", status: types.StepStatusSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
			defer sourceDB.Close()
			if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
				t.Fatal(err)
			}
			steps, err := sourceDB.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := sourceDB.UpdateStepStatus(steps[0].ID, tc.status); err != nil {
				t.Fatal(err)
			}
			store, err := Open(p.EvalDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cases, err := Capture(ctx, store, p, sourceDB, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(cases) != 1 || cases[0].Labels.HasGold() {
				t.Fatalf("captured labels = %#v, want unlabeled pending gold", cases)
			}
			output := RenderSets(mustInspectSets(t, store))
			if !strings.Contains(output, "0 with finding-level gold") || !strings.Contains(output, "1 unlabeled / pending") {
				t.Fatalf("sets output = %q, want unlabeled / pending, not a pass", output)
			}
			if strings.Contains(output, "park") || strings.Contains(output, "verdict") || strings.Contains(output, ", pass ") {
				t.Fatalf("sets output still uses park/pass accuracy language: %q", output)
			}
		})
	}
}

func TestCaptureWritesFalseNegativeGoldForUserAddedFinding(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	userFindings := `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"},{"id":"user-1","severity":"warning","file":"main.go","line":1,"description":"missing audit","action":"auto-fix","source":"user"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if err := sourceDB.SetStepRoundUserFindings(reviewRound.ID, &userFindings); err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug","user-1"]`
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || len(cases[0].Labels.Findings) != 2 {
		t.Fatalf("captured gold = %#v, want accepted finding plus human-added miss", cases)
	}
	byID := map[string]FindingGold{}
	for _, gold := range cases[0].Labels.Findings {
		byID[gold.ID] = gold
	}
	if got := byID["real-bug"]; got.Kind != GoldTruePositive || got.Source != goldSourceUserFix {
		t.Fatalf("selected agent finding gold = %#v, want true-positive", got)
	}
	if got := byID["user-1"]; got.Kind != GoldFalseNegative || got.Source != goldSourceUserAdded || got.Description != "missing audit" {
		t.Fatalf("user-added gold = %#v, want false-negative miss", got)
	}
}

func TestCaptureWritesUserAddedGoldWithoutSelectionSource(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	userFindings := `{"findings":[{"id":"user-1","severity":"warning","file":"main.go","line":1,"description":"missing audit","action":"auto-fix","source":"user"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`
	if err := sourceDB.SetStepRoundUserFindings(reviewRound.ID, &userFindings); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || len(cases[0].Labels.Findings) != 1 {
		t.Fatalf("captured gold = %#v, want independent human-added miss", cases)
	}
	if got := cases[0].Labels.Findings[0]; got.ID != "user-1" || got.Kind != GoldFalseNegative || got.Source != goldSourceUserAdded {
		t.Fatalf("user-added gold = %#v, want false-negative without selection evidence", got)
	}
}

func TestCaptureLeavesUnknownSelectedFindingUnlabeled(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	selected := `["user-added-write-was-lost"]`
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].Labels.HasGold() {
		t.Fatalf("captured labels = %#v, want unknown selection left unlabeled", cases)
	}
}

func TestCaptureAndReportScoresMatchingCandidateAsTruePositive(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	installFakeReviewAgent(t, p, `{"findings":[{"id":"other","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "labeled", Candidate: Candidate{Agent: types.AgentClaude, Model: "test", Vendor: "test"}, Repeats: 1}); err != nil {
		t.Fatal(err)
	} else if len(evaluations) != 1 || evaluations[0].TruePositive != 1 || evaluations[0].FalseNegative != 0 || evaluations[0].Pending != 0 {
		t.Fatalf("replay scores = %#v, want true-positive match on the same issue", evaluations)
	}
	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	output := RenderReport(reports)
	if !strings.Contains(output, "true-positive 1, false-negative 0, false-positive 0, pending 0") || !strings.Contains(output, "recall: 100.0%") {
		t.Fatalf("report = %q, want true-positive recall", output)
	}
	if strings.Contains(output, "park") || strings.Contains(output, "verdict") || strings.Contains(output, "agreement") {
		t.Fatalf("report still uses park/pass accuracy language: %q", output)
	}
}

func TestCaptureAndReportLeavesUnmatchedCandidateFindingsPending(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, reviewRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	if err := sourceDB.SetStepRoundSelection(reviewRound.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	steps, err := sourceDB.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	installFakeReviewAgent(t, p, `{"findings":[{"id":"new-issue","severity":"error","file":"main.go","line":3,"description":"unexpected later issue","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"new","risk_scope":"source-or-external"}`)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].Labels.HasGold() {
		t.Fatalf("approve capture = %#v, want unlabeled gold", cases)
	}
	if _, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test", Vendor: "test"}, Repeats: 1}); err != nil {
		t.Fatal(err)
	} else if len(evaluations) != 1 || evaluations[0].FalsePositive != 0 || evaluations[0].Pending != 1 {
		t.Fatalf("replay scores = %#v, want unmatched finding queued, not a false-positive", evaluations)
	}
	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	output := RenderReport(reports)
	if !strings.Contains(output, "unlabeled / pending") || !strings.Contains(output, "queued unmatched candidate findings: 1") {
		t.Fatalf("report = %q, want unlabeled pending, not a pass or false-positive", output)
	}
	if strings.Contains(output, "false-positive 1") || strings.Contains(output, "park") || strings.Contains(output, "verdict") {
		t.Fatalf("report punished or passed an unlabeled approve case: %q", output)
	}
}

func TestParseCandidateRequiresExplicitModelAndVendor(t *testing.T) {
	for _, input := range []string{
		"",
		"claude",
		"claude,model=opus",
		"claude,vendor=anthropic",
		"claude,model=opus,vendor=anthropic,model=sonnet",
		"claude,model=opus,vendor=anthropic,unknown=value",
	} {
		if _, err := ParseCandidate(input); err == nil {
			t.Errorf("ParseCandidate(%q) succeeded, want error", input)
		}
	}

	candidate, err := ParseCandidate(" claude,model=opus,vendor=anthropic ")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Agent != types.AgentClaude || candidate.Model != "opus" || candidate.Vendor != "anthropic" || candidate.Effort != "" {
		t.Fatalf("candidate = %#v", candidate)
	}
}

func TestParseCandidateRejectsReplacedAgentPlusModelSpelling(t *testing.T) {
	_, err := ParseCandidate("codex+gpt-5.4")
	if err == nil || !strings.Contains(err.Error(), "was replaced") || !strings.Contains(err.Error(), "vendor=") {
		t.Fatalf("ParseCandidate(agent+model) error = %v, want migration guidance", err)
	}

	candidate, err := ParseCandidate("claude,model=some+model,vendor=anthropic")
	if err != nil {
		t.Fatalf("ParseCandidate with plus in model: %v", err)
	}
	if candidate.Model != "some+model" {
		t.Fatalf("candidate = %#v", candidate)
	}
}

func TestParseCandidateCarriesEffortInIdentity(t *testing.T) {
	low, err := ParseCandidate("codex,model=gpt-5.4,vendor=openai,effort=low")
	if err != nil {
		t.Fatal(err)
	}
	if low.Effort != "low" {
		t.Fatalf("candidate effort = %q, want low", low.Effort)
	}
	if got := low.String(); got != "codex,model=gpt-5.4,vendor=openai,effort=low" {
		t.Fatalf("candidate string = %q", got)
	}
	high, err := ParseCandidate("codex,model=gpt-5.4,vendor=openai,effort=high")
	if err != nil {
		t.Fatal(err)
	}
	if high.String() == low.String() {
		t.Fatal("two efforts of one model share a candidate identity")
	}
}

func TestParseCandidateAcceptsACPModelButRejectsACPEffort(t *testing.T) {
	for _, input := range []string{
		"acp:custom,model=gpt-5,vendor=openai",
		"cursor,model=gpt-5,vendor=openai",
	} {
		if _, err := ParseCandidate(input); err != nil {
			t.Fatalf("ParseCandidate(%q) = %v, want accepted model route", input, err)
		}
	}
	if _, err := ParseCandidate("acp:custom,model=gpt-5,vendor=openai,effort=high"); err == nil || !strings.Contains(err.Error(), "effort") {
		t.Fatalf("ParseCandidate(ACP effort) error = %v, want unsupported effort", err)
	}
}

func setupCapturedRun(t *testing.T, ctx context.Context) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithFindings(t, ctx, `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`)
}

func setupCapturedRunWithFindings(t *testing.T, ctx context.Context, findings string) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithHistoryAndFindings(t, ctx, 0, findings)
}

// setupCapturedRunWithHistory builds the fixture run. padCommits adds that many
// commits of incompressible content to the default branch BEFORE the reviewed
// branch is cut, so the padding is real ancestry of every commit a case pins -
// which is what makes duplicated history measurable.
func setupCapturedRunWithHistory(t *testing.T, ctx context.Context, padCommits int) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	return setupCapturedRunWithHistoryAndFindings(t, ctx, padCommits, `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`)
}

func setupCapturedRunWithHistoryAndFindings(t *testing.T, ctx context.Context, padCommits int, findings string) (*paths.Paths, *db.DB, *db.Run, *db.Repo, *db.StepRound) {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}

	gateDir := p.RepoDir("eval-repo")
	if err := git.InitBare(ctx, gateDir); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(root, "source")
	mustGit(t, ctx, root, "clone", gateDir, workDir)
	mustGit(t, ctx, workDir, "config", "user.email", "eval@example.test")
	mustGit(t, ctx, workDir, "config", "user.name", "Eval Test")
	if err := os.WriteFile(filepath.Join(workDir, ".no-mistakes.yaml"), []byte("review:\n  path_instructions:\n    - path: '*.go'\n      instructions: review error paths\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", ".")
	mustGit(t, ctx, workDir, "commit", "-m", "base")
	mustGit(t, ctx, workDir, "branch", "-M", "main")
	padHistory(t, ctx, workDir, padCommits)
	mustGit(t, ctx, workDir, "push", "origin", "main")
	baseSHA := mustGit(t, ctx, workDir, "rev-parse", "HEAD")
	mustGit(t, ctx, workDir, "checkout", "-b", "feature/eval")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package sample\n\nfunc Changed() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", "main.go")
	mustGit(t, ctx, workDir, "commit", "-m", "change")
	mustGit(t, ctx, workDir, "push", "origin", "feature/eval")
	headSHA := mustGit(t, ctx, workDir, "rev-parse", "HEAD")

	repo, err := database.InsertRepoWithID("eval-repo", workDir, "https://secret-token@example.test/org/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/eval", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	repoConfigYAML, err := os.ReadFile(filepath.Join(workDir, ".no-mistakes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reviewRound, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, baseSHA, []byte("{}\n"), repoConfigYAML, 50)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug"]`
	if err := database.SetStepRoundSelection(reviewRound.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	return p, database, run, repo, reviewRound
}

func mustGit(t *testing.T, ctx context.Context, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func mustInspectSets(t *testing.T, store *Store) []SetSummary {
	t.Helper()
	summaries, err := InspectSets(store)
	if err != nil {
		t.Fatal(err)
	}
	return summaries
}

func installFakeReviewAgent(t *testing.T, p *paths.Paths, findingsJSON string) {
	t.Helper()
	fakeDir := t.TempDir()
	fake := filepath.Join(fakeDir, "claude")
	reply := `{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"review"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":` + findingsJSON + `,"usage":{"input_tokens":12,"output_tokens":3}}
`
	var script string
	if runtime.GOOS == "windows" {
		fake += ".cmd"
		script = "@echo off\r\nmore >nul\r\necho " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\n[ \"$NM_HOME\" = \"" + p.Root() + "\" ] && touch \"" + p.Root() + "/shared-home-used\"\ncat >/dev/null\ncat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
}
