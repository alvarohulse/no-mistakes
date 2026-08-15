package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const gitSHA1Size = 20

var policyResolutionRefCounter atomic.Uint64

// globalOverrideInput is the global config's machine-local override matched
// to one run's registered repository, pinned to the exact global-config bytes
// read at launch. Digest and Path identify the global config file the entry
// came from, so recovery can refuse a drifted file just like it refuses a
// drifted committed input.
type globalOverrideInput struct {
	Config *config.RepoConfig
	Key    string
	Digest string
	Path   string
}

type repoConfigInput struct {
	Config *config.RepoConfig
	Source *db.ConfigSource
}

type globalConfigInput struct {
	Config *config.GlobalConfig
	Source *db.ConfigSource
}

// runPolicyResolution is the complete pre-run result shared by launch and
// config explain. Every field is resolved before a run row, worktree, hook, or
// agent exists, so persistence and execution consume one immutable decision.
type runPolicyResolution struct {
	Config               *config.Config
	Sources              []db.ConfigSource
	ResolvedRouting      string
	ResolvedPolicy       string
	ResolvedPolicyDigest string
	Policy               *resolvedPolicy
	RefreshStrategy      types.RefreshStrategy
	Steps                []pipeline.Step
	HeadSHA              string
	TrustedSHA           string
	GateDir              string
	TrustedRef           string
}

// ResolvePolicy resolves the current launch policy without creating a run or
// worktree. Callers must supply a commit object ID already selected from the
// registered gate; the resolver still verifies that exact object in the gate.
func (m *RunManager) ResolvePolicy(ctx context.Context, repo *db.Repo, headSHA string, skipped []types.StepName, refreshStrategy types.RefreshStrategy) (*PolicyExplanation, error) {
	resolved, err := m.resolveRunPolicyFromBareGate(ctx, repo, headSHA, skipped, refreshStrategy)
	if err != nil {
		return nil, err
	}
	defer resolved.releaseTrustedRef(context.Background())
	return newPolicyExplanation(resolved)
}

