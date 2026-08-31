package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

const (
	effectiveConfigSchemaVersion = 1
	effectiveConfigYAMLMaxBytes  = 256 * 1024
	effectiveConfigGenerator     = "no-mistakes/effective-config"
)

const (
	effectiveConfigSourceGlobal         = "global"
	effectiveConfigSourceGlobalOverride = "global-override"
	effectiveConfigSourceTrusted        = "trusted"
	effectiveConfigSourcePushed         = "pushed"
	effectiveConfigSourceRunRequest     = "run-request"
	effectiveConfigSourceRuntime        = "runtime"
)

type effectiveConfigProvenanceValue struct {
	Source     string
	IsDefault  bool
	Qualifiers []string
}

func (p effectiveConfigProvenanceValue) comment() string {
	comment := fmt.Sprintf("source=%s; is_default=%t", p.Source, p.IsDefault)
	if len(p.Qualifiers) > 0 {
		comment += "; qualifier=" + strings.Join(p.Qualifiers, ",")
	}
	return comment
}

type effectiveConfigProvenance struct {
	values          map[string]effectiveConfigProvenanceValue
	disabledReaders map[string]effectiveConfigProvenanceValue
}

func captureEffectiveConfigProvenance(global *globalConfigInput, pushed, trusted *repoConfigInput, override *globalOverrideInput, effectiveRepo *config.RepoConfig, allow bool) *effectiveConfigProvenance {
	globalPaths := map[string]bool(nil)
	pushedPaths := map[string]bool(nil)
	trustedPaths := map[string]bool(nil)
	overridePaths := map[string]bool(nil)
	if global != nil && global.Config != nil {
		globalPaths = global.Config.DeclaredPaths()
	}
	if pushed != nil && pushed.Config != nil {
		pushedPaths = pushed.Config.DeclaredPaths()
	}
	if trusted != nil && trusted.Config != nil {
		trustedPaths = trusted.Config.DeclaredPaths()
	}
	if override != nil && override.Config != nil {
		overridePaths = override.Config.DeclaredPaths()
	}
	for _, paths := range []map[string]bool{globalPaths, pushedPaths, trustedPaths, overridePaths} {
		normalizeEffectiveConfigPresence(paths)
	}
	ledger := &effectiveConfigProvenance{values: make(map[string]effectiveConfigProvenanceValue)}
	allPaths := make(map[string]bool)
	for _, declaredPaths := range []map[string]bool{globalPaths, pushedPaths, trustedPaths, overridePaths} {
		for path := range declaredPaths {
			allPaths[path] = true
		}
	}
	for _, path := range effectiveConfigKnownInputPaths() {
		allPaths[path] = true
	}
	for path := range allPaths {
		value := effectiveConfigProvenanceValue{Source: effectiveConfigSourceGlobal, IsDefault: !declaresEffectiveConfigPath(globalPaths, path)}
		selection := effectiveRepoSelectionForPath(path)
		if selection != effectiveRepoNone {
			declared := false
			source := ""
			switch selection {
			case effectiveRepoPushed:
				declared, source = declaresEffectiveConfigPath(pushedPaths, path), effectiveConfigSourcePushed
			case effectiveRepoTrusted:
				declared, source = declaresEffectiveConfigPath(trustedPaths, path), effectiveConfigSourceTrusted
			case effectiveRepoRouted:
				if allow {
					declared, source = declaresEffectiveConfigPath(pushedPaths, path), effectiveConfigSourcePushed
				} else {
					declared, source = declaresEffectiveConfigPath(trustedPaths, path), effectiveConfigSourceTrusted
				}
			}
			if declared {
				value = effectiveConfigProvenanceValue{Source: source}
			}
			if declaresEffectiveConfigPath(overridePaths, path) {
				value = effectiveConfigProvenanceValue{Source: effectiveConfigSourceGlobalOverride}
			}
		}
		ledger.values[path] = value
	}
	for _, field := range []string{"shared", "intent", "refresh", "review", "build", "test", "document", "lint", "pr", "ci"} {
		path := "prompts." + field
		selectedRepo := pushedPaths[path] && allow || trustedPaths[path] && !allow || overridePaths[path]
		if globalPaths[path] && selectedRepo {
			value := ledger.value(path)
			value.Qualifiers = append(value.Qualifiers, "append")
			ledger.values[path] = value
		}
	}
	ledger.disabledReaders = captureDisabledReaderProvenance(global, effectiveRepo, ledger.value("intent.disabled_readers"))
	return ledger
}

func normalizeEffectiveConfigPresence(paths map[string]bool) {
	if len(paths) == 0 {
		return
	}
	aliases := map[string]string{
		"babysit_timeout":  "ci_timeout",
		"auto_fix.babysit": "auto_fix.ci",
		"auto_fix.rebase":  "auto_fix.refresh",
	}
	for legacy, canonical := range aliases {
		if paths[legacy] {
			paths[canonical] = true
		}
	}
	for path := range paths {
		if path == "rebase" || strings.HasPrefix(path, "rebase.") {
			paths["refresh"+strings.TrimPrefix(path, "rebase")] = true
		}
	}
}

