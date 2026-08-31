package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	resolvedPolicyVersion     = 8
	resolvedPolicyStepEnabled = "enabled"
	resolvedPolicyStepSkipped = "skipped"
)

type resolvedPolicy struct {
	Version                int                    `json:"version"`
	Managed                bool                   `json:"managed"`
	Binary                 resolvedPolicyBinary   `json:"binary"`
	Steps                  []resolvedPolicyStep   `json:"steps"`
	Preflight              []runner.Command       `json:"preflight"`
	Commands               resolvedPolicyCommands `json:"commands"`
	Routing                resolvedAgentRouting   `json:"routing"`
	Runner                 resolvedPolicyRunner   `json:"runner"`
	Budgets                resolvedPolicyBudgets  `json:"budgets"`
	Sources                []db.ConfigSource      `json:"sources"`
	TrustedConfigSHA       string                 `json:"trusted_config_sha"`
	RefreshStrategy        types.RefreshStrategy  `json:"refresh_strategy"`
	LogLevel               string                 `json:"log_level"`
	SessionReuse           bool                   `json:"session_reuse"`
	DisableProjectSettings bool                   `json:"disable_project_settings"`
	NoCI                   bool                   `json:"no_ci"`
	IgnorePatterns         []string               `json:"ignore_patterns"`
	CommitFixMessage       string                 `json:"commit_fix_message"`
	Intent                 resolvedPolicyIntent   `json:"intent"`
	Eval                   resolvedPolicyEval     `json:"eval"`
	TestEvidence           resolvedPolicyEvidence `json:"test_evidence"`
	LegacyPricing          *resolvedPolicyPricing `json:"pricing,omitempty"`
}

type resolvedPolicyBinary struct {
	Version  string `json:"version"`
	BuildSHA string `json:"build_sha"`
}

type resolvedPolicyStep struct {
	Name       types.StepName   `json:"name"`
	Status     string           `json:"status"`
	SkipSource types.SkipSource `json:"skip_source,omitempty"`
}

type resolvedPolicyCommands struct {
	Build        string                    `json:"build"`
	Test         string                    `json:"test"`
	Lint         string                    `json:"lint"`
	Format       string                    `json:"format"`
	PostWorktree string                    `json:"post_worktree"`
	PRBody       string                    `json:"pr_body"`
	Definitions  map[string]runner.Command `json:"definitions,omitempty"`
}

