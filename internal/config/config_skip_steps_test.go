package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCommittedRepoConfigRejectsPipelineSkipSteps(t *testing.T) {
	_, err := LoadRepoFromBytes([]byte("pipeline:\n  skip_steps: [ci]\n"))
	if err == nil || !strings.Contains(err.Error(), "machine-owned global override") {
		t.Fatalf("committed skip policy error = %v", err)
	}
}

func TestGlobalOverrideAcceptsAndCanonicalizesPipelineSkipSteps(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("overrides:\n  owner/repo:\n    pipeline:\n      skip_steps: [babysit, rebase, ci]\n"))
	if err != nil {
		t.Fatal(err)
	}
	override := global.Overrides["owner/repo"]
	want := []types.StepName{types.StepCI, types.StepRefresh}
	if override == nil || len(override.Pipeline.SkipSteps) != len(want) {
		t.Fatalf("skip steps = %+v", override)
	}
	for i := range want {
		if override.Pipeline.SkipSteps[i] != want[i] {
			t.Fatalf("skip steps = %v, want %v", override.Pipeline.SkipSteps, want)
		}
	}
}

func TestGlobalOverrideRejectsUnknownPipelineSkipStep(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("overrides:\n  owner/repo:\n    pipeline:\n      skip_steps: [deploy]\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown pipeline skip step "deploy"`) {
		t.Fatalf("unknown skip error = %v", err)
	}
}

func TestGlobalOverrideRejectsUnknownPipelineField(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("overrides:\n  owner/repo:\n    pipeline:\n      skips: [ci]\n"))
	if err == nil || !strings.Contains(err.Error(), `field skips not found`) {
		t.Fatalf("unknown pipeline field error = %v", err)
	}
}

func TestMergeCarriesConfiguredSkipSteps(t *testing.T) {
	repo := &RepoConfig{Pipeline: PipelineRaw{SkipSteps: []types.StepName{types.StepCI}}}
	cfg := Merge(DefaultGlobalConfig(), repo)
	if len(cfg.ConfiguredSkipSteps) != 1 || cfg.ConfiguredSkipSteps[0] != types.StepCI {
		t.Fatalf("configured skips = %v", cfg.ConfiguredSkipSteps)
	}
	repo.Pipeline.SkipSteps[0] = types.StepBuild
	if cfg.ConfiguredSkipSteps[0] != types.StepCI {
		t.Fatalf("merged skips alias source config: %v", cfg.ConfiguredSkipSteps)
	}
}

func TestMachineOverrideCanClearConfiguredSkipSteps(t *testing.T) {
	base := &RepoConfig{Pipeline: PipelineRaw{SkipSteps: []types.StepName{types.StepCI}}}
	override, err := parseGlobalOverrideRepoConfig([]byte("pipeline:\n  skip_steps: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := OverlayRepoConfig(base, override)
	if got.Pipeline.SkipSteps == nil || len(got.Pipeline.SkipSteps) != 0 {
		t.Fatalf("cleared skip steps = %#v, want explicit empty", got.Pipeline.SkipSteps)
	}
}

func parseGlobalOverrideRepoConfig(data []byte) (*RepoConfig, error) {
	global, err := LoadGlobalFromBytes(append([]byte("overrides:\n  owner/repo:\n"), indentYAML(data, 4)...))
	if err != nil {
		return nil, err
	}
	return global.Overrides["owner/repo"], nil
}

func indentYAML(data []byte, spaces int) []byte {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	return []byte(prefix + strings.Join(lines, "\n"+prefix) + "\n")
}