// declaresEffectiveConfigPath reports whether a layer supplied the exact
// value. A declared scalar or sequence owns its descendants in the rendered
// explanatory projection; a mapping parent does not, because a later overlay
// may have changed only one of its child fields.
func declaresEffectiveConfigPath(paths map[string]bool, path string) bool {
	if paths[path] {
		return true
	}
	for candidate := path; ; {
		index := strings.LastIndex(candidate, ".")
		if index < 0 {
			return false
		}
		candidate = candidate[:index]
		if !paths[candidate] {
			continue
		}
		prefix := candidate + "."
		for declared := range paths {
			if strings.HasPrefix(declared, prefix) {
				return false
			}
		}
		return true
	}
}

type effectiveRepoSource int

const (
	effectiveRepoNone effectiveRepoSource = iota
	effectiveRepoPushed
	effectiveRepoTrusted
	effectiveRepoRouted
)

func (p *effectiveConfigProvenance) value(path string) effectiveConfigProvenanceValue {
	if p != nil {
		if value, ok := p.values[path]; ok {
			return value
		}
	}
	return effectiveConfigProvenanceValue{Source: effectiveConfigSourceGlobal, IsDefault: true}
}

func effectiveRepoSelectionForPath(path string) effectiveRepoSource {
	globalOnly := []string{
		"managed", "runner", "acpx_path", "acp_registry_overrides", "agent_path_override", "agent_args_override",
		"ci_timeout", "step_quiet_warning", "process_termination_grace", "log_level", "session_reuse", "eval",
		"test.evidence.local_root", "test.evidence.retention", "test.evidence.max_runs",
	}
	for _, prefix := range globalOnly {
		if path == prefix || strings.HasPrefix(path, prefix+".") {
			return effectiveRepoNone
		}
	}
	trustedOnly := []string{
		"refresh.strategy", "document.instructions", "review.path_instructions", "disable_project_settings", "no_ci",
		"ci.rerun_transient", "test.evidence.branch", "pipeline.skip_steps", "allow_repo_commands",
	}
	for _, prefix := range trustedOnly {
		if path == prefix || strings.HasPrefix(path, prefix+".") {
			return effectiveRepoTrusted
		}
	}
	routed := []string{
		"agent", "commands", "preflight", "hooks", "prompts", "intent.agent", "intent.model", "refresh.agent", "refresh.model",
		"review.agent", "review.model", "review.candidates", "build.agent", "build.model", "test.agent", "test.model",
		"document.agent", "document.model", "lint.agent", "lint.model", "pr.agent", "pr.model", "ci.agent", "ci.model",
	}
	for _, prefix := range routed {
		if path == prefix || strings.HasPrefix(path, prefix+".") {
			return effectiveRepoRouted
		}
	}
	return effectiveRepoPushed
}

func effectiveConfigKnownInputPaths() []string {
	paths := []string{
		"managed", "runner.executable", "runner.args", "agent", "preflight", "hooks.post_worktree", "hooks.pr_body",
		"acpx_path", "acp_registry_overrides", "agent_path_override", "agent_args_override", "ci_timeout", "step_quiet_warning",
		"process_termination_grace", "log_level", "session_reuse", "ignore_patterns", "ci.rerun_transient", "commit.fix_message",
		"intent.enabled", "intent.threshold", "intent.slack_days", "intent.disabled_readers", "test.evidence.store_in_repo",
		"test.evidence.dir", "test.evidence.branch", "test.evidence.local_root", "test.evidence.retention", "test.evidence.max_runs",
		"document.instructions", "review.path_instructions", "refresh.strategy", "pipeline.skip_steps", "disable_project_settings", "no_ci",
	}
	for _, prefix := range []string{"auto_fix", "eval", "prompts"} {
		var fields []string
		switch prefix {
		case "auto_fix":
			fields = []string{"refresh", "review", "build", "test", "document", "lint", "ci"}
		case "eval":
			fields = []string{"capture_provenance", "auto_capture", "max_cases", "diversified_size"}
		case "prompts":
			fields = []string{"shared", "intent", "refresh", "review", "build", "test", "document", "lint", "pr", "ci"}
		}
		for _, field := range fields {
			paths = append(paths, prefix+"."+field)
		}
	}
	for _, command := range []string{"build", "test", "lint", "format"} {
		paths = append(paths, "commands."+command)
		for _, suffix := range []string{"run", "runner", "runner.executable", "runner.args", "linux", "macos", "windows"} {
			paths = append(paths, "commands."+command+"."+suffix)
		}
	}
	return paths
}

