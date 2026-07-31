package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResolvedAgentRoutingSnapshotRestoresConcreteLaunchIdentity(t *testing.T) {
	launch := resolvedRoutingTestConfig()
	encoded, err := marshalResolvedAgentRouting(launch, false)
	if err != nil {
		t.Fatal(err)
	}

	changed := &config.Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepReview: {types.AgentClaude},
		},
		StepModels: map[types.StepName]config.ModelRoute{
			types.StepReview: {Name: "claude-opus-6", Vendor: "anthropic"},
		},
	}
	legacy, err := restoreResolvedAgentRouting(changed, &encoded, false)
	if err != nil {
		t.Fatal(err)
	}
	if legacy {
		t.Fatal("persisted routing took legacy recovery path")
	}
	if err := validateResolvedAgentRouting(changed, &encoded, false); err != nil {
		t.Fatalf("restored routing did not match snapshot: %v", err)
	}

	if !reflect.DeepEqual(changed.Agents, launch.Agents) || !reflect.DeepEqual(changed.StepAgents, launch.StepAgents) {
		t.Fatalf("restored agents = %v/%v, want %v/%v", changed.Agents, changed.StepAgents, launch.Agents, launch.StepAgents)
	}
	if !reflect.DeepEqual(changed.StepModels, launch.StepModels) || !reflect.DeepEqual(changed.ReviewAdversaryAgents, launch.ReviewAdversaryAgents) || changed.ReviewAdversaryModel != launch.ReviewAdversaryModel {
		t.Fatalf("restored model/adversary routing = models %v adversary %v/%+v", changed.StepModels, changed.ReviewAdversaryAgents, changed.ReviewAdversaryModel)
	}
}

func TestRestoreResolvedAgentRoutingDistinguishesLegacyAndInvalidNewRuns(t *testing.T) {
	cfg := &config.Config{}
	legacy, err := restoreResolvedAgentRouting(cfg, nil, false)
	if err != nil || !legacy {
		t.Fatalf("legacy restore = legacy %v error %v, want allowed legacy path", legacy, err)
	}

	for _, persisted := range []string{"", "not-json", `{"version":99}`} {
		t.Run(persisted, func(t *testing.T) {
			if _, err := restoreResolvedAgentRouting(&config.Config{}, &persisted, false); err == nil {
				t.Fatalf("restore accepted invalid persisted routing %q", persisted)
			}
		})
	}
}

