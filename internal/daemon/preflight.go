package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

var preflightAuthorizationPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*(?:bearer|basic)\s+)[^\s]+`)

func (m *RunManager) prepareResolvedPreflight(ctx context.Context, cfg *config.Config) ([]runner.Prepared, error) {
	if cfg == nil || len(cfg.Preflight) == 0 {
		return nil, nil
	}
	prepare := m.preparePreflight
	if prepare == nil {
		prepare = runner.Prepare
	}
	options := runner.ExecuteOptions{
		Timeout:                 m.resolvedPreflightTimeout(),
		ProcessTerminationGrace: cfg.ProcessTerminationGrace,
		OutputLimit:             preflightOutputLimit,
	}
	prepared := make([]runner.Prepared, 0, len(cfg.Preflight))
	for i, command := range cfg.Preflight {
		if err := command.ValidateRunners(); err != nil {
			return nil, fmt.Errorf("preflight command %d is invalid: %s", i+1, preflightFailureText(err))
		}
		resolved, err := prepare(ctx, command, cfg.Runner, options)
		if err != nil {
			return nil, fmt.Errorf("preflight command %d is invalid: %s", i+1, preflightFailureText(err))
		}
		prepared = append(prepared, resolved)
	}
	return prepared, nil
}

func (m *RunManager) executeResolvedPreflight(ctx context.Context, resolved *runPolicyResolution, workDir string) error {
	if resolved == nil || len(resolved.PreparedPreflight) == 0 {
		return nil
	}
	execute := m.executePreflight
	if execute == nil {
		execute = func(ctx context.Context, prepared runner.Prepared, options runner.ExecuteOptions) (runner.Result, error) {
			return prepared.Execute(ctx, options)
		}
	}
	options := runner.ExecuteOptions{
		Dir:                     workDir,
		Timeout:                 m.resolvedPreflightTimeout(),
		ProcessTerminationGrace: resolved.Config.ProcessTerminationGrace,
		OutputLimit:             preflightOutputLimit,
	}
	for i, prepared := range resolved.PreparedPreflight {
		result, err := execute(ctx, prepared, options)
		provenance := preflightProvenance(prepared)
		diagnostics := preflightDiagnostics(result)
		if err != nil {
			if errors.Is(err, runner.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("preflight command %d timed out %s%s", i+1, provenance, diagnostics)
			}
			if ctx.Err() != nil {
				return fmt.Errorf("preflight command %d was interrupted %s: %w", i+1, provenance, ctx.Err())
			}
			return fmt.Errorf("preflight command %d launch failed %s%s: %s", i+1, provenance, diagnostics, preflightFailureText(err))
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("preflight command %d exited with code %d %s%s", i+1, result.ExitCode, provenance, diagnostics)
		}
	}
	return nil
}

func (m *RunManager) resolvedPreflightTimeout() time.Duration {
	if m.preflightTimeout > 0 {
		return m.preflightTimeout
	}
	return defaultPreflightTimeout
}

func preflightProvenance(prepared runner.Prepared) string {
	resolved := prepared.Resolution()
	version := "unknown"
	if resolved.Provenance.Version != nil {
		version = *resolved.Provenance.Version
	}
	return fmt.Sprintf("(command_source=%s runner_source=%s runner=%s version=%s)", resolved.CommandSource, resolved.Provenance.Source, resolved.Provenance.Executable, version)
}

func preflightDiagnostics(result runner.Result) string {
	text, sanitizedTruncated := sanitizedPreflightText(result.Output, preflightOutputLimit)
	if result.Truncated || sanitizedTruncated {
		if text == "" {
			return ": [output truncated]"
		}
		return ": " + text + "\n[output truncated]"
	}
	if text == "" {
		return ""
	}
	return ": " + text
}

func preflightFailureText(err error) string {
	if err == nil {
		return "unknown runner error"
	}
	text, truncated := sanitizedPreflightText(err.Error(), 1024)
	if text == "" {
		text = "unknown runner error"
	}
	if truncated {
		return text + " [truncated]"
	}
	return text
}

func sanitizedPreflightText(raw string, limit int) (string, bool) {
	text := safeurl.RedactText(intent.RedactSecrets(raw))
	text = preflightAuthorizationPattern.ReplaceAllString(text, `${1}[REDACTED]`)
	text = strings.TrimSpace(strings.ToValidUTF8(text, "?"))
	text = strings.Map(func(value rune) rune {
		if value == '\n' || value == '\t' || !unicode.IsControl(value) {
			return value
		}
		return '?'
	}, text)
	if limit <= 0 || len(text) <= limit {
		return text, false
	}
	return strings.TrimSpace(strings.ToValidUTF8(text[:limit], "?")), true
}
