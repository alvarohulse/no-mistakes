package daemon

import (
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

func loadMachineRepoConfig(repo *db.Repo, lookupEnv func(string) (string, bool)) (*machineRepoConfig, error) {
	rawPath, set := lookupEnv(machineRepoConfigEnv)
	if !set {
		return nil, nil
	}
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return nil, fmt.Errorf("%s is set but empty", machineRepoConfigEnv)
	}

	path, err := filepath.Abs(rawPath)
	if err != nil {
		return nil, fmt.Errorf("resolve %s path: %w", machineRepoConfigEnv, err)
	}
	path = filepath.Clean(path)
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
	registeredIdentity, err := gate.RemoteIdentity(repo.UpstreamURL)
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

func repoConfigInputFromBytes(repoConfig *config.RepoConfig, data []byte, kind, ref string) *repoConfigInput {
	return &repoConfigInput{
		Config: repoConfig,
		Source: &db.ConfigSource{Kind: kind, Digest: configDigest(data), Ref: ref},
	}
}

func effectiveRepoConfigAndSources(global *config.GlobalConfig, pushed, trusted *repoConfigInput, machine *machineRepoConfig) (*config.RepoConfig, []db.ConfigSource) {
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
	resolved := config.Merge(global, effective)

	var sources []db.ConfigSource
	if pushed.Source != nil {
		withoutPushed := resolve(&config.RepoConfig{}, trustedConfig, allowRepoCommands)
		if !reflect.DeepEqual(resolved, config.Merge(global, withoutPushed)) {
			sources = append(sources, *pushed.Source)
		}
	}
	if trusted != nil && trusted.Source != nil {
		withoutTrusted := resolve(pushed.Config, nil, false)
		if !reflect.DeepEqual(resolved, config.Merge(global, withoutTrusted)) {
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
