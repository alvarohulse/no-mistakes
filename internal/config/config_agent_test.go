package config

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAgentPath_Override(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentClaude,
		AgentPathOverride: map[string]string{"claude": "/custom/claude"},
	}
	if got := cfg.AgentPath(); got != "/custom/claude" {
		t.Errorf("AgentPath() = %q, want %q", got, "/custom/claude")
	}
}

func TestAgentPath_DefaultBinaries(t *testing.T) {
	tests := []struct {
		agent types.AgentName
		want  string
	}{
		{types.AgentClaude, "claude"},
		{types.AgentCodex, "codex"},
		{types.AgentRovoDev, "acli"},
		{types.AgentOpenCode, "opencode"},
		{types.AgentPi, "pi"},
		{types.AgentCopilot, "copilot"},
		{types.AgentCursor, "cursor-agent"},
	}
	for _, tt := range tests {
		cfg := &Config{Agent: tt.agent}
		if got := cfg.AgentPath(); got != tt.want {
			t.Errorf("AgentPath() for %q = %q, want %q", tt.agent, got, tt.want)
		}
	}
}

func TestAgentPath_CursorNativeAndExplicitACPStayDistinct(t *testing.T) {
	native := &Config{Agent: types.AgentCursor, ACPXPath: "/opt/bin/acpx"}
	if got := native.AgentPath(); got != "cursor-agent" {
		t.Fatalf("native cursor path = %q, want cursor-agent", got)
	}

	acp := &Config{Agent: "acp:cursor", ACPXPath: "/opt/bin/acpx"}
	if got := acp.AgentPath(); got != "/opt/bin/acpx" {
		t.Fatalf("Cursor ACP path = %q, want /opt/bin/acpx", got)
	}
}

func TestAgentPath_ACPTargetsUseAcpxPath(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{name: "default", cfg: &Config{Agent: "acp:gemini"}, want: "acpx"},
		{name: "override", cfg: &Config{Agent: "acp:gemini", ACPXPath: "/opt/bin/acpx"}, want: "/opt/bin/acpx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.AgentPath(); got != tt.want {
				t.Errorf("AgentPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"DEBUG", slog.LevelInfo}, // case-sensitive, unrecognized defaults to info
	}
	for _, tt := range tests {
		got := ParseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestResolveAgent_ExplicitAgent(t *testing.T) {
	cfg := &Config{Agent: types.AgentCodex}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin != "codex" {
			t.Fatalf("lookPath(%q), want codex", bin)
		}
		return "/usr/local/bin/codex", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
}

func TestResolveAgent_ExplicitAgentMustBeRunnable(t *testing.T) {
	cfg := &Config{Agent: types.AgentCodex}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected unavailable explicit agent to fail resolution")
	}
	for _, want := range []string{"no runnable agent", "codex", "gate cannot validate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ResolveAgent() error should contain %q, got: %v", want, err)
		}
	}
}

func TestResolveAgent_ExplicitACPAgent(t *testing.T) {
	cfg := &Config{Agent: "acp:gemini"}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin != "acpx" {
			t.Fatalf("lookPath(%q), want acpx", bin)
		}
		return "/usr/local/bin/acpx", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "acp:gemini" {
		t.Errorf("agent = %q, want %q", cfg.Agent, "acp:gemini")
	}
}

func TestResolveAgent_ExplicitACPAgentMustHaveACPX(t *testing.T) {
	cfg := &Config{Agent: "acp:gemini"}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected ACP agent without acpx to fail resolution")
	}
	for _, want := range []string{"no runnable agent", "acp:gemini", "acpx", "gate cannot validate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ResolveAgent() error should contain %q, got: %v", want, err)
		}
	}
}