type resolvedPolicyRunner struct {
	SchemaVersion int      `json:"schema_version,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	Platform      string   `json:"platform"`
	Source        string   `json:"source,omitempty"`
	Executable    string   `json:"executable,omitempty"`
	Args          []string `json:"args,omitempty"`
	Version       *string  `json:"version"`
}

type resolvedPolicyBudgets struct {
	AutoFix                   resolvedPolicyAutoFix `json:"auto_fix"`
	CITimeoutNS               int64                 `json:"ci_timeout_ns"`
	StepQuietWarningNS        int64                 `json:"step_quiet_warning_ns"`
	ProcessTerminationGraceNS int64                 `json:"process_termination_grace_ns"`
	CIRerunTransient          int                   `json:"ci_rerun_transient"`
}

type resolvedPolicyAutoFix struct {
	Lint     int `json:"lint"`
	Build    int `json:"build"`
	Test     int `json:"test"`
	Review   int `json:"review"`
	Document int `json:"document"`
	CI       int `json:"ci"`
	Refresh  int `json:"refresh"`
}

type resolvedPolicyEvidence struct {
	// LegacyStoreInRepo survives only to identify version 7 and older snapshots
	// that depended on the retired remote publisher. New snapshots omit it.
	LegacyStoreInRepo bool  `json:"store_in_repo,omitempty"`
	RetentionNS       int64 `json:"retention_ns"`
	MaxRuns           int   `json:"max_runs"`
}

type resolvedPolicyIntent struct {
	Enabled         bool     `json:"enabled"`
	Threshold       float64  `json:"threshold"`
	SlackDays       int      `json:"slack_days"`
	DisabledReaders []string `json:"disabled_readers"`
}

type resolvedPolicyEval struct {
	CaptureProvenance bool `json:"capture_provenance"`
	AutoCapture       bool `json:"auto_capture"`
	MaxCases          int  `json:"max_cases"`
	DiversifiedSize   int  `json:"diversified_size"`
}

type resolvedPolicyPricing struct {
	Profiles map[string]string `json:"profiles"`
}

// PolicyExplanation is a presentation wrapper around the exact NM-02 policy
// DTO. It deliberately carries no second policy model: text pretty-prints the
// canonical DTO and JSON embeds the same bytes beside their digest.
type PolicyExplanation struct {
	policyJSON string
	digest     string
}

type policyExplanationEnvelope struct {
	PolicyDigest string          `json:"policy_digest"`
	Policy       json.RawMessage `json:"policy"`
}

func newPolicyExplanation(resolved *runPolicyResolution) (*PolicyExplanation, error) {
	if resolved == nil || resolved.Policy == nil {
		return nil, fmt.Errorf("resolved policy explanation is missing")
	}
	if strings.TrimSpace(resolved.ResolvedPolicy) == "" || strings.TrimSpace(resolved.ResolvedPolicyDigest) == "" {
		return nil, fmt.Errorf("resolved policy explanation is incomplete")
	}
	return &PolicyExplanation{policyJSON: resolved.ResolvedPolicy, digest: resolved.ResolvedPolicyDigest}, nil
}

// Digest returns the SHA-256 digest of the canonical policy bytes.
func (e *PolicyExplanation) Digest() string {
	if e == nil {
		return ""
	}
	return e.digest
}

// CanonicalJSON returns a compact JSON envelope containing the exact canonical
// policy bytes and their digest.
func (e *PolicyExplanation) CanonicalJSON() (string, error) {
	if e == nil || strings.TrimSpace(e.policyJSON) == "" || strings.TrimSpace(e.digest) == "" {
		return "", fmt.Errorf("resolved policy explanation is incomplete")
	}
	encoded, err := json.Marshal(policyExplanationEnvelope{
		PolicyDigest: e.digest,
		Policy:       json.RawMessage(e.policyJSON),
	})
	if err != nil {
		return "", fmt.Errorf("encode resolved policy explanation: %w", err)
	}
	return string(encoded), nil
}

// Text returns a readable projection of the exact policy DTO.
func (e *PolicyExplanation) Text() (string, error) {
	if e == nil || strings.TrimSpace(e.policyJSON) == "" || strings.TrimSpace(e.digest) == "" {
		return "", fmt.Errorf("resolved policy explanation is incomplete")
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(e.policyJSON), "", "  "); err != nil {
		return "", fmt.Errorf("format resolved policy explanation: %w", err)
	}
	return fmt.Sprintf("policy digest: %s\npolicy:\n%s\n", e.digest, pretty.String()), nil
}

func marshalResolvedPolicy(cfg *config.Config, sources []db.ConfigSource, steps []pipeline.Step, skipped []types.StepName, refreshStrategy types.RefreshStrategy, demo bool) (string, string, error) {
	policy, err := resolvedPolicyFromConfig(cfg, sources, steps, skipped, refreshStrategy, demo)
	if err != nil {
		return "", "", err
	}
	return marshalResolvedPolicyDTO(policy)
}

func marshalResolvedPolicyDTO(policy *resolvedPolicy) (string, string, error) {
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", "", fmt.Errorf("encode resolved policy: %w", err)
	}
	return string(encoded), resolvedPolicyDigest(encoded), nil
}

func decodeResolvedPolicy(encoded, digest *string) (*resolvedPolicy, bool, error) {
	if encoded == nil && digest == nil {
		return nil, true, nil
	}
	if encoded == nil || digest == nil {
		return nil, false, fmt.Errorf("resolved policy snapshot is incomplete")
	}
	if strings.TrimSpace(*encoded) == "" || strings.TrimSpace(*digest) == "" {
		return nil, false, fmt.Errorf("resolved policy was not persisted at launch")
	}
	if actual := resolvedPolicyDigest([]byte(*encoded)); actual != *digest {
		return nil, false, fmt.Errorf("resolved policy digest does not match snapshot")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(*encoded))
	decoder.DisallowUnknownFields()
	var policy resolvedPolicy
	if err := decoder.Decode(&policy); err != nil {
		return nil, false, fmt.Errorf("decode resolved policy: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, false, fmt.Errorf("decode resolved policy: trailing content")
	}
	if err := policy.validate(); err != nil {
		return nil, false, err
	}
	return &policy, false, nil
}

func validateResolvedPolicy(cfg *config.Config, run *db.Run, steps []pipeline.Step) error {
	if run == nil {
		return fmt.Errorf("resolved policy run is nil")
	}
	expected, legacy, err := decodeResolvedPolicy(run.ResolvedPolicy, run.ResolvedPolicyDigest)
	if err != nil || legacy {
		return err
	}
	normalizeResolvedPolicyForComparison(expected)
	var skipped []types.StepSkip
	for _, step := range expected.Steps {
		if step.Status == resolvedPolicyStepSkipped {
			skipped = append(skipped, types.StepSkip{Step: step.Name, Source: step.SkipSource})
		}
	}
	actual, err := resolvedPolicyFromConfigWithSkips(cfg, run.ConfigSources, steps, skipped, run.RefreshStrategy, expected.Routing.Demo)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(*actual, *expected) {
		return fmt.Errorf("resolved policy differs from launch")
	}
	return nil
}

// Older versions predate managed routing, resolved runner provenance, trusted
// preflight, source-aware skip receipts, removal of pricing profiles, or
// removal of the evidence publisher. Their missing or retired fields are
// normalized before semantic comparison.
func normalizeResolvedPolicyForComparison(policy *resolvedPolicy) {
	if policy.Version == 1 {
		if policy.Routing.Version == 1 {
			policy.Routing.Version = resolvedAgentRoutingVersion
		}
	}
	if policy.Version >= 1 && policy.Version <= 3 {
		policy.Preflight = []runner.Command{}
	}
	if policy.Version >= 1 && policy.Version <= 4 {
		for i := range policy.Steps {
			if policy.Steps[i].Status == resolvedPolicyStepSkipped && policy.Steps[i].SkipSource == "" {
				policy.Steps[i].SkipSource = types.SkipSourceRunRequest
			}
		}
	}
	if policy.Version >= 1 && policy.Version <= 6 {
		policy.LegacyPricing = nil
	}
	if policy.Version >= 1 && policy.Version <= 7 {
		policy.TestEvidence.LegacyStoreInRepo = false
		policy.Version = resolvedPolicyVersion
	}
}

func resolvedPolicyFromConfig(cfg *config.Config, sources []db.ConfigSource, steps []pipeline.Step, skipped []types.StepName, refreshStrategy types.RefreshStrategy, demo bool) (*resolvedPolicy, error) {
	receipts := make([]types.StepSkip, 0, len(skipped))
	for _, step := range skipped {
		receipts = append(receipts, types.StepSkip{Step: step.Canonical(), Source: types.SkipSourceRunRequest})
	}
	return resolvedPolicyFromConfigWithSkips(cfg, sources, steps, receipts, refreshStrategy, demo)
}

func resolvedPolicyFromConfigWithSkips(cfg *config.Config, sources []db.ConfigSource, steps []pipeline.Step, skipped []types.StepSkip, refreshStrategy types.RefreshStrategy, demo bool) (*resolvedPolicy, error) {
	if cfg == nil {
		return nil, fmt.Errorf("resolved policy config is nil")
	}
	routing, err := resolvedAgentRoutingFromConfig(cfg, demo)
	if err != nil {
		return nil, err
	}
	skipSet := make(map[types.StepName]types.SkipSource, len(skipped))
	for _, receipt := range skipped {
		name := receipt.Step.Canonical()
		if !receipt.Source.Valid() {
			return nil, fmt.Errorf("resolved policy skip %q has unsupported source %q", name, receipt.Source)
		}
		skipSet[name] = receipt.Source
	}
	resolvedSteps := make([]resolvedPolicyStep, 0, len(steps))
	for _, step := range steps {
		if step == nil {
			return nil, fmt.Errorf("resolved policy contains a nil pipeline step")
		}
		name := step.Name().Canonical()
		status := resolvedPolicyStepEnabled
		source, skipped := skipSet[name]
		if skipped {
			status = resolvedPolicyStepSkipped
			delete(skipSet, name)
		}
		resolvedSteps = append(resolvedSteps, resolvedPolicyStep{Name: name, Status: status, SkipSource: source})
	}
	if len(skipSet) > 0 {
		return nil, fmt.Errorf("resolved policy skip list contains a step outside the pipeline")
	}
	policy := &resolvedPolicy{
		Version:   resolvedPolicyVersion,
		Managed:   cfg.Managed,
		Binary:    resolvedPolicyBinary{Version: buildinfo.CurrentVersion(), BuildSHA: buildinfo.Commit},
		Steps:     resolvedSteps,
		Preflight: canonicalResolvedCommands(cfg.Preflight),
		Commands: resolvedPolicyCommands{
			Build: cfg.Commands.Build, Test: cfg.Commands.Test, Lint: cfg.Commands.Lint, Format: cfg.Commands.Format,
			PostWorktree: cfg.Hooks.PostWorktree, PRBody: cfg.Hooks.PRBody, Definitions: cfg.Commands.StructuredDefinitions(),
		},
		Routing: *routing,
		Runner:  resolvedPolicyRunnerFromConfig(cfg),
		Budgets: resolvedPolicyBudgets{
			AutoFix: resolvedPolicyAutoFix{
				Lint: cfg.AutoFix.Lint, Build: cfg.AutoFix.Build, Test: cfg.AutoFix.Test, Review: cfg.AutoFix.Review,
				Document: cfg.AutoFix.Document, CI: cfg.AutoFix.CI, Refresh: cfg.AutoFix.Refresh,
			},
			CITimeoutNS: int64(cfg.CITimeout), StepQuietWarningNS: int64(cfg.StepQuietWarning),
			ProcessTerminationGraceNS: int64(cfg.ProcessTerminationGrace), CIRerunTransient: cfg.CI.RerunTransient,
		},
		Sources:                append([]db.ConfigSource(nil), sources...),
		TrustedConfigSHA:       cfg.TrustedConfigSHA,
		RefreshStrategy:        refreshStrategy.OrDefault(),
		LogLevel:               cfg.LogLevel,
		SessionReuse:           cfg.SessionReuse,
		DisableProjectSettings: cfg.DisableProjectSettings,
		NoCI:                   cfg.NoCI,
		IgnorePatterns:         append([]string(nil), cfg.IgnorePatterns...),
		CommitFixMessage:       cfg.Commit.FixMessage,
		Intent: resolvedPolicyIntent{
			Enabled: cfg.Intent.Enabled, Threshold: cfg.Intent.Threshold, SlackDays: cfg.Intent.SlackDays,
			DisabledReaders: sortedResolvedPolicyKeys(cfg.Intent.DisabledReaders),
		},
		Eval: resolvedPolicyEval{
			CaptureProvenance: cfg.Eval.CaptureProvenance, AutoCapture: cfg.Eval.AutoCapture,
			MaxCases: cfg.Eval.MaxCases, DiversifiedSize: cfg.Eval.DiversifiedSize,
		},
		TestEvidence: resolvedPolicyEvidence{
			RetentionNS: int64(cfg.Test.Evidence.Retention),
			MaxRuns:     cfg.Test.Evidence.MaxRuns,
		},
	}
	if policy.Sources == nil {
		policy.Sources = []db.ConfigSource{}
	}
	if policy.IgnorePatterns == nil {
		policy.IgnorePatterns = []string{}
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	return policy, nil
}

func (p *resolvedPolicy) validate() error {
	if p.Version < 1 || p.Version > resolvedPolicyVersion {
		return fmt.Errorf("resolved policy version %d is unsupported", p.Version)
	}
	if strings.TrimSpace(p.Binary.Version) == "" || strings.TrimSpace(p.Binary.BuildSHA) == "" {
		return fmt.Errorf("resolved policy binary identity is incomplete")
	}
	if p.Steps == nil {
		return fmt.Errorf("resolved policy steps are missing")
	}
	seen := make(map[types.StepName]bool, len(p.Steps))
	for _, step := range p.Steps {
		if !resolvedPolicyPipelineStep(step.Name) {
			return fmt.Errorf("resolved policy contains unsupported step %q", step.Name)
		}
		if seen[step.Name] {
			return fmt.Errorf("resolved policy contains duplicate step %q", step.Name)
		}
		seen[step.Name] = true
		if step.Status != resolvedPolicyStepEnabled && step.Status != resolvedPolicyStepSkipped {
			return fmt.Errorf("resolved policy step %q has unsupported status %q", step.Name, step.Status)
		}
		if step.Status == resolvedPolicyStepEnabled && step.SkipSource != "" {
			return fmt.Errorf("resolved policy enabled step %q has a skip source", step.Name)
		}
		if step.Status == resolvedPolicyStepSkipped {
			if step.SkipSource == "" && p.Version >= 5 {
				return fmt.Errorf("resolved policy skipped step %q has no skip source", step.Name)
			}
			if step.SkipSource != "" && !step.SkipSource.Valid() {
				return fmt.Errorf("resolved policy skipped step %q has unsupported source %q", step.Name, step.SkipSource)
			}
		}
	}
	for name, definition := range p.Commands.Definitions {
		var legacy string
		switch name {
		case "build":
			legacy = p.Commands.Build
		case "test":
			legacy = p.Commands.Test
		case "lint":
			legacy = p.Commands.Lint
		case "format":
			legacy = p.Commands.Format
		default:
			return fmt.Errorf("resolved policy contains unsupported command definition %q", name)
		}
		if definition.Run != legacy || definition.Equal(runner.Command{Run: definition.Run}) {
			return fmt.Errorf("resolved policy command definition %q is inconsistent", name)
		}
		if err := definition.ValidateRunners(); err != nil {
			return fmt.Errorf("resolved policy command definition %q: %w", name, err)
		}
	}
	if p.Version >= 4 && p.Preflight == nil {
		return fmt.Errorf("resolved policy preflight commands are missing")
	}
	for i, command := range p.Preflight {
		if command.IsZero() {
			return fmt.Errorf("resolved policy preflight command %d is empty", i+1)
		}
		if err := command.ValidateRunners(); err != nil {
			return fmt.Errorf("resolved policy preflight command %d: %w", i+1, err)
		}
	}
	if err := p.Routing.validate(); err != nil {
		return err
	}
	if p.Managed && !p.Routing.Demo {
		if err := p.Routing.validateManaged(); err != nil {
			return err
		}
	}
	if err := p.Runner.validate(); err != nil {
		return err
	}
	if _, err := types.ParseRefreshStrategy(string(p.RefreshStrategy)); err != nil || p.RefreshStrategy == "" {
		return fmt.Errorf("resolved policy refresh strategy is invalid")
	}
	if p.Budgets.CITimeoutNS < 0 || p.Budgets.StepQuietWarningNS < 0 || p.Budgets.ProcessTerminationGraceNS < 0 || p.Budgets.CIRerunTransient < 0 || p.TestEvidence.RetentionNS < 0 || p.TestEvidence.MaxRuns < 0 || p.Intent.SlackDays < 0 || p.Eval.MaxCases < 0 || p.Eval.DiversifiedSize < 0 {
		return fmt.Errorf("resolved policy contains a negative budget")
	}
	if p.Version == 6 {
		if p.LegacyPricing == nil || p.LegacyPricing.Profiles == nil {
			return fmt.Errorf("resolved policy pricing profiles are missing")
		}
	}
	if p.Version >= 7 && p.LegacyPricing != nil {
		return fmt.Errorf("resolved policy contains retired pricing profiles")
	}
	if p.TestEvidence.LegacyStoreInRepo {
		return fmt.Errorf("resolved policy uses retired test.evidence.store_in_repo; remove test.evidence.store_in_repo, test.evidence.dir, and test.evidence.branch, then start a new run")
	}
	for _, limit := range []int{p.Budgets.AutoFix.Lint, p.Budgets.AutoFix.Build, p.Budgets.AutoFix.Test, p.Budgets.AutoFix.Review, p.Budgets.AutoFix.Document, p.Budgets.AutoFix.CI, p.Budgets.AutoFix.Refresh} {
		if limit < 0 {
			return fmt.Errorf("resolved policy contains a negative auto-fix budget")
		}
	}
	seenSources := make(map[string]bool, len(p.Sources))
	for _, source := range p.Sources {
		if seenSources[source.Kind] {
			return fmt.Errorf("resolved policy contains duplicate %s config source", source.Kind)
		}
		seenSources[source.Kind] = true
		if !resolvedPolicyConfigSourceKind(source.Kind) || strings.TrimSpace(source.Digest) == "" {
			return fmt.Errorf("resolved policy contains incomplete config source")
		}
		decodedDigest, err := hex.DecodeString(source.Digest)
		if err != nil || len(decodedDigest) != sha256.Size {
			return fmt.Errorf("resolved policy contains invalid %s config source digest", source.Kind)
		}
		switch source.Kind {
		case db.ConfigSourceGlobal:
			if strings.TrimSpace(source.Path) == "" {
				return fmt.Errorf("resolved policy global config source has no path")
			}
		case db.ConfigSourceBranch, db.ConfigSourceDefault:
			if strings.TrimSpace(source.Ref) == "" {
				return fmt.Errorf("resolved policy %s config source has no ref", source.Kind)
			}
		case db.ConfigSourceGlobalOverride:
			if strings.TrimSpace(source.Ref) == "" || strings.TrimSpace(source.Path) == "" {
				return fmt.Errorf("resolved policy global override source is incomplete")
			}
		}
	}
	return nil
}

func resolvedPolicyRunnerFromConfig(cfg *config.Config) resolvedPolicyRunner {
	if cfg.ResolvedRunner == nil {
		version := "1"
		return resolvedPolicyRunner{Kind: "legacy-platform-default", Platform: runtime.GOOS, Version: &version}
	}
	resolved := cfg.ResolvedRunner
	var version *string
	if resolved.Version != nil {
		value := *resolved.Version
		version = &value
	}
	return resolvedPolicyRunner{
		SchemaVersion: resolved.SchemaVersion,
		Platform:      resolved.Platform,
		Source:        resolved.Source,
		Executable:    resolved.Executable,
		Args:          append([]string(nil), resolved.Args...),
		Version:       version,
	}
}

func canonicalResolvedCommands(commands []runner.Command) []runner.Command {
	if len(commands) == 0 {
		return []runner.Command{}
	}
	canonical := make([]runner.Command, len(commands))
	for i, command := range commands {
		canonical[i] = command.Canonical()
	}
	return canonical
}

func (r resolvedPolicyRunner) validate() error {
	if r.SchemaVersion == 0 {
		if strings.TrimSpace(r.Kind) == "" || strings.TrimSpace(r.Platform) == "" || r.Version == nil || strings.TrimSpace(*r.Version) == "" {
			return fmt.Errorf("resolved policy runner identity is incomplete")
		}
		return nil
	}
	if r.SchemaVersion != runner.SchemaVersion {
		return fmt.Errorf("resolved policy runner schema version %d is unsupported", r.SchemaVersion)
	}
	if r.Kind != "" || strings.TrimSpace(r.Platform) == "" || strings.TrimSpace(r.Source) == "" || strings.TrimSpace(r.Executable) == "" || len(r.Args) == 0 {
		return fmt.Errorf("resolved policy runner provenance is incomplete")
	}
	if err := runner.ValidateSpec(runner.Spec{Executable: r.Executable, Args: r.Args}); err != nil {
		return fmt.Errorf("resolved policy runner provenance: %w", err)
	}
	if err := runner.ValidateVersion(r.Version); err != nil {
		return fmt.Errorf("resolved policy runner provenance: %w", err)
	}
	return nil
}

func resolvedPolicyPipelineStep(step types.StepName) bool {
	switch step.Canonical() {
	case types.StepIntent, types.StepRefresh, types.StepReview, types.StepBuild, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI:
		return true
	default:
		return false
	}
}

func resolvedPolicyConfigSourceKind(kind string) bool {
	switch kind {
	case db.ConfigSourceGlobal, db.ConfigSourceBranch, db.ConfigSourceDefault, db.ConfigSourceGlobalOverride:
		return true
	default:
		return false
	}
}

func resolvedPolicyDigest(encoded []byte) string {
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func sortedResolvedPolicyKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, enabled := range values {
		if enabled {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func copyResolvedPolicyStrings(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
