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
	effectiveConfigSourceGlobal         = config.EffectiveConfigSourceGlobal
	effectiveConfigSourceGlobalOverride = config.EffectiveConfigSourceGlobalOverride
	effectiveConfigSourceRunRequest     = config.EffectiveConfigSourceRunRequest
	effectiveConfigSourceRuntime        = config.EffectiveConfigSourceRuntime
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
	AllowRepoCommands       bool                      `yaml:"allow_repo_commands"`
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
	LocalRoot string `yaml:"local_root"`
	Retention string `yaml:"retention"`
	MaxRuns   int    `yaml:"max_runs"`
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
	if err := materializeEffectiveConfigCommandClears(&root, resolved.Provenance, document.Commands); err != nil {
		return nil, fmt.Errorf("serialize effective config command clears: %w", err)
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
		AllowRepoCommands:       cfg.AllowRepoCommands,
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
		Test:                   effectiveTestDocument{Evidence: effectiveEvidenceDocument{LocalRoot: cfg.Test.Evidence.LocalRoot, Retention: cfg.Test.Evidence.Retention.String(), MaxRuns: cfg.Test.Evidence.MaxRuns}},
		Document:               effectiveDocumentDocument{Instructions: cfg.Document.Instructions},
		Review:                 effectiveReviewDocument{PathInstructions: append([]config.PathInstruction(nil), cfg.Review.PathInstructions...)},
		Eval:                   effectiveEvalDocument{CaptureProvenance: cfg.Eval.CaptureProvenance, AutoCapture: cfg.Eval.AutoCapture, MaxCases: cfg.Eval.MaxCases, DiversifiedSize: cfg.Eval.DiversifiedSize},
		Prompts:                cfg.Prompts,
		DisableProjectSettings: cfg.DisableProjectSettings,
		NoCI:                   cfg.NoCI,
	}
}

func materializeEffectiveConfigCommandClears(root *yaml.Node, ledger *config.EffectiveConfigProvenance, commands effectiveCommandsDocument) error {
	commandNodes, err := effectiveConfigMappingValue(root, "commands")
	if err != nil {
		return err
	}
	for _, command := range []struct {
		name  string
		value runner.Command
	}{
		{name: "build", value: commands.Build},
		{name: "test", value: commands.Test},
		{name: "lint", value: commands.Lint},
		{name: "format", value: commands.Format},
	} {
		commandNode, err := effectiveConfigMappingValue(commandNodes, command.name)
		if err != nil {
			return err
		}
		prefix := "commands." + command.name
		if command.value.Runner == nil && effectiveConfigHasExactClear(ledger, prefix+".runner") {
			if err := materializeEffectiveConfigCommandNull(commandNode, []string{"runner"}); err != nil {
				return fmt.Errorf("%s.runner: %w", prefix, err)
			}
		}
		for _, platform := range []struct {
			name  string
			value *runner.Override
		}{
			{name: "linux", value: command.value.Linux},
			{name: "macos", value: command.value.MacOS},
			{name: "windows", value: command.value.Windows},
		} {
			platformPath := prefix + "." + platform.name
			if platform.value == nil {
				if effectiveConfigHasExactClear(ledger, platformPath) {
					if err := materializeEffectiveConfigCommandNull(commandNode, []string{platform.name}); err != nil {
						return fmt.Errorf("%s: %w", platformPath, err)
					}
				}
				continue
			}
			if platform.value.Run == nil && effectiveConfigHasExactClear(ledger, platformPath+".run") {
				if err := materializeEffectiveConfigCommandNull(commandNode, []string{platform.name, "run"}); err != nil {
					return fmt.Errorf("%s.run: %w", platformPath, err)
				}
			}
			if platform.value.Runner == nil && effectiveConfigHasExactClear(ledger, platformPath+".runner") {
				if err := materializeEffectiveConfigCommandNull(commandNode, []string{platform.name, "runner"}); err != nil {
					return fmt.Errorf("%s.runner: %w", platformPath, err)
				}
			}
		}
	}
	return nil
}

func effectiveConfigHasExactClear(ledger *config.EffectiveConfigProvenance, path string) bool {
	value, ok := ledger.ExactValue(path)
	return ok && !value.IsDefault
}

