package config

import "testing"

func TestResolveEffectiveConfigRecordsAppliedLeaves(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`hooks:
  pr_body: global-formatter
overrides:
  test/repo:
    commands:
      build:
        windows:
          runner:
            executable: powershell
`))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte(`hooks:
  pr_body: ""
commands:
  build:
    run: trusted-build
    windows:
      run: trusted-windows-build
      runner:
        executable: pwsh
        args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`))
	if err != nil {
		t.Fatal(err)
	}

	resolved := ResolveEffectiveConfig(global, &RepoConfig{}, trusted, global.Overrides["test/repo"], false)
	for path, want := range map[string]string{
		"hooks.pr_body":                            EffectiveConfigSourceGlobal,
		"commands.build.windows.run":               EffectiveConfigSourceTrusted,
		"commands.build.windows.runner.executable": EffectiveConfigSourceGlobalOverride,
		"commands.build.windows.runner.args":       EffectiveConfigSourceTrusted,
	} {
		if got := resolved.Provenance.Value(path).Source; got != want {
			t.Errorf("%s source = %q, want %q", path, got, want)
		}
	}
}

func TestResolveEffectiveConfigKeepsTrustedAllowRepoCommands(t *testing.T) {
	global := DefaultGlobalConfig()
	trusted, err := LoadRepoFromBytes([]byte("allow_repo_commands: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	override, err := LoadRepoFromBytes([]byte("allow_repo_commands: false\n"))
	if err != nil {
		t.Fatal(err)
	}

	resolved := ResolveEffectiveConfig(global, &RepoConfig{}, trusted, override, true)
	if !resolved.Config.AllowRepoCommands {
		t.Fatal("AllowRepoCommands = false, want trusted true")
	}
	if got := resolved.Provenance.Value("allow_repo_commands").Source; got != EffectiveConfigSourceTrusted {
		t.Fatalf("allow_repo_commands source = %q, want trusted", got)
	}
}

func TestResolveEffectiveConfigScalarCommandDoesNotOwnAddedRunnerSiblings(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`overrides:
  test/repo:
    commands:
      build:
        runner:
          executable: zsh
`))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("commands:\n  build: make build\n"))
	if err != nil {
		t.Fatal(err)
	}

	resolved := ResolveEffectiveConfig(global, &RepoConfig{}, trusted, global.Overrides["test/repo"], false)
	for path, want := range map[string]EffectiveConfigProvenanceValue{
		"commands.build.run":               {Source: EffectiveConfigSourceTrusted},
		"commands.build.runner.executable": {Source: EffectiveConfigSourceGlobalOverride},
		"commands.build.runner.args":       {Source: EffectiveConfigSourceGlobal, IsDefault: true},
	} {
		if got := resolved.Provenance.Value(path); got.Source != want.Source || got.IsDefault != want.IsDefault {
			t.Errorf("%s provenance = %+v, want %+v", path, got, want)
		}
	}
}

func TestResolveEffectiveConfigDistinguishesIgnoredGlobalValuesFromExplicitDefaults(t *testing.T) {
	ignored, err := LoadGlobalFromBytes([]byte(`managed: null
step_quiet_warning: 0s
auto_fix:
  build: null
ci:
  rerun_transient: null
commit:
  fix_message: null
intent:
  enabled: null
eval:
  capture_provenance: null
prompts:
  review: ""
`))
	if err != nil {
		t.Fatal(err)
	}
	ignoredResolution := ResolveEffectiveConfig(ignored, &RepoConfig{}, nil, nil, false)
	for _, path := range []string{
		"managed", "step_quiet_warning", "auto_fix.build", "ci.rerun_transient", "commit.fix_message",
		"intent.enabled", "eval.capture_provenance", "prompts.review",
	} {
		if got := ignoredResolution.Provenance.Value(path); got.Source != EffectiveConfigSourceGlobal || !got.IsDefault {
			t.Errorf("ignored %s provenance = %+v, want global default", path, got)
		}
	}

	explicit, err := LoadGlobalFromBytes([]byte(`managed: false
auto_fix:
  build: 0
ci:
  rerun_transient: 0
intent:
  enabled: false
eval:
  capture_provenance: false
`))
	if err != nil {
		t.Fatal(err)
	}
	explicitResolution := ResolveEffectiveConfig(explicit, &RepoConfig{}, nil, nil, false)
	for _, path := range []string{"managed", "auto_fix.build", "ci.rerun_transient", "intent.enabled", "eval.capture_provenance"} {
		if got := explicitResolution.Provenance.Value(path); got.Source != EffectiveConfigSourceGlobal || got.IsDefault {
			t.Errorf("explicit %s provenance = %+v, want non-default global", path, got)
		}
	}
}
