//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const staleTwoFileEvidence = "Inspected only final files: internal/example/flag.go and cmd/example/main.go."

func writeFinalPRScopeScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "final-pr-scope-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      summary: "review clean"
      risk_level: medium
      risk_rationale: "medium risk because only two source files changed"
  - match: "You are validating a code change by testing it. Examine the repository and run the smallest relevant tests yourself."
    text: "two-file test evidence"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "` + staleTwoFileEvidence + `"
      testing_summary: "Focused validation passed at the test step target commit."
      artifacts: []
  - match: "Perform the combined documentation and lint housekeeping pass for this change."
    text: "documentation updated"
    edits:
      - path: "docs/flag.md"
        new: "# Flag\n"
      - path: "docs/reference.md"
        new: "# Reference\n"
    structured:
      findings: []
      summary: "update flag documentation"
  - match: "Draft a pull request title and summary for the full branch delta."
    text: "full four-file PR summary"
    structured:
      title: "feat: add example flag"
      body: |
        ## What Changed

        - Add flag behavior in ` + "`internal/example/flag.go`" + ` and CLI wiring in ` + "`cmd/example/main.go`" + `.
        - Add documentation in ` + "`docs/flag.md`" + ` and ` + "`docs/reference.md`" + `.
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected"
      tested:
        - "fakeagent: simulated build"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write final PR scope scenario: %v", err)
	}
	return path
}