func (m *RunManager) resolveRunPolicyFromBareGate(ctx context.Context, repo *db.Repo, headSHA string, skipped []types.StepName, refreshStrategy types.RefreshStrategy) (*runPolicyResolution, error) {
	if repo == nil {
		return nil, fmt.Errorf("resolve run policy: repository is nil")
	}
	gateDir := m.paths.RepoDir(repo.ID)
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return nil, fmt.Errorf("resolve run policy gate: %w", err)
	}
	candidateSHA, err := resolveBareCandidateCommit(ctx, gateDir, headSHA)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repo.DefaultBranch) == "" {
		return nil, fmt.Errorf("resolve trusted default branch: repository has no known default branch")
	}
	trustedRef := nextPolicyResolutionRef()
	keepTrustedRef := false
	defer func() {
		if !keepTrustedRef {
			deletePolicyTrustedRef(context.Background(), gateDir, trustedRef)
		}
	}()
	if err := fetchRunDefaultBranchToPrivateRef(ctx, gateDir, repo, trustedRef); err != nil {
		return nil, fmt.Errorf("resolve trusted default branch %q: %w", repo.DefaultBranch, err)
	}
	trustedSHA, err := git.ResolveRef(ctx, gateDir, trustedRef)
	if err != nil {
		return nil, fmt.Errorf("resolve trusted default branch %q: %w", repo.DefaultBranch, err)
	}

	globalInput, err := loadGlobalConfigInput(m.paths.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	pushedInput, err := loadBareRepoConfigInput(ctx, gateDir, candidateSHA, db.ConfigSourceBranch, true)
	if err != nil {
		return nil, err
	}
	trustedInput, err := loadBareRepoConfigInput(ctx, gateDir, trustedSHA, db.ConfigSourceDefault, false)
	if err != nil {
		return nil, err
	}
	override := resolveGlobalOverride(globalInput, repo)
	effectiveRepoConfig, sources := effectiveRepoConfigAndSources(globalInput, pushedInput, trustedInput, override)
	if override != nil {
		slog.Warn("global config override is active: honoring machine-local repository configuration", "repo_id", repo.ID, "override", override.Key)
	}
	allowRepoCommands := trustedInput != nil && trustedInput.Config.AllowRepoCommands
	if allowRepoCommands {
		slog.Warn("allow_repo_commands is enabled on the default branch: honoring commands/hooks/agent routes from pushed branch", "repo_id", repo.ID)
	} else if pushedConfigUsesDifferentTrustedControls(pushedInput.Config, effectiveRepoConfig) {
		slog.Info("repo commands/hooks/agent/model routes loaded from default branch, not pushed branch", "repo_id", repo.ID, "default_branch", repo.DefaultBranch)
	}

	cfg := config.Merge(globalInput.Config, effectiveRepoConfig)
	if err := m.paths.ValidateEvidenceRoot(cfg.Test.Evidence.LocalRoot); err != nil {
		return nil, err
	}
	cfg.TrustedConfigSHA = trustedSHA
	if globalInput.Config.Eval.CaptureProvenance {
		if err := cfg.EnableEvalProvenance(globalInput.Config, effectiveRepoConfig); err != nil {
			return nil, err
		}
	}
	demo := steps.IsDemoMode()
	execSteps := m.steps()
	stepNames := make([]types.StepName, 0, len(execSteps))
	for _, step := range execSteps {
		if step == nil {
			return nil, fmt.Errorf("resolve managed step plan: pipeline contains a nil step")
		}
		stepNames = append(stepNames, step.Name().Canonical())
	}
	if err := cfg.ValidateManagedStepPlan(stepNames); err != nil {
		return nil, fmt.Errorf("resolve managed step plan: %w", err)
	}
	if !demo {
		if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
			return nil, fmt.Errorf("resolve agent routes: %w", err)
		}
	}
	resolvedRouting, err := marshalResolvedAgentRouting(cfg, demo)
	if err != nil {
		return nil, err
	}
	resolvedRefreshStrategy := resolveRefreshStrategy(refreshStrategy, cfg.RefreshStrategy)
	policy, err := resolvedPolicyFromConfig(cfg, sources, execSteps, skipped, resolvedRefreshStrategy, demo)
	if err != nil {
		return nil, err
	}
	resolvedPolicy, resolvedPolicyDigest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		return nil, err
	}
	resolved := &runPolicyResolution{
		Config:               cfg,
		Sources:              sources,
		ResolvedRouting:      resolvedRouting,
		ResolvedPolicy:       resolvedPolicy,
		ResolvedPolicyDigest: resolvedPolicyDigest,
		Policy:               policy,
		RefreshStrategy:      resolvedRefreshStrategy,
		Steps:                execSteps,
		HeadSHA:              candidateSHA,
		TrustedSHA:           trustedSHA,
		GateDir:              gateDir,
		TrustedRef:           trustedRef,
	}
	keepTrustedRef = true
	return resolved, nil
}

func resolveBareCandidateCommit(ctx context.Context, gateDir, headSHA string) (string, error) {
	headSHA = strings.TrimSpace(headSHA)
	decoded, err := hex.DecodeString(headSHA)
	if err != nil || (len(decoded) != gitSHA1Size && len(decoded) != sha256.Size) {
		return "", fmt.Errorf("candidate head %q is not a full Git object ID", headSHA)
	}
	resolved, err := git.ResolveRef(ctx, gateDir, headSHA)
	if err != nil {
		return "", fmt.Errorf("resolve candidate head %s: %w", headSHA, err)
	}
	if !strings.EqualFold(resolved, headSHA) {
		return "", fmt.Errorf("candidate head %s resolved to unexpected commit %s", headSHA, resolved)
	}
	return resolved, nil
}

func nextPolicyResolutionRef() string {
	return fmt.Sprintf("refs/no-mistakes/policy-resolution/%d-%d", os.Getpid(), policyResolutionRefCounter.Add(1))
}

func (r *runPolicyResolution) bindTrustedRefToRun(ctx context.Context, runID string) error {
	if r == nil || strings.TrimSpace(r.GateDir) == "" || strings.TrimSpace(r.TrustedRef) == "" || strings.TrimSpace(r.TrustedSHA) == "" {
		return fmt.Errorf("resolved trusted policy ref is incomplete")
	}
	stableRef := policyTrustedRunRef(runID)
	if _, err := git.RunBare(ctx, r.GateDir, "update-ref", stableRef, r.TrustedSHA); err != nil {
		return fmt.Errorf("bind trusted policy ref to run: %w", err)
	}
	previousRef := r.TrustedRef
	r.TrustedRef = stableRef
	deletePolicyTrustedRef(context.Background(), r.GateDir, previousRef)
	return nil
}

