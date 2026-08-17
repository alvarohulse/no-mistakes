package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type recoveredCIEnvStep struct {
	inner *CIStep
	env   []string
}

func (s *recoveredCIEnvStep) Name() types.StepName { return types.StepCI }

func (s *recoveredCIEnvStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	sctx.Env = append([]string(nil), s.env...)
	return s.inner.Execute(sctx)
}

func TestCIStep_CIFailureAutoFix(t *testing.T) {
	t.Parallel()
	// Set up upstream bare repo for push
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"FAILURE","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	agentCalled := false
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCalled = true
			// Agent "fixes" CI by creating a file
			os.WriteFile(filepath.Join(opts.CWD, "ci-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Provider: "cursor", Model: "gpt-5.6-terra-medium"}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.UserIntent = "user wanted CI autofix to preserve the extracted intent"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 2 {
				cancel()
			}
			return ctx.Err()
		},
	}
	_, err := step.Execute(sctx)
	// Expect explicit context cancellation after the second poll, once the post-fix wait path is exercised.
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if !agentCalled {
		t.Error("expected agent to be called for CI auto-fix")
	}

	if len(ag.calls) == 0 {
		t.Fatal("expected agent call")
	}

	foundAutoFix := false
	for _, l := range logs {
		if strings.Contains(l, "issues detected") && strings.Contains(l, "auto-fixing") {
			foundAutoFix = true
			break
		}
	}
	if !foundAutoFix {
		t.Errorf("expected issue detection in logs, got: %v", logs)
	}
	body := gitCmd(t, dir, "log", "-1", "--pretty=%B")
	if !strings.HasPrefix(body, "fix(ci): apply CI fixes\n") {
		t.Fatalf("CI fix commit body starts with %q", body)
	}
	if !strings.Contains(body, "Co-authored-by: cursoragent <cursoragent@cursor.com>") {
		t.Fatalf("CI fix commit lacks Cursor attribution:\n%s", body)
	}
	if !strings.Contains(body, "No-Mistakes-Model: gpt-5.6-terra-medium") {
		t.Fatalf("CI fix commit lacks model attribution:\n%s", body)
	}
}

