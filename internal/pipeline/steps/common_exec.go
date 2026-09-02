package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func envValue(env []string, key string) (string, bool) {
	return envValueForOS(env, key, runtime.GOOS)
}

func envValueForOS(env []string, key, goos string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
		if goos == "windows" && len(entry) >= len(prefix) && strings.EqualFold(entry[:len(prefix)], prefix) {
			return entry[len(prefix):], true
		}
	}
	return "", false
}

func envKey(entry string) string {
	key, _, found := strings.Cut(entry, "=")
	if !found {
		key = entry
	}
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func mergeEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	merged := make([]string, 0, len(os.Environ())+len(extra))
	overrides := make(map[string]string, len(extra))
	for _, entry := range extra {
		overrides[envKey(entry)] = entry
	}
	for _, entry := range os.Environ() {
		key := envKey(entry)
		if override, ok := overrides[key]; ok {
			merged = append(merged, override)
			delete(overrides, key)
			continue
		}
		merged = append(merged, entry)
	}
	for _, entry := range extra {
		key := envKey(entry)
		if override, ok := overrides[key]; ok {
			merged = append(merged, override)
			delete(overrides, key)
		}
	}
	return merged
}

func executableCandidates(name string, env []string) []string {
	return executableCandidatesForOS(runtime.GOOS, name, env)
}

func executableCandidatesForOS(goos, name string, env []string) []string {
	candidates := []string{name}
	if goos != "windows" || filepath.Ext(name) != "" {
		return candidates
	}
	pathExt := ".COM;.EXE;.BAT;.CMD"
	if customPathExt, ok := envValueForOS(env, "PATHEXT", goos); ok {
		pathExt = customPathExt
	}
	for _, ext := range strings.Split(pathExt, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		candidates = append(candidates, name+ext)
	}
	return candidates
}

func findInCustomPath(workDir string, env []string, name string) string {
	customPath, ok := envValue(env, "PATH")
	if !ok {
		return ""
	}
	for _, dir := range filepath.SplitList(customPath) {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workDir, dir)
		}
		for _, candidateName := range executableCandidates(name, env) {
			candidate := filepath.Join(dir, candidateName)
			if fi, err := os.Stat(candidate); err == nil && pathCandidateUsable(runtime.GOOS, candidate, fi) {
				return candidate
			}
		}
	}
	return ""
}

func pathCandidateUsable(goos, path string, fi os.FileInfo) bool {
	if fi.IsDir() {
		return false
	}
	if goos == "windows" {
		return filepath.Ext(path) != ""
	}
	return fi.Mode().Perm()&0o111 != 0
}

func missingFromCustomPath(env []string, name string) string {
	customPath, ok := envValue(env, "PATH")
	if !ok {
		return ""
	}
	missing := filepath.Join(".", executableCandidates(name, env)[0])
	for _, dir := range filepath.SplitList(customPath) {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		return filepath.Join(dir, executableCandidates(name, env)[0])
	}
	return missing
}

// stepCmd creates an exec.Cmd that inherits the StepContext's extra Env, if any.
// When sctx.Env overrides PATH, the binary is resolved from the overridden PATH
// so that tests can inject fake binaries without modifying the process environment.
func stepCmd(sctx *pipeline.StepContext, name string, args ...string) *exec.Cmd {
	resolved := name
	missingFromPath := false
	if len(sctx.Env) > 0 && !strings.Contains(name, string(filepath.Separator)) {
		if candidate := findInCustomPath(sctx.WorkDir, sctx.Env, name); candidate != "" {
			resolved = candidate
		} else if _, ok := envValue(sctx.Env, "PATH"); ok {
			resolved = missingFromCustomPath(sctx.Env, name)
			missingFromPath = true
		}
	}
	cmd := exec.CommandContext(sctx.Ctx, resolved, args...)
	cmd.Dir = sctx.WorkDir
	winproc.Harden(cmd)
	if len(sctx.Env) > 0 {
		cmd.Env = mergeEnv(sctx.Env)
	}
	if missingFromPath {
		cmd.Err = &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	return cmd
}

// stepGitRun runs git with the StepContext's environment plus the standard
// non-interactive git overrides. It is like git.Run but respects sctx.Env so
// step-scoped PATH and credential environment stay in effect.
func stepGitRun(sctx *pipeline.StepContext, args ...string) (string, error) {
	cmd := stepCmd(sctx, "git", args...)
	cmd.Env = git.NonInteractiveEnvFrom(cmd.Env, sctx.WorkDir)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", safeurl.RedactText(strings.Join(args, " ")), err, safeurl.RedactText(stderr))
	}
	return strings.TrimSpace(string(out)), nil
}

