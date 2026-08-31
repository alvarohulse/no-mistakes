package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResolvedPolicyCanonicalRoundTripExcludesPrivateLaunchMaterial(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	cfg.Commands = config.Commands{Build: "make build", Test: "make test", Lint: "make lint", Format: "make fmt"}
	cfg.Hooks = config.Hooks{PostWorktree: "./setup", PRBody: "format-pr"}
	cfg.AutoFix = config.AutoFix{Review: 2, Build: 1, Test: 3}
	cfg.CITimeout = 3 * time.Hour
	cfg.StepQuietWarning = 15 * time.Minute
	cfg.ProcessTerminationGrace = 2 * time.Second
	cfg.CI = config.CI{RerunTransient: 2}
	cfg.Test.Evidence = config.Evidence{LocalRoot: "/private/evidence", Retention: 14 * 24 * time.Hour, MaxRuns: 50}
	cfg.AgentPathOverride = map[string]string{"codex": "/private/codex"}
	cfg.AgentArgsOverride = map[string][]string{"codex": {"--secret", "token-value"}}
	cfg.ACPRegistryOverrides = map[string]string{"private": "/private/acp --token secret"}
	cfg.Prompts = config.PromptConfig{Shared: "private prompt"}
	cfg.Document = config.Document{Instructions: "private document steering"}
	cfg.Review = config.Review{PathInstructions: []config.PathInstruction{{Path: "private/**", Instructions: "private review steering"}}}
	cfg.DisableProjectSettings = true
	cfg.NoCI = true

	policySteps := []pipeline.Step{policyTestStep{name: types.StepReview}, policyTestStep{name: types.StepBuild}, policyTestStep{name: types.StepPush}}
	sources := []db.ConfigSource{{Kind: db.ConfigSourceGlobal, Digest: strings.Repeat("a", 64), Path: "/private/config.yaml"}, {Kind: db.ConfigSourceDefault, Digest: strings.Repeat("b", 64), Ref: "abc123"}}
	encoded, digest, err := marshalResolvedPolicy(cfg, sources, policySteps, []types.StepName{types.StepBuild}, types.RefreshStrategyMerge, false)
	if err != nil {
		t.Fatal(err)
	}
	encodedAgain, digestAgain, err := marshalResolvedPolicy(cfg, sources, policySteps, []types.StepName{types.StepBuild}, types.RefreshStrategyMerge, false)
	if err != nil {
		t.Fatal(err)
	}
	if encoded != encodedAgain || digest != digestAgain {
		t.Fatalf("resolved policy is not canonical:\n%s\n%s\n%s\n%s", encoded, encodedAgain, digest, digestAgain)
	}
	for _, required := range []string{"make build", "make test", "make lint", "make fmt", "./setup", "format-pr", `"name":"build","status":"skipped","skip_source":"run-request"`, `"refresh_strategy":"merge"`, `"retention_ns":1209600000000000`} {
		if !strings.Contains(encoded, required) {
			t.Errorf("resolved policy omitted %q:\n%s", required, encoded)
		}
	}
	if strings.Contains(encoded, `"pricing"`) {
		t.Errorf("resolved policy retained pricing selection:\n%s", encoded)
	}
	for _, forbidden := range []string{"store_in_repo", "/private/codex", "token-value", "/private/acp", "private prompt", "/private/evidence", "private document steering", "private review steering"} {
		if strings.Contains(encoded, forbidden) {
			t.Errorf("resolved policy leaked %q:\n%s", forbidden, encoded)
		}
	}
	decoded, legacy, err := decodeResolvedPolicy(&encoded, &digest)
	if err != nil || legacy {
		t.Fatalf("decodeResolvedPolicy = legacy %v error %v", legacy, err)
	}
	if len(decoded.Steps) != 3 || decoded.Steps[1].Status != resolvedPolicyStepSkipped {
		t.Fatalf("decoded steps = %#v", decoded.Steps)
	}
	if decoded.Steps[1].SkipSource != types.SkipSourceRunRequest {
		t.Fatalf("decoded skip source = %q", decoded.Steps[1].SkipSource)
	}
}