func captureDisabledReaderProvenance(global *globalConfigInput, effectiveRepo *config.RepoConfig, repoValue effectiveConfigProvenanceValue) map[string]effectiveConfigProvenanceValue {
	values := make(map[string]effectiveConfigProvenanceValue)
	globalReaders := make(map[string]bool)
	if global != nil && global.Config != nil {
		for _, reader := range global.Config.Intent.DisabledReaders {
			reader = strings.ToLower(strings.TrimSpace(reader))
			globalReaders[reader] = true
			values[reader] = effectiveConfigProvenanceValue{Source: effectiveConfigSourceGlobal}
		}
	}
	if effectiveRepo != nil {
		for _, reader := range effectiveRepo.Intent.DisabledReaders {
			reader = strings.ToLower(strings.TrimSpace(reader))
			value := repoValue
			if globalReaders[reader] {
				value.Qualifiers = append(value.Qualifiers, "merge")
			}
			values[reader] = value
		}
	}
	return values
}

type effectiveConfigArtifacts struct {
	YAML         []byte
	Meta         []byte
	PolicyDigest string
}

type effectiveConfigMetadata struct {
	SchemaVersion   int    `json:"schema_version"`
	RunID           string `json:"run_id"`
	PolicyDigest    string `json:"policy_digest"`
	YAMLSHA256      string `json:"yaml_sha256"`
	BinaryVersion   string `json:"binary_version"`
	BinaryBuildSHA  string `json:"binary_build_sha"`
	Generator       string `json:"generator"`
	GeneratorSchema int    `json:"generator_schema"`
}

type effectiveConfigDocument struct {
	Managed                 bool                      `yaml:"managed"`
	Agent                   effectiveAgentDocument    `yaml:"agent"`
	Runner                  effectiveRunnerDocument   `yaml:"runner"`
	Run                     effectiveRunDocument      `yaml:"run"`
	Pipeline                effectivePipelineDocument `yaml:"pipeline"`
	Commands                effectiveCommandsDocument `yaml:"commands"`
	Preflight               []runner.Command          `yaml:"preflight"`
	Hooks                   config.Hooks              `yaml:"hooks"`
	ACPXPath                string                    `yaml:"acpx_path"`
	ACPRegistryOverrides    map[string]string         `yaml:"acp_registry_overrides"`
	AgentPathOverride       map[string]string         `yaml:"agent_path_override"`
	AgentArgsOverride       map[string][]string       `yaml:"agent_args_override"`
	CITimeout               string                    `yaml:"ci_timeout"`
	StepQuietWarning        string                    `yaml:"step_quiet_warning"`
	ProcessTerminationGrace string                    `yaml:"process_termination_grace"`
	LogLevel                string                    `yaml:"log_level"`
	SessionReuse            bool                      `yaml:"session_reuse"`
	IgnorePatterns          []string                  `yaml:"ignore_patterns"`
	AutoFix                 effectiveAutoFixDocument  `yaml:"auto_fix"`
	CI                      effectiveCIDocument       `yaml:"ci"`
	Commit                  effectiveCommitDocument   `yaml:"commit"`
	Intent                  effectiveIntentDocument   `yaml:"intent"`
	Test                    effectiveTestDocument     `yaml:"test"`
	Document                effectiveDocumentDocument `yaml:"document"`
	Review                  effectiveReviewDocument   `yaml:"review"`
	Eval                    effectiveEvalDocument     `yaml:"eval"`
	Prompts                 config.PromptConfig       `yaml:"prompts"`
	DisableProjectSettings  bool                      `yaml:"disable_project_settings"`
	NoCI                    bool                      `yaml:"no_ci"`
}

type effectiveAgentDocument struct {
	Demo             bool                                   `yaml:"demo"`
	Default          []types.AgentName                      `yaml:"default"`
	StepRoutes       map[types.StepName]effectiveAgentRoute `yaml:"step_routes"`
	ReviewCandidates []effectiveReviewCandidate             `yaml:"review_candidates"`
}

type effectiveAgentRoute struct {
	Agents []types.AgentName `yaml:"agents"`
	Model  config.ModelRoute `yaml:"model"`
}

type effectiveReviewCandidate struct {
	Agent    types.AgentName   `yaml:"agent"`
	Model    config.ModelRoute `yaml:"model"`
	Optional bool              `yaml:"optional"`
}

type effectiveRunnerDocument struct {
	Configured runner.Spec             `yaml:"configured"`
	Resolved   effectiveResolvedRunner `yaml:"resolved"`
}

type effectiveResolvedRunner struct {
	SchemaVersion int      `yaml:"schema_version"`
	Platform      string   `yaml:"platform"`
	Source        string   `yaml:"source"`
	Executable    string   `yaml:"executable"`
	Args          []string `yaml:"args"`
	Version       *string  `yaml:"version"`
}

