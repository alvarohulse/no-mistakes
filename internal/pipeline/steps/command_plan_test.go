package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPlanPipelineCommandRejectsCommittedMutation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "planner-mutation.txt"), []byte("changed\n"), 0o644); err != nil {
				return nil, err
			}
			if _, err := gitutil.Run(ctx, dir, "add", "planner-mutation.txt"); err != nil {
				return nil, err
			}
			if _, err := gitutil.Run(ctx, dir, "commit", "-m", "planner mutation"); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"command":"true"}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := planPipelineCommand(sctx, types.StepLint, "Select lint.")
	if err == nil || !strings.Contains(err.Error(), "modified the worktree") {
		t.Fatalf("planPipelineCommand() error = %v, want committed-mutation refusal", err)
	}
}
