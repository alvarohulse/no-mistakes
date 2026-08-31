package config

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	EffectiveConfigSourceGlobal         = "global"
	EffectiveConfigSourceGlobalOverride = "global-override"
	EffectiveConfigSourceTrusted        = "trusted"
	EffectiveConfigSourcePushed         = "pushed"
	EffectiveConfigSourceRunRequest     = "run-request"
	EffectiveConfigSourceRuntime        = "runtime"
)

// EffectiveConfigProvenanceValue identifies the layer that supplied one
// effective value. Qualifiers describe composition performed while applying
// that layer rather than properties inferred from the rendered value.
type EffectiveConfigProvenanceValue struct {
	Source     string
	IsDefault  bool
	Qualifiers []string
}

// EffectiveConfigProvenance is the application ledger for one resolved
// configuration. Values are recorded by the operations that select, overlay,
// merge, and resolve them so explanatory output never has to reverse-engineer
// provenance from the final value.
type EffectiveConfigProvenance struct {
	values          map[string]EffectiveConfigProvenanceValue
	disabledReaders map[string]EffectiveConfigProvenanceValue
}

// EffectiveConfigResolution is the effective repository input, merged runtime
// configuration, and the provenance ledger produced while resolving both.
type EffectiveConfigResolution struct {
	Repo              *RepoConfig
	Config            *Config
	Provenance        *EffectiveConfigProvenance
	AllowRepoCommands bool
}

// ResolveEffectiveConfig applies the trusted routing boundary, the optional
// machine override, and the global/repository merge as one provenance-aware
// operation. The allow value must come from the trusted default-branch copy.
func ResolveEffectiveConfig(global *GlobalConfig, pushed, trusted, override *RepoConfig, allow bool) *EffectiveConfigResolution {
	if global == nil {
		global = DefaultGlobalConfig()
	}
	repo, repoProvenance := effectiveRepoConfigWithProvenance(pushed, trusted, allow)
	if override != nil {
		repo, repoProvenance = overlayRepoConfigWithProvenance(repo, override, repoProvenance)
	}
	// The machine override is RepoConfig-shaped for convenience, but this
	// trust switch is owned exclusively by the trusted default-branch copy.
	repo.AllowRepoCommands = allow
	provenance := newEffectiveConfigProvenance(global)
	cfg := mergeConfig(global, repo, &effectiveMergeProvenance{resolved: provenance, repo: repoProvenance})
	return &EffectiveConfigResolution{
		Repo:              repo,
		Config:            cfg,
		Provenance:        provenance,
		AllowRepoCommands: allow,
	}
}

// ApplyEffectiveConfigPublishOverride applies an explicit per-run publication
// choice after all persisted configuration layers. A nil choice leaves both
// the resolved value and its provenance unchanged.
func (r *EffectiveConfigResolution) ApplyEffectiveConfigPublishOverride(publish *bool) {
	if r == nil || r.Config == nil || publish == nil {
		return
	}
	r.Config.EffectiveConfig.Publish = *publish
	if r.Provenance != nil {
		r.Provenance.setSubtree("effective_config.publish", EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceRunRequest})
	}
}

