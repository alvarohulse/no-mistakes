package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runner"
	"gopkg.in/yaml.v3"
)

func TestLoadGlobal_ConfiguresDefaultRunner(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`
runner:
  executable: zsh
  args: [-lc]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Executable != "zsh" || !reflect.DeepEqual(cfg.Runner.Args, []string{"-lc"}) {
		t.Fatalf("runner = %+v", cfg.Runner)
	}
}

func TestLoadRepo_CommandsAcceptScalarAndStructuredForms(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte(`
commands:
  test: make test
  build:
    run: make build
    runner:
      executable: zsh
      args: [-lc]
    linux:
      run: make build-linux
    windows:
      runner:
        executable: pwsh
        args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commands.Test != "make test" || cfg.Commands.Build != "make build" {
		t.Fatalf("legacy command projections = test %q build %q", cfg.Commands.Test, cfg.Commands.Build)
	}
	build := cfg.Commands.BuildCommand()
	if build.Runner == nil || build.Runner.Executable != "zsh" || build.Linux == nil || build.Windows == nil {
		t.Fatalf("structured build command = %+v", build)
	}
	if definitions := cfg.Commands.StructuredDefinitions(); len(definitions) != 1 || definitions["build"].Run != "make build" {
		t.Fatalf("structured definitions = %+v", definitions)
	}
}

func TestLoadRepo_CommandsPreserveLegacyTypedScalar(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("commands:\n  lint: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Commands.Lint != "true" {
		t.Fatalf("commands.lint = %q, want legacy shell command true", cfg.Commands.Lint)
	}
}

func TestLoadRepo_RejectsUnknownStructuredCommandFields(t *testing.T) {
	for _, input := range []string{
		"commands:\n  test:\n    command: make test\n",
		"commands:\n  test:\n    run: make test\n    runner:\n      executable: zsh\n      typo: true\n",
		"commands:\n  test:\n    run: make test\n    linux:\n      typo: true\n",
	} {
		if _, err := LoadRepoFromBytes([]byte(input)); err == nil {
			t.Fatalf("accepted unknown structured command field:\n%s", input)
		}
	}
}

func TestOverlayRepoConfig_MergesStructuredCommandFieldsAndScalarClear(t *testing.T) {
	base, err := LoadRepoFromBytes([]byte(`
commands:
  build:
    run: make build
    runner:
      executable: zsh
      args: [-lc]
    windows:
      run: make build-windows
      runner:
        executable: pwsh
        args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
  test:
    run: make test
    runner:
      executable: zsh
      args: [-lc]
`))
	if err != nil {
		t.Fatal(err)
	}
	override, err := LoadRepoFromBytes([]byte(`
commands:
  build:
    runner:
      args: [-c]
    windows:
      run: make build-windows-override
  test: ""
`))
	if err != nil {
		t.Fatal(err)
	}
	merged := OverlayRepoConfig(base, override)
	build := merged.Commands.BuildCommand()
	if build.Run != "make build" || build.Runner == nil || build.Runner.Executable != "zsh" || !reflect.DeepEqual(build.Runner.Args, []string{"-c"}) {
		t.Fatalf("merged build command = %+v", build)
	}
	if build.Windows == nil || build.Windows.Run == nil || *build.Windows.Run != "make build-windows-override" || build.Windows.Runner == nil {
		t.Fatalf("merged Windows command = %+v", build.Windows)
	}
	if !merged.Commands.TestCommand().IsZero() || merged.Commands.Test != "" {
		t.Fatalf("scalar clear retained test command: %+v", merged.Commands.TestCommand())
	}
}

func TestEffectiveRepoConfig_ProtectsStructuredCommandRunners(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte(`
commands:
  build:
    run: hostile-build
    runner: {executable: bash, args: [-c]}
`))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte(`
commands:
  build:
    run: safe-build
    runner: {executable: zsh, args: [-lc]}
`))
	if err != nil {
		t.Fatal(err)
	}

	protected := EffectiveRepoConfig(pushed, trusted, false)
	if protected.Commands.Build != "safe-build" || protected.Commands.BuildCommand().Runner.Executable != "zsh" {
		t.Fatalf("protected command = %+v", protected.Commands.BuildCommand())
	}
	optedIn := EffectiveRepoConfig(pushed, trusted, true)
	if optedIn.Commands.Build != "hostile-build" || optedIn.Commands.BuildCommand().Runner.Executable != "bash" {
		t.Fatalf("opted-in command = %+v", optedIn.Commands.BuildCommand())
	}
	withoutTrusted := EffectiveRepoConfig(pushed, nil, false)
	if !withoutTrusted.Commands.IsZero() {
		t.Fatalf("untrusted command survived without trusted config: %+v", withoutTrusted.Commands.BuildCommand())
	}
}

func TestMergeCarriesGlobalRunnerAndStructuredCommands(t *testing.T) {
	global := DefaultGlobalConfig()
	global.Runner = runner.Spec{Executable: "zsh", Args: []string{"-lc"}}
	repo, err := LoadRepoFromBytes([]byte("commands:\n  test:\n    run: make test\n    linux: make test-linux\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(global, repo)
	if cfg.Runner.Executable != "zsh" || !reflect.DeepEqual(cfg.Runner.Args, []string{"-lc"}) {
		t.Fatalf("merged runner = %+v", cfg.Runner)
	}
	if cfg.Commands.Test != "make test" || cfg.Commands.TestCommand().Linux == nil {
		t.Fatalf("merged test command = %+v", cfg.Commands.TestCommand())
	}
}

func TestCommandsMarshalPreservesStructuredShape(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("commands:\n  test:\n    run: make test\n    runner: {executable: zsh, args: [-lc]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := repo.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.TrimSpace(toYAML(t, encoded)), "runner:") {
		t.Fatalf("structured repo config was flattened: %+v", encoded)
	}
}

func toYAML(t *testing.T, value any) string {
	t.Helper()
	encoded, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