type effectiveRunDocument struct {
	RefreshStrategy types.RefreshStrategy `yaml:"refresh_strategy"`
	StackedOn       string                `yaml:"stacked_on"`
	SkipSteps       []types.StepSkip      `yaml:"skip_steps"`
}

type effectivePipelineDocument struct {
	ConfiguredSkipSteps []types.StepName      `yaml:"configured_skip_steps"`
	Steps               []effectivePolicyStep `yaml:"steps"`
}

type effectivePolicyStep struct {
	Name       types.StepName   `yaml:"name"`
	Status     string           `yaml:"status"`
	SkipSource types.SkipSource `yaml:"skip_source"`
}

type effectiveCommandsDocument struct {
	Build  runner.Command `yaml:"build"`
	Test   runner.Command `yaml:"test"`
	Lint   runner.Command `yaml:"lint"`
	Format runner.Command `yaml:"format"`
}

type effectiveAutoFixDocument struct {
	Refresh  int `yaml:"refresh"`
	Review   int `yaml:"review"`
	Build    int `yaml:"build"`
	Test     int `yaml:"test"`
	Document int `yaml:"document"`
	Lint     int `yaml:"lint"`
	CI       int `yaml:"ci"`
}

type effectiveCIDocument struct {
	RerunTransient int `yaml:"rerun_transient"`
}

type effectiveCommitDocument struct {
	FixMessage string `yaml:"fix_message"`
}

type effectiveIntentDocument struct {
	Enabled         bool     `yaml:"enabled"`
	Threshold       float64  `yaml:"threshold"`
	SlackDays       int      `yaml:"slack_days"`
	DisabledReaders []string `yaml:"disabled_readers"`
}

type effectiveTestDocument struct {
	Evidence effectiveEvidenceDocument `yaml:"evidence"`
}

type effectiveEvidenceDocument struct {
	StoreInRepo bool   `yaml:"store_in_repo"`
	Dir         string `yaml:"dir"`
	Branch      string `yaml:"branch"`
	LocalRoot   string `yaml:"local_root"`
	Retention   string `yaml:"retention"`
	MaxRuns     int    `yaml:"max_runs"`
}

type effectiveDocumentDocument struct {
	Instructions string `yaml:"instructions"`
}

type effectiveReviewDocument struct {
	PathInstructions []config.PathInstruction `yaml:"path_instructions"`
}

type effectiveEvalDocument struct {
	CaptureProvenance bool `yaml:"capture_provenance"`
	AutoCapture       bool `yaml:"auto_capture"`
	MaxCases          int  `yaml:"max_cases"`
	DiversifiedSize   int  `yaml:"diversified_size"`
}

func renderEffectiveConfigArtifacts(runID, stackedOn string, resolved *runPolicyResolution) (*effectiveConfigArtifacts, error) {
	if resolved == nil || resolved.Config == nil || resolved.Policy == nil || resolved.Provenance == nil {
		return nil, fmt.Errorf("effective config resolution is incomplete")
	}
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("effective config run ID is empty")
	}
	if !validSHA256Hex(resolved.ResolvedPolicyDigest) {
		return nil, fmt.Errorf("effective config policy digest is invalid")
	}
	document := effectiveConfigDocumentFromResolution(stackedOn, resolved)
	var root yaml.Node
	if err := root.Encode(document); err != nil {
		return nil, fmt.Errorf("serialize effective config: %w", err)
	}
	annotations := effectiveConfigAnnotations(resolved, document)
	annotateEffectiveConfigNode(&root, "", annotations)
	if err := validateEffectiveConfigAnnotations(&root, ""); err != nil {
		return nil, fmt.Errorf("effective config is incomplete: %w", err)
	}
	var rendered bytes.Buffer
	encoder := yaml.NewEncoder(&rendered)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, fmt.Errorf("serialize effective config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("serialize effective config: %w", err)
	}
	if rendered.Len() > effectiveConfigYAMLMaxBytes {
		return nil, fmt.Errorf("effective config YAML is %d bytes; complete artifacts must not exceed %d bytes", rendered.Len(), effectiveConfigYAMLMaxBytes)
	}
	yamlBytes := append([]byte(nil), rendered.Bytes()...)
	digest := sha256.Sum256(yamlBytes)
	metadata := effectiveConfigMetadata{
		SchemaVersion:   effectiveConfigSchemaVersion,
		RunID:           runID,
		PolicyDigest:    resolved.ResolvedPolicyDigest,
		YAMLSHA256:      hex.EncodeToString(digest[:]),
		BinaryVersion:   buildinfo.CurrentVersion(),
		BinaryBuildSHA:  buildinfo.Commit,
		Generator:       effectiveConfigGenerator,
		GeneratorSchema: effectiveConfigSchemaVersion,
	}
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("serialize effective config sidecar: %w", err)
	}
	metaBytes = append(metaBytes, '\n')
	if err := validateEffectiveConfigArtifacts(yamlBytes, metaBytes, runID, resolved.ResolvedPolicyDigest); err != nil {
		return nil, err
	}
	return &effectiveConfigArtifacts{YAML: yamlBytes, Meta: metaBytes, PolicyDigest: resolved.ResolvedPolicyDigest}, nil
}