// ResolveAgent resolves runtime routing and records only selections that
// actually changed as runtime-derived. Explicit routes that survive resolution
// unchanged retain the layer that configured them.
func (r *EffectiveConfigResolution) ResolveAgent(ctx context.Context, lookPath func(string) (string, error)) error {
	if r == nil || r.Config == nil {
		return nil
	}
	cfg := r.Config
	configuredDefault := cfg.configuredAgents()
	configuredSteps := cloneAgentRoutes(cfg.StepAgents)
	configuredCandidates := copyReviewCandidates(cfg.ReviewCandidates)
	origins, err := cfg.resolveAgentWithOrigins(ctx, lookPath)
	if err != nil {
		return err
	}
	if r.Provenance == nil {
		return nil
	}

	r.Provenance.recordResolvedAgents("resolved.agent.default", "agent", configuredDefault, cfg.Agents, origins.Default)

	steps := make(map[types.StepName]bool, len(cfg.StepAgents)+len(cfg.StepModels))
	for step := range cfg.StepAgents {
		steps[step] = true
	}
	for step := range cfg.StepModels {
		steps[step] = true
	}
	for step := range steps {
		configuredAgents, explicit := configuredSteps[step]
		sourcePath := string(step) + ".agent"
		if !explicit || len(configuredAgents) == 0 {
			configuredAgents = configuredDefault
			sourcePath = "agent"
		}
		resolvedAgents := cfg.StepAgents[step]
		prefix := "resolved.agent.step_routes." + string(step)
		r.Provenance.recordResolvedAgents(prefix+".agents", sourcePath, configuredAgents, resolvedAgents, origins.Steps[step])
		r.Provenance.setSubtree(prefix+".model.name", r.Provenance.Value(string(step)+".model.name"))
		r.Provenance.setSubtree(prefix+".model.vendor", r.Provenance.Value(string(step)+".model.vendor"))
	}

	usedCandidates := make([]bool, len(configuredCandidates))
	candidatesRuntime := len(configuredCandidates) != len(cfg.ReviewCandidates)
	for i, candidate := range cfg.ReviewCandidates {
		value := r.Provenance.Value("review.candidates")
		agentValue := value
		matched := -1
		for j, configured := range configuredCandidates {
			if usedCandidates[j] || configured.Model != candidate.Model || configured.Optional != candidate.Optional {
				continue
			}
			matched = j
			if configured.Agent == candidate.Agent {
				break
			}
		}
		if matched < 0 {
			value = EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceRuntime}
			agentValue = value
			candidatesRuntime = true
		} else {
			usedCandidates[matched] = true
			if configuredCandidates[matched].Agent != candidate.Agent {
				agentValue = EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceRuntime}
				candidatesRuntime = true
			}
		}
		prefix := "resolved.agent.review_candidates[" + strconv.Itoa(i) + "]"
		r.Provenance.setSubtree(prefix+".agent", agentValue)
		r.Provenance.setSubtree(prefix+".model.name", value)
		r.Provenance.setSubtree(prefix+".model.vendor", value)
		r.Provenance.setSubtree(prefix+".optional", value)
	}
	if candidatesRuntime {
		r.Provenance.setSubtree("resolved.agent.review_candidates", EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceRuntime})
	} else {
		r.Provenance.setSubtree("resolved.agent.review_candidates", r.Provenance.Value("review.candidates"))
	}
	return nil
}

func (p *EffectiveConfigProvenance) recordResolvedAgents(path, sourcePath string, configured, resolved []types.AgentName, runtimeOrigins []bool) {
	configuredValue := p.Value(sourcePath)
	listValue := configuredValue
	if !slices.Equal(configured, resolved) || slices.Contains(runtimeOrigins, true) {
		listValue = EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceRuntime}
	}
	p.setSubtree(path, listValue)
	for i := range resolved {
		value := EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceRuntime}
		if i < len(runtimeOrigins) && !runtimeOrigins[i] {
			value = configuredValue
		}
		p.setSubtree(path+"["+strconv.Itoa(i)+"]", value)
	}
}

func cloneAgentRoutes(source map[types.StepName][]types.AgentName) map[types.StepName][]types.AgentName {
	cloned := make(map[types.StepName][]types.AgentName, len(source))
	for step, agents := range source {
		cloned[step] = copyAgents(agents)
	}
	return cloned
}

// Value returns the receipt for path, inheriting a receipt from the nearest
// applied ancestor when a scalar/list replacement owns the whole subtree.
func (p *EffectiveConfigProvenance) Value(path string) EffectiveConfigProvenanceValue {
	if value, ok := p.lookup(path); ok {
		return value
	}
	return EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceGlobal, IsDefault: true}
}

// ExactValue returns the receipt recorded directly for path without inheriting
// from an applied ancestor. Renderers use it when the distinction between an
// explicitly cleared field and an omitted descendant is part of the output.
func (p *EffectiveConfigProvenance) ExactValue(path string) (EffectiveConfigProvenanceValue, bool) {
	if p == nil {
		return EffectiveConfigProvenanceValue{}, false
	}
	value, ok := p.values[path]
	return cloneEffectiveConfigProvenanceValue(value), ok
}

// DisabledReaderValue returns the receipt for one normalized disabled reader.
func (p *EffectiveConfigProvenance) DisabledReaderValue(reader string) (EffectiveConfigProvenanceValue, bool) {
	if p == nil {
		return EffectiveConfigProvenanceValue{}, false
	}
	value, ok := p.disabledReaders[strings.ToLower(strings.TrimSpace(reader))]
	return cloneEffectiveConfigProvenanceValue(value), ok
}