func TestRestoreResolvedAgentRoutingRejectsInvalidModelIdentity(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{name: "whitespace-only name", model: `{"name":"   ","vendor":"openai"}`},
		{name: "vendor spacing", model: `{"name":"gemini-3.5-pro","vendor":"google labs"}`},
		{name: "incomplete identity", model: `{"name":"gpt-5.6-sol","vendor":""}`},
		{name: "control character", model: `{"name":"gpt-5.6\u0001-sol","vendor":"openai"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			persisted := `{"version":1,"default_agents":["pi"],"step_routes":{"test":{"agents":["pi"],"model":` + tt.model + `}}}`
			cfg := &config.Config{}

			if _, err := restoreResolvedAgentRouting(cfg, &persisted, false); err == nil {
				t.Fatalf("restore accepted invalid persisted model identity %s", tt.model)
			}
			if len(cfg.StepAgents) != 0 || len(cfg.StepModels) != 0 {
				t.Fatalf("restore mutated config before rejecting corrupt model identity: agents=%v models=%v", cfg.StepAgents, cfg.StepModels)
			}
		})
	}
}

func TestRestoreResolvedAgentRoutingAcceptsParameterizedModelIdentity(t *testing.T) {
	persisted := `{"version":1,"default_agents":["pi"],"step_routes":{"test":{"agents":["pi"],"model":{"name":"openai/gpt-5.6?reasoning_effort=high","vendor":"openai-compatible-v2"}}}}`
	cfg := &config.Config{}

	if _, err := restoreResolvedAgentRouting(cfg, &persisted, false); err != nil {
		t.Fatalf("restore rejected valid parameterized model identity: %v", err)
	}
	want := config.ModelRoute{Name: "openai/gpt-5.6?reasoning_effort=high", Vendor: "openai-compatible-v2"}
	if got := cfg.StepModels[types.StepTest]; got != want {
		t.Fatalf("restored model = %+v, want %+v", got, want)
	}
}

func TestValidateResolvedAgentRoutingRejectsChangedConcreteFallback(t *testing.T) {
	cfg := resolvedRoutingTestConfig()
	encoded, err := marshalResolvedAgentRouting(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents = cfg.Agents[:1]
	if err := validateResolvedAgentRouting(cfg, &encoded, false); err == nil || !strings.Contains(err.Error(), "differs from launch") {
		t.Fatalf("validateResolvedAgentRouting() error = %v, want concrete fallback drift refusal", err)
	}
}

func TestLoadRecoveredConfigRestoresLaunchRoutesAfterGlobalAndDefaultAdvance(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "resolved-routing-recovery")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))

	launchRepoConfig := `agent: codex
review:
  agent: codex
  model: {name: gpt-5.6-sol, vendor: openai}
  adversary_agent: claude
  adversary_model: {name: claude-opus-5, vendor: anthropic}
`
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(launchRepoConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "configure launch routing")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	launchHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")

	parsedLaunch, err := config.LoadRepoFromBytes([]byte(launchRepoConfig))
	if err != nil {
		t.Fatal(err)
	}
	launchConfig := config.Merge(config.DefaultGlobalConfig(), parsedLaunch)
	if err := launchConfig.ResolveAgent(context.Background(), fakeLookPath); err != nil {
		t.Fatal(err)
	}
	snapshot, err := marshalResolvedAgentRouting(launchConfig, false)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "main", launchHead, refreshTestZeroSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunResolvedAgentRouting(run.ID, snapshot); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changer := filepath.Join(t.TempDir(), "changer")
	gitCmd(t, "", "clone", "-b", "main", p.RepoDir(repo.ID), changer)
	gitCmd(t, changer, "config", "user.email", "test@test.com")
	gitCmd(t, changer, "config", "user.name", "Test")
	advanced := `agent: claude
review:
  agent: claude
  model: {name: claude-opus-6, vendor: anthropic}
  adversary_agent: codex
  adversary_model: {name: gpt-5.7, vendor: openai}
`
	if err := os.WriteFile(filepath.Join(changer, ".no-mistakes.yaml"), []byte(advanced), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, changer, "add", ".no-mistakes.yaml")
	gitCmd(t, changer, "commit", "-m", "advance default routing")
	gitCmd(t, changer, "push", "origin", "HEAD:refs/heads/main")

	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)
	recovered, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath)
	if err != nil {
		t.Fatalf("loadRecoveredConfig() error = %v", err)
	}
	if !reflect.DeepEqual(recovered.Agents, launchConfig.Agents) || !reflect.DeepEqual(recovered.StepAgents, launchConfig.StepAgents) || !reflect.DeepEqual(recovered.StepModels, launchConfig.StepModels) {
		t.Fatalf("recovered primary routing = %v/%v/%v, want launch %v/%v/%v", recovered.Agents, recovered.StepAgents, recovered.StepModels, launchConfig.Agents, launchConfig.StepAgents, launchConfig.StepModels)
	}
	if !reflect.DeepEqual(recovered.ReviewAdversaryAgents, launchConfig.ReviewAdversaryAgents) || recovered.ReviewAdversaryModel != launchConfig.ReviewAdversaryModel {
		t.Fatalf("recovered adversary = %v/%+v, want %v/%+v", recovered.ReviewAdversaryAgents, recovered.ReviewAdversaryModel, launchConfig.ReviewAdversaryAgents, launchConfig.ReviewAdversaryModel)
	}
	if got := configSourceKinds(run.ConfigSources); got != "" {
		t.Fatalf("ordinary recovered run exposed config provenance %q", got)
	}
	if strings.Contains(snapshot, p.Root()) {
		t.Fatal("resolved routing snapshot contains a private filesystem path")
	}
}

func TestStartRunPersistsResolvedRoutingWithoutPublicConfigProvenance(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	if err := os.WriteFile(p.ConfigFile(), []byte("agent_path_override:\n  codex: /bin/true\n  claude: /bin/true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, _ := setupTestGitRepo(t, p, database, "resolved-routing-start")
	repoConfig := `agent: codex
review:
  agent: codex
  model: {name: gpt-5.6-sol, vendor: openai}
  adversary_agent: claude
  adversary_model: {name: claude-opus-5, vendor: anthropic}
`
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(repoConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "configure resolved routing")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")

	step := &captureRunConfigStep{captured: make(chan capturedRunConfig, 1)}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "resolved routing", "", "", "")
	if err != nil {
		t.Fatalf("startRun() error = %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ResolvedAgentRouting == nil || strings.TrimSpace(*run.ResolvedAgentRouting) == "" {
		t.Fatal("run did not persist resolved agent routing")
	}
	if len(run.ConfigSources) != 0 {
		t.Fatalf("ordinary run config sources = %#v, want unchanged empty public provenance", run.ConfigSources)
	}
}

func resolvedRoutingTestConfig() *config.Config {
	return &config.Config{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex, types.AgentPi},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepReview: {types.AgentCodex, types.AgentPi},
			types.StepTest:   {types.AgentPi},
		},
		StepModels: map[types.StepName]config.ModelRoute{
			types.StepReview: {Name: "gpt-5.6-sol", Vendor: "openai"},
			types.StepTest:   {Name: "google/gemini-3.5-pro", Vendor: "google"},
		},
		ReviewAdversaryAgents: []types.AgentName{types.AgentClaude},
		ReviewAdversaryModel:  config.ModelRoute{Name: "claude-opus-5", Vendor: "anthropic"},
	}
}

func persistResolvedRoutingForTest(t *testing.T, database *db.DB, run *db.Run, names ...types.AgentName) {
	t.Helper()
	if len(names) == 0 {
		names = []types.AgentName{types.AgentCodex}
	}
	cfg := &config.Config{Agent: names[0], Agents: append([]types.AgentName(nil), names...), StepAgents: map[types.StepName][]types.AgentName{}, StepModels: map[types.StepName]config.ModelRoute{}}
	snapshot, err := marshalResolvedAgentRouting(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunResolvedAgentRouting(run.ID, snapshot); err != nil {
		t.Fatal(err)
	}
	run.ResolvedAgentRouting = &snapshot
}
