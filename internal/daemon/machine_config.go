package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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

	digest := sha256.Sum256(data)
	return &machineRepoConfig{
		Config: repoConfig,
		Path:   resolvedPath,
		Digest: hex.EncodeToString(digest[:]),
	}, nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