func stepGitHeadSHA(sctx *pipeline.StepContext) (string, error) {
	return stepGitRun(sctx, "rev-parse", "HEAD")
}

func stepGitPush(sctx *pipeline.StepContext, remote, ref, expectedSHA string, forceWithLease bool) error {
	args := []string{"push", remote}
	if forceWithLease {
		if expectedSHA != "" {
			args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA))
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, "HEAD:"+ref)
	_, err := stepGitRun(sctx, args...)
	return err
}

// stepCLIAvailable checks whether the provider CLI binary is available,
// respecting any custom PATH in sctx.Env.
func stepCLIAvailable(sctx *pipeline.StepContext, provider scm.Provider) bool {
	name := provider.CLIName()
	if name == "" {
		return false
	}
	if len(sctx.Env) == 0 {
		return scm.CLIAvailable(provider)
	}
	if candidate := findInCustomPath(sctx.WorkDir, sctx.Env, name); candidate != "" {
		return true
	}
	_, ok := envValue(sctx.Env, "PATH")
	if ok {
		return false
	}
	return scm.CLIAvailable(provider)
}

// stepAuthConfigured checks whether the provider CLI is authenticated,
// using sctx.Env to resolve the binary and pass environment variables.
func stepAuthConfigured(sctx *pipeline.StepContext, provider scm.Provider) bool {
	args := provider.AuthCheckCommand()
	if len(args) == 0 {
		return false
	}
	cmd := stepCmd(sctx, args[0], args[1:]...)
	return cmd.Run() == nil
}

// runShellCommand executes a shell command and returns stdout+stderr, exit code, and error.
// A non-zero exit code is not treated as an error - only exec failures return error.
func runShellCommand(ctx context.Context, dir, cmdStr string) (string, int, error) {
	return runShellCommandWithEnv(ctx, dir, nil, cmdStr, shellenv.DefaultProcessTerminationGrace)
}

func runStepShellCommand(sctx *pipeline.StepContext, cmdStr string, purpose ...string) (string, int, error) {
	resolvedPurpose := "command"
	if len(purpose) > 0 {
		resolvedPurpose = purpose[0]
	}
	return runStepPlannedCommand(sctx, runner.Command{Run: cmdStr}, resolvedPurpose)
}

func runStepRunnerCommand(sctx *pipeline.StepContext, command runner.Command, purpose ...string) (string, int, error) {
	return runStepCommand(sctx, command, commandPurpose(sctx, purpose), "")
}

func runStepPlannedCommand(sctx *pipeline.StepContext, command runner.Command, purpose string) (string, int, error) {
	return runStepCommand(sctx, command, purpose, db.CommandDefinitionSourcePlanned)
}