func (p *EffectiveConfigProvenance) lookup(path string) (EffectiveConfigProvenanceValue, bool) {
	if p == nil {
		return EffectiveConfigProvenanceValue{}, false
	}
	for candidate := path; candidate != ""; {
		if value, ok := p.values[candidate]; ok {
			return cloneEffectiveConfigProvenanceValue(value), true
		}
		index := strings.LastIndex(candidate, ".")
		if index < 0 {
			break
		}
		candidate = candidate[:index]
	}
	return EffectiveConfigProvenanceValue{}, false
}

func (p *EffectiveConfigProvenance) clone() *EffectiveConfigProvenance {
	if p == nil {
		return &EffectiveConfigProvenance{values: make(map[string]EffectiveConfigProvenanceValue), disabledReaders: make(map[string]EffectiveConfigProvenanceValue)}
	}
	cloned := &EffectiveConfigProvenance{
		values:          make(map[string]EffectiveConfigProvenanceValue, len(p.values)),
		disabledReaders: make(map[string]EffectiveConfigProvenanceValue, len(p.disabledReaders)),
	}
	for path, value := range p.values {
		cloned.values[path] = cloneEffectiveConfigProvenanceValue(value)
	}
	for reader, value := range p.disabledReaders {
		cloned.disabledReaders[reader] = cloneEffectiveConfigProvenanceValue(value)
	}
	return cloned
}

func cloneEffectiveConfigProvenanceValue(value EffectiveConfigProvenanceValue) EffectiveConfigProvenanceValue {
	value.Qualifiers = append([]string(nil), value.Qualifiers...)
	return value
}

func (p *EffectiveConfigProvenance) clearSubtree(path string) {
	if p == nil {
		return
	}
	for candidate := range p.values {
		if candidate == path || strings.HasPrefix(candidate, path+".") {
			delete(p.values, candidate)
		}
	}
}

func (p *EffectiveConfigProvenance) setSubtree(path string, value EffectiveConfigProvenanceValue) {
	if p == nil || path == "" {
		return
	}
	p.clearSubtree(path)
	p.values[path] = cloneEffectiveConfigProvenanceValue(value)
}

func (p *EffectiveConfigProvenance) addQualifier(path, qualifier string) {
	value, ok := p.lookup(path)
	if !ok {
		return
	}
	if !slices.Contains(value.Qualifiers, qualifier) {
		value.Qualifiers = append(value.Qualifiers, qualifier)
	}
	p.setSubtree(path, value)
}

func (p *EffectiveConfigProvenance) applyPrefix(source *EffectiveConfigProvenance, prefix string) {
	if p == nil || source == nil {
		return
	}
	paths := make([]string, 0)
	for path := range source.values {
		if prefix == "" || path == prefix || strings.HasPrefix(path, prefix+".") {
			paths = append(paths, path)
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := strings.Count(paths[i], ".")
		rightDepth := strings.Count(paths[j], ".")
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return paths[i] < paths[j]
	})
	for _, path := range paths {
		p.setSubtree(path, source.values[path])
	}
}

func newEffectiveConfigProvenance(global *GlobalConfig) *EffectiveConfigProvenance {
	provenance := &EffectiveConfigProvenance{
		values:          make(map[string]EffectiveConfigProvenanceValue),
		disabledReaders: make(map[string]EffectiveConfigProvenanceValue),
	}
	applied := map[string]bool(nil)
	if global != nil {
		applied = clonePresenceMap(global.applied)
		if applied == nil {
			applied = global.DeclaredPaths()
			for path := range applied {
				if !globalEffectiveConfigPathApplies(global, path) {
					delete(applied, path)
				}
			}
		}
	}
	normalizeEffectiveConfigPresence(applied)
	for _, path := range effectiveConfigKnownInputPaths() {
		provenance.values[path] = EffectiveConfigProvenanceValue{
			Source:    EffectiveConfigSourceGlobal,
			IsDefault: !applied[path],
		}
	}
	if global != nil {
		for _, reader := range global.Intent.DisabledReaders {
			reader = strings.ToLower(strings.TrimSpace(reader))
			provenance.disabledReaders[reader] = EffectiveConfigProvenanceValue{Source: EffectiveConfigSourceGlobal}
		}
	}
	return provenance
}

