package config

import "testing"

func TestResolveEffectiveConfigPublicationUsesGlobalDefaultAndExplicitValues(t *testing.T) {
	tests := []struct {
		name          string
		globalYAML    string
		wantPublish   bool
		wantIsDefault bool
	}{
		{name: "absent defaults false", globalYAML: "{}\n", wantPublish: false, wantIsDefault: true},
		{name: "explicit true", globalYAML: "effective_config:\n  publish: true\n", wantPublish: true, wantIsDefault: false},
		{name: "explicit false", globalYAML: "effective_config:\n  publish: false\n", wantPublish: false, wantIsDefault: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tt.globalYAML))
			if err != nil {
				t.Fatal(err)
			}

			resolved := ResolveEffectiveConfig(global, &RepoConfig{}, nil, nil, false)
			if got := resolved.Config.EffectiveConfig.Publish; got != tt.wantPublish {
				t.Errorf("EffectiveConfig.Publish = %t, want %t", got, tt.wantPublish)
			}
			if got := resolved.Provenance.Value("effective_config.publish"); got.Source != EffectiveConfigSourceGlobal || got.IsDefault != tt.wantIsDefault {
				t.Errorf("effective_config.publish provenance = %+v, want global is_default=%t", got, tt.wantIsDefault)
			}
		})
	}
}

func TestResolveEffectiveConfigPublicationIsTrustedOnly(t *testing.T) {
	tests := []struct {
		name        string
		globalYAML  string
		pushedYAML  string
		trustedYAML string
		allow       bool
		wantPublish bool
		wantSource  string
	}{
		{
			name:        "pushed true cannot enable publication",
			globalYAML:  "{}\n",
			pushedYAML:  "effective_config:\n  publish: true\n",
			trustedYAML: "{}\n",
			wantPublish: false,
			wantSource:  EffectiveConfigSourceGlobal,
		},
		{
			name:        "allow repo commands does not trust publication",
			globalYAML:  "{}\n",
			pushedYAML:  "effective_config:\n  publish: true\n",
			trustedYAML: "{}\n",
			allow:       true,
			wantPublish: false,
			wantSource:  EffectiveConfigSourceGlobal,
		},
		{
			name:        "pushed false cannot disable publication",
			globalYAML:  "effective_config:\n  publish: true\n",
			pushedYAML:  "effective_config:\n  publish: false\n",
			trustedYAML: "{}\n",
			wantPublish: true,
			wantSource:  EffectiveConfigSourceGlobal,
		},
		{
			name:        "trusted true overrides global false",
			globalYAML:  "effective_config:\n  publish: false\n",
			pushedYAML:  "effective_config:\n  publish: false\n",
			trustedYAML: "effective_config:\n  publish: true\n",
			wantPublish: true,
			wantSource:  EffectiveConfigSourceTrusted,
		},
		{
			name:        "trusted explicit false overrides global true",
			globalYAML:  "effective_config:\n  publish: true\n",
			pushedYAML:  "effective_config:\n  publish: true\n",
			trustedYAML: "effective_config:\n  publish: false\n",
			allow:       true,
			wantPublish: false,
			wantSource:  EffectiveConfigSourceTrusted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte(tt.globalYAML))
			if err != nil {
				t.Fatal(err)
			}
			pushed, err := LoadRepoFromBytes([]byte(tt.pushedYAML))
			if err != nil {
				t.Fatal(err)
			}
			trusted, err := LoadRepoFromBytes([]byte(tt.trustedYAML))
			if err != nil {
				t.Fatal(err)
			}

			resolved := ResolveEffectiveConfig(global, pushed, trusted, nil, tt.allow)
			if got := resolved.Config.EffectiveConfig.Publish; got != tt.wantPublish {
				t.Errorf("EffectiveConfig.Publish = %t, want %t", got, tt.wantPublish)
			}
			if got := resolved.Provenance.Value("effective_config.publish").Source; got != tt.wantSource {
				t.Errorf("effective_config.publish source = %q, want %q", got, tt.wantSource)
			}
		})
	}
}

func TestResolveEffectiveConfigPublicationUsesMachineOverrideLast(t *testing.T) {
	tests := []struct {
		name            string
		trustedPublish  bool
		overridePublish bool
	}{
		{name: "override enables trusted false", trustedPublish: false, overridePublish: true},
		{name: "override explicitly disables trusted true", trustedPublish: true, overridePublish: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte("effective_config:\n  publish: true\n"))
			if err != nil {
				t.Fatal(err)
			}
			trustedYAML := "effective_config:\n  publish: false\n"
			if tt.trustedPublish {
				trustedYAML = "effective_config:\n  publish: true\n"
			}
			trusted, err := LoadRepoFromBytes([]byte(trustedYAML))
			if err != nil {
				t.Fatal(err)
			}
			overrideYAML := "effective_config:\n  publish: false\n"
			if tt.overridePublish {
				overrideYAML = "effective_config:\n  publish: true\n"
			}
			override, err := LoadRepoFromBytes([]byte(overrideYAML))
			if err != nil {
				t.Fatal(err)
			}

			resolved := ResolveEffectiveConfig(global, &RepoConfig{}, trusted, override, false)
			if got := resolved.Config.EffectiveConfig.Publish; got != tt.overridePublish {
				t.Errorf("EffectiveConfig.Publish = %t, want override %t", got, tt.overridePublish)
			}
			if got := resolved.Provenance.Value("effective_config.publish"); got.Source != EffectiveConfigSourceGlobalOverride || got.IsDefault {
				t.Errorf("effective_config.publish provenance = %+v, want non-default global-override", got)
			}
		})
	}
}

func TestApplyEffectiveConfigPublishOverrideRecordsRunRequest(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("effective_config:\n  publish: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	resolved := ResolveEffectiveConfig(global, &RepoConfig{}, nil, nil, false)

	resolved.ApplyEffectiveConfigPublishOverride(nil)
	if !resolved.Config.EffectiveConfig.Publish {
		t.Fatal("nil override changed EffectiveConfig.Publish")
	}
	if got := resolved.Provenance.Value("effective_config.publish").Source; got != EffectiveConfigSourceGlobal {
		t.Fatalf("nil override source = %q, want global", got)
	}

	publish := false
	resolved.ApplyEffectiveConfigPublishOverride(&publish)
	if resolved.Config.EffectiveConfig.Publish {
		t.Fatal("explicit false override left EffectiveConfig.Publish enabled")
	}
	if got := resolved.Provenance.Value("effective_config.publish"); got.Source != EffectiveConfigSourceRunRequest || got.IsDefault {
		t.Fatalf("explicit false provenance = %+v, want non-default run-request", got)
	}

	publish = true
	resolved.ApplyEffectiveConfigPublishOverride(&publish)
	if !resolved.Config.EffectiveConfig.Publish {
		t.Fatal("explicit true override left EffectiveConfig.Publish disabled")
	}
	if got := resolved.Provenance.Value("effective_config.publish"); got.Source != EffectiveConfigSourceRunRequest || got.IsDefault {
		t.Fatalf("explicit true provenance = %+v, want non-default run-request", got)
	}
}

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
