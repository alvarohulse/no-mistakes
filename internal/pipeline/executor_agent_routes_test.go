package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type routedTestAgent struct {
	name  string
	calls int
}

func (a *routedTestAgent) Name() string { return a.name }

func (a *routedTestAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	a.calls++
	return &agent.Result{}, nil
}

func (a *routedTestAgent) Close() error { return nil }

func TestExecutor_RoutesEachStepAndAttributesActualAgent(t *testing.T) {
	database, p, run, repo := setupTest(t)
	defaultAgent := &routedTestAgent{name: "claude"}
	reviewAgent := &routedTestAgent{name: "codex"}
	testAgent := &routedTestAgent{name: "pi"}
	steps := []Step{
		&adaptiveCallStep{name: types.StepReview, fn: runRoutedAgent},
		&adaptiveCallStep{name: types.StepTest, fn: runRoutedAgent},
	}
	routes := AgentRoutes{
		Default: defaultAgent,
		ByStep: map[types.StepName]agent.Agent{
			types.StepReview: reviewAgent,
			types.StepTest:   testAgent,
		},
	}
	exec := NewExecutorWithAgentRoutes(database, p, &config.Config{}, routes, steps, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if defaultAgent.calls != 0 || reviewAgent.calls != 1 || testAgent.calls != 1 {
		t.Fatalf("calls default/review/test = %d/%d/%d", defaultAgent.calls, reviewAgent.calls, testAgent.calls)
	}

	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("GetAgentInvocationsByRun() error = %v", err)
	}
	if len(invocations) != 2 {
		t.Fatalf("invocations = %d, want 2", len(invocations))
	}
	if invocations[0].StepName != string(types.StepReview) || invocations[0].Agent != "codex" {
		t.Fatalf("review invocation = %+v", invocations[0])
	}
	if invocations[1].StepName != string(types.StepTest) || invocations[1].Agent != "pi" {
		t.Fatalf("test invocation = %+v", invocations[1])
	}
}

func TestExecutor_ReviewRouteOwnsDurableSessionReuse(t *testing.T) {
	database, p, run, repo := setupTest(t)
	defaultAgent := &routedTestAgent{name: "claude"}
	reviewAgent := newFakeSessionAgent()
	reviewAgent.name = "codex"
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.RunAgentSession(SessionRoleReviewer, agent.RunOpts{Purpose: "review"}); err != nil {
				return nil, err
			}
			if _, err := sctx.RunAgentSession(SessionRoleReviewer, agent.RunOpts{Purpose: "review"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutorWithAgentRoutes(
		database,
		p,
		&config.Config{SessionReuse: true},
		AgentRoutes{Default: defaultAgent, ByStep: map[types.StepName]agent.Agent{types.StepReview: reviewAgent}},
		[]Step{step},
		nil,
	)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if defaultAgent.calls != 0 {
		t.Fatalf("run-wide fallback calls = %d, want 0", defaultAgent.calls)
	}
	if len(reviewAgent.calls) != 2 {
		t.Fatalf("review calls = %d, want 2", len(reviewAgent.calls))
	}
	if got := reviewAgent.calls[1].session; got == nil || got.ID != "sess-1" || got.Agent != "codex" {
		t.Fatalf("second review session = %+v, want resumed codex sess-1", got)
	}
}

func TestExecutor_ReviewAdversaryIsInstrumentedAndSessionIsolated(t *testing.T) {
	database, p, run, repo := setupTest(t)
	primary := newFakeSessionAgent()
	primary.name = "codex"
	adversary := &routedTestAgent{name: "claude"}
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.RunAgentSession(SessionRoleReviewer, agent.RunOpts{Purpose: "review"}); err != nil {
				return nil, err
			}
			if sctx.ReviewAdversary == nil {
				t.Fatal("review adversary route was not attached to the step context")
			}
			if _, err := sctx.ReviewAdversary.Run(sctx.Ctx, agent.RunOpts{Purpose: "review-adversary"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutorWithAgentRoutes(
		database,
		p,
		&config.Config{SessionReuse: true},
		AgentRoutes{Default: primary, ByStep: map[types.StepName]agent.Agent{types.StepReview: primary}, ReviewAdversary: adversary},
		[]Step{step},
		nil,
	)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(primary.calls) != 1 || primary.calls[0].session == nil {
		t.Fatalf("primary session calls = %+v, want one managed reviewer session", primary.calls)
	}
	if adversary.calls != 1 {
		t.Fatalf("adversary calls = %d, want 1", adversary.calls)
	}
	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 2 {
		t.Fatalf("invocations = %+v, want primary and adversary", invocations)
	}
	if invocations[0].Agent != "codex" || invocations[0].Purpose != "review" {
		t.Fatalf("primary invocation = %+v", invocations[0])
	}
	if invocations[1].Agent != "claude" || invocations[1].Purpose != "review-adversary" || invocations[1].SessionMode != db.InvocationModeCold {
		t.Fatalf("adversary invocation = %+v", invocations[1])
	}
}

func runRoutedAgent(sctx *StepContext) (*StepOutcome, error) {
	if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{}); err != nil {
		return nil, err
	}
	return &StepOutcome{}, nil
}
