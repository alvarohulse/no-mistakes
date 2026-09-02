package runner

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const versionProbeTimeout = 5 * time.Second

var runnerVersionPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){1,3}$`)
var runnerVersionSearchPattern = regexp.MustCompile(`[0-9]+(?:\.[0-9]+){1,3}`)

type shellKind string

const (
	shellPOSIX      shellKind = "posix"
	shellPowerShell shellKind = "powershell"
)

// Provenance is the versioned, content-free identity of a resolved runner.
type Provenance struct {
	SchemaVersion int      `json:"schema_version"`
	Platform      string   `json:"platform"`
	Source        string   `json:"source"`
	Executable    string   `json:"executable"`
	Args          []string `json:"args"`
	Version       *string  `json:"version"`
}

// Resolved is one command string bound to an exact executable argv and runner
// provenance. Argv includes the command string as its final element.
type Resolved struct {
	Script        string     `json:"script"`
	Argv          []string   `json:"argv"`
	CommandSource string     `json:"command_source"`
	Provenance    Provenance `json:"provenance"`
	executable    string
}

type resolverDeps struct {
	platform     string
	lookPath     func(string) (string, error)
	probeVersion func(context.Context, shellKind, string) (*string, error)
}

// Resolve applies inline and platform precedence, resolves the executable,
// and captures runner provenance for the current platform.
func Resolve(ctx context.Context, command Command, defaultRunner Spec) (Resolved, error) {
	return resolve(ctx, command, defaultRunner, resolverDeps{
		platform:     runtime.GOOS,
		lookPath:     exec.LookPath,
		probeVersion: probeShellVersion,
	})
}

// ResolveDefault resolves only the configured top-level runner. It is used by
// policy explanation before any individual command is selected.
func ResolveDefault(ctx context.Context, defaultRunner Spec) (Provenance, error) {
	return resolveDefault(ctx, defaultRunner, resolverDeps{
		platform:     runtime.GOOS,
		lookPath:     exec.LookPath,
		probeVersion: probeShellVersion,
	})
}

func resolve(ctx context.Context, command Command, defaultRunner Spec, deps resolverDeps) (Resolved, error) {
	platform, err := normalizePlatform(deps.platform)
	if err != nil {
		return Resolved{}, err
	}
	script, commandSource, selectedRunner, runnerSource := effectiveCommand(command, defaultRunner, platform)
	if strings.TrimSpace(script) == "" {
		return Resolved{}, fmt.Errorf("runner command is empty for platform %s", platform)
	}
	resolved := Resolved{Script: script, CommandSource: commandSource}
	provenance, provenancePath, err := resolveSpec(ctx, selectedRunner, runnerSource, platform, deps)
	resolved.Provenance = provenance
	if err != nil {
		return resolved, err
	}
	argv := make([]string, 0, len(provenance.Args)+2)
	argv = append(argv, provenancePath)
	argv = append(argv, selectedRunner.Args...)
	argv = append(argv, script)
	resolved.Argv = argv
	resolved.executable = provenancePath
	return resolved, nil
}

func resolveDefault(ctx context.Context, configured Spec, deps resolverDeps) (Provenance, error) {
	platform, err := normalizePlatform(deps.platform)
	if err != nil {
		return Provenance{}, err
	}
	selected := configured
	source := SourceDefault
	if isZeroSpec(configured) {
		selected = portableSpec(platform)
		source = SourcePortableDefault
	}
	provenance, _, err := resolveSpec(ctx, selected, source, platform, deps)
	return provenance, err
}

func effectiveCommand(command Command, defaultRunner Spec, platform string) (string, string, Spec, string) {
	script := command.Run
	commandSource := SourceBase
	selectedRunner := defaultRunner
	runnerSource := SourceDefault
	if isZeroSpec(selectedRunner) {
		selectedRunner = portableSpec(platform)
		runnerSource = SourcePortableDefault
	}
	if command.Runner != nil {
		selectedRunner = *command.Runner
		runnerSource = SourceInline
	}

	override, source := command.platformOverride(platform)
	if override == nil {
		return script, commandSource, selectedRunner, runnerSource
	}
	if override.Run != nil {
		script = *override.Run
		commandSource = source
	}
	if override.Runner != nil {
		selectedRunner = *override.Runner
		runnerSource = source
	}
	return script, commandSource, selectedRunner, runnerSource
}

func (c Command) platformOverride(platform string) (*Override, string) {
	switch platform {
	case "linux":
		return c.Linux, SourceLinux
	case "darwin":
		return c.MacOS, SourceMacOS
	case "windows":
		return c.Windows, SourceWindows
	default:
		return nil, ""
	}
}

func resolveSpec(ctx context.Context, spec Spec, source, platform string, deps resolverDeps) (Provenance, string, error) {
	kind, err := validateSpec(spec)
	if err != nil {
		return Provenance{}, "", err
	}
	provenance := Provenance{
		SchemaVersion: SchemaVersion,
		Platform:      platform,
		Source:        source,
		Executable:    spec.Executable,
		Args:          append([]string(nil), spec.Args...),
	}
	if platform == "windows" && kind == shellPowerShell {
		provenance.Executable = strings.ToLower(provenance.Executable)
		for i := range provenance.Args {
			provenance.Args[i] = strings.ToLower(provenance.Args[i])
		}
	}
	path, err := deps.lookPath(spec.Executable)
	if err != nil {
		return provenance, "", fmt.Errorf("resolve runner executable %q: %w", spec.Executable, err)
	}
	version, err := deps.probeVersion(ctx, kind, path)
	if err != nil {
		return provenance, path, fmt.Errorf("probe runner version for %q: %w", spec.Executable, err)
	}
	if err := ValidateVersion(version); err != nil {
		return provenance, path, err
	}
	provenance.Version = version
	return provenance, path, nil
}

func validateSpec(spec Spec) (shellKind, error) {
	executable := strings.TrimSpace(spec.Executable)
	if executable == "" {
		return "", fmt.Errorf("runner executable is empty")
	}
	if executable != spec.Executable || strings.IndexFunc(executable, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("runner executable must be a trimmed printable path")
	}
	for i, arg := range spec.Args {
		if strings.TrimSpace(arg) == "" {
			return "", fmt.Errorf("runner argument %d is empty", i+1)
		}
		if strings.IndexFunc(arg, unicode.IsControl) >= 0 {
			return "", fmt.Errorf("runner argument %d contains control characters", i+1)
		}
	}

	name := strings.ToLower(executable)
	switch name {
	case "sh", "bash", "zsh":
		if len(spec.Args) != 1 || (spec.Args[0] != "-c" && spec.Args[0] != "-lc") {
			return "", fmt.Errorf("runner %s must use a supported argument shape: -c or -lc", name)
		}
		return shellPOSIX, nil
	case "pwsh", "powershell":
		want := []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}
		if len(spec.Args) != len(want) {
			return "", fmt.Errorf("runner %s must use the supported noninteractive argument shape", name)
		}
		for i := range want {
			if !strings.EqualFold(spec.Args[i], want[i]) {
				return "", fmt.Errorf("runner %s must use the supported noninteractive argument shape", name)
			}
		}
		return shellPowerShell, nil
	default:
		return "", fmt.Errorf("runner executable must be a bare supported shell name (sh, bash, zsh, pwsh, or powershell), got %q", spec.Executable)
	}
}

// ValidateSpec rejects runner material that cannot be persisted as a
// secret-free, machine-independent policy identity.
func ValidateSpec(spec Spec) error {
	_, err := validateSpec(spec)
	return err
}

// ValidateVersion accepts only the numeric identity extracted from a shell's
// version output. The full command output can contain machine paths or other
// arbitrary text and must never enter a policy snapshot.
func ValidateVersion(version *string) error {
	if version == nil {
		return nil
	}
	if !runnerVersionPattern.MatchString(*version) {
		return fmt.Errorf("unsafe runner version %q; expected a numeric dotted version", *version)
	}
	return nil
}

func normalizePlatform(platform string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "linux":
		return "linux", nil
	case "darwin", "macos":
		return "darwin", nil
	case "windows":
		return "windows", nil
	default:
		return "", fmt.Errorf("unsupported runner platform %q", platform)
	}
}

func portableSpec(platform string) Spec {
	if platform == "windows" {
		return Spec{Executable: "pwsh", Args: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}}
	}
	return Spec{Executable: "sh", Args: []string{"-c"}}
}

func isZeroSpec(spec Spec) bool {
	return spec.Executable == "" && len(spec.Args) == 0
}

func probeShellVersion(ctx context.Context, kind shellKind, executable string) (*string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if kind == shellPowerShell {
		cmd = exec.CommandContext(probeCtx, executable, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "$PSVersionTable.PSVersion.ToString()")
	} else {
		cmd = exec.CommandContext(probeCtx, executable, "--version")
	}
	shellenv.ConfigureShellCommand(cmd, shellenv.DefaultProcessTerminationGrace)
	output := newBoundedBuffer(4 * 1024)
	cmd.Stdout = output
	cmd.Stderr = output
	err := shellenv.RunShellCommand(cmd)
	if probeCtx.Err() != nil {
		return nil, probeCtx.Err()
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil, nil
		}
		return nil, err
	}
	version := runnerVersionSearchPattern.FindString(output.String())
	if version == "" {
		return nil, nil
	}
	return &version, nil
}