func globalEffectiveConfigAppliedPaths(raw *globalConfigRaw, cfg *GlobalConfig) map[string]bool {
	applied := make(map[string]bool)
	if raw == nil || cfg == nil {
		return applied
	}
	declared := cfg.DeclaredPaths()
	normalizeEffectiveConfigPresence(declared)
	mark := func(path string, condition bool) {
		if condition && declared[path] {
			applied[path] = true
		}
	}

	mark("managed", raw.Managed != nil)
	mark("agent", len(raw.Agent) > 0)
	mark("runner.executable", strings.TrimSpace(cfg.Runner.Executable) != "")
	mark("runner.args", len(cfg.Runner.Args) > 0)
	mark("hooks.pr_body", strings.TrimSpace(cfg.Hooks.PRBody) != "")
	mark("effective_config.publish", raw.EffectiveConfig.Publish != nil)
	mark("acpx_path", raw.ACPXPath != "")
	mark("acp_registry_overrides", raw.ACPRegistryOverrides != nil)
	mark("agent_path_override", raw.AgentPathOverride != nil)
	mark("agent_args_override", raw.AgentArgsOverride != nil)
	mark("ci_timeout", strings.TrimSpace(raw.CITimeout) != "" || strings.TrimSpace(raw.BabysitTimeout) != "")
	if raw.StepQuietWarning != "" {
		if duration, err := time.ParseDuration(raw.StepQuietWarning); err == nil {
			mark("step_quiet_warning", duration > 0)
		}
	}
	mark("process_termination_grace", raw.ProcessTerminationGrace != "")
	mark("log_level", strings.TrimSpace(raw.LogLevel) != "")
	mark("session_reuse", raw.SessionReuse != nil)

	for _, field := range []struct {
		path  string
		value *int
	}{
		{path: "auto_fix.lint", value: cfg.AutoFix.Lint},
		{path: "auto_fix.build", value: cfg.AutoFix.Build},
		{path: "auto_fix.test", value: cfg.AutoFix.Test},
		{path: "auto_fix.review", value: cfg.AutoFix.Review},
		{path: "auto_fix.document", value: cfg.AutoFix.Document},
		{path: "auto_fix.ci", value: cfg.AutoFix.CI},
		{path: "auto_fix.refresh", value: cfg.AutoFix.Refresh},
	} {
		mark(field.path, field.value != nil)
	}
	mark("ci.rerun_transient", cfg.CI.RerunTransient != nil)
	mark("commit.fix_message", cfg.Commit.FixMessage != nil)
	mark("intent.enabled", cfg.Intent.Enabled != nil)
	mark("intent.threshold", cfg.Intent.Threshold != nil)
	mark("intent.slack_days", cfg.Intent.SlackDays != nil)
	mark("intent.disabled_readers", len(cfg.Intent.DisabledReaders) > 0)
	mark("test.evidence.local_root", cfg.Test.Evidence.LocalRoot != nil && strings.TrimSpace(*cfg.Test.Evidence.LocalRoot) != "")
	mark("test.evidence.retention", cfg.Test.Evidence.Retention != nil)
	mark("test.evidence.max_runs", cfg.Test.Evidence.MaxRuns != nil)
	mark("review.candidates", cfg.Review.Candidates != nil)

	for step := range cfg.configuredStepAgents() {
		mark(string(step)+".agent", true)
	}
	for step := range cfg.configuredStepModels() {
		mark(string(step)+".model.name", true)
		mark(string(step)+".model.vendor", true)
	}
	for _, field := range []string{"shared", "intent", "refresh", "review", "build", "test", "document", "lint", "pr", "ci"} {
		mark("prompts."+field, strings.TrimSpace(promptConfigValue(cfg.Prompts, field)) != "")
	}
	for _, field := range []struct {
		path    string
		present bool
	}{
		{path: "eval.capture_provenance", present: raw.Eval.CaptureProvenance != nil},
		{path: "eval.auto_capture", present: raw.Eval.AutoCapture != nil},
		{path: "eval.max_cases", present: raw.Eval.MaxCases != nil},
		{path: "eval.diversified_size", present: raw.Eval.DiversifiedSize != nil},
	} {
		mark(field.path, field.present)
	}
	return applied
}

func globalEffectiveConfigPathApplies(global *GlobalConfig, path string) bool {
	for _, prefix := range []string{
		"commands", "preflight", "pipeline.skip_steps", "hooks.post_worktree", "allow_repo_commands", "ignore_patterns",
		"refresh.strategy", "document.instructions", "review.path_instructions", "disable_project_settings", "no_ci",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+".") {
			return false
		}
	}
	if strings.HasPrefix(path, "prompts.") {
		return strings.TrimSpace(promptConfigValue(global.Prompts, strings.TrimPrefix(path, "prompts."))) != ""
	}
	if path == "test.evidence.local_root" {
		return global.Test.Evidence.LocalRoot != nil && strings.TrimSpace(*global.Test.Evidence.LocalRoot) != ""
	}
	return true
}