func (r *runPolicyResolution) releaseTrustedRef(ctx context.Context) {
	if r == nil {
		return
	}
	deletePolicyTrustedRef(ctx, r.GateDir, r.TrustedRef)
}

func policyTrustedRunRef(runID string) string {
	return "refs/no-mistakes/policy-run/" + runID
}

func deletePolicyTrustedRef(ctx context.Context, gateDir, ref string) {
	if strings.TrimSpace(gateDir) == "" || strings.TrimSpace(ref) == "" {
		return
	}
	if _, err := git.RunBare(ctx, gateDir, "update-ref", "-d", ref); err != nil {
		slog.Warn("failed to delete trusted policy ref", "ref", ref, "error", err)
	}
}

// reapPolicyTrustedRefs removes temporary resolution refs and run-owned refs
// whose run is no longer active. It runs only after stale-run recovery while
// the daemon singleton lock is held, so no live resolver can own a temporary
// ref and terminal status is authoritative.
func reapPolicyTrustedRefs(ctx context.Context, database *db.DB, p *paths.Paths) int {
	repos, err := database.GetRepos()
	if err != nil {
		slog.Warn("failed to list repositories for trusted policy ref cleanup", "error", err)
		return 0
	}
	reaped := 0
	for _, repo := range repos {
		gateDir := p.RepoDir(repo.ID)
		if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
			continue
		}
		out, err := git.RunBare(ctx, gateDir, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/policy-resolution", "refs/no-mistakes/policy-run")
		if err != nil {
			slog.Warn("failed to list trusted policy refs", "repo_id", repo.ID, "error", err)
			continue
		}
		for _, ref := range strings.Fields(out) {
			if strings.HasPrefix(ref, "refs/no-mistakes/policy-run/") {
				runID := strings.TrimPrefix(ref, "refs/no-mistakes/policy-run/")
				run, err := database.GetRun(runID)
				if err != nil {
					slog.Warn("failed to inspect trusted policy ref owner", "repo_id", repo.ID, "run_id", runID, "error", err)
					continue
				}
				if run != nil && run.RepoID == repo.ID && (run.Status == types.RunPending || run.Status == types.RunRunning) {
					continue
				}
			}
			deletePolicyTrustedRef(ctx, gateDir, ref)
			reaped++
		}
	}
	return reaped
}

func loadBareRepoConfigInput(ctx context.Context, gateDir, ref, kind string, emptyWhenMissing bool) (*repoConfigInput, error) {
	label := "trusted"
	if kind == db.ConfigSourceBranch {
		label = "pushed"
	}
	entry, err := git.RunBare(ctx, gateDir, "ls-tree", ref, "--", ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("read %s config tree at %s: %w", label, ref, err)
	}
	if strings.TrimSpace(entry) == "" {
		if emptyWhenMissing {
			return &repoConfigInput{Config: &config.RepoConfig{}}, nil
		}
		return nil, nil
	}
	data, err := git.ShowFileBytes(ctx, gateDir, ref, ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("%s .no-mistakes.yaml at %s is present but unreadable: %w", label, ref, err)
	}
	repoConfig, err := config.LoadRepoFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s .no-mistakes.yaml at %s: %w", label, ref, err)
	}
	return repoConfigInputFromBytes(repoConfig, data, kind, ref), nil
}

func pushedConfigUsesDifferentTrustedControls(pushed, effective *config.RepoConfig) bool {
	if pushed == nil || effective == nil {
		return false
	}
	return pushed.Commands != effective.Commands ||
		pushed.Hooks != effective.Hooks ||
		pushed.Agent != effective.Agent ||
		!agentListsEqual(pushed.Agents, effective.Agents) ||
		!stepAgentRoutesEqual(pushed.ConfiguredStepAgents(), effective.ConfiguredStepAgents()) ||
		!stepModelRoutesEqual(pushed.ConfiguredStepModels(), effective.ConfiguredStepModels()) ||
		!reviewAdversaryRoutesEqual(pushed.Review, effective.Review)
}