func runStepCommand(sctx *pipeline.StepContext, command runner.Command, purpose, definitionSource string) (string, int, error) {
	sequence := sctx.NextCommandSequence()
	processTerminationGrace := shellenv.DefaultProcessTerminationGrace
	defaultRunner := runner.Spec{}
	if sctx.Config != nil && sctx.Config.ProcessTerminationGrace > 0 {
		processTerminationGrace = sctx.Config.ProcessTerminationGrace
	}
	if sctx.Config != nil {
		defaultRunner = sctx.Config.Runner
	}
	options := runner.ExecuteOptions{
		Dir:                     sctx.WorkDir,
		ExtraEnv:                sctx.Env,
		ProcessTerminationGrace: processTerminationGrace,
		CaptureFullOutput:       true,
	}
	prepared, err := runner.Prepare(sctx.Ctx, command, defaultRunner, options)
	resolved := prepared.Resolution()
	if err != nil {
		err = fmt.Errorf("prepare command %q: %w", command.Run, err)
		sctx.RecordResolvedCommandAtSequence(resolved, sequence, nil, err)
		return "", -1, err
	}

	var attempt *db.CommandAttempt
	if sctx.DB != nil && sctx.Run != nil && sctx.StepResultID != "" && sctx.RoundID != "" {
		definitionResolution := resolved
		if definitionSource != "" {
			definitionResolution.CommandSource = definitionSource
		}
		definition, persistErr := sctx.DB.EnsureCommandDefinition(sctx.Run.ID, definitionResolution)
		if persistErr != nil {
			return "", -1, fmt.Errorf("persist command definition: %w", persistErr)
		}
		beforeSHA, headErr := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if headErr != nil {
			return "", -1, fmt.Errorf("resolve command subject: %w", headErr)
		}
		inputStateID, stateErr := cleanCommandStateID(sctx.Ctx, sctx.WorkDir, beforeSHA)
		if stateErr != nil {
			return "", -1, fmt.Errorf("resolve command input state: %w", stateErr)
		}
		var retryOf *string
		var retryReason *string
		priorAttempts, lookupErr := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
		if lookupErr != nil {
			return "", -1, fmt.Errorf("resolve command retry: %w", lookupErr)
		}
		for i := len(priorAttempts) - 1; i >= 0; i-- {
			candidate := priorAttempts[i]
			if candidate.CommandID == definition.ID && candidate.StepID == sctx.StepResultID &&
				candidate.Purpose == purpose && candidate.BeforeSHA == beforeSHA &&
				sameStateID(candidate.ResultStateID, inputStateID) &&
				candidate.CompletedAt != nil && candidate.Outcome != nil &&
				isRetryableCommandOutcome(*candidate.Outcome) {
				retryOf = &candidate.ID
				reason := db.CommandRetryReasonTransientFailure
				retryReason = &reason
				break
			}
		}
		attempt, persistErr = sctx.DB.StartCommandAttempt(db.CommandAttempt{
			RunID:               sctx.Run.ID,
			CommandID:           definition.ID,
			StepID:              sctx.StepResultID,
			RoundID:             sctx.RoundID,
			Sequence:            sequence,
			Purpose:             purpose,
			Observer:            db.CommandObserverController,
			Trigger:             sctx.RoundTrigger,
			BeforeSHA:           beforeSHA,
			CommandSource:       definitionResolution.CommandSource,
			RunnerSchemaVersion: definitionResolution.Provenance.SchemaVersion,
			RunnerSource:        definitionResolution.Provenance.Source,
			RunnerVersion:       definitionResolution.Provenance.Version,
			InputStateID:        inputStateID,
			RetryOfAttemptID:    retryOf,
			RetryReason:         retryReason,
		})
		if persistErr != nil {
			return "", -1, fmt.Errorf("persist command attempt start: %w", persistErr)
		}
	}

	result, err := prepared.Execute(sctx.Ctx, options)
	if err != nil {
		err = fmt.Errorf("run command %q: %w", resolved.Script, err)
	}
	var recordedExitCode *int
	if err == nil {
		recordedExitCode = &result.ExitCode
	}
	if attempt != nil {
		outcome := commandAttemptOutcome(sctx.Ctx, result.ExitCode, err)
		var resultStateID *string
		var resultStateErr error
		if sctx.Ctx.Err() == nil {
			afterSHA, stateErr := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
			if stateErr != nil {
				resultStateErr = fmt.Errorf("resolve command result subject: %w", stateErr)
			} else {
				resultStateID, stateErr = cleanCommandStateID(sctx.Ctx, sctx.WorkDir, afterSHA)
				if stateErr != nil {
					resultStateErr = fmt.Errorf("resolve command result state: %w", stateErr)
				}
			}
		}
		var testedSHA *string
		if commandEstablishesTestedHead(purpose) && outcome != db.CommandOutcomeProcessError && sameStateID(attempt.InputStateID, resultStateID) {
			tested := attempt.BeforeSHA
			testedSHA = &tested
		}
		attemptExitCode := recordedExitCode
		if result.Signal != nil {
			attemptExitCode = nil
		}
		if persistErr := sctx.DB.CompleteCommandAttempt(attempt.ID, outcome, attemptExitCode, result.Signal, resultStateID, testedSHA); persistErr != nil {
			return result.Output, result.ExitCode, errors.Join(err, fmt.Errorf("persist command attempt completion: %w", persistErr))
		}
		if resultStateErr != nil {
			return result.Output, result.ExitCode, resultStateErr
		}
	}
	if resolved.Script != "" {
		sctx.RecordResolvedCommandAtSequence(resolved, sequence, recordedExitCode, err)
	} else {
		sctx.RecordCommand(command.Run, recordedExitCode, err)
	}
	return result.Output, result.ExitCode, err
}

