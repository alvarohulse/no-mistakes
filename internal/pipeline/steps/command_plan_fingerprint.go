package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

type commandPlanningFingerprint struct {
	headSHA    string
	headRef    string
	status     string
	indexTree  string
	indexFlags string
	refs       string
	config     string
	submodules string
	controller string
}

func inspectCommandPlanningFingerprint(ctx context.Context, workDir string) (*commandPlanningFingerprint, error) {
	fingerprint := &commandPlanningFingerprint{}
	commands := []struct {
		name   string
		args   []string
		target *string
	}{
		{name: "HEAD", args: []string{"rev-parse", "HEAD"}, target: &fingerprint.headSHA},
		{name: "HEAD attachment", args: []string{"rev-parse", "--abbrev-ref", "HEAD"}, target: &fingerprint.headRef},
		{name: "Git status", args: []string{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none"}, target: &fingerprint.status},
		{name: "Git index", args: []string{"write-tree"}, target: &fingerprint.indexTree},
		{name: "Git index flags", args: []string{"ls-files", "-v", "-z"}, target: &fingerprint.indexFlags},
		{name: "Git refs", args: []string{"for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)"}, target: &fingerprint.refs},
		{name: "Git config", args: []string{"config", "--local", "--list", "-z"}, target: &fingerprint.config},
		{name: "Git submodules", args: []string{"submodule", "status", "--recursive"}, target: &fingerprint.submodules},
	}
	for _, command := range commands {
		output, err := git.Run(ctx, workDir, command.args...)
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", command.name, err)
		}
		*command.target = output
	}
	controller, err := inspectCommandPlanningControllerMetadata(workDir)
	if err != nil {
		return nil, err
	}
	fingerprint.controller = controller
	return fingerprint, nil
}

func (f *commandPlanningFingerprint) equal(other *commandPlanningFingerprint) bool {
	return f != nil && other != nil && *f == *other
}

func inspectCommandPlanningControllerMetadata(workDir string) (string, error) {
	markerPath := filepath.Join(workDir, ".git", "no-mistakes-command-planner")
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return "", fmt.Errorf("inspect command planning ownership marker: %w", err)
	}
	if !markerInfo.Mode().IsRegular() {
		return "", fmt.Errorf("inspect command planning ownership marker: not a regular file")
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		return "", fmt.Errorf("read command planning ownership marker: %w", err)
	}

	hooksDir := filepath.Join(workDir, ".git", "hooks")
	hooksInfo, err := os.Lstat(hooksDir)
	if err != nil {
		return "", fmt.Errorf("inspect command planning hooks: %w", err)
	}
	if !hooksInfo.IsDir() || hooksInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("inspect command planning hooks: not a directory")
	}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return "", fmt.Errorf("list command planning hooks: %w", err)
	}
	var hooks strings.Builder
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return "", fmt.Errorf("inspect command planning hook %s: %w", entry.Name(), err)
		}
		fmt.Fprintf(&hooks, "%s:%s:%o\x00", entry.Name(), info.Mode().Type(), info.Mode().Perm())
	}
	return fmt.Sprintf("marker:%o:%s\x00hooks:%o:%s", markerInfo.Mode().Perm(), marker, hooksInfo.Mode().Perm(), hooks.String()), nil
}