func promptConfigValue(prompts PromptConfig, field string) string {
	switch field {
	case "shared":
		return prompts.Shared
	case "intent":
		return prompts.Intent
	case "refresh":
		return prompts.Refresh
	case "review":
		return prompts.Review
	case "build":
		return prompts.Build
	case "test":
		return prompts.Test
	case "document":
		return prompts.Document
	case "lint":
		return prompts.Lint
	case "pr":
		return prompts.PR
	case "ci":
		return prompts.CI
	default:
		return ""
	}
}

type effectiveMergeProvenance struct {
	resolved *EffectiveConfigProvenance
	repo     *EffectiveConfigProvenance
}

func (p *effectiveMergeProvenance) apply(path string) {
	if p == nil || p.resolved == nil || p.repo == nil {
		return
	}
	if value, ok := p.repo.lookup(path); ok {
		p.resolved.setSubtree(path, value)
	}
}

func (p *effectiveMergeProvenance) applyPrefix(prefix string) {
	if p == nil || p.resolved == nil {
		return
	}
	p.resolved.applyPrefix(p.repo, prefix)
}

func (p *effectiveMergeProvenance) append(path string) {
	p.apply(path)
	if p != nil && p.resolved != nil {
		p.resolved.addQualifier(path, "append")
	}
}

func (p *effectiveMergeProvenance) applyAutoFix(raw *AutoFixRaw) {
	if p == nil || raw == nil {
		return
	}
	for _, field := range []struct {
		path  string
		value *int
	}{
		{path: "auto_fix.lint", value: raw.Lint},
		{path: "auto_fix.build", value: raw.Build},
		{path: "auto_fix.test", value: raw.Test},
		{path: "auto_fix.review", value: raw.Review},
		{path: "auto_fix.document", value: raw.Document},
		{path: "auto_fix.ci", value: raw.CI},
		{path: "auto_fix.refresh", value: raw.Refresh},
	} {
		if field.value != nil {
			p.apply(field.path)
		}
	}
}

func (p *effectiveMergeProvenance) applyCI(raw *CIRaw) {
	if p != nil && raw != nil && raw.RerunTransient != nil {
		p.apply("ci.rerun_transient")
	}
}

func (p *effectiveMergeProvenance) applyIntent(global *GlobalConfig, repo *RepoConfig) {
	if p == nil || repo == nil {
		return
	}
	for _, field := range []struct {
		path    string
		applied bool
	}{
		{path: "intent.enabled", applied: repo.Intent.Enabled != nil},
		{path: "intent.threshold", applied: repo.Intent.Threshold != nil},
		{path: "intent.slack_days", applied: repo.Intent.SlackDays != nil},
	} {
		if field.applied {
			p.apply(field.path)
		}
	}
	if len(repo.Intent.DisabledReaders) == 0 {
		return
	}
	p.apply("intent.disabled_readers")
	globalReaders := make(map[string]bool)
	if global != nil {
		for _, reader := range global.Intent.DisabledReaders {
			globalReaders[strings.ToLower(strings.TrimSpace(reader))] = true
		}
	}
	repoValue := p.resolved.Value("intent.disabled_readers")
	for _, reader := range repo.Intent.DisabledReaders {
		reader = strings.ToLower(strings.TrimSpace(reader))
		value := repoValue
		if globalReaders[reader] && !slices.Contains(value.Qualifiers, "merge") {
			value.Qualifiers = append(value.Qualifiers, "merge")
		}
		p.resolved.disabledReaders[reader] = value
	}
}

func (p *effectiveMergeProvenance) applyPrompts(global, repo PromptConfig) {
	if p == nil {
		return
	}
	for _, field := range []struct {
		path        string
		globalValue string
		repoValue   string
	}{
		{path: "prompts.shared", globalValue: global.Shared, repoValue: repo.Shared},
		{path: "prompts.intent", globalValue: global.Intent, repoValue: repo.Intent},
		{path: "prompts.refresh", globalValue: global.Refresh, repoValue: repo.Refresh},
		{path: "prompts.review", globalValue: global.Review, repoValue: repo.Review},
		{path: "prompts.build", globalValue: global.Build, repoValue: repo.Build},
		{path: "prompts.test", globalValue: global.Test, repoValue: repo.Test},
		{path: "prompts.document", globalValue: global.Document, repoValue: repo.Document},
		{path: "prompts.lint", globalValue: global.Lint, repoValue: repo.Lint},
		{path: "prompts.pr", globalValue: global.PR, repoValue: repo.PR},
		{path: "prompts.ci", globalValue: global.CI, repoValue: repo.CI},
	} {
		if strings.TrimSpace(field.repoValue) == "" {
			continue
		}
		if strings.TrimSpace(field.globalValue) == "" {
			p.apply(field.path)
		} else {
			p.append(field.path)
		}
	}
}

