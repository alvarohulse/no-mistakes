package steps

import (
	"context"
	"fmt"

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
	return fingerprint, nil
}

func (f *commandPlanningFingerprint) equal(other *commandPlanningFingerprint) bool {
	return f != nil && other != nil && *f == *other
}