func TestDecodeResolvedPolicyAcceptsAndDropsVersionSixPricingSelection(t *testing.T) {
	policy, err := resolvedPolicyFromConfig(
		resolvedRoutingTestConfig(),
		nil,
		[]pipeline.Step{policyTestStep{name: types.StepReview}},
		nil,
		types.RefreshStrategyRebase,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 6
	policy.LegacyPricing = &resolvedPolicyPricing{Profiles: map[string]string{"cursor": "retired-profile"}}
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	policy, legacy, err := decodeResolvedPolicy(&encoded, &digest)
	if err != nil || legacy {
		t.Fatalf("decodeResolvedPolicy = policy %#v legacy %v error %v", policy, legacy, err)
	}
	normalizeResolvedPolicyForComparison(policy)
	if policy.Version != resolvedPolicyVersion {
		t.Fatalf("normalized version = %d, want %d", policy.Version, resolvedPolicyVersion)
	}
	normalized, _, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(normalized, `"pricing"`) {
		t.Fatalf("normalized policy retained legacy pricing: %s", normalized)
	}
}

func TestDecodeResolvedPolicyRejectsLegacyEvidencePublisher(t *testing.T) {
	policy, err := resolvedPolicyFromConfig(
		resolvedRoutingTestConfig(),
		nil,
		[]pipeline.Step{policyTestStep{name: types.StepReview}},
		nil,
		types.RefreshStrategyRebase,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 7
	policy.TestEvidence.LegacyStoreInRepo = true
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = decodeResolvedPolicy(&encoded, &digest)
	if err == nil {
		t.Fatal("legacy evidence publisher policy was accepted for recovery")
	}
	for _, want := range []string{"test.evidence.store_in_repo", "remove", "new run"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Fatalf("recovery error %q does not contain %q", err, want)
		}
	}
}

func TestValidateResolvedPolicyNormalizesVersionSevenLocalEvidencePolicy(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	steps := []pipeline.Step{policyTestStep{name: types.StepReview}}
	policy, err := resolvedPolicyFromConfig(cfg, nil, steps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 7
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}

	if err := validateResolvedPolicy(cfg, run, steps); err != nil {
		t.Fatalf("version-seven local-only policy rejected: %v", err)
	}
}

func TestValidateResolvedPolicyNormalizesVersionEightBeforeArtifactsWereRequired(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	steps := []pipeline.Step{policyTestStep{name: types.StepReview}}
	policy, err := resolvedPolicyFromConfig(cfg, nil, steps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 8
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}
	if err := validateResolvedPolicy(cfg, run, steps); err != nil {
		t.Fatalf("version-eight policy rejected: %v", err)
	}
}

func TestValidateResolvedPolicyNormalizesVersionFourSkippedStepsToRunRequest(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	policySteps := []pipeline.Step{policyTestStep{name: types.StepBuild}}
	policy, err := resolvedPolicyFromConfig(cfg, nil, policySteps, []types.StepName{types.StepBuild}, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 4
	policy.Steps[0].SkipSource = ""
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}
	if err := validateResolvedPolicy(cfg, run, policySteps); err != nil {
		t.Fatalf("version-four skip receipt rejected: %v", err)
	}
}

func TestDecodeResolvedPolicyDistinguishesLegacyIncompleteAndTamperedRows(t *testing.T) {
	legacy, isLegacy, err := decodeResolvedPolicy(nil, nil)
	if err != nil || !isLegacy || legacy != nil {
		t.Fatalf("legacy decode = policy %#v legacy %v error %v", legacy, isLegacy, err)
	}
	valid := `{"version":1}`
	digest := resolvedPolicyDigest([]byte(valid))
	tests := []struct {
		name   string
		policy *string
		digest *string
	}{
		{name: "missing digest", policy: &valid},
		{name: "missing policy", digest: &digest},
		{name: "empty new row", policy: stringPointer(""), digest: stringPointer("")},
		{name: "unknown field", policy: stringPointer(`{"version":1,"unknown":true}`), digest: stringPointer(resolvedPolicyDigest([]byte(`{"version":1,"unknown":true}`)))},
		{name: "tampered digest", policy: &valid, digest: stringPointer(strings.Repeat("0", 64))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := decodeResolvedPolicy(tt.policy, tt.digest); err == nil {
				t.Fatal("decodeResolvedPolicy accepted invalid new row")
			}
		})
	}
}