func materializeEffectiveConfigCommandNull(command *yaml.Node, path []string) error {
	if command.Kind == yaml.ScalarNode {
		run := *command
		*command = yaml.Node{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "run"},
				&run,
			},
		}
	}
	if command.Kind != yaml.MappingNode {
		return fmt.Errorf("command is YAML node kind %d, want scalar or mapping", command.Kind)
	}
	parent := command
	for _, field := range path[:len(path)-1] {
		var err error
		parent, err = effectiveConfigMappingValue(parent, field)
		if err != nil {
			return err
		}
	}
	return insertEffectiveConfigNull(parent, path[len(path)-1])
}

func effectiveConfigMappingValue(node *yaml.Node, key string) (*yaml.Node, error) {
	if node != nil && node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return nil, fmt.Errorf("document has %d roots", len(node.Content))
		}
		node = node.Content[0]
	}
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parent of %q is not a mapping", key)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1], nil
		}
	}
	return nil, fmt.Errorf("mapping is missing %q", key)
}

func insertEffectiveConfigNull(mapping *yaml.Node, key string) error {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return fmt.Errorf("parent of %q is not a mapping", key)
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return fmt.Errorf("mapping already contains %q", key)
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"},
	)
	return nil
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
		annotations[artifactPath] = effectiveConfigProvenanceFromConfig(ledger.Value(configPath))
	}
	setGlobal := func(path string) { annotations[path] = effectiveConfigProvenanceFromConfig(ledger.Value(path)) }
	runtimeValue := effectiveConfigProvenanceValue{Source: effectiveConfigSourceRuntime}
	for _, path := range []string{"agent.demo", "runner.resolved", "pipeline.steps"} {
		annotations[path] = runtimeValue
	}
	if document.Agent.Demo {
		annotations["agent.default"] = runtimeValue
		annotations["agent.step_routes"] = runtimeValue
		annotations["agent.review_candidates"] = runtimeValue
	} else {
		set("agent.default", "resolved.agent.default")
		for i := range document.Agent.Default {
			set(fmt.Sprintf("agent.default[%d]", i), fmt.Sprintf("resolved.agent.default[%d]", i))
		}
		annotations["agent.step_routes"] = runtimeValue
		annotations["agent.review_candidates"] = runtimeValue
		for step := range document.Agent.StepRoutes {
			prefix := "agent.step_routes." + string(step)
			resolvedPrefix := "resolved." + prefix
			set(prefix+".agents", resolvedPrefix+".agents")
			for i := range document.Agent.StepRoutes[step].Agents {
				set(fmt.Sprintf("%s.agents[%d]", prefix, i), fmt.Sprintf("%s.agents[%d]", resolvedPrefix, i))
			}
			set(prefix+".model.name", resolvedPrefix+".model.name")
			set(prefix+".model.vendor", resolvedPrefix+".model.vendor")
		}
		for i := range document.Agent.ReviewCandidates {
			prefix := fmt.Sprintf("agent.review_candidates[%d]", i)
			resolvedPrefix := "resolved." + prefix
			set(prefix+".agent", resolvedPrefix+".agent")
			set(prefix+".model.name", resolvedPrefix+".model.name")
			set(prefix+".model.vendor", resolvedPrefix+".model.vendor")
			set(prefix+".optional", resolvedPrefix+".optional")
		}
	}
	setGlobal("managed")
	annotations["runner.configured.executable"] = effectiveConfigProvenanceFromConfig(ledger.Value("runner.executable"))
	annotations["runner.configured.args"] = effectiveConfigProvenanceFromConfig(ledger.Value("runner.args"))
	annotations["run.refresh_strategy"] = effectiveConfigProvenanceFromConfig(resolved.RefreshProvenance)
	annotations["run.stacked_on"] = effectiveConfigProvenanceValue{Source: effectiveConfigSourceGlobal, IsDefault: true}
	if strings.TrimSpace(document.Run.StackedOn) != "" {
		annotations["run.stacked_on"] = effectiveConfigProvenanceValue{Source: effectiveConfigSourceRunRequest}
	}
	annotations["run.skip_steps"] = effectiveConfigProvenanceFromConfig(resolved.SkipProvenance)
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
		// A scalar command is the shorthand form of its run leaf. Mapping
		// commands annotate their individual leaves below.
		set("commands."+command, "commands."+command+".run")
		for _, suffix := range []string{
			"run", "runner", "runner.executable", "runner.args",
			"linux", "linux.run", "linux.runner", "linux.runner.executable", "linux.runner.args",
			"macos", "macos.run", "macos.runner", "macos.runner.executable", "macos.runner.args",
			"windows", "windows.run", "windows.runner", "windows.runner.executable", "windows.runner.args",
		} {
			set("commands."+command+"."+suffix, "commands."+command+"."+suffix)
		}
	}
	set("preflight", "preflight")
	set("hooks.post_worktree", "hooks.post_worktree")
	set("hooks.pr_body", "hooks.pr_body")
	set("allow_repo_commands", "allow_repo_commands")
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