// resolveGlobalOverride matches the run's registered repository against the
// global config's overrides map. The registered upstream URL is normalized
// through the same identity rules as gate refresh (so equivalent SSH and
// HTTPS forms match) and compared against the `<owner>/<repo>` overrides
// keys. No matching key means no overlay: the repository behaves exactly as
// if nothing was configured. A registered remote whose identity cannot be
// normalized (for example a local-path remote) can never match a key, so it
// resolves to no overlay with a warning rather than failing every run on the
// machine.
func resolveGlobalOverride(global *globalConfigInput, repo *db.Repo) *globalOverrideInput {
	if global == nil || len(global.Config.Overrides) == 0 {
		return nil
	}
	identity, err := gate.RegisteredRemoteIdentity(repo.UpstreamURL)
	if err != nil {
		slog.Warn("global config overrides are configured but the registered repository remote has no normalizable identity; no override applies", "repo_id", repo.ID)
		return nil
	}
	override, key, ok := global.Config.OverrideForRepoIdentity(identity)
	if !ok {
		return nil
	}
	input := &globalOverrideInput{Config: override, Key: key}
	if global.Source != nil {
		input.Digest = global.Source.Digest
		input.Path = global.Source.Path
	}
	return input
}

func loadRepoConfigInput(path, kind, ref string) (*repoConfigInput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &repoConfigInput{Config: &config.RepoConfig{}}, nil
		}
		return nil, err
	}
	repoConfig, err := config.LoadRepoFromBytes(data)
	if err != nil {
		return nil, err
	}
	return repoConfigInputFromBytes(repoConfig, data, kind, ref), nil
}

func loadGlobalConfigInput(path string) (*globalConfigInput, error) {
	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	resolvedPath = filepath.Clean(resolvedPath)
	if target, err := filepath.EvalSymlinks(resolvedPath); err == nil {
		resolvedPath = target
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &globalConfigInput{Config: config.DefaultGlobalConfig()}, nil
		}
		return nil, err
	}
	globalConfig, err := config.LoadGlobalFromBytes(data)
	if err != nil {
		return nil, err
	}
	return &globalConfigInput{
		Config: globalConfig,
		Source: &db.ConfigSource{
			Kind:   db.ConfigSourceGlobal,
			Digest: configDigest(data),
			Path:   resolvedPath,
		},
	}, nil
}

func repoConfigInputFromBytes(repoConfig *config.RepoConfig, data []byte, kind, ref string) *repoConfigInput {
	return &repoConfigInput{
		Config: repoConfig,
		Source: &db.ConfigSource{Kind: kind, Digest: configDigest(data), Ref: ref},
	}
}

func effectiveRepoConfigAndSources(global *globalConfigInput, pushed, trusted *repoConfigInput, override *globalOverrideInput) (*config.RepoConfig, []db.ConfigSource) {
	if global == nil {
		global = &globalConfigInput{Config: config.DefaultGlobalConfig()}
	}
	if pushed == nil {
		pushed = &repoConfigInput{Config: &config.RepoConfig{}}
	}
	trustedConfig := (*config.RepoConfig)(nil)
	if trusted != nil {
		trustedConfig = trusted.Config
	}
	allowRepoCommands := trustedConfig != nil && trustedConfig.AllowRepoCommands
	resolve := func(pushedConfig, trustedConfig *config.RepoConfig, allow bool) *config.RepoConfig {
		effective := config.EffectiveRepoConfig(pushedConfig, trustedConfig, allow)
		if override != nil {
			effective = config.OverlayRepoConfig(effective, override.Config)
		}
		return effective
	}
	effective := resolve(pushed.Config, trustedConfig, allowRepoCommands)
	resolved := config.Merge(global.Config, effective)

	var sources []db.ConfigSource
	if global.Source != nil && !reflect.DeepEqual(resolved, config.Merge(config.DefaultGlobalConfig(), effective)) {
		sources = append(sources, *global.Source)
	}
	if pushed.Source != nil {
		withoutPushed := resolve(&config.RepoConfig{}, trustedConfig, allowRepoCommands)
		if !reflect.DeepEqual(resolved, config.Merge(global.Config, withoutPushed)) {
			sources = append(sources, *pushed.Source)
		}
	}
	if trusted != nil && trusted.Source != nil {
		withoutTrusted := resolve(pushed.Config, nil, false)
		if !reflect.DeepEqual(resolved, config.Merge(global.Config, withoutTrusted)) {
			sources = append(sources, *trusted.Source)
		}
	}
	if override != nil {
		sources = append(sources, db.ConfigSource{
			Kind:   db.ConfigSourceGlobalOverride,
			Digest: override.Digest,
			Ref:    override.Key,
			Path:   override.Path,
		})
	}
	return effective, sources
}

