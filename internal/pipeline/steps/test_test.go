package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestStep_FixMode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	previousFindings := `{"items":[{"id":"test-1 =======","severity":"error","file":"internal/pipeline/steps/test.go >>>>>>> prompt","description":"tests failed with exit code 1 <<<<<<< HEAD"}],"summary":"FAIL: TestFoo expected 42 got 0 ======="}`

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"  \"fix test failures.\"  "}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings
	sctx.Config.Prompts = config.PromptConfig{
		Shared: "shared prompt config",
		Test:   "test prompt config",
	}

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after fix + passing tests")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call (fix), got %d", callCount)
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "FAIL: TestFoo expected 42 got 0") {
		t.Error("expected fix prompt to contain previous test failure summary")
	}
	if strings.Contains(ag.calls[0].Prompt, "test-1 =======") {
		t.Error("expected test fix prompt to sanitize finding IDs")
	}
	if strings.Contains(ag.calls[0].Prompt, "test.go >>>>>>> prompt") {
		t.Error("expected test fix prompt to sanitize finding file paths")
	}
	if strings.Contains(ag.calls[0].Prompt, "<<<<<<< HEAD") {
		t.Error("expected test fix prompt to exclude merge markers")
	}
	if !strings.Contains(ag.calls[0].Prompt, "smallest correct root-cause fix") {
		t.Error("expected test fix prompt to prefer root-cause fixes over bandaids")
	}
	if !strings.Contains(ag.calls[0].Prompt, "shared prompt config") {
		t.Fatalf("expected test fix prompt to include shared prompt config, got:\n%s", ag.calls[0].Prompt)
	}
	if !strings.Contains(ag.calls[0].Prompt, "test prompt config") {
		t.Fatalf("expected test fix prompt to include test prompt config, got:\n%s", ag.calls[0].Prompt)
	}
	if strings.Index(ag.calls[0].Prompt, "shared prompt config") > strings.Index(ag.calls[0].Prompt, "test prompt config") {
		t.Fatalf("expected shared prompt config before test prompt config, got:\n%s", ag.calls[0].Prompt)
	}
	if !strings.Contains(ag.calls[0].Prompt, "remove any transient artifacts your testing created in the working tree") {
		t.Error("expected test fix prompt to ask the agent to clean up transient testing artifacts before finishing")
	}
	if strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected test fix prompt not to prefer narrow minimal changes")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_UsesConfiguredCommitMessage(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix test failures"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Config.Commit = config.Commit{FixMessage: "fix({{.Step}}): {{.Summary}}"}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fix and passing tests")
	}
	if got := lastCommitMessage(t, dir); got != "fix(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_UsesFallbackSummaryWhenStructuredSummaryMalformed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"not_summary":"oops"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fallback summary commit and passing tests")
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_AgentWritesNewTests_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			// Simulate agent creating a new test file during fix in another supported language
			os.WriteFile(filepath.Join(dir, "component behavior.spec.tsx"), []byte("export {}\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"add regression test"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval when the agent writes a new test file")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call in fix mode, got %d", callCount)
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "component behavior.spec.tsx") {
			foundTestFile = true
			if item.Severity != "warning" {
				t.Errorf("expected new-test-file finding severity warning, got %q", item.Severity)
			}
			if item.Action != types.ActionAskUser {
				t.Errorf("expected new-test-file finding action %q, got %q", types.ActionAskUser, item.Action)
			}
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning component behavior.spec.tsx, got findings: %+v", f.Items)
	}
}

func TestTestStep_FixModeTestChangesSurviveEvidenceAgent(t *testing.T) {
	for _, test := range []struct {
		name     string
		file     string
		original string
		updated  string
	}{
		{name: "created", file: "new behavior_test.go", updated: "package behavior\n"},
		{name: "modified", file: "existing behavior_test.go", original: "package behavior\n", updated: "package behavior\n\n// changed assertion\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			if test.original != "" {
				if err := os.WriteFile(filepath.Join(dir, test.file), []byte(test.original), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", test.file)
				gitCmd(t, dir, "commit", "-m", "add existing test")
				headSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			}

			callCount := 0
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					callCount++
					if callCount == 1 {
						if err := os.WriteFile(filepath.Join(dir, test.file), []byte(test.updated), 0o644); err != nil {
							return nil, err
						}
						return &agent.Result{Output: json.RawMessage(`{"summary":"fix failing test"}`)}, nil
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"evidence passed","tested":["focused check"],"testing_summary":"focused evidence passed"}`)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
			sctx.Fixing = true
			sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed","action":"auto-fix"}],"summary":"tests failed"}`
			sctx.UserIntent = "Keep the focused behavior working"

			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if callCount != 2 {
				t.Fatalf("agent calls = %d, want fixer and evidence agent", callCount)
			}
			if !outcome.NeedsApproval {
				t.Fatal("expected fixer test-file change to require approval after evidence agent")
			}
			var findings Findings
			if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || findings.Items[0].File != test.file || findings.Items[0].Action != types.ActionAskUser {
				t.Fatalf("fixer test-file findings = %+v", findings.Items)
			}
		})
	}
}