func TestCIStep_CIAutoFixDisabledWithZero(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	checksJSON := `[
		{"name":"build","state":"SUCCESS","bucket":"pass"},
		{"name":"test","state":"FAILURE","bucket":"fail"},
		{"name":"lint","state":"ACTION_REQUIRED","bucket":"fail"},
		{"name":"deploy","state":"NEUTRAL"}
	]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	ag := &mockAgent{name: "test"}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0} // disabled
	sctx.Config.CITimeout = 3 * time.Second

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed when CI auto-fix is disabled")
	}
	if outcome.AutoFixable {
		t.Fatal("expected manual intervention outcome to be non-auto-fixable")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if findings.Summary != "CI failures require manual intervention" {
		t.Fatalf("findings summary = %q, want %q", findings.Summary, "CI failures require manual intervention")
	}
	if len(findings.Items) != 2 {
		t.Fatalf("expected 2 failing-check findings, got %d: %+v", len(findings.Items), findings.Items)
	}
	if findings.Items[0].Description != "CI check failing: lint" {
		t.Fatalf("first finding = %q, want %q", findings.Items[0].Description, "CI check failing: lint")
	}
	if findings.Items[1].Description != "CI check failing: test" {
		t.Fatalf("second finding = %q, want %q", findings.Items[1].Description, "CI check failing: test")
	}

	// Agent should NOT have been called
	if len(ag.calls) > 0 {
		t.Errorf("expected no agent calls when ci=0, got %d", len(ag.calls))
	}

	// Should log that auto-fix is disabled
	foundDisabled := false
	for _, l := range logs {
		if strings.Contains(l, "auto-fix disabled") {
			foundDisabled = true
			break
		}
	}
	if !foundDisabled {
		t.Errorf("expected 'auto-fix disabled' in logs, got: %v", logs)
	}
}

func TestCIStep_CIAutoFixLimitExhausted(t *testing.T) {
	t.Parallel()
	// Set up upstream bare repo for push
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			// Agent "fixes" but the check will keep failing (same checksJSON)
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1} // only 1 attempt allowed

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed when CI auto-fix limit is exhausted")
	}
	if outcome.AutoFixable {
		t.Fatal("expected exhausted CI outcome to be non-auto-fixable")
	}

	// Agent should have been called exactly once (limit is 1)
	if fixCount != 1 {
		t.Errorf("expected 1 auto-fix attempt (limit=1), got %d", fixCount)
	}
	if pollCount != 1 {
		t.Errorf("expected 1 poll wait before limit-exhausted outcome, got %d", pollCount)
	}

	// The same normalized failure is the earlier stop condition on the
	// subsequent poll; no second attempt is spent.
	foundRepeated := false
	for _, l := range logs {
		if strings.Contains(l, "normalized failure fingerprint repeated") {
			foundRepeated = true
			break
		}
	}
	if !foundRepeated {
		t.Errorf("expected repeated-failure stop in logs, got: %v", logs)
	}
}

func TestCIStep_RestartDoesNotResetAutoFixBudget(t *testing.T) {
	t.Parallel()
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	failed := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	pending := `[{"name":"test","status":"IN_PROGRESS","bucket":"pending"}]`

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			if err := os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("restart-fix-%d.txt", fixCount)), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.PRURL = &prURL
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.Env = fakeCIGHSequence(t, "OPEN", []string{failed, pending, failed})

	first := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}
	firstOutcome, err := first.Execute(sctx)
	if err != nil {
		t.Fatalf("initial CI execution: %v", err)
	}
	if !firstOutcome.NeedsApproval || fixCount != 1 {
		t.Fatalf("initial CI outcome = %+v, fixes = %d; want parked after one automatic fix", firstOutcome, fixCount)
	}

	recoveredRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recovered := *sctx
	recovered.Run = recoveredRun
	recovered.Fixing = true
	recovered.Env = fakeCIGHSequence(t, "OPEN", []string{failed, pending, failed, pending, failed})

	resumed := &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }}
	resumedOutcome, err := resumed.Execute(&recovered)
	if err != nil {
		t.Fatalf("recovered CI execution: %v", err)
	}
	if !resumedOutcome.NeedsApproval {
		t.Fatalf("recovered CI outcome = %+v, want parked", resumedOutcome)
	}
	if fixCount != 2 {
		t.Fatalf("CI fixes across restart = %d, want one automatic plus the explicit user fix", fixCount)
	}
	if resumedOutcome.RepairAudit.Result != pipeline.RepairResultAttemptLimit {
		t.Fatalf("recovered repair audit = %+v, want exhausted persisted budget", resumedOutcome.RepairAudit)
	}
}

func TestCIStep_RecoveredLegacyBudgetAllowsOnlyExplicitUserFix(t *testing.T) {
	dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
	failedBeforeFix := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":"2026-08-17T01:00:00Z"}]`
	failedAfterFix := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":"2026-08-17T01:01:00Z"}]`
	pending := `[{"name":"test","status":"IN_PROGRESS","bucket":"pending"}]`

	var fixCount atomic.Int32
	ag := &mockAgent{
		name: "test",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount.Add(1)
			if err := os.WriteFile(filepath.Join(opts.CWD, "legacy-user-fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	sctx.Repo.UpstreamURL = upstream
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.StartStepWithAutoFixLimit(stepResult.ID, pipeline.MaxRepairAttempts); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-1","severity":"error","description":"CI check failing: test","action":"ask-user"}],"summary":"CI failures require manual intervention"}`
	if err := sctx.DB.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusFixReview, 25); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetRunAwaitingAgent(sctx.Run.ID); err != nil {
		t.Fatal(err)
	}
	recoveredRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A run created before ci_fix_attempts existed migrates with NULL. This is
	// the StepContext state Resume passes back into CI after the user chooses Fix.
	recoveredRun.CIFixAttempts = nil
	recoveredRun.PRURL = &prURL
	recoveredRun.Branch = "refs/heads/feature"
	cfg := &config.Config{CITimeout: 30 * time.Second, AutoFix: config.AutoFix{CI: 3}}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	step := &recoveredCIEnvStep{
		inner: &CIStep{waitForNextPoll: func(context.Context, time.Duration) error { return nil }},
		env:   fakeCIGHSequence(t, "OPEN", []string{failedBeforeFix, pending, failedAfterFix}),
	}
	executor := pipeline.NewExecutor(sctx.DB, p, cfg, ag, []pipeline.Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- executor.Resume(context.Background(), recoveredRun, sctx.Repo, dir) }()

	responseDeadline := time.Now().Add(30 * time.Second)
	for {
		if err := executor.Respond(types.StepCI, types.ActionFix, []string{"ci-1"}); err == nil {
			break
		}
		if time.Now().After(responseDeadline) {
			t.Fatal("recovered legacy CI gate never accepted the explicit user fix")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var repairResult string
	repairDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(repairDeadline) {
		rounds, roundsErr := sctx.DB.GetRoundsByStep(stepResult.ID)
		if roundsErr != nil {
			t.Fatal(roundsErr)
		}
		if len(rounds) >= 2 && rounds[len(rounds)-1].RepairResult != nil {
			repairResult = *rounds[len(rounds)-1].RepairResult
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fixCount.Load() != 1 || repairResult != pipeline.RepairResultAttemptLimit {
		t.Fatalf("recovered legacy CI fixes = %d repair result = %q, want explicit fix only and exhausted automatic budget", fixCount.Load(), repairResult)
	}
	if err := executor.Respond(types.StepCI, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "aborted by user") {
			t.Fatalf("recovered legacy CI completion error = %v, want user abort", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("recovered legacy CI executor did not stop")
	}
}

func TestCIStep_CIAutoFixStopsOnRepeatedFailureAfterChecksRerun(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksSequence := []string{
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 2}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval after exhausting rerun-backed retries")
	}
	if outcome.AutoFixable {
		t.Fatal("expected exhausted CI outcome to be non-auto-fixable")
	}
	if fixCount != 1 {
		t.Fatalf("expected repeated failure to stop after 1 auto-fix attempt, got %d", fixCount)
	}
	if pollCount != 2 {
		t.Fatalf("expected 2 poll waits before the repeated post-rerun failure, got %d", pollCount)
	}

	foundRepeated := false
	for _, l := range logs {
		if strings.Contains(l, "normalized failure fingerprint repeated") {
			foundRepeated = true
			break
		}
	}
	if !foundRepeated {
		t.Fatalf("expected repeated-failure stop after CI reran, got: %v", logs)
	}
}

func TestCIStep_CIAutoFixStopsRepeatedFailureWhenGitHubClockLagsLocalClock(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	start := time.Date(2026, 4, 24, 4, 14, 0, 0, time.UTC)
	oldCompletedAt := start.Add(1 * time.Minute).Format(time.RFC3339)
	newCompletedAt := start.Add(2 * time.Minute).Format(time.RFC3339)
	checksSequence := []string{
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, oldCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 5 * time.Minute
	sctx.Config.AutoFix = config.AutoFix{CI: 2}

	localNow := start.Add(30 * time.Minute)
	step := &CIStep{
		now: func() time.Time { return localNow },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			localNow = localNow.Add(3 * time.Minute)
			return nil
		},
	}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval after exhausting rerun-backed retries")
	}
	if fixCount != 1 {
		t.Fatalf("expected repeated failure to stop after 1 attempt when GitHub timestamps advance, got %d", fixCount)
	}
}

// TestCIStep_CIAutoFixStopsRepeatedFailureWhenFastChecksSkipPendingObservation reproduces
// the real-world scenario where a failing CI check completes so fast between
// polls that the pipeline never observes it in a pending state, but the check's
// completedAt timestamp moves past the last-fix time - proving CI re-ran. The
// pipeline should recognize it as a fresh observation, then stop because the
// normalized failure itself repeated rather than looping indefinitely.
func TestCIStep_CIAutoFixStopsRepeatedFailureWhenFastChecksSkipPendingObservation(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Simulate a fake "now" that advances across polls. The failing check's
	// completedAt on poll 2 is after the autofix push time, proving CI re-ran.
	// But neither poll observes a pending state - the pipeline must detect
	// the rerun from completedAt.
	start := time.Date(2026, 4, 24, 4, 14, 0, 0, time.UTC)
	oldCompletedAt := start.Add(1 * time.Minute).Format(time.RFC3339)  // pre-fix failure
	newCompletedAt := start.Add(10 * time.Minute).Format(time.RFC3339) // post-fix failure (rerun)
	checksSequence := []string{
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, oldCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
		fmt.Sprintf(`[{"name":"e2e","status":"COMPLETED","conclusion":"failure","bucket":"fail","completedAt":%q}]`, newCompletedAt),
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 1 * time.Hour
	sctx.Config.AutoFix = config.AutoFix{CI: 2}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	fakeNow := start
	step := &CIStep{
		now: func() time.Time { return fakeNow },
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			// Advance fake clock past the autofix push so the second poll's
			// check completedAt looks "after" lastFixedAt.
			fakeNow = fakeNow.Add(3 * time.Minute)
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval after exhausting rerun-backed retries")
	}
	if fixCount != 1 {
		t.Fatalf("expected repeated post-push failure to stop after 1 attempt, got %d", fixCount)
	}

	foundRepeated := false
	for _, l := range logs {
		if strings.Contains(l, "normalized failure fingerprint repeated") {
			foundRepeated = true
			break
		}
	}
	if !foundRepeated {
		t.Fatalf("expected repeated-failure log after completedAt proved the rerun, got: %v", logs)
	}
}

// TestCIStep_CIAutoFixStopsRepeatedFailureWhenSomeChecksStayFailing reproduces the real-world
// scenario where multiple checks fail, the fix push causes only some of them to
// re-run (and thus transit through pending) while at least one check keeps
// reporting as failing throughout. The pipeline recognizes the post-rerun
// observation and stops on the repeated normalized check set rather than
// logging "fix already attempted" indefinitely until CI timeout.
func TestCIStep_CIAutoFixStopsRepeatedFailureWhenSomeChecksStayFailing(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// At least one check stays failing throughout the push+rerun transition,
	// so `failing` is never empty and the original "all pass" reset never fires.
	checksSequence := []string{
		`[{"name":"a","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"IN_PROGRESS","bucket":"pending"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"IN_PROGRESS","bucket":"pending"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"a","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"b","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 2}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval after exhausting rerun-backed retries")
	}
	if fixCount != 1 {
		t.Fatalf("expected repeated check set to stop after 1 attempt, got %d", fixCount)
	}

	foundRepeated := false
	for _, l := range logs {
		if strings.Contains(l, "normalized failure fingerprint repeated") {
			foundRepeated = true
			break
		}
	}
	if !foundRepeated {
		t.Fatalf("expected repeated-failure stop after rerun-backed observation, got: %v", logs)
	}
}

func TestCIStep_DoesNotRetryOnUnrelatedPendingCheck(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksSequence := []string{
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"},{"name":"docs","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
		`[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`,
	}
	env := fakeCIGHSequence(t, "OPEN", checksSequence)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("fix-%d.txt", fixCount)), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 2}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 3 {
				cancel()
			}
			return ctx.Err()
		},
	}

	_, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation after observing repeated stale failure, got %v", err)
	}
	if fixCount != 1 {
		t.Fatalf("expected unrelated pending checks not to trigger a second auto-fix attempt, got %d", fixCount)
	}

	foundWait := false
	for _, l := range logs {
		if strings.Contains(l, "fix already attempted for these issues") {
			foundWait = true
			break
		}
	}
	if !foundWait {
		t.Fatalf("expected stale failures to stay guarded while unrelated checks finish, got logs: %v", logs)
	}
}

func TestCIStep_StopsRepeatedMergeConflictAfterRerun(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksSequence := []string{
		`[{"name":"build","status":"COMPLETED","conclusion":"success","bucket":"pass"}]`,
		`[{"name":"build","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","status":"COMPLETED","conclusion":"success","bucket":"pass"}]`,
		`[{"name":"build","status":"IN_PROGRESS","bucket":"pending"}]`,
		`[{"name":"build","status":"COMPLETED","conclusion":"success","bucket":"pass"}]`,
	}
	env := fakeCIGHSequenceMergeable(t, "OPEN", checksSequence, "CONFLICTING")

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, fmt.Sprintf("conflict-fix-%d.txt", fixCount)), []byte("resolved"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 2}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome after retries, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval after exhausting conflict rerun-backed retries")
	}
	if fixCount != 1 {
		t.Fatalf("expected repeated merge conflict to stop after 1 attempt, got %d", fixCount)
	}

	foundRepeated := false
	for _, l := range logs {
		if strings.Contains(l, "normalized failure fingerprint repeated") {
			foundRepeated = true
			break
		}
	}
	if !foundRepeated {
		t.Fatalf("expected repeated-failure log after conflict rerun, got: %v", logs)
	}
}

func TestCIStep_FixMode_ManualInterventionRunsCIFix(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			os.WriteFile(filepath.Join(opts.CWD, "manual-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix failing CI"}`)}, nil
		},
	}

	findingsJSON, err := json.Marshal(Findings{
		Summary: "CI failures require manual intervention",
		Items: []Finding{{
			ID:          "review-1",
			Severity:    "warning",
			Description: "CI check failing: test",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Fixing = true
	sctx.PreviousFindings = string(findingsJSON)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			if pollCount == 2 {
				cancel()
			}
			return ctx.Err()
		},
	}
	_, err = step.Execute(sctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation after manual CI fix attempt, got %v", err)
	}
	if fixCount != 1 {
		t.Fatalf("expected 1 manual CI fix attempt, got %d", fixCount)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
}

// TestCIStep_AutoFixNoChangesStopsImmediately verifies that an unchanged Git
// content state consumes one attempt and then stops instead of retrying.
func TestCIStep_AutoFixNoChangesStopsImmediately(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			// Agent "investigates" but produces NO changes
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 2}

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed after exhausting fix attempts with no changes")
	}
	if outcome.RepairAudit.Result != pipeline.RepairResultNoProgress || outcome.RepairAudit.FailureFingerprint == "" {
		t.Fatalf("repair audit = %+v, want content-free no-progress receipt", outcome.RepairAudit)
	}

	if fixCount != 1 {
		t.Fatalf("expected no-progress repair to stop after 1 attempt, got %d", fixCount)
	}

	foundNoProgress := false
	for _, l := range logs {
		if strings.Contains(l, "worktree and HEAD made no content progress") {
			foundNoProgress = true
			break
		}
	}
	if !foundNoProgress {
		t.Errorf("expected no-progress stop in logs, got: %v", logs)
	}

	// Should never log "fix already attempted" indefinitely
	waitCount := 0
	for _, l := range logs {
		if strings.Contains(l, "fix already attempted") {
			waitCount++
		}
	}
	if waitCount > 0 {
		t.Errorf("expected no 'fix already attempted' loops when agent produces no changes, got %d", waitCount)
	}
}

// TestCIStep_FixMode_NoChanges_CountsAsAttempt verifies the same no-changes
// behavior for manual fix mode (sctx.Fixing = true).
func TestCIStep_FixMode_NoChanges_CountsAsAttempt(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	fixCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			fixCount++
			// Agent produces NO changes
			return &agent.Result{}, nil
		},
	}

	findingsJSON, err := json.Marshal(Findings{
		Summary: "CI failures require manual intervention",
		Items: []Finding{{
			Severity:    "warning",
			Description: "CI check failing: test",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0}
	sctx.Fixing = true
	sctx.PreviousFindings = string(findingsJSON)

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	pollCount := 0
	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			pollCount++
			return nil
		},
	}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected approval outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected approval needed after fix mode with no changes")
	}

	if fixCount != 1 {
		t.Fatalf("expected 1 manual fix attempt, got %d", fixCount)
	}

	// Should return failure outcome, not spin forever
	foundFailed := false
	for _, l := range logs {
		if strings.Contains(l, "CI fix produced no changes") {
			foundFailed = true
			break
		}
	}
	if !foundFailed {
		t.Errorf("expected 'CI fix produced no changes' in logs, got: %v", logs)
	}
}

// TestCIStep_AutoFixPromptIncludesMustFixInstruction verifies the agent prompt
// includes a strong instruction that the agent must produce changes.
func TestCIStep_AutoFixPromptIncludesMustFixInstruction(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGH(t, "OPEN", checksJSON)

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.UserIntent = "user wanted CI autofix to preserve the extracted intent"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	sctx.Log = func(s string) {}

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	step.Execute(sctx)

	if capturedPrompt == "" {
		t.Fatal("expected agent to be called with a prompt")
	}
	if !strings.Contains(capturedPrompt, "You MUST produce file changes") {
		t.Errorf("prompt should instruct agent to produce changes, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "smallest correct root-cause fix") {
		t.Errorf("prompt should prefer root-cause fixes over bandaids, got:\n%s", capturedPrompt)
	}
	assertTestQualityRulePrompt(t, capturedPrompt)
	if strings.Contains(capturedPrompt, "Make the minimal change needed") {
		t.Errorf("prompt should not prefer narrow minimal changes, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "user wanted CI autofix to preserve the extracted intent") {
		t.Errorf("prompt should include extracted user intent, got:\n%s", capturedPrompt)
	}
}