func effectiveConfigDocumentFromResolution(stackedOn string, resolved *runPolicyResolution) effectiveConfigDocument {
	cfg := resolved.Config
	routing := resolved.Policy.Routing
	stepRoutes := make(map[types.StepName]effectiveAgentRoute, len(routing.StepRoutes))
	for step, route := range routing.StepRoutes {
		stepRoutes[step] = effectiveAgentRoute{
			Agents: append([]types.AgentName(nil), route.Agents...),
			Model:  config.ModelRoute{Name: route.Model.Name, Vendor: route.Model.Vendor},
		}
	}
	candidates := make([]effectiveReviewCandidate, 0, len(routing.ReviewCandidates))
	for _, candidate := range routing.ReviewCandidates {
		candidates = append(candidates, effectiveReviewCandidate{
			Agent: candidate.Agent, Model: config.ModelRoute{Name: candidate.Model.Name, Vendor: candidate.Model.Vendor}, Optional: candidate.Optional,
		})
	}
	steps := make([]effectivePolicyStep, 0, len(resolved.Policy.Steps))
	for _, step := range resolved.Policy.Steps {
		steps = append(steps, effectivePolicyStep{Name: step.Name, Status: step.Status, SkipSource: step.SkipSource})
	}
	disabledReaders := sortedResolvedPolicyKeys(cfg.Intent.DisabledReaders)
	return effectiveConfigDocument{
		Managed: cfg.Managed,
		Agent: effectiveAgentDocument{
			Demo: routing.Demo, Default: append([]types.AgentName(nil), routing.DefaultAgents...), StepRoutes: stepRoutes, ReviewCandidates: candidates,
		},
		Runner: effectiveRunnerDocument{
			Configured: cfg.Runner.Clone(),
			Resolved: effectiveResolvedRunner{
				SchemaVersion: resolved.Policy.Runner.SchemaVersion, Platform: resolved.Policy.Runner.Platform,
				Source: resolved.Policy.Runner.Source, Executable: resolved.Policy.Runner.Executable,
				Args: append([]string(nil), resolved.Policy.Runner.Args...), Version: resolved.Policy.Runner.Version,
			},
		},
		Run:      effectiveRunDocument{RefreshStrategy: resolved.RefreshStrategy, StackedOn: stackedOn, SkipSteps: append([]types.StepSkip(nil), resolved.Skips...)},
		Pipeline: effectivePipelineDocument{ConfiguredSkipSteps: append([]types.StepName(nil), cfg.ConfiguredSkipSteps...), Steps: steps},
		Commands: effectiveCommandsDocument{
			Build: cfg.Commands.BuildCommand().Canonical(), Test: cfg.Commands.TestCommand().Canonical(),
			Lint: cfg.Commands.LintCommand().Canonical(), Format: cfg.Commands.FormatCommand().Canonical(),
		},
		Preflight:               canonicalResolvedCommands(cfg.Preflight),
		Hooks:                   cfg.Hooks,
		ACPXPath:                cfg.ACPXPath,
		ACPRegistryOverrides:    copyStringMap(cfg.ACPRegistryOverrides),
		AgentPathOverride:       copyStringMap(cfg.AgentPathOverride),
		AgentArgsOverride:       copyStringSliceMap(cfg.AgentArgsOverride),
		CITimeout:               cfg.CITimeout.String(),
		StepQuietWarning:        cfg.StepQuietWarning.String(),
		ProcessTerminationGrace: cfg.ProcessTerminationGrace.String(),
		LogLevel:                cfg.LogLevel,
		SessionReuse:            cfg.SessionReuse,
		IgnorePatterns:          append([]string(nil), cfg.IgnorePatterns...),
		AutoFix: effectiveAutoFixDocument{
			Refresh: cfg.AutoFix.Refresh, Review: cfg.AutoFix.Review, Build: cfg.AutoFix.Build, Test: cfg.AutoFix.Test,
			Document: cfg.AutoFix.Document, Lint: cfg.AutoFix.Lint, CI: cfg.AutoFix.CI,
		},
		CI:                     effectiveCIDocument{RerunTransient: cfg.CI.RerunTransient},
		Commit:                 effectiveCommitDocument{FixMessage: cfg.Commit.FixMessage},
		Intent:                 effectiveIntentDocument{Enabled: cfg.Intent.Enabled, Threshold: cfg.Intent.Threshold, SlackDays: cfg.Intent.SlackDays, DisabledReaders: disabledReaders},
		Test:                   effectiveTestDocument{Evidence: effectiveEvidenceDocument{StoreInRepo: cfg.Test.Evidence.StoreInRepo, Dir: cfg.Test.Evidence.Dir, Branch: cfg.Test.Evidence.Branch, LocalRoot: cfg.Test.Evidence.LocalRoot, Retention: cfg.Test.Evidence.Retention.String(), MaxRuns: cfg.Test.Evidence.MaxRuns}},
		Document:               effectiveDocumentDocument{Instructions: cfg.Document.Instructions},
		Review:                 effectiveReviewDocument{PathInstructions: append([]config.PathInstruction(nil), cfg.Review.PathInstructions...)},
		Eval:                   effectiveEvalDocument{CaptureProvenance: cfg.Eval.CaptureProvenance, AutoCapture: cfg.Eval.AutoCapture, MaxCases: cfg.Eval.MaxCases, DiversifiedSize: cfg.Eval.DiversifiedSize},
		Prompts:                cfg.Prompts,
		DisableProjectSettings: cfg.DisableProjectSettings,
		NoCI:                   cfg.NoCI,
	}
}

func copyStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func copyStringSliceMap(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for key, value := range source {
		result[key] = append([]string(nil), value...)
	}
	return result
}

func effectiveConfigAnnotations(resolved *runPolicyResolution, document effectiveConfigDocument) map[string]effectiveConfigProvenanceValue {
	ledger := resolved.Provenance
	annotations := make(map[string]effectiveConfigProvenanceValue)
	set := func(artifactPath, configPath string) {
		annotations[artifactPath] = ledger.value(configPath)
	}
	setGlobal := func(path string) { annotations[path] = ledger.value(path) }
	runtimeValue := effectiveConfigProvenanceValue{Source: effectiveConfigSourceRuntime}
	for _, path := range []string{"agent", "runner.resolved", "pipeline.steps"} {
		annotations[path] = runtimeValue
	}
	setGlobal("managed")
	annotations["runner.configured.executable"] = ledger.value("runner.executable")
	annotations["runner.configured.args"] = ledger.value("runner.args")
	annotations["run.refresh_strategy"] = resolved.RefreshProvenance
	annotations["run.stacked_on"] = effectiveConfigProvenanceValue{Source: effectiveConfigSourceGlobal, IsDefault: true}
	if strings.TrimSpace(document.Run.StackedOn) != "" {
		annotations["run.stacked_on"] = effectiveConfigProvenanceValue{Source: effectiveConfigSourceRunRequest}
	}
	annotations["run.skip_steps"] = resolved.SkipProvenance
	for i, skip := range document.Run.SkipSteps {
		source := effectiveConfigSourceRuntime
		if skip.Source == types.SkipSourceRunRequest {
			source = effectiveConfigSourceRunRequest
		} else if skip.Source == types.SkipSourceGlobalOverride {
			source = effectiveConfigSourceGlobalOverride
		}
		annotations[fmt.Sprintf("run.skip_steps[%d]", i)] = effectiveConfigProvenanceValue{Source: source}
	}
	set("pipeline.configured_skip_steps", "pipeline.skip_steps")
	for _, command := range []string{"build", "test", "lint", "format"} {
		set("commands."+command, "commands."+command)
		for _, suffix := range []string{"run", "runner", "runner.executable", "runner.args", "linux", "macos", "windows"} {
			set("commands."+command+"."+suffix, "commands."+command+"."+suffix)
		}
	}
	set("preflight", "preflight")
	set("hooks.post_worktree", "hooks.post_worktree")
	set("hooks.pr_body", "hooks.pr_body")
	for _, path := range []string{"acpx_path", "acp_registry_overrides", "agent_path_override", "agent_args_override", "ci_timeout", "step_quiet_warning", "process_termination_grace", "log_level", "session_reuse"} {
		setGlobal(path)
	}
	set("ignore_patterns", "ignore_patterns")
	for _, field := range []string{"refresh", "review", "build", "test", "document", "lint", "ci"} {
		set("auto_fix."+field, "auto_fix."+field)
	}
	set("ci.rerun_transient", "ci.rerun_transient")
	set("commit.fix_message", "commit.fix_message")
	for _, field := range []string{"enabled", "threshold", "slack_days"} {
		set("intent."+field, "intent."+field)
	}
	set("intent.disabled_readers", "intent.disabled_readers")
	set("test.evidence.store_in_repo", "test.evidence.store_in_repo")
	set("test.evidence.dir", "test.evidence.dir")
	set("test.evidence.branch", "test.evidence.branch")
	for _, field := range []string{"local_root", "retention", "max_runs"} {
		setGlobal("test.evidence." + field)
	}
	set("document.instructions", "document.instructions")
	set("review.path_instructions", "review.path_instructions")
	for _, field := range []string{"capture_provenance", "auto_capture", "max_cases", "diversified_size"} {
		setGlobal("eval." + field)
	}
	for _, field := range []string{"shared", "intent", "refresh", "review", "build", "test", "document", "lint", "pr", "ci"} {
		path := "prompts." + field
		set(path, path)
	}
	set("disable_project_settings", "disable_project_settings")
	set("no_ci", "no_ci")
	annotateDisabledReaderSources(annotations, ledger, document.Intent.DisabledReaders)
	return annotations
}