func effectiveRepoLayerProvenance(cfg *RepoConfig, source string) *EffectiveConfigProvenance {
	provenance := (&EffectiveConfigProvenance{}).clone()
	provenance.replaceRepoLayer(cfg, source)
	return provenance
}

func (p *EffectiveConfigProvenance) replaceRepoLayer(cfg *RepoConfig, source string, prefixes ...string) {
	if p == nil {
		return
	}
	if len(prefixes) == 0 {
		p.values = make(map[string]EffectiveConfigProvenanceValue)
		prefixes = []string{""}
	} else {
		for _, prefix := range prefixes {
			p.clearSubtree(prefix)
		}
	}
	if cfg == nil {
		return
	}
	declared := cfg.DeclaredPaths()
	normalizeEffectiveConfigPresence(declared)
	value := EffectiveConfigProvenanceValue{Source: source}
	for _, path := range effectiveConfigKnownInputPaths() {
		if strings.HasPrefix(path, "commands.") || !matchesEffectiveConfigPrefix(path, prefixes) {
			continue
		}
		if declared[path] {
			p.setSubtree(path, value)
		}
	}
	for _, command := range []struct {
		name  string
		value runner.Command
	}{
		{name: "build", value: cfg.Commands.BuildCommand()},
		{name: "test", value: cfg.Commands.TestCommand()},
		{name: "lint", value: cfg.Commands.LintCommand()},
		{name: "format", value: cfg.Commands.FormatCommand()},
	} {
		root := "commands." + command.name
		if !matchesEffectiveConfigPrefix(root, prefixes) || !cfg.has(root) {
			continue
		}
		_, applied := (runner.Command{}).OverlayWithAppliedPaths(command.value)
		for _, suffix := range applied {
			path := root + "." + suffix
			if suffix == "" {
				path = root + ".run"
			}
			p.setSubtree(path, value)
		}
		for _, platform := range []string{"linux", "macos", "windows"} {
			path := root + "." + platform
			if declared[path] && !hasEffectiveConfigPath(p, path) {
				p.setSubtree(path, value)
			}
		}
	}
}

func hasEffectiveConfigPath(p *EffectiveConfigProvenance, path string) bool {
	if p == nil {
		return false
	}
	for candidate := range p.values {
		if candidate == path || strings.HasPrefix(candidate, path+".") {
			return true
		}
	}
	return false
}

func matchesEffectiveConfigPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix == "" || path == prefix || strings.HasPrefix(path, prefix+".") {
			return true
		}
	}
	return false
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

func effectiveConfigKnownInputPaths() []string {
	paths := []string{
		"managed", "agent", "runner.executable", "runner.args", "preflight", "pipeline.skip_steps",
		"hooks.post_worktree", "hooks.pr_body", "effective_config.publish", "allow_repo_commands", "acpx_path", "acp_registry_overrides",
		"agent_path_override", "agent_args_override", "ci_timeout", "step_quiet_warning", "process_termination_grace",
		"log_level", "session_reuse", "ignore_patterns", "ci.rerun_transient", "commit.fix_message",
		"intent.enabled", "intent.threshold", "intent.slack_days", "intent.disabled_readers",
		"test.evidence.local_root", "test.evidence.retention", "test.evidence.max_runs", "document.instructions",
		"review.path_instructions", "refresh.strategy", "disable_project_settings", "no_ci", "review.candidates",
	}
	for _, step := range []string{"intent", "refresh", "review", "build", "test", "document", "lint", "pr", "ci"} {
		paths = append(paths, step+".agent", step+".model.name", step+".model.vendor")
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
		for _, suffix := range []string{
			"run", "runner.executable", "runner.args",
			"linux.run", "linux.runner.executable", "linux.runner.args",
			"macos.run", "macos.runner.executable", "macos.runner.args",
			"windows.run", "windows.runner.executable", "windows.runner.args",
		} {
			paths = append(paths, "commands."+command+"."+suffix)
		}
	}
	return paths
}