func cleanCommandStateID(ctx context.Context, dir, sha string) (*string, error) {
	dirty, err := git.HasUncommittedChanges(ctx, dir)
	if err != nil {
		return nil, err
	}
	if dirty {
		return nil, nil
	}
	state := "git:" + sha
	return &state, nil
}

func sameStateID(left, right *string) bool {
	return left != nil && right != nil && *left == *right
}

func commandPurpose(sctx *pipeline.StepContext, explicit []string) string {
	if len(explicit) > 0 && strings.TrimSpace(explicit[0]) != "" {
		return explicit[0]
	}
	if sctx != nil && sctx.DB != nil && sctx.StepResultID != "" {
		if step, err := sctx.DB.GetStepResult(sctx.StepResultID); err == nil && step != nil {
			return string(step.StepName)
		}
	}
	return "command"
}

func commandEstablishesTestedHead(purpose string) bool {
	switch purpose {
	case string(types.StepBuild), string(types.StepTest), string(types.StepLint):
		return true
	default:
		return false
	}
}

func isRetryableCommandOutcome(outcome string) bool {
	switch outcome {
	case db.CommandOutcomeFail, db.CommandOutcomeProcessError, db.CommandOutcomeCancelled, db.CommandOutcomeTimeout:
		return true
	default:
		return false
	}
}

func commandAttemptOutcome(ctx context.Context, exitCode int, runErr error) string {
	if runErr == nil {
		if exitCode == 0 {
			return db.CommandOutcomePass
		}
		return db.CommandOutcomeFail
	}
	if errors.Is(runErr, runner.ErrTimeout) || errors.Is(runErr, context.DeadlineExceeded) {
		return db.CommandOutcomeTimeout
	}
	if errors.Is(runErr, context.Canceled) || ctx.Err() != nil {
		return db.CommandOutcomeCancelled
	}
	return db.CommandOutcomeProcessError
}

func runShellCommandWithEnv(ctx context.Context, dir string, env []string, cmdStr string, processTerminationGrace time.Duration) (string, int, error) {
	result, _, err := runRunnerCommandWithEnv(ctx, dir, env, runner.Command{Run: cmdStr}, runner.Spec{}, processTerminationGrace)
	return result.Output, result.ExitCode, err
}

func runRunnerCommandWithEnv(ctx context.Context, dir string, env []string, command runner.Command, defaultRunner runner.Spec, processTerminationGrace time.Duration) (runner.Result, runner.Resolved, error) {
	options := runner.ExecuteOptions{
		Dir:                     dir,
		ExtraEnv:                env,
		ProcessTerminationGrace: processTerminationGrace,
		CaptureFullOutput:       true,
	}
	prepared, err := runner.Prepare(ctx, command, defaultRunner, options)
	resolved := prepared.Resolution()
	if err != nil {
		return runner.Result{ExitCode: -1}, resolved, fmt.Errorf("prepare command %q: %w", command.Run, err)
	}
	result, err := prepared.Execute(ctx, options)
	if err != nil {
		return result, resolved, fmt.Errorf("run command %q: %w", resolved.Script, err)
	}
	return result, resolved, nil
}