func annotateDisabledReaderSources(annotations map[string]effectiveConfigProvenanceValue, ledger *effectiveConfigProvenance, readers []string) {
	if ledger == nil {
		return
	}
	for i, reader := range readers {
		value, ok := ledger.disabledReaders[reader]
		if !ok {
			value = ledger.value("intent.disabled_readers")
		}
		annotations[fmt.Sprintf("intent.disabled_readers[%d]", i)] = value
	}
}

var effectiveConfigIndexPattern = regexp.MustCompile(`\[[0-9]+\]`)
var effectiveConfigCommentPattern = regexp.MustCompile(`^#?\s*source=(global|global-override|trusted|pushed|run-request|runtime); is_default=(true|false)(; qualifier=(clear|append|merge)(,(clear|append|merge))*)?$`)

func annotationForEffectiveConfigPath(path string, annotations map[string]effectiveConfigProvenanceValue) effectiveConfigProvenanceValue {
	for _, initial := range []string{path, effectiveConfigIndexPattern.ReplaceAllString(path, "[]")} {
		for candidate := initial; candidate != ""; {
			if value, ok := annotations[candidate]; ok {
				return value
			}
			if strings.HasSuffix(candidate, "[]") {
				candidate = strings.TrimSuffix(candidate, "[]")
				continue
			}
			if index := strings.LastIndex(candidate, "."); index >= 0 {
				candidate = candidate[:index]
			} else {
				break
			}
		}
	}
	return effectiveConfigProvenanceValue{}
}

func annotateEffectiveConfigNode(node *yaml.Node, path string, annotations map[string]effectiveConfigProvenanceValue) {
	if node == nil {
		return
	}
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			annotateEffectiveConfigNode(child, path, annotations)
		}
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		if len(node.Content) == 0 && path != "" {
			setEffectiveConfigComment(node, annotationForEffectiveConfigPath(path, annotations), true)
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			childPath := key.Value
			if path != "" {
				childPath = path + "." + key.Value
			}
			annotateEffectiveConfigNode(value, childPath, annotations)
		}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			setEffectiveConfigComment(node, annotationForEffectiveConfigPath(path, annotations), true)
			return
		}
		for i, item := range node.Content {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			setEffectiveConfigComment(item, annotationForEffectiveConfigPath(itemPath, annotations), effectiveConfigNodeIsEmpty(item))
			annotateEffectiveConfigNode(item, itemPath, annotations)
		}
	case yaml.ScalarNode:
		setEffectiveConfigComment(node, annotationForEffectiveConfigPath(path, annotations), effectiveConfigNodeIsEmpty(node))
	}
}

func setEffectiveConfigComment(node *yaml.Node, provenance effectiveConfigProvenanceValue, empty bool) {
	if empty && !provenance.IsDefault && provenance.Source != effectiveConfigSourceRuntime && !containsString(provenance.Qualifiers, "clear") {
		provenance.Qualifiers = append(provenance.Qualifiers, "clear")
	}
	comment := provenance.comment()
	if (node.Kind == yaml.MappingNode || node.Kind == yaml.SequenceNode) && len(node.Content) > 0 {
		node.HeadComment = comment
		return
	}
	node.LineComment = comment
}

