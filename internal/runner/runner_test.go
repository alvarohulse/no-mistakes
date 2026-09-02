package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestResolveAppliesRunnerAndPlatformPrecedence(t *testing.T) {
	linuxRun := "linux command"
	macOSRun := "macos command"
	tests := []struct {
		name          string
		platform      string
		command       Command
		defaultRunner Spec
		wantArgv      []string
		wantSource    string
		wantCommand   string
	}{
		{
			name: "portable linux default", platform: "linux",
			command:  Command{Run: "base command"},
			wantArgv: []string{"/resolved/sh", "-c", "base command"}, wantSource: SourcePortableDefault, wantCommand: SourceBase,
		},
		{
			name: "configured default", platform: "linux",
			command:       Command{Run: "base command"},
			defaultRunner: Spec{Executable: "zsh", Args: []string{"-lc"}},
			wantArgv:      []string{"/resolved/zsh", "-lc", "base command"}, wantSource: SourceDefault, wantCommand: SourceBase,
		},
		{
			name: "inline runner", platform: "linux",
			command:       Command{Run: "base command", Runner: &Spec{Executable: "bash", Args: []string{"-c"}}},
			defaultRunner: Spec{Executable: "zsh", Args: []string{"-lc"}},
			wantArgv:      []string{"/resolved/bash", "-c", "base command"}, wantSource: SourceInline, wantCommand: SourceBase,
		},
		{
			name: "linux command and runner override", platform: "linux",
			command: Command{
				Run: "base command", Runner: &Spec{Executable: "bash", Args: []string{"-c"}},
				Linux: &Override{Run: &linuxRun, Runner: &Spec{Executable: "zsh", Args: []string{"-lc"}}},
			},
			wantArgv: []string{"/resolved/zsh", "-lc", "linux command"}, wantSource: SourceLinux, wantCommand: SourceLinux,
		},
		{
			name: "macos command override inherits inline runner", platform: "darwin",
			command: Command{
				Run: "base command", Runner: &Spec{Executable: "zsh", Args: []string{"-lc"}},
				MacOS: &Override{Run: &macOSRun},
			},
			wantArgv: []string{"/resolved/zsh", "-lc", "macos command"}, wantSource: SourceInline, wantCommand: SourceMacOS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := resolve(context.Background(), tt.command, tt.defaultRunner, resolverDeps{
				platform: tt.platform,
				lookPath: func(name string) (string, error) { return "/resolved/" + name, nil },
				probeVersion: func(context.Context, shellKind, string) (*string, error) {
					return stringPointer("1.2.3"), nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(resolved.Argv, tt.wantArgv) {
				t.Fatalf("argv = %#v, want %#v", resolved.Argv, tt.wantArgv)
			}
			if resolved.Provenance.Source != tt.wantSource || resolved.CommandSource != tt.wantCommand {
				t.Fatalf("sources = runner %q command %q, want %q/%q", resolved.Provenance.Source, resolved.CommandSource, tt.wantSource, tt.wantCommand)
			}
			if resolved.Provenance.SchemaVersion != SchemaVersion || resolved.Provenance.Version == nil || *resolved.Provenance.Version != "1.2.3" {
				t.Fatalf("provenance = %+v", resolved.Provenance)
			}
			if strings.HasPrefix(resolved.Provenance.Executable, "/resolved/") {
				t.Fatalf("provenance leaked resolved machine path: %+v", resolved.Provenance)
			}
		})
	}
}

func TestResolveBuildsNonInteractivePowerShellArgv(t *testing.T) {
	windowsRun := "Write-Output ready"
	resolved, err := resolve(context.Background(), Command{
		Run:     "base command",
		Windows: &Override{Run: &windowsRun},
	}, Spec{}, resolverDeps{
		platform:     "windows",
		lookPath:     func(name string) (string, error) { return `C:\\Tools\\` + name + ".exe", nil },
		probeVersion: func(context.Context, shellKind, string) (*string, error) { return stringPointer("7.5"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`C:\\Tools\\pwsh.exe`, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsRun}
	if !reflect.DeepEqual(resolved.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", resolved.Argv, want)
	}
	if resolved.CommandSource != SourceWindows || resolved.Provenance.Source != SourcePortableDefault {
		t.Fatalf("sources = command %q runner %q", resolved.CommandSource, resolved.Provenance.Source)
	}
	syntaxArgv, err := resolved.syntaxArgv()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(syntaxArgv, " "), windowsRun) {
		t.Fatalf("PowerShell syntax probe embedded source in argv: %#v", syntaxArgv)
	}
}

func TestResolveCanonicalizesPowerShellIdentityWithoutChangingExecutionArgv(t *testing.T) {
	args := []string{"-NoLogo", "-NOPROFILE", "-NonInteractive", "-COMMAND"}
	resolved, err := resolve(context.Background(), Command{Run: "Write-Output ready"}, Spec{Executable: "PowerShell", Args: args}, resolverDeps{
		platform:     "windows",
		lookPath:     func(string) (string, error) { return `C:\\Tools\\PowerShell.exe`, nil },
		probeVersion: func(context.Context, shellKind, string) (*string, error) { return stringPointer("7.5"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	wantProvenanceArgs := []string{"-nologo", "-noprofile", "-noninteractive", "-command"}
	if resolved.Provenance.Executable != "powershell" || !reflect.DeepEqual(resolved.Provenance.Args, wantProvenanceArgs) {
		t.Fatalf("canonical provenance = %+v", resolved.Provenance)
	}
	wantArgv := []string{`C:\\Tools\\PowerShell.exe`, "-NoLogo", "-NOPROFILE", "-NonInteractive", "-COMMAND", "Write-Output ready"}
	if !reflect.DeepEqual(resolved.Argv, wantArgv) {
		t.Fatalf("execution argv = %#v, want %#v", resolved.Argv, wantArgv)
	}
}

func TestResolveRejectsInvalidCommandAndRunnerArgv(t *testing.T) {
	tests := []struct {
		name    string
		command Command
		want    string
	}{
		{name: "empty command", command: Command{}, want: "command is empty"},
		{name: "empty executable", command: Command{Run: "echo ok", Runner: &Spec{}}, want: "executable is empty"},
		{name: "empty argument", command: Command{Run: "echo ok", Runner: &Spec{Executable: "zsh", Args: []string{""}}}, want: "argument 1 is empty"},
		{name: "missing command flag", command: Command{Run: "echo ok", Runner: &Spec{Executable: "zsh", Args: []string{"-l"}}}, want: "supported argument shape"},
		{name: "interactive powershell", command: Command{Run: "echo ok", Runner: &Spec{Executable: "pwsh", Args: []string{"-Command"}}}, want: "supported noninteractive argument shape"},
		{name: "unsupported shell", command: Command{Run: "echo ok", Runner: &Spec{Executable: "python", Args: []string{"-c"}}}, want: "bare supported shell name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolve(context.Background(), tt.command, Spec{}, resolverDeps{
				platform:     "linux",
				lookPath:     func(name string) (string, error) { return "/resolved/" + name, nil },
				probeVersion: func(context.Context, shellKind, string) (*string, error) { return stringPointer("1.0"), nil },
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resolve() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestResolveRejectsMissingRunnerBinary(t *testing.T) {
	_, err := resolve(context.Background(), Command{Run: "echo ok"}, Spec{Executable: "zsh", Args: []string{"-lc"}}, resolverDeps{
		platform:     "linux",
		lookPath:     func(name string) (string, error) { return "", &exec.Error{Name: name, Err: exec.ErrNotFound} },
		probeVersion: func(context.Context, shellKind, string) (*string, error) { return nil, nil },
	})
	if err == nil || !errors.Is(err, exec.ErrNotFound) || !strings.Contains(err.Error(), "resolve runner executable") {
		t.Fatalf("resolve() error = %v", err)
	}
}

func TestResolveRetainsConfiguredProvenanceOnOperationalErrors(t *testing.T) {
	linuxRun := "linux command"
	probeErr := errors.New("version probe unavailable")
	tests := []struct {
		name         string
		lookPath     func(string) (string, error)
		probeVersion func(context.Context, shellKind, string) (*string, error)
		wantErr      error
	}{
		{
			name: "missing executable",
			lookPath: func(name string) (string, error) {
				return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
			},
			probeVersion: func(context.Context, shellKind, string) (*string, error) { return nil, nil },
			wantErr:      exec.ErrNotFound,
		},
		{
			name:         "version probe failure",
			lookPath:     func(name string) (string, error) { return "/resolved/" + name, nil },
			probeVersion: func(context.Context, shellKind, string) (*string, error) { return nil, probeErr },
			wantErr:      probeErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := resolve(context.Background(), Command{
				Run:   "base command",
				Linux: &Override{Run: &linuxRun},
			}, Spec{Executable: "zsh", Args: []string{"-lc"}}, resolverDeps{
				platform:     "linux",
				lookPath:     tt.lookPath,
				probeVersion: tt.probeVersion,
			})
			if err == nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("resolve() error = %v, want %v", err, tt.wantErr)
			}
			if resolved.Script != linuxRun || resolved.CommandSource != SourceLinux {
				t.Fatalf("partial command resolution = %+v", resolved)
			}
			provenance := resolved.Provenance
			if provenance.SchemaVersion != SchemaVersion || provenance.Platform != "linux" || provenance.Source != SourceDefault || provenance.Executable != "zsh" || !reflect.DeepEqual(provenance.Args, []string{"-lc"}) || provenance.Version != nil {
				t.Fatalf("partial runner provenance = %+v", provenance)
			}
			if len(resolved.Argv) != 0 {
				t.Fatalf("failed resolution exposed executable argv = %#v", resolved.Argv)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func TestResolvedRunDistinguishesTimeoutLaunchAndExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	prepared, err := Prepare(context.Background(), Command{Run: "sleep 5"}, Spec{}, ExecuteOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepared.Execute(context.Background(), ExecuteOptions{Timeout: 25 * time.Millisecond})
	if err == nil || !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %v", err)
	}

	missing := Prepared{resolved: Resolved{Argv: []string{filepath.Join(t.TempDir(), "removed-shell")}}, validated: true}
	_, err = missing.Execute(context.Background(), ExecuteOptions{})
	if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "launch runner") {
		t.Fatalf("launch error = %v", err)
	}

	nonzero, err := Prepare(context.Background(), Command{Run: "exit 7"}, Spec{}, ExecuteOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result, err := nonzero.Execute(context.Background(), ExecuteOptions{})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("nonzero result = %+v error %v", result, err)
	}
}

func TestPreparedExecuteBoundsCapturedOutputAndReportsTruncation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, tt := range []struct {
		name      string
		requested int
		want      int
	}{
		{name: "smaller caller limit", requested: 1024, want: 1024},
		{name: "safe default", requested: 0, want: maxCapturedOutputBytes},
		{name: "hard cap", requested: 1024 * 1024, want: maxCapturedOutputBytes},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := Prepare(context.Background(), Command{Run: `head -c 131072 /dev/zero`}, Spec{}, ExecuteOptions{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			result, err := prepared.Execute(context.Background(), ExecuteOptions{Timeout: time.Second, OutputLimit: tt.requested})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Output) != tt.want || !result.Truncated {
				t.Fatalf("captured output = %d bytes, truncated=%t; want %d/true", len(result.Output), result.Truncated, tt.want)
			}
		})
	}
}

func TestPreparedExecuteRetainsFullOutputWhenPipelineLoggingRequestsIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	prepared, err := Prepare(context.Background(), Command{Run: `head -c 131072 /dev/zero`}, Spec{}, ExecuteOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	result, err := prepared.Execute(context.Background(), ExecuteOptions{Timeout: time.Second, CaptureFullOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) != 131072 || result.Truncated {
		t.Fatalf("captured output = %d bytes, truncated=%t; want 131072/false", len(result.Output), result.Truncated)
	}
}

func TestPrepareRetainsResolvedProvenanceWhenSyntaxValidationFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	prepared, err := Prepare(context.Background(), Command{Run: "if true; then"}, Spec{}, ExecuteOptions{Timeout: time.Second})
	if err == nil || !errors.Is(err, ErrInvalidSyntax) {
		t.Fatalf("Prepare() error = %v, want invalid syntax", err)
	}
	resolved := prepared.Resolution()
	if resolved.Script != "if true; then" || resolved.CommandSource != SourceBase || resolved.Provenance.Executable != "sh" {
		t.Fatalf("resolved syntax failure = %+v", resolved)
	}
}

func TestPreparedExecutePreservesParentCancellationIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	prepared, err := Prepare(context.Background(), Command{Run: "sleep 5"}, Spec{}, ExecuteOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = prepared.Execute(ctx, ExecuteOptions{Timeout: time.Second})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestPreparedExecuteMergesExtraEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	prepared, err := Prepare(context.Background(), Command{Run: `printf '%s:%s' "$HOME" "$NM_RUNNER_TEST"`}, Spec{}, ExecuteOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Execute(context.Background(), ExecuteOptions{ExtraEnv: []string{"NM_RUNNER_TEST=ready"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(result.Output, ":ready") || strings.HasPrefix(result.Output, ":") {
		t.Fatalf("merged environment output = %q", result.Output)
	}
	if result.Truncated {
		t.Fatal("short output was reported as truncated")
	}
}

func TestResolveRejectsUnsafePersistedRunnerIdentity(t *testing.T) {
	tests := []struct {
		name         string
		runner       Spec
		probeVersion *string
		want         string
	}{
		{
			name:         "configured executable path",
			runner:       Spec{Executable: "/tmp/private-token/zsh", Args: []string{"-lc"}},
			probeVersion: stringPointer("5.9"),
			want:         "bare supported shell name",
		},
		{
			name:         "arbitrary shell argument",
			runner:       Spec{Executable: "zsh", Args: []string{"--rcs=/tmp/private-token", "-lc"}},
			probeVersion: stringPointer("5.9"),
			want:         "supported argument shape",
		},
		{
			name:         "arbitrary version output",
			runner:       Spec{Executable: "zsh", Args: []string{"-lc"}},
			probeVersion: stringPointer("secret-token 5.9"),
			want:         "unsafe runner version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolve(context.Background(), Command{Run: "echo ok"}, tt.runner, resolverDeps{
				platform: "linux",
				lookPath: func(name string) (string, error) { return "/resolved/" + filepath.Base(name), nil },
				probeVersion: func(context.Context, shellKind, string) (*string, error) {
					return tt.probeVersion, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("resolve() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCommandValidateRunnersChecksInactivePlatformOverrides(t *testing.T) {
	command := Command{
		Run: "echo ok",
		Windows: &Override{Runner: &Spec{
			Executable: `C:\\private-token\\pwsh.exe`,
			Args:       []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"},
		}},
	}
	if err := command.ValidateRunners(); err == nil || !strings.Contains(err.Error(), "windows runner") || !strings.Contains(err.Error(), "bare supported shell name") {
		t.Fatalf("ValidateRunners() error = %v", err)
	}
}

func TestResolvedValidateSyntaxUsesResolvedPOSIXShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, shell := range []Spec{{Executable: "sh", Args: []string{"-c"}}, {Executable: "zsh", Args: []string{"-lc"}}} {
		if _, err := exec.LookPath(shell.Executable); err != nil {
			continue
		}
		t.Run(shell.Executable, func(t *testing.T) {
			valid, err := Resolve(context.Background(), Command{Run: "if true; then echo ready; fi"}, shell)
			if err != nil {
				t.Fatal(err)
			}
			if err := valid.ValidateSyntax(context.Background(), ExecuteOptions{Timeout: time.Second}); err != nil {
				t.Fatalf("valid syntax rejected: %v", err)
			}

			invalid, err := Resolve(context.Background(), Command{Run: "if true; then"}, shell)
			if err != nil {
				t.Fatal(err)
			}
			if err := invalid.ValidateSyntax(context.Background(), ExecuteOptions{Timeout: time.Second}); err == nil || !errors.Is(err, ErrInvalidSyntax) {
				t.Fatalf("invalid syntax error = %v", err)
			}
		})
	}
}

func TestCommandYAMLAcceptsLegacyScalarAndStructuredOverrides(t *testing.T) {
	var legacy Command
	if err := yaml.Unmarshal([]byte("make test\n"), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Run != "make test" || legacy.Runner != nil {
		t.Fatalf("legacy command = %+v", legacy)
	}

	var structured Command
	if err := yaml.Unmarshal([]byte(`
run: make test
runner:
  executable: zsh
  args: [-lc]
linux:
  run: make test-linux
windows:
  runner:
    executable: pwsh
    args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`), &structured); err != nil {
		t.Fatal(err)
	}
	if structured.Run != "make test" || structured.Runner == nil || structured.Linux == nil || structured.Windows == nil {
		t.Fatalf("structured command = %+v", structured)
	}
	encoded, err := yaml.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "run: make test") || !strings.Contains(string(encoded), "linux:") {
		t.Fatalf("structured YAML = %s", encoded)
	}
}

func TestCommandYAMLRejectsUnknownFields(t *testing.T) {
	for _, input := range []string{
		"run: make test\nunknown: true\n",
		"run: make test\nrunner:\n  executable: sh\n  unknown: true\n",
		"run: make test\nlinux:\n  unknown: true\n",
	} {
		var command Command
		if err := yaml.Unmarshal([]byte(input), &command); err == nil {
			t.Fatalf("accepted unknown field:\n%s", input)
		}
	}
}

func TestCommandYAMLAcceptsMergeKeysWithoutWeakeningStrictFields(t *testing.T) {
	var command Command
	if err := yaml.Unmarshal([]byte(`
run: make test
runner:
  <<: &shell
    executable: zsh
    args: [-lc]
linux:
  runner:
    <<: *shell
`), &command); err != nil {
		t.Fatal(err)
	}
	if command.Runner == nil || command.Linux == nil || command.Linux.Runner == nil || command.Linux.Runner.Executable != "zsh" {
		t.Fatalf("merged command = %+v", command)
	}
}

func TestCommandOverlayPreservesNestedFieldsAndScalarClearReplaces(t *testing.T) {
	decode := func(input string) Command {
		t.Helper()
		var command Command
		if err := yaml.Unmarshal([]byte(input), &command); err != nil {
			t.Fatal(err)
		}
		return command
	}
	base := decode(`
run: make test
runner:
  executable: zsh
  args: [-lc]
windows:
  run: make test-windows
  runner:
    executable: pwsh
    args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`)
	override := decode(`
runner:
  args: [-c]
windows:
  run: make test-windows-override
`)
	merged := base.Overlay(override)
	if merged.Run != "make test" || merged.Runner == nil || merged.Runner.Executable != "zsh" || !reflect.DeepEqual(merged.Runner.Args, []string{"-c"}) {
		t.Fatalf("merged command = %+v", merged)
	}
	if merged.Windows == nil || merged.Windows.Run == nil || *merged.Windows.Run != "make test-windows-override" || merged.Windows.Runner == nil || merged.Windows.Runner.Executable != "pwsh" {
		t.Fatalf("merged windows override = %+v", merged.Windows)
	}

	cleared := base.Overlay(decode(`""`))
	if !cleared.IsZero() {
		t.Fatalf("scalar clear retained fields: %+v", cleared)
	}

	mapping := decode("run: make test\n")
	legacy := decode("make test\n")
	if !mapping.Equal(legacy) {
		t.Fatalf("equivalent scalar and mapping commands differ: %+v / %+v", mapping, legacy)
	}
}

func TestCommandOverlayReportsOnlyAppliedNestedLeaves(t *testing.T) {
	decode := func(input string) Command {
		t.Helper()
		var command Command
		if err := yaml.Unmarshal([]byte(input), &command); err != nil {
			t.Fatal(err)
		}
		return command
	}
	base := decode(`
run: make build
windows:
  run: make build-windows
  runner:
    executable: pwsh
    args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`)
	override := decode(`
windows:
  runner:
    executable: powershell
`)

	merged, applied := base.OverlayWithAppliedPaths(override)
	if merged.Windows == nil || merged.Windows.Run == nil || *merged.Windows.Run != "make build-windows" || merged.Windows.Runner == nil {
		t.Fatalf("merged Windows command = %#v", merged.Windows)
	}
	if got, want := applied, []string{"windows.runner.executable"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("applied paths = %v, want %v", got, want)
	}
}
