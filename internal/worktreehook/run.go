package worktreehook

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const maxErrorRunes = 4 * 1024

// Run prepares a newly created pipeline worktree with the configured trusted
// repository hook.
func Run(ctx context.Context, workDir string, cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	hook := strings.TrimSpace(cfg.Hooks.PostWorktree)
	if hook == "" {
		return nil
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", hook)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", hook)
	}
	cmd.Dir = workDir
	shellenv.ConfigureShellCommand(cmd, cfg.ProcessTerminationGrace)
	output, err := shellenv.CombinedOutputShellCommand(cmd)
	if err == nil {
		return nil
	}

	detail := boundedOutput(output)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("post-worktree hook failed with exit code %d%s", exitErr.ExitCode(), detail)
	}
	return fmt.Errorf("post-worktree hook failed: %s%s", safeurl.RedactText(err.Error()), detail)
}

func boundedOutput(output []byte) string {
	text := strings.TrimSpace(safeurl.RedactText(string(output)))
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxErrorRunes {
		text = "…" + string(runes[len(runes)-maxErrorRunes:])
	}
	return ": " + text
}