func effectiveConfigNodeIsEmpty(node *yaml.Node) bool {
	if node == nil {
		return true
	}
	if node.Kind == yaml.ScalarNode {
		return node.Tag == "!!null" || node.Tag == "!!str" && node.Value == ""
	}
	return (node.Kind == yaml.MappingNode || node.Kind == yaml.SequenceNode) && len(node.Content) == 0
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func validateEffectiveConfigAnnotations(node *yaml.Node, path string) error {
	if node == nil {
		return fmt.Errorf("missing YAML node at %s", path)
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return fmt.Errorf("document has %d roots", len(node.Content))
		}
		return validateEffectiveConfigAnnotations(node.Content[0], path)
	}
	switch node.Kind {
	case yaml.MappingNode:
		if len(node.Content) == 0 {
			if err := validateEffectiveConfigComment(node, path); err != nil {
				return err
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			childPath := node.Content[i].Value
			if path != "" {
				childPath = path + "." + childPath
			}
			if err := validateEffectiveConfigAnnotations(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			if err := validateEffectiveConfigComment(node, path); err != nil {
				return err
			}
		}
		for i, item := range node.Content {
			if err := validateEffectiveConfigComment(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
			if err := validateEffectiveConfigAnnotations(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if err := validateEffectiveConfigComment(node, path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported YAML node kind %d at %s", node.Kind, path)
	}
	return nil
}

func validateEffectiveConfigComment(node *yaml.Node, path string) error {
	comment := node.LineComment
	if comment == "" {
		comment = node.HeadComment
	}
	if !effectiveConfigCommentPattern.MatchString(comment) {
		return fmt.Errorf("value %s has invalid or missing provenance", path)
	}
	return nil
}

func validSHA256Hex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validateEffectiveConfigArtifacts(yamlBytes, metaBytes []byte, runID, policyDigest string) error {
	if len(yamlBytes) == 0 || len(yamlBytes) > effectiveConfigYAMLMaxBytes {
		return fmt.Errorf("effective config YAML completeness or size validation failed")
	}
	var root yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &root); err != nil {
		return fmt.Errorf("validate effective config YAML: %w", err)
	}
	if err := validateEffectiveConfigAnnotations(&root, ""); err != nil {
		return fmt.Errorf("effective config is incomplete: %w", err)
	}
	var metadata effectiveConfigMetadata
	decoder := json.NewDecoder(bytes.NewReader(metaBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return fmt.Errorf("validate effective config sidecar: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("validate effective config sidecar: trailing content")
	}
	digest := sha256.Sum256(yamlBytes)
	if !validSHA256Hex(policyDigest) || !validSHA256Hex(metadata.PolicyDigest) || metadata.SchemaVersion != effectiveConfigSchemaVersion || metadata.GeneratorSchema != effectiveConfigSchemaVersion || metadata.Generator != effectiveConfigGenerator || metadata.RunID != runID || metadata.PolicyDigest != policyDigest || metadata.YAMLSHA256 != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("effective config sidecar integrity does not match rendered YAML")
	}
	if strings.TrimSpace(metadata.BinaryVersion) == "" || strings.TrimSpace(metadata.BinaryBuildSHA) == "" {
		return fmt.Errorf("effective config sidecar binary identity is incomplete")
	}
	return nil
}

func persistEffectiveConfigArtifacts(p *paths.Paths, runID string, artifacts *effectiveConfigArtifacts) (err error) {
	if p == nil || artifacts == nil {
		return fmt.Errorf("persist effective config: artifacts are incomplete")
	}
	if err := os.MkdirAll(p.RunsDir(), 0o755); err != nil {
		return fmt.Errorf("persist effective config: create runs directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(p.RunsDir(), "."+runID+"-")
	if err != nil {
		return fmt.Errorf("persist effective config: create staging directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(tempDir); err == nil && cleanupErr != nil {
			err = fmt.Errorf("persist effective config: clean staging directory: %w", cleanupErr)
		}
	}()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		return fmt.Errorf("persist effective config: protect staging directory: %w", err)
	}
	if err := writeOwnerOnlyFile(filepath.Join(tempDir, "effective-config.yaml"), artifacts.YAML); err != nil {
		return fmt.Errorf("persist effective config YAML: %w", err)
	}
	if err := writeOwnerOnlyFile(filepath.Join(tempDir, "effective-config.meta.json"), artifacts.Meta); err != nil {
		return fmt.Errorf("persist effective config sidecar: %w", err)
	}
	if err := validatePersistedEffectiveConfig(tempDir, runID, artifacts.PolicyDigest); err != nil {
		return err
	}
	if _, statErr := os.Stat(p.RunDir(runID)); statErr == nil {
		return fmt.Errorf("persist effective config: run artifact directory already exists")
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("persist effective config: inspect run artifact directory: %w", statErr)
	}
	if err := os.Rename(tempDir, p.RunDir(runID)); err != nil {
		return fmt.Errorf("persist effective config atomically: %w", err)
	}
	return nil
}

func writeOwnerOnlyFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func validatePersistedEffectiveConfig(dir, runID, policyDigest string) error {
	yamlBytes, err := os.ReadFile(filepath.Join(dir, "effective-config.yaml"))
	if err != nil {
		return fmt.Errorf("verify effective config YAML: %w", err)
	}
	metaBytes, err := os.ReadFile(filepath.Join(dir, "effective-config.meta.json"))
	if err != nil {
		return fmt.Errorf("verify effective config sidecar: %w", err)
	}
	return validateEffectiveConfigArtifacts(yamlBytes, metaBytes, runID, policyDigest)
}

func removeEffectiveConfigArtifacts(p *paths.Paths, runID string) {
	if p == nil || strings.TrimSpace(runID) == "" {
		return
	}
	_ = os.RemoveAll(p.RunDir(runID))
}