func TestValidateResolvedPolicyRejectsRecoveredPolicyDrift(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	cfg.Commands.Build = "make build"
	policySteps := []pipeline.Step{policyTestStep{name: types.StepReview}, policyTestStep{name: types.StepBuild}}
	encoded, digest, err := marshalResolvedPolicy(cfg, nil, policySteps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}
	if err := validateResolvedPolicy(cfg, run, policySteps); err != nil {
		t.Fatalf("unchanged policy rejected: %v", err)
	}
	cfg.Commands.Build = "make changed-build"
	if err := validateResolvedPolicy(cfg, run, policySteps); err == nil || !strings.Contains(err.Error(), "differs from launch") {
		t.Fatalf("policy drift error = %v", err)
	}
	if err := validateResolvedPolicy(cfg, &db.Run{}, policySteps); err != nil {
		t.Fatalf("legacy policy rejected: %v", err)
	}
	empty := ""
	if err := validateResolvedPolicy(cfg, &db.Run{ResolvedPolicy: &empty, ResolvedPolicyDigest: &empty}, policySteps); err == nil {
		t.Fatal("incomplete new policy was accepted")
	}
}

func TestValidateResolvedPolicyAcceptsVersionOneSnapshotWithoutAdversary(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	cfg.ReviewCandidates = nil
	policySteps := []pipeline.Step{policyTestStep{name: types.StepReview}, policyTestStep{name: types.StepBuild}}
	policy, err := resolvedPolicyFromConfig(cfg, nil, policySteps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 1
	policy.Routing.Version = 1
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}
	if err := validateResolvedPolicy(cfg, run, policySteps); err != nil {
		t.Fatalf("version-one policy rejected: %v", err)
	}
}

func TestValidateResolvedPolicyAcceptsVersionTwoLegacyRunner(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	policySteps := []pipeline.Step{policyTestStep{name: types.StepReview}, policyTestStep{name: types.StepBuild}}
	policy, err := resolvedPolicyFromConfig(cfg, nil, policySteps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 2
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}
	if err := validateResolvedPolicy(cfg, run, policySteps); err != nil {
		t.Fatalf("version-two policy rejected: %v", err)
	}
}

func TestValidateResolvedPolicyAcceptsVersionThreeWithoutPreflight(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	policySteps := []pipeline.Step{policyTestStep{name: types.StepReview}}
	policy, err := resolvedPolicyFromConfig(cfg, nil, policySteps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 3
	policy.Preflight = nil
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}
	if err := validateResolvedPolicy(cfg, run, policySteps); err != nil {
		t.Fatalf("version-three policy rejected: %v", err)
	}
}

func TestResolvedPolicyCarriesCanonicalPreflightCommands(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	cfg.Preflight = []runner.Command{{
		Run:    "echo ready",
		Runner: &runner.Spec{Executable: "zsh", Args: []string{"-lc"}},
	}}
	encoded, _, err := marshalResolvedPolicy(cfg, nil, []pipeline.Step{policyTestStep{name: types.StepReview}}, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, `"preflight":[{"run":"echo ready","runner":{"executable":"zsh","args":["-lc"]}}]`) {
		t.Fatalf("resolved policy omitted canonical preflight: %s", encoded)
	}
}

func TestResolvedPolicyCarriesRunnerProvenanceAndStructuredCommands(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	repo, err := config.LoadRepoFromBytes([]byte(`
commands:
  build:
    run: make build
    runner: {executable: zsh, args: [-lc]}
    linux: make build-linux
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Commands = repo.Commands
	version := "5.8.1"
	cfg.ResolvedRunner = &runner.Provenance{
		SchemaVersion: runner.SchemaVersion,
		Platform:      "linux",
		Source:        runner.SourceDefault,
		Executable:    "zsh",
		Args:          []string{"-lc"},
		Version:       &version,
	}
	policySteps := []pipeline.Step{policyTestStep{name: types.StepReview}, policyTestStep{name: types.StepBuild}}
	encoded, digest, err := marshalResolvedPolicy(cfg, nil, policySteps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"schema_version":1`, `"executable":"zsh"`, `"version":"5.8.1"`, `"definitions":{"build"`, `"linux":{"run":"make build-linux"`} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("resolved policy omitted %s: %s", required, encoded)
		}
	}
	if strings.Contains(encoded, "/usr/bin/zsh") {
		t.Fatalf("resolved policy leaked machine path: %s", encoded)
	}
	run := &db.Run{ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest, RefreshStrategy: types.RefreshStrategyRebase}
	if err := validateResolvedPolicy(cfg, run, policySteps); err != nil {
		t.Fatalf("runner policy round trip failed: %v", err)
	}
	changedVersion := "5.9"
	cfg.ResolvedRunner.Version = &changedVersion
	if err := validateResolvedPolicy(cfg, run, policySteps); err == nil || !strings.Contains(err.Error(), "differs from launch") {
		t.Fatalf("runner drift error = %v", err)
	}
}

