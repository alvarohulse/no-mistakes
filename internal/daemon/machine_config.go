package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

const machineRepoConfigEnv = "NM_REPO_CONFIG"

type machineRepoConfig struct {
	Config *config.RepoConfig
	Path   string
	Digest string
}

type repoConfigInput struct {
	Config *config.RepoConfig
	Source *db.ConfigSource
}

type globalConfigInput struct {
	Config *config.GlobalConfig
	Source *db.ConfigSource
}

func loadMachineRepoConfig(repo *db.Repo, lookupEnv func(string) (string, bool)) (*machineRepoConfig, error) {
	rawPath, set := lookupEnv(machineRepoConfigEnv)
	if !set {
		return nil, nil
	}
	path, err := ValidateMachineRepoConfigPath(rawPath)
	if err != nil {
		return nil, err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %s target: %w", machineRepoConfigEnv, err)
	}
	repoPath, err := filepath.Abs(repo.WorkingPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path for %s: %w", machineRepoConfigEnv, err)
	}
	repoPath = filepath.Clean(repoPath)
	resolvedRepoPath, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path for %s: %w", machineRepoConfigEnv, err)
	}
	if pathWithin(repoPath, path) || pathWithin(resolvedRepoPath, resolvedPath) {
		return nil, fmt.Errorf("%s must be outside the repository", machineRepoConfigEnv)
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", machineRepoConfigEnv, err)
	}
	repoConfig, err := config.LoadRepoFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", machineRepoConfigEnv, err)
	}
	if strings.TrimSpace(repoConfig.Repo) == "" {
		return nil, fmt.Errorf("%s must declare repo", machineRepoConfigEnv)
	}
	configuredIdentity, err := gate.RemoteIdentity(repoConfig.Repo)
	if err != nil {
		return nil, fmt.Errorf("%s repo binding is invalid", machineRepoConfigEnv)
	}
	registeredIdentity, err := gate.RegisteredRemoteIdentity(repo.UpstreamURL)
	if err != nil {
		return nil, fmt.Errorf("registered repository remote cannot be validated for %s", machineRepoConfigEnv)
	}
	if configuredIdentity != registeredIdentity {
		return nil, fmt.Errorf("%s repo binding does not match the registered repository", machineRepoConfigEnv)
	}

	return &machineRepoConfig{
		Config: repoConfig,
		Path:   resolvedPath,
		Digest: configDigest(data),
	}, nil
}

// ValidateMachineRepoConfigPath validates the environment-level path contract
// shared by run startup and doctor. Relative paths are refused because managed
// daemons resolve them from the service working directory, not the shell that
// installed or refreshed the service.
func ValidateMachineRepoConfigPath(rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", fmt.Errorf("%s is set but empty", machineRepoConfigEnv)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path", machineRepoConfigEnv)
	}
	return filepath.Clean(path), nil
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

func effectiveRepoConfigAndSources(global *globalConfigInput, pushed, trusted *repoConfigInput, machine *machineRepoConfig) (*config.RepoConfig, []db.ConfigSource) {
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
		if machine != nil {
			effective = config.OverlayRepoConfig(effective, machine.Config)
		}
		return effective
	}
	effective := resolve(pushed.Config, trustedConfig, allowRepoCommands)
	if machine == nil {
		return effective, nil
	}
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
	if machine != nil {
		sources = append(sources, db.ConfigSource{
			Kind:   db.ConfigSourceMachine,
			Digest: machine.Digest,
			Path:   machine.Path,
		})
	}
	return effective, sources
}

func validateRecoveredMachineRepoConfig(sources []db.ConfigSource, machine *machineRepoConfig) error {
	var expected *db.ConfigSource
	for i := range sources {
		if sources[i].Kind != db.ConfigSourceMachine {
			continue
		}
		if expected != nil {
			return fmt.Errorf("run records multiple machine-local config sources")
		}
		expected = &sources[i]
	}
	if expected == nil {
		if machine != nil {
			return fmt.Errorf("recovered run was launched without %s; refusing to apply it mid-run", machineRepoConfigEnv)
		}
		return nil
	}
	if machine == nil {
		return fmt.Errorf("recovered run requires the launch-time %s", machineRepoConfigEnv)
	}
	if machine.Path != expected.Path {
		return fmt.Errorf("recovered run %s path differs from launch", machineRepoConfigEnv)
	}
	if machine.Digest != expected.Digest {
		return fmt.Errorf("recovered run %s digest differs from launch", machineRepoConfigEnv)
	}
	return nil
}

func loadRecordedRunConfig(ctx context.Context, run *db.Run, workDir string, machine *machineRepoConfig) (*config.Config, error) {
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
	effective, sources := effectiveRepoConfigAndSources(globalInput, pushedInput, trustedInput, machine)
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

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