func TestLoadGlobal_ACPConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`agent: acp:gemini
acpx_path: /opt/bin/acpx
acp_registry_overrides:
  local-gemini: node /tmp/mock-acp.mjs
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal() error = %v", err)
	}
	if cfg.Agent != "acp:gemini" {
		t.Errorf("agent = %q, want acp:gemini", cfg.Agent)
	}
	if cfg.ACPXPath != "/opt/bin/acpx" {
		t.Errorf("ACPXPath = %q, want /opt/bin/acpx", cfg.ACPXPath)
	}
	if got := cfg.ACPRegistryOverrides["local-gemini"]; got != "node /tmp/mock-acp.mjs" {
		t.Errorf("ACPRegistryOverrides[local-gemini] = %q", got)
	}
}

func TestResolveAgent_AutoPicksFirstAvailable(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	// Simulate: claude not found, codex found
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
}

func TestResolveAgent_ListPicksFirstAvailableAndKeepsFallbacks(t *testing.T) {
	cfg := &Config{Agents: []types.AgentName{types.AgentClaude, types.AgentCodex, types.AgentPi}}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "codex", "pi":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
	want := []types.AgentName{types.AgentCodex, types.AgentPi}
	if len(cfg.Agents) != len(want) {
		t.Fatalf("agents = %v, want %v", cfg.Agents, want)
	}
	for i := range want {
		if cfg.Agents[i] != want[i] {
			t.Fatalf("agents = %v, want %v", cfg.Agents, want)
		}
	}
}

func TestResolveAgent_ListKeepsNativeCursorAndACPAsDistinctFallbacks(t *testing.T) {
	tests := []struct {
		name       string
		candidates []types.AgentName
		want       []types.AgentName
	}{
		{name: "native before target", candidates: []types.AgentName{types.AgentCursor, "acp:cursor"}, want: []types.AgentName{types.AgentCursor, "acp:cursor"}},
		{name: "target before native", candidates: []types.AgentName{"acp:cursor", types.AgentCursor}, want: []types.AgentName{"acp:cursor", types.AgentCursor}},
		{name: "auto before target", candidates: []types.AgentName{types.AgentAuto, "acp:cursor"}, want: []types.AgentName{types.AgentCursor, "acp:cursor"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Agents: tt.candidates}
			err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
				switch bin {
				case "cursor-agent", "acpx":
					return "/usr/bin/" + bin, nil
				default:
					return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
				}
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Agent != tt.want[0] {
				t.Errorf("agent = %q, want %q", cfg.Agent, tt.want[0])
			}
			if !reflect.DeepEqual(cfg.Agents, tt.want) {
				t.Fatalf("agents = %v, want %v", cfg.Agents, tt.want)
			}
		})
	}
}

func TestResolveAgent_ListSkipsUnavailableAuto(t *testing.T) {
	cfg := &Config{Agents: []types.AgentName{types.AgentAuto, "acp:gemini"}}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "acpx" {
			return "/usr/bin/acpx", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "acp:gemini" {
		t.Errorf("agent = %q, want acp:gemini", cfg.Agent)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0] != "acp:gemini" {
		t.Fatalf("agents = %v, want [acp:gemini]", cfg.Agents)
	}
}

func TestResolveAgent_AutoPicksClaude(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "claude" {
			return "/usr/bin/claude", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
}

func TestResolveAgent_AutoPicksNativeCursorWithoutACPX(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "cursor-agent" {
			return "/usr/bin/cursor-agent", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("ResolveAgent() error = %v", err)
	}
	if cfg.Agent != types.AgentCursor {
		t.Fatalf("agent = %q, want native cursor", cfg.Agent)
	}
}

func TestResolveAgent_AutoKeepsCursorACPFallbackProbe(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentAuto,
		AgentPathOverride: map[string]string{"cursor": "/missing/native-cursor"},
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "cursor-agent", "acpx":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("ResolveAgent() error = %v", err)
	}
	if cfg.Agent != "acp:cursor" {
		t.Fatalf("agent = %q, want acp:cursor fallback", cfg.Agent)
	}
}

func TestResolveAgent_NativeCursorAcceptsParameterizedCrossVendorModels(t *testing.T) {
	models := []ModelRoute{
		{Name: "claude-opus-5[effort=high]", Vendor: "anthropic"},
		{Name: "gpt-5.6-sol[reasoning=high]", Vendor: "openai"},
	}
	for _, model := range models {
		t.Run(model.Vendor, func(t *testing.T) {
			cfg := &Config{
				Agent:      types.AgentCursor,
				Agents:     []types.AgentName{types.AgentCursor},
				StepAgents: map[types.StepName][]types.AgentName{types.StepReview: {types.AgentCursor}},
				StepModels: map[types.StepName]ModelRoute{types.StepReview: model},
			}
			err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
				if bin == "cursor-agent" {
					return "/usr/bin/cursor-agent", nil
				}
				return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
			})
			if err != nil {
				t.Fatalf("ResolveAgent() error = %v", err)
			}
			if got := cfg.StepAgents[types.StepReview]; len(got) != 1 || got[0] != types.AgentCursor {
				t.Fatalf("review agents = %v, want [cursor]", got)
			}
		})
	}
}

func TestResolveAgent_AutoRespectsPathOverride(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentAuto,
		AgentPathOverride: map[string]string{"opencode": "/custom/opencode"},
	}
	// Only opencode override path exists
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "/custom/opencode" {
			return "/custom/opencode", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentOpenCode {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentOpenCode)
	}
}

func TestResolveAgent_AutoSkipsMissingOverrideAndFallsBack(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentAuto,
		AgentPathOverride: map[string]string{"claude": "/custom/claude"},
	}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "/custom/claude":
			return "", &exec.Error{Name: bin, Err: fs.ErrNotExist}
		case "codex":
			return "/usr/bin/codex", nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCodex)
	}
}

func TestResolveAgent_AutoSkipsRovoDevWithoutSubcommand(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	originalProbe := probeRovoDevSupport
	probeRovoDevSupport = func(_ context.Context, bin string) (bool, error) {
		if bin != "/usr/bin/acli" {
			t.Fatalf("unexpected rovodev probe for %q", bin)
		}
		return false, nil
	}
	t.Cleanup(func() {
		probeRovoDevSupport = originalProbe
	})

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "claude", "codex", "opencode", "pi", "copilot", "cursor-agent", "acpx":
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		case "acli":
			return "/usr/bin/acli", nil
		default:
			t.Fatalf("unexpected probe for %q", bin)
			return "", nil
		}
	})

	if err == nil {
		t.Fatal("expected error when rovodev subcommand is unavailable")
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}

func TestResolveAgent_AutoReturnsRovoDevProbeExitError(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	script := filepath.Join(t.TempDir(), "acli")
	contents := []byte("#!/bin/sh\nexit 1\n")
	if runtime.GOOS == "windows" {
		script += ".cmd"
		contents = []byte("@echo off\r\nexit /b 1\r\n")
	}
	if err := os.WriteFile(script, contents, 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "claude", "codex", "opencode", "pi":
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		case "acli":
			return script, nil
		default:
			t.Fatalf("unexpected probe for %q", bin)
			return "", nil
		}
	})

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected exit error, got %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}

func TestResolveAgent_AutoReturnsOverrideProbeError(t *testing.T) {
	cfg := &Config{
		Agent:             types.AgentAuto,
		AgentPathOverride: map[string]string{"claude": "/custom/claude"},
	}
	wantErr := &exec.Error{Name: "/custom/claude", Err: fs.ErrPermission}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "/custom/claude" {
			return "", wantErr
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})

	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected permission error, got %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}

func TestResolveAgent_AutoNoneAvailable(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected error when no agents found")
	}
	if !strings.Contains(err.Error(), "no runnable agent found") {
		t.Errorf("expected 'no runnable agent found' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "config") {
		t.Errorf("expected config guidance in error, got: %v", err)
	}
}

func TestResolveAgent_AutoNoneAvailableIncludesOverridePaths(t *testing.T) {
	cfg := &Config{
		Agent: types.AgentAuto,
		AgentPathOverride: map[string]string{
			"claude":   "/custom/claude",
			"rovodev":  "/custom/acli",
			"opencode": "/custom/opencode",
		},
	}

	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})

	if err == nil {
		t.Fatal("expected error when no agents found")
	}
	for _, want := range []string{"/custom/claude", "/custom/opencode", "/custom/acli"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got: %v", want, err)
		}
	}
}

func TestResolveAgent_ExplicitCursorACPRequiresAcpx(t *testing.T) {
	cfg := &Config{Agent: "acp:cursor"}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "cursor-agent" {
			return "/usr/bin/cursor-agent", nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	})
	if err == nil {
		t.Fatal("expected error when acpx is missing")
	}
	if !strings.Contains(err.Error(), "no runnable agent found") {
		t.Errorf("expected 'no runnable agent found', got: %v", err)
	}
	if !strings.Contains(err.Error(), "acpx") {
		t.Errorf("expected error to list the missing acpx binary, got: %v", err)
	}
}

func TestResolveAgent_AutoPicksNativeCursorWhenBothPathsArePresent(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "cursor-agent":
			return "/usr/bin/cursor-agent", nil
		case "acpx":
			return "/usr/bin/acpx", nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCursor {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCursor)
	}
}

func TestResolveAgent_ListSkipsRegisteredACPFallbackMissingCommandBinary(t *testing.T) {
	cfg := &Config{Agents: []types.AgentName{types.AgentCursor, types.AgentClaude}}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "acpx", "claude":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentClaude {
		t.Fatalf("agents = %v, want [claude]", cfg.Agents)
	}
}

func TestResolveAgent_ListPicksRegisteredACPFallbackWhenBinariesPresent(t *testing.T) {
	cfg := &Config{Agents: []types.AgentName{types.AgentCursor}}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "acpx", "cursor-agent":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentCursor {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentCursor)
	}
}

func TestResolveAgent_ListSkipsACPTargetMissingCommandBinary(t *testing.T) {
	// acp:cursor runs the registered Cursor ACP raw command, so list
	// resolution must skip it when cursor-agent is missing even though acpx
	// is present.
	cfg := &Config{Agents: []types.AgentName{"acp:cursor", types.AgentClaude}}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "acpx", "claude":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
	if len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentClaude {
		t.Fatalf("agents = %v, want [claude]", cfg.Agents)
	}
}

func TestResolveAgent_ListKeepsACPTargetWhenBinariesPresent(t *testing.T) {
	cfg := &Config{Agents: []types.AgentName{"acp:cursor", types.AgentClaude}}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "acpx", "cursor-agent", "claude":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "acp:cursor" {
		t.Errorf("agent = %q, want acp:cursor", cfg.Agent)
	}
}

func TestResolveAgent_ACPTargetRegistryOverrideBinaryProbed(t *testing.T) {
	// A registry override makes acp:gemini run `acpx --agent "gemini-cli acp"`,
	// so the override's binary must be probed alongside acpx.
	cfg := &Config{
		Agents:               []types.AgentName{"acp:gemini", types.AgentClaude},
		ACPRegistryOverrides: map[string]string{"gemini": "gemini-cli acp"},
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "acpx", "claude":
			return "/usr/bin/" + bin, nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
}

func TestResolveAgent_CursorACPRegistryOverrideBinaryProbed(t *testing.T) {
	cfg := &Config{
		Agents:               []types.AgentName{"acp:cursor"},
		ACPRegistryOverrides: map[string]string{"cursor": "/opt/cursor/cursor-agent acp --profile work"},
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		switch bin {
		case "acpx", "/opt/cursor/cursor-agent":
			return bin, nil
		case "cursor-agent":
			t.Fatalf("must probe the override binary, not the default cursor-agent")
			return "", nil
		default:
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Agent != "acp:cursor" {
		t.Errorf("agent = %q, want acp:cursor", cfg.Agent)
	}
}

func TestResolveAgent_CursorACPCommandAvailability(t *testing.T) {
	tests := []struct {
		name       string
		override   string
		wantErr    bool
		wantProbes string
	}{
		{name: "default requires command binary", wantErr: true, wantProbes: "cursor-agent"},
		{name: "relative override skips command probe", override: "./bin/local-agent acp", wantProbes: "acpx"},
		{name: "quoted override skips command probe", override: `"/opt/Cursor Agent/cursor-agent" acp`, wantProbes: "acpx"},
		{name: "escaped absolute override skips command probe", override: `/opt/Cursor\ Agent/cursor-agent acp`, wantProbes: "acpx"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Agent: "acp:cursor"}
			if tt.override != "" {
				cfg.ACPRegistryOverrides = map[string]string{"cursor": tt.override}
			}
			var probes []string
			err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
				probes = append(probes, bin)
				if bin == "acpx" {
					return "/usr/bin/acpx", nil
				}
				return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
			})
			if tt.wantErr && err == nil {
				t.Fatal("expected unavailable agent to fail resolution")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := strings.Join(probes, ", "); got != tt.wantProbes {
				t.Errorf("probes = %q, want %q", got, tt.wantProbes)
			}
		})
	}
}

func TestACPCommandBinaryForProbeForOS(t *testing.T) {
	tests := []struct {
		name    string
		goos    string
		command string
		wantBin string
		wantOK  bool
	}{
		{name: "windows drive path", goos: "windows", command: `C:\tools\cursor-agent.exe acp`, wantBin: `C:\tools\cursor-agent.exe`, wantOK: true},
		{name: "windows UNC path", goos: "windows", command: `\\host\share\cursor-agent.exe acp`, wantBin: `\\host\share\cursor-agent.exe`, wantOK: true},
		{name: "unix absolute path", goos: "linux", command: "/opt/cursor-agent acp", wantBin: "/opt/cursor-agent", wantOK: true},
		{name: "unix escaped space", goos: "linux", command: `/opt/Cursor\ Agent/cursor-agent acp`},
		{name: "windows double-quoted path", goos: "windows", command: `"C:\Program Files\cursor-agent.exe" acp`},
		{name: "windows single-quoted path", goos: "windows", command: `'C:\Program Files\cursor-agent.exe' acp`},
		{name: "unix double-quoted path", goos: "linux", command: `"/opt/Cursor Agent/cursor-agent" acp`},
		{name: "unix single-quoted path", goos: "linux", command: `'/opt/Cursor Agent/cursor-agent' acp`},
		{name: "relative path", goos: "linux", command: "./bin/x acp"},
		{name: "bare command", goos: "linux", command: "cursor-agent acp", wantBin: "cursor-agent", wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBin, gotOK := acpCommandBinaryForProbeForOS(tt.command, tt.goos)
			if gotBin != tt.wantBin || gotOK != tt.wantOK {
				t.Errorf("acpCommandBinaryForProbeForOS(%q, %q) = (%q, %t), want (%q, %t)", tt.command, tt.goos, gotBin, gotOK, tt.wantBin, tt.wantOK)
			}
		})
	}
}

func TestResolveAgent_AutoPassesContextToRovoDevProbe(t *testing.T) {
	cfg := &Config{Agent: types.AgentAuto}
	originalProbe := probeRovoDevSupport
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probeRovoDevSupport = func(ctx context.Context, bin string) (bool, error) {
		if bin != "/usr/bin/acli" {
			t.Fatalf("unexpected rovodev probe for %q", bin)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("probe context error = %v, want %v", ctx.Err(), context.Canceled)
		}
		return false, ctx.Err()
	}
	t.Cleanup(func() {
		probeRovoDevSupport = originalProbe
	})

	err := cfg.ResolveAgent(ctx, func(bin string) (string, error) {
		switch bin {
		case "claude", "codex", "opencode", "pi":
			return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
		case "acli":
			return "/usr/bin/acli", nil
		default:
			t.Fatalf("unexpected probe for %q", bin)
			return "", nil
		}
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
	if cfg.Agent != types.AgentAuto {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentAuto)
	}
}