func TestDecodeResolvedPolicyRejectsInvalidRunnerProvenance(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	version := "zsh 5.8.1"
	cfg.ResolvedRunner = &runner.Provenance{
		SchemaVersion: runner.SchemaVersion + 1,
		Platform:      "linux",
		Source:        runner.SourceDefault,
		Executable:    "zsh",
		Args:          []string{"-lc"},
		Version:       &version,
	}
	_, _, err := marshalResolvedPolicy(cfg, nil, []pipeline.Step{policyTestStep{name: types.StepReview}}, nil, types.RefreshStrategyRebase, false)
	if err == nil || !strings.Contains(err.Error(), "runner schema version") {
		t.Fatalf("marshal error = %v, want invalid runner schema", err)
	}
}

func TestResolvedPolicyRejectsUnsafeInactiveRunnerDefinition(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	repo, loadErr := config.LoadRepoFromBytes([]byte(`
commands:
  build:
    run: make build
    windows:
      runner:
        executable: C:\\private-token\\pwsh.exe
        args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	cfg.Commands = repo.Commands
	_, _, err := marshalResolvedPolicy(cfg, nil, []pipeline.Step{policyTestStep{name: types.StepReview}}, nil, types.RefreshStrategyRebase, false)
	if err == nil || !strings.Contains(err.Error(), "windows runner") || !strings.Contains(err.Error(), "bare supported shell name") {
		t.Fatalf("marshal error = %v", err)
	}
}

func TestResolvedPolicyRejectsIncompleteManagedRouting(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	cfg.Managed = true
	_, _, err := marshalResolvedPolicy(cfg, nil, []pipeline.Step{policyTestStep{name: types.StepReview}}, nil, types.RefreshStrategyRebase, false)
	if err == nil || !strings.Contains(err.Error(), "managed resolved routing is missing intent route") {
		t.Fatalf("marshalResolvedPolicy() error = %v, want incomplete managed route refusal", err)
	}
}

func TestResolvedPolicyCarriesCompleteManagedRouting(t *testing.T) {
	stepAgents := make(map[types.StepName][]types.AgentName)
	stepModels := make(map[types.StepName]config.ModelRoute)
	policySteps := make([]pipeline.Step, 0, len(types.AllSteps()))
	for _, step := range types.AllSteps() {
		policySteps = append(policySteps, policyTestStep{name: step})
		if step == types.StepPush {
			continue
		}
		stepAgents[step] = []types.AgentName{types.AgentCodex}
		stepModels[step] = config.ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}
	}
	cfg := &config.Config{
		Managed: true, Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex},
		StepAgents: stepAgents, StepModels: stepModels,
		ReviewCandidates: []config.ReviewCandidate{{Agent: types.AgentCodex, Model: config.ModelRoute{Name: "gpt-5.6-sol", Vendor: "openai"}}},
	}
	encoded, _, err := marshalResolvedPolicy(cfg, nil, policySteps, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatalf("marshalResolvedPolicy() error = %v", err)
	}
	if !strings.Contains(encoded, `"managed":true`) || !strings.Contains(encoded, `"review_candidates"`) {
		t.Fatalf("managed policy omitted strict routing facts: %s", encoded)
	}
}

type policyTestStep struct{ name types.StepName }

func (s policyTestStep) Name() types.StepName { return s.name }

func (s policyTestStep) Execute(*pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return nil, nil
}

func stringPointer(value string) *string { return &value }