func annotateDisabledReaderSources(annotations map[string]effectiveConfigProvenanceValue, ledger *config.EffectiveConfigProvenance, readers []string) {
	if ledger == nil {
		return
	}
	for i, reader := range readers {
		value, ok := ledger.DisabledReaderValue(reader)
		if !ok {
			value = ledger.Value("intent.disabled_readers")
		}
		annotations[fmt.Sprintf("intent.disabled_readers[%d]", i)] = effectiveConfigProvenanceFromConfig(value)
	}
}

func effectiveConfigProvenanceFromConfig(value config.EffectiveConfigProvenanceValue) effectiveConfigProvenanceValue {
	return effectiveConfigProvenanceValue{
		Source:     value.Source,
		IsDefault:  value.IsDefault,
		Qualifiers: append([]string(nil), value.Qualifiers...),
	}
}

var effectiveConfigIndexPattern = regexp.MustCompile(`\[[0-9]+\]`)
var effectiveConfigCommentPattern = regexp.MustCompile(`^#?\s*source=(global|global-override|trusted|pushed|run-request|runtime); is_default=(true|false)(; qualifier=(clear|append|merge)(,(clear|append|merge))*)?$`)

func annotationForEffectiveConfigPath(path string, annotations map[string]effectiveConfigProvenanceValue) effectiveConfigProvenanceValue {
	if value, ok := annotations[path]; ok {
		return value
	}
	normalized := effectiveConfigIndexPattern.ReplaceAllString(path, "[]")
	for candidate := normalized; candidate != ""; {
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
	if err := os.MkdirAll(p.RunsDir(), 0o700); err != nil {
		return fmt.Errorf("persist effective config: create runs directory: %w", err)
	}
	if err := protectEffectiveConfigDirectory(p.RunsDir()); err != nil {
		return fmt.Errorf("persist effective config: protect runs directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(p.RunsDir(), "."+runID+"-")
	if err != nil {
		return fmt.Errorf("persist effective config: create staging directory: %w", err)
	}
	renamed := false
	defer func() {
		if renamed {
			if err != nil {
				_ = os.RemoveAll(p.RunDir(runID))
			}
			return
		}
		if cleanupErr := os.RemoveAll(tempDir); err == nil && cleanupErr != nil {
			err = fmt.Errorf("persist effective config: clean staging directory: %w", cleanupErr)
		}
	}()
	if err := protectEffectiveConfigDirectory(tempDir); err != nil {
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
	if err := syncEffectiveConfigDirectory(tempDir); err != nil {
		return fmt.Errorf("persist effective config: sync staging directory: %w", err)
	}
	if _, statErr := os.Stat(p.RunDir(runID)); statErr == nil {
		return fmt.Errorf("persist effective config: run artifact directory already exists")
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("persist effective config: inspect run artifact directory: %w", statErr)
	}
	if err := os.Rename(tempDir, p.RunDir(runID)); err != nil {
		return fmt.Errorf("persist effective config atomically: %w", err)
	}
	renamed = true
	if err := syncEffectiveConfigDirectory(p.RunsDir()); err != nil {
		return fmt.Errorf("persist effective config: sync runs directory: %w", err)
	}
	return nil
}

func writeOwnerOnlyFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := protectEffectiveConfigFile(path); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
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
	return nil
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