func TestTestStep_TestChangesSurviveFailedFixRound(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "existing_test.go"), []byte("package product\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "existing_test.go")
	gitCmd(t, dir, "commit", "-m", "add existing test")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	fixCalls := 0
	var sctx *pipeline.StepContext
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			fixCalls++
			path := "existing_test.go"
			contents := "package product\n\n// changed assertion\n"
			if fixCalls == 1 {
				path = "regression_test.go"
				contents = "package product\n"
			} else {
				sctx.Config.Commands.Test = "exit 0"
			}
			if err := os.WriteFile(filepath.Join(dir, path), []byte(contents), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix failing test"}`)}, nil
		},
	}
	sctx = newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 1"})
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	exec := pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, []pipeline.Step{&TestStep{}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, sctx.Run, sctx.Repo, dir)
	}()

	waitForRound := func(round int, status types.StepStatus) Findings {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case err := <-done:
				t.Fatalf("pipeline completed before round %d approval: %v", round, err)
			default:
			}
			steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) == 1 && steps[0].Status == status {
				rounds, err := sctx.DB.GetRoundsByStep(steps[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(rounds) >= round && steps[0].FindingsJSON != nil {
					var findings Findings
					if err := json.Unmarshal([]byte(*steps[0].FindingsJSON), &findings); err != nil {
						t.Fatal(err)
					}
					return findings
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for round %d status %s", round, status)
		return Findings{}
	}
	findTestFailureID := func(findings Findings) string {
		t.Helper()
		for _, finding := range findings.Items {
			if strings.HasPrefix(finding.Description, "tests failed with exit code") {
				return finding.ID
			}
		}
		t.Fatalf("test failure finding missing from %+v", findings.Items)
		return ""
	}

	initial := waitForRound(1, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepTest, types.ActionFix, []string{findTestFailureID(initial)}); err != nil {
		t.Fatal(err)
	}
	intermediate := waitForRound(2, types.StepStatusFixReview)
	if len(intermediate.Items) != 2 {
		t.Fatalf("intermediate findings = %+v, want test failure and test-file change", intermediate.Items)
	}
	if err := exec.Respond(types.StepTest, types.ActionFix, []string{findTestFailureID(intermediate)}); err != nil {
		t.Fatal(err)
	}
	final := waitForRound(3, types.StepStatusFixReview)
	if len(final.Items) != 2 {
		t.Fatalf("final findings = %+v, want both accumulated test-file approvals", final.Items)
	}
	for _, file := range []string{"existing_test.go", "regression_test.go"} {
		found := false
		for _, finding := range final.Items {
			if finding.File == file && finding.Action == types.ActionAskUser {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("final findings = %+v, missing retained approval for %s", final.Items, file)
		}
	}
	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not complete after approval")
	}
}

func TestTestStep_EvidenceAgentTestFileMutations_NeedOneExplicitApproval(t *testing.T) {
	tests := []struct {
		name            string
		oldPath         string
		newPath         string
		mutation        string
		wantFile        string
		descriptionText []string
	}{
		{
			name:            "modified",
			oldPath:         "component behavior_test.go",
			newPath:         "component behavior_test.go",
			mutation:        "modify",
			wantFile:        "component behavior_test.go",
			descriptionText: []string{"existing test file modified by agent"},
		},
		{
			name:            "deleted",
			oldPath:         "component behavior_test.go",
			mutation:        "delete",
			wantFile:        "component behavior_test.go",
			descriptionText: []string{"existing test file deleted by agent"},
		},
		{
			name:            "renamed/test to test",
			oldPath:         "old component_test.go",
			newPath:         "new component_test.go",
			mutation:        "rename",
			wantFile:        "new component_test.go",
			descriptionText: []string{"test file renamed by agent", "old component_test.go", "new component_test.go"},
		},
		{
			name:            "renamed/test to non-test",
			oldPath:         "old component_test.go",
			newPath:         "component.go",
			mutation:        "rename",
			wantFile:        "component.go",
			descriptionText: []string{"test file renamed by agent", "old component_test.go", "component.go"},
		},
		{
			name:            "renamed/non-test to test",
			oldPath:         "old component.go",
			newPath:         "new component_test.go",
			mutation:        "rename",
			wantFile:        "new component_test.go",
			descriptionText: []string{"test file renamed by agent", "old component.go", "new component_test.go"},
		},
	}
	for _, tt := range tests {
		for _, staged := range []bool{false, true} {
			name := tt.name + "/unstaged"
			if staged {
				name = tt.name + "/staged"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				dir, baseSHA, _ := setupGitRepo(t)
				oldFullPath := filepath.Join(dir, tt.oldPath)
				newFullPath := filepath.Join(dir, tt.newPath)
				if err := os.WriteFile(oldFullPath, []byte("package component\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", tt.oldPath)
				gitCmd(t, dir, "commit", "-m", "add component file")
				headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

				ag := &mockAgent{
					name: "test",
					runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
						var err error
						switch tt.mutation {
						case "modify":
							err = os.WriteFile(oldFullPath, []byte("package component\n\n// weakened assertion\n"), 0o644)
						case "delete":
							err = os.Remove(oldFullPath)
						case "rename":
							err = os.Rename(oldFullPath, newFullPath)
						}
						if err != nil {
							return nil, err
						}
						if staged {
							gitCmd(t, dir, "add", "--all")
						}
						return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"tests passed","tested":["go test ./..."],"testing_summary":"tests passed"}`)}, nil
					},
				}
				sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

				outcome, err := (&TestStep{}).Execute(sctx)
				if err != nil {
					t.Fatal(err)
				}
				if !outcome.NeedsApproval {
					t.Fatal("expected approval when the agent mutates a test file")
				}

				var findings Findings
				if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
					t.Fatal(err)
				}
				if len(findings.Items) != 1 {
					t.Fatalf("expected one test-file finding, got %+v", findings.Items)
				}
				finding := findings.Items[0]
				if finding.Severity != "warning" || finding.Action != types.ActionAskUser {
					t.Fatalf("test-file finding = severity %q action %q, want warning/%s", finding.Severity, finding.Action, types.ActionAskUser)
				}
				if finding.File != tt.wantFile {
					t.Fatalf("test-file finding path = %q, want %q", finding.File, tt.wantFile)
				}
				for _, text := range tt.descriptionText {
					if !strings.Contains(finding.Description, text) {
						t.Fatalf("test-file finding description = %q, want %q", finding.Description, text)
					}
				}
				if !finding.RequiresExplicitApproval {
					t.Fatal("test-file finding must require explicit approval")
				}
			})
		}
	}
}

func TestTestStep_RetainsDeletionAndRenameApprovalsFromPreviousRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"tests passed","tested":["focused check"],"testing_summary":"tests passed"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID

	const deletedPath = "deleted behavior_test.go"
	const oldRenamePath = "old -> behavior_test.go"
	const newRenamePath = "new -> behavior_test.go"
	priorFindings, err := json.Marshal(Findings{Items: testFileChangeFindings([]testFileChange{
		{Path: deletedPath, Kind: testFileDeleted},
		{Path: newRenamePath, PreviousPath: oldRenamePath, Kind: testFileRenamed},
	})})
	if err != nil {
		t.Fatal(err)
	}
	priorFindingsJSON := string(priorFindings)
	if _, err := sctx.DB.InsertStepRound(sctx.StepResultID, 1, "initial", &priorFindingsJSON, nil, 10); err != nil {
		t.Fatal(err)
	}

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected prior test-file mutations to continue requiring approval")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 2 {
		t.Fatalf("retained findings = %+v, want deletion and rename", findings.Items)
	}
	for _, finding := range findings.Items {
		if finding.Action != types.ActionAskUser || !finding.RequiresExplicitApproval {
			t.Fatalf("retained finding = %+v, want explicit ask-user approval", finding)
		}
	}
	if findings.Items[0].File != deletedPath {
		t.Fatalf("deletion path = %q, want %q", findings.Items[0].File, deletedPath)
	}
	if findings.Items[1].File != newRenamePath || !strings.Contains(findings.Items[1].Description, oldRenamePath) {
		t.Fatalf("rename finding = %+v, want %q -> %q", findings.Items[1], oldRenamePath, newRenamePath)
	}
}

func TestTestStep_UserIntentRunsConfiguredCommandThenEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	baselineLog := filepath.Join(dir, "baseline.log")
	testCmd := "go env GOOS > baseline.log"

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"evidence demonstrates intent","tested":["manual screenshot review"],"testing_summary":"captured screenshot evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})
	sctx.UserIntent = "Show users a success screen after checkout"
	sctx.Config.Prompts = config.PromptConfig{
		Shared: "shared prompt config",
		Test:   "test prompt config",
	}

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when evidence-oriented agent testing passes")
	}
	if callCount != 1 {
		t.Fatalf("expected evidence agent to run after configured test command, got %d calls", callCount)
	}
	data, err := os.ReadFile(baselineLog)
	if err != nil {
		t.Fatalf("expected configured test command to run: %v", err)
	}
	if strings.TrimSpace(string(data)) != runtime.GOOS {
		t.Fatalf("configured test command output = %q, want %s", string(data), runtime.GOOS)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Show users a success screen after checkout",
		"Decide what evidence or artifacts would clearly demonstrate the user intent is satisfied",
		"Unit tests passing is not sufficient evidence by itself",
		"Demonstrate the user intent working end-to-end in a way consistent with how an end user would actually experience it",
		"Prefer product-level artifacts",
		"Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior",
		"Configured test command already ran successfully as baseline",
		testCmd,
		"The \"testing_summary\" must account for the complete test step: baseline commands that already ran, automated tests, manual or evidence-producing checks, artifacts gathered, and the overall result",
		"screenshots, GIFs, videos, rendered UI, CLI transcripts",
		"For UI, HTML, CSS, Electron renderer, browser, visual layout, or copy-placement changes, attempt to capture reviewer-visible visual evidence",
		"DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available",
		"If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary",
		"Write new evidence files into this temporary evidence directory:",
		filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID),
		"Do not move, commit, or modify source files only to make evidence linkable",
		"If no existing test produces sufficient evidence, write or improve a focused test",
		"If automated testing cannot produce the needed evidence, execute manual verification steps",
		"Always include an \"artifacts\" array",
		"If sufficient evidence is not possible, report a warning finding",
		"remove any transient artifacts your testing created in the working tree",
		"shared prompt config",
		"test prompt config",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	if strings.Index(prompt, "shared prompt config") > strings.Index(prompt, "test prompt config") {
		t.Fatalf("expected shared prompt config before test prompt config, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "will be available from the pushed commit") || strings.Contains(prompt, "files that already exist in the repository") {
		t.Fatalf("expected prompt not to make the testing agent worry about committed evidence files, got:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID)); err != nil {
		t.Fatalf("expected temporary evidence directory to exist: %v", err)
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	t.Logf("evidence findings JSON: %s", outcome.Findings)
	if len(findings.Tested) != 2 || findings.Tested[0] != testCmd || findings.Tested[1] != "manual screenshot review" {
		t.Fatalf("expected baseline command and agent-tested evidence to be recorded, got %+v", findings.Tested)
	}
}

func TestTestStep_InRepoEvidenceFallsBackWhenConfiguredDirEscapesWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["manual evidence check"],"testing_summary":"checked evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "../outside"}

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	prompt := ag.calls[0].Prompt
	wantDir := filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID)
	if !strings.Contains(prompt, "Write new evidence files into this temporary evidence directory: "+wantDir) {
		t.Fatalf("expected temporary evidence guidance for unsafe in-repo dir, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "in-repo evidence directory") || strings.Contains(prompt, "committed and pushed automatically") {
		t.Fatalf("did not expect in-repo publishing promise for unsafe evidence dir, got:\n%s", prompt)
	}
}

func TestTestStep_InRepoEvidenceFallsBackWhenEvidenceDirIsIgnored(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("evidence/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["manual evidence check"],"testing_summary":"checked evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: "evidence"}

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	prompt := ag.calls[0].Prompt
	wantDir := filepath.Join(os.TempDir(), "no-mistakes-evidence", sctx.Run.ID)
	if !strings.Contains(prompt, "Write new evidence files into this temporary evidence directory: "+wantDir) {
		t.Fatalf("expected temporary evidence guidance for ignored in-repo dir, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "in-repo evidence directory") || strings.Contains(prompt, "committed and pushed automatically") {
		t.Fatalf("did not expect in-repo publishing promise for ignored evidence dir, got:\n%s", prompt)
	}
}

// Local Test is targeted validation of the requested intent, never a complete
// repository-suite walk. Broad regression belongs to remote CI. Pins the
// normal evidence-agent contract wording so a soft "run the appropriate tests"
// regression is caught.
func TestTestStep_InitialAgent_TargetedValidationContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["go test ./internal/cli -run TestDoctor -count=1"],"testing_summary":"targeted check passed"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Keep doctor checks green for CLI users"

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 evidence agent call, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt

	for _, want := range []string{
		"run the smallest relevant tests yourself",
		"Do NOT run the complete repository test suite",
		"Local Test is targeted validation of the requested intent",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
		"Never treat \"do not run everything\" as permission to run nothing",
		"report a warning finding that sufficient targeted evidence is not possible",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected initial test prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	for _, forbid := range []string{
		"run the appropriate tests yourself",
		"Run the tests, identify failures",
	} {
		if strings.Contains(prompt, forbid) {
			t.Errorf("initial test prompt still carries open-ended suite language %q:\n%s", forbid, prompt)
		}
	}
}

// Test repair must reproduce the specific failure, fix its root cause, and
// re-verify only with focused checks. Soft "Run the tests" / "relevant tests"
// wording invited complete-suite walks after a one-line fix.
func TestTestStep_FixMode_TargetedVerificationContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix targeted failure"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix"}],"summary":"FAIL: TestFoo"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the test fixer to be invoked")
	}
	fixPrompt := ag.calls[0].Prompt

	for _, want := range []string{
		"Reproduce the specific failure",
		"Reproduce the specific failing case first",
		"re-run only that focused verification after the fix",
		"Do NOT run the complete repository test suite",
		"Local Test is targeted validation of the failure and the requested intent",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary",
		"Never treat \"do not run everything\" as permission to run nothing",
	} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("expected test fixer prompt to contain %q, got:\n%s", want, fixPrompt)
		}
	}
	for _, forbid := range []string{
		"Run the tests, identify failures, and fix either the tests or the code to make them pass",
		"Re-run the relevant tests before finishing",
	} {
		if strings.Contains(fixPrompt, forbid) {
			t.Errorf("test fixer prompt still carries open-ended suite language %q:\n%s", forbid, fixPrompt)
		}
	}
}

// A driver/user instruction that asks for full-suite confirmation must still
// be accompanied by the hard product boundary so the repair agent does not
// treat that instruction as license to expand scope.
func TestTestStep_FixMode_DriverFullSuiteInstructionDoesNotOverrideContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix focused failure"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix","user_instructions":"confirm the full suite path for this failure is green"}],"summary":"FAIL: TestFoo"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	fixPrompt := ag.calls[0].Prompt
	if !strings.Contains(fixPrompt, "confirm the full suite path for this failure is green") {
		t.Fatalf("expected previous findings to still carry the driver instruction, got:\n%s", fixPrompt)
	}
	if !strings.Contains(fixPrompt, "A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary") {
		t.Fatalf("expected product boundary to outrank the driver full-suite instruction, got:\n%s", fixPrompt)
	}
	if !strings.Contains(fixPrompt, "Do NOT run the complete repository test suite") {
		t.Fatalf("expected explicit no-full-suite rule in repair prompt, got:\n%s", fixPrompt)
	}
}

// Honest failure reporting when no targeted check can establish intent must
// remain mandatory; the no-full-suite rule must not collapse into "skip tests".
func TestTestStep_InitialAgent_NoTargetedEvidenceRequiresHonestFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"no targeted test can prove the intent","action":"ask-user"}],"summary":"missing evidence","tested":["manual review of changed packages"],"testing_summary":"could not produce targeted evidence"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Prove the checkout success screen works end-to-end"

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected missing targeted evidence to require approval")
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Never treat \"do not run everything\" as permission to run nothing",
		"write or improve a focused test",
		"perform manual verification with evidence",
		"report a warning finding that sufficient targeted evidence is not possible",
		"If sufficient evidence is not possible, report a warning finding",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected no-targeted-evidence guidance %q in prompt:\n%s", want, prompt)
		}
	}
}