// TestPRFinalScopeExcludesEarlierStepEvidence guards the PR 1272 failure at the
// closest supported user-visible boundary: a real gate push executes the full
// pipeline and a GitHub PR creation receives the final body on stdin.
//
// The bug was never that step evidence appears in the PR - the deterministic
// Risk Assessment, Testing, and Pipeline sections are the point of the body.
// It was that pre-Document two-file Test evidence read as a claim about the
// shipped four-file branch. So the invariant this test pins is a section
// boundary, not a section ban: `## What Changed` is the sole owner of final
// branch scope and must match the real final diff, while earlier Review and
// Test output stays inside the evidence sections that name the step it came
// from. Section ownership is documented in the PR step reference.
//
// Reproduction record, before source-level cause assignment:
//   - Expected behavior: `## What Changed` describes the actual four-file
//     branch delta; earlier Test evidence stays step-scoped.
//   - Observed pre-fix: the PR presented two-file Test evidence as though it
//     described the shipped branch after Document added two files.
//   - Initiating trigger: a legitimate Document stage commit after Test.
//   - Masking condition: no later local mutation, or an evidence claim that
//     happens to match the final diff, leaves no visible contradiction.
//   - Visible symptom: a reviewer sees an "only final files" two-file claim
//     presented as the branch's scope while it covers four files.
//   - Earliest divergence from the proven accurate path: the pre-Document Test
//     target is presented as later final scope instead of remaining evidence
//     for that completed step.
//   - Relevant history: the merged Firstmate PR #1272 supplied the concrete
//     two-file final-scope wording that motivated this regression shape.
//   - Smallest causal counterfactual: if Document made no downstream files,
//     the same two-file evidence would coincide with the final diff and not
//     misstate its scope.
//   - Disconfirming evidence: this test proves the final pushed head, final
//     diff, and PR drafting prompt all contain the two documentation files, so
//     it is not a dropped mutation, stale push, or branch-sync failure.
func TestPRFinalScopeExcludesEarlierStepEvidence(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeFinalPRScopeScenario(t)})
	ctx := context.Background()

	parentURL := "https://github.com/example/no-mistakes.git"
	forkURL := "https://github.com/example-fork/no-mistakes.git"
	forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
	if err := os.MkdirAll(forkDir, 0o755); err != nil {
		t.Fatalf("mkdir fork: %v", err)
	}
	if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatalf("init fork: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
		t.Fatalf("seed fork main: %v\n%s", err, out)
	}
	configureGitURLRewrite(t, h, parentURL, h.UpstreamDir)
	configureGitURLRewrite(t, h, forkURL, forkDir)
	if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", parentURL); err != nil {
		t.Fatalf("set parent origin: %v\n%s", err, out)
	}

	ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-final-pr-scope.log")
	t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
	t.Setenv("FAKEAGENT_GH_LOG", ghLog)
	t.Setenv("FAKEAGENT_GH_PARENT", "example/no-mistakes")

	if out, err := h.Run("init", "--fork-url", forkURL); err != nil {
		t.Fatalf("init with fork URL: %v\n%s", err, out)
	}

	const branch = "feature/final-pr-scope"
	h.CommitChange(branch, "internal/example/flag.go", "package example\n", "add flag behavior")
	preDocumentHead := h.CommitChange(branch, "cmd/example/main.go", "package main\n", "add flag CLI")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, run.Error)
	}
	if run.HeadSHA == preDocumentHead {
		t.Fatalf("Document did not advance the tested head %s", preDocumentHead)
	}

	finalHead, err := h.runGit(ctx, forkDir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read final fork head: %v\n%s", err, finalHead)
	}
	if got := strings.TrimSpace(string(finalHead)); got != run.HeadSHA {
		t.Fatalf("final fork head = %s, want run head %s", got, run.HeadSHA)
	}
	finalDiff, err := h.runGit(ctx, forkDir, "diff", "--name-only", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read final branch diff: %v\n%s", err, finalDiff)
	}
	wantFiles := []string{
		"cmd/example/main.go",
		"docs/flag.md",
		"docs/reference.md",
		"internal/example/flag.go",
	}
	if got := strings.Fields(string(finalDiff)); !equalStrings(got, wantFiles) {
		t.Fatalf("final branch files = %q, want %q", got, wantFiles)
	}

	testPrompt := findInvocationContaining(h.AgentInvocations(), "You are validating a code change")
	if !strings.Contains(testPrompt, "target commit: "+preDocumentHead) {
		t.Fatalf("Test evidence was not bound to its pre-Document target %s:\n%s", preDocumentHead, testPrompt)
	}
	prPrompt := findInvocationContaining(h.AgentInvocations(), "Draft a pull request title and summary for the full branch delta.")
	for _, want := range append([]string{"target commit: " + run.HeadSHA}, wantFiles...) {
		if !strings.Contains(prPrompt, want) {
			t.Fatalf("final PR drafting prompt missing %q:\n%s", want, prPrompt)
		}
	}

	body := createdPRBody(t, readGHStubInvocations(t, ghLog))

	// The deterministic evidence sections must be present and populated: an
	// empty Pipeline shell is the regression this contract exists to prevent.
	for _, want := range []string{
		"## Risk Assessment",
		"## Testing",
		"## Pipeline",
		"<summary>✅ **Test** - passed</summary>",
		"<summary>⚠️ **Review** - medium risk</summary>",
		"medium risk because only two source files changed",
		staleTwoFileEvidence,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("final PR body missing recorded step evidence %q:\n%s", want, body)
		}
	}

	// `## What Changed` is the sole owner of final branch scope: it must cover
	// the real four-file diff and carry none of the earlier step's narrower
	// claims about what the change touched.
	whatChanged := prBodySection(t, body, "## What Changed")
	for _, want := range wantFiles {
		if !strings.Contains(whatChanged, want) {
			t.Fatalf("final-scope What Changed missing final-diff file %q:\n%s", want, whatChanged)
		}
	}
	for _, stale := range []string{staleTwoFileEvidence, "medium risk because only two source files changed"} {
		if strings.Contains(whatChanged, stale) {
			t.Fatalf("earlier step evidence %q leaked into final-scope What Changed:\n%s", stale, whatChanged)
		}
	}

	// Evidence stays attributed to the step that produced it, so a reviewer
	// reads the two-file claim as Test's target rather than the branch's scope.
	if !strings.Contains(prBodySection(t, body, "## Pipeline"), staleTwoFileEvidence) {
		t.Fatalf("Test evidence must render inside the step-attributed Pipeline section:\n%s", body)
	}
}

// prBodySection returns the named `## ` section of a PR body, up to the next
// top-level heading.
func prBodySection(t *testing.T, body, heading string) string {
	t.Helper()
	start := strings.Index(body, heading+"\n")
	if start < 0 {
		t.Fatalf("PR body has no %q section:\n%s", heading, body)
	}
	rest := body[start+len(heading):]
	if next := strings.Index(rest, "\n## "); next >= 0 {
		rest = rest[:next]
	}
	return heading + rest
}

func createdPRBody(t *testing.T, invocations []ghStubInvocation) string {
	t.Helper()
	for _, inv := range invocations {
		if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
			if inv.Body == "" {
				t.Fatalf("PR create did not receive a body on stdin: %+v", inv)
			}
			return inv.Body
		}
	}
	t.Fatalf("no PR create invocation in %+v", invocations)
	return ""
}

func equalStrings(got, want []string) bool {
	return bytes.Equal([]byte(strings.Join(got, "\n")), []byte(strings.Join(want, "\n")))
}