func validateRecoveredGlobalOverride(sources []db.ConfigSource, override *globalOverrideInput) error {
	var expected *db.ConfigSource
	for i := range sources {
		if sources[i].Kind != db.ConfigSourceGlobalOverride {
			continue
		}
		if expected != nil {
			return fmt.Errorf("run records multiple global-override config sources")
		}
		expected = &sources[i]
	}
	if expected == nil {
		if override != nil {
			return fmt.Errorf("recovered run was launched without a global config override for this repository; refusing to apply one mid-run")
		}
		return nil
	}
	if override == nil {
		return fmt.Errorf("recovered run requires the launch-time global config override")
	}
	if override.Key != expected.Ref {
		return fmt.Errorf("recovered run global config override key differs from launch")
	}
	if override.Path != expected.Path {
		return fmt.Errorf("recovered run global config path differs from launch")
	}
	if override.Digest != expected.Digest {
		return fmt.Errorf("recovered run global config digest differs from launch")
	}
	return nil
}

func loadRecordedRunConfig(ctx context.Context, run *db.Run, workDir string, override *globalOverrideInput) (*config.Config, error) {
	globalInput, err := loadRecordedGlobalConfig(run.ConfigSources)
	if err != nil {
		return nil, err
	}
	pushedInput, err := loadRecordedRepoConfig(ctx, workDir, run.ConfigSources, db.ConfigSourceBranch, true)
	if err != nil {
		return nil, err
	}
	trustedInput, err := loadRecordedRepoConfig(ctx, workDir, run.ConfigSources, db.ConfigSourceDefault, false)
	if err != nil {
		return nil, err
	}
	effective, sources := effectiveRepoConfigAndSources(globalInput, pushedInput, trustedInput, override)
	if !reflect.DeepEqual(sources, run.ConfigSources) {
		return nil, fmt.Errorf("recovered run config provenance differs from launch")
	}
	return config.Merge(globalInput.Config, effective), nil
}

func loadRecordedGlobalConfig(sources []db.ConfigSource) (*globalConfigInput, error) {
	source, err := uniqueConfigSource(sources, db.ConfigSourceGlobal)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return &globalConfigInput{Config: config.DefaultGlobalConfig()}, nil
	}
	if strings.TrimSpace(source.Path) == "" {
		return nil, fmt.Errorf("recorded global config source has no path")
	}
	data, err := os.ReadFile(source.Path)
	if err != nil {
		return nil, fmt.Errorf("read launch-time global config: %w", err)
	}
	if configDigest(data) != source.Digest {
		return nil, fmt.Errorf("recovered run global config digest differs from launch")
	}
	globalConfig, err := config.LoadGlobalFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse launch-time global config: %w", err)
	}
	copy := *source
	return &globalConfigInput{Config: globalConfig, Source: &copy}, nil
}

func loadRecordedRepoConfig(ctx context.Context, workDir string, sources []db.ConfigSource, kind string, emptyWhenMissing bool) (*repoConfigInput, error) {
	source, err := uniqueConfigSource(sources, kind)
	if err != nil {
		return nil, err
	}
	if source == nil {
		if emptyWhenMissing {
			return &repoConfigInput{Config: &config.RepoConfig{}}, nil
		}
		return nil, nil
	}
	if strings.TrimSpace(source.Ref) == "" {
		return nil, fmt.Errorf("recorded %s config source has no ref", kind)
	}
	data, err := git.ShowFileBytes(ctx, workDir, source.Ref, ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("read launch-time %s config: %w", kind, err)
	}
	if configDigest(data) != source.Digest {
		return nil, fmt.Errorf("recovered run %s config digest differs from launch", kind)
	}
	repoConfig, err := config.LoadRepoFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse launch-time %s config: %w", kind, err)
	}
	copy := *source
	return &repoConfigInput{Config: repoConfig, Source: &copy}, nil
}

func uniqueConfigSource(sources []db.ConfigSource, kind string) (*db.ConfigSource, error) {
	var found *db.ConfigSource
	for i := range sources {
		if sources[i].Kind != kind {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("run records multiple %s config sources", kind)
		}
		copy := sources[i]
		found = &copy
	}
	return found, nil
}

func hasConfigSourceKind(sources []db.ConfigSource, kind string) bool {
	for _, source := range sources {
		if source.Kind == kind {
			return true
		}
	}
	return false
}

func repoConfigFromInput(input *repoConfigInput) *config.RepoConfig {
	if input == nil {
		return nil
	}
	return input.Config
}

func configDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
