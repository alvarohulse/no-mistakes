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
	"path/filepath"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

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

// loadPushedRepoConfigInput reads the branch (pushed) .no-mistakes.yaml from the
// committed blob at headSHA rather than the worktree filesystem. Recovery
// revalidates this source via git.ShowFileBytes at the same ref
// (loadRecordedRepoConfig), so working-tree normalization (core.autocrlf, a
// smudge/clean filter) must not make the launch-time digest diverge from the
// blob digest and abort recovery. Presence is still determined from the
// worktree, which startRun checks out exactly at headSHA: a missing file yields
// an empty config with no recorded source, matching recovery's
// emptyWhenMissing branch.
func loadPushedRepoConfigInput(ctx context.Context, wtDir, headSHA string) (*repoConfigInput, error) {
	if _, err := os.Stat(filepath.Join(wtDir, ".no-mistakes.yaml")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &repoConfigInput{Config: &config.RepoConfig{}}, nil
		}
		return nil, err
	}
	data, err := git.ShowFileBytes(ctx, wtDir, headSHA, ".no-mistakes.yaml")
	if err != nil {
		return nil, err
	}
	repoConfig, err := config.LoadRepoFromBytes(data)
	if err != nil {
		return nil, err
	}
	return repoConfigInputFromBytes(repoConfig, data, db.ConfigSourceBranch, headSHA), nil
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
