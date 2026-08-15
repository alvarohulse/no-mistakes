package pipeline

import (
	"context"
	cryptorand "crypto/rand"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

type reviewCandidatePicker func(candidateCount int) (int, error)

type reviewPoolAgent struct {
	candidates []agent.Agent
	pick       reviewCandidatePicker
}

func newReviewPoolAgent(candidates []agent.Agent, pick reviewCandidatePicker) agent.Agent {
	if len(candidates) == 0 {
		return nil
	}
	return &reviewPoolAgent{candidates: append([]agent.Agent(nil), candidates...), pick: pick}
}

func (a *reviewPoolAgent) Name() string { return "review-pool" }

func (a *reviewPoolAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if len(a.candidates) == 0 {
		return nil, fmt.Errorf("review candidate pool is empty")
	}
	index := 0
	if len(a.candidates) > 1 {
		var err error
		index, err = a.pick(len(a.candidates))
		if err != nil {
			return nil, fmt.Errorf("select review candidate: %w", err)
		}
	}
	if index < 0 || index >= len(a.candidates) {
		return nil, fmt.Errorf("review candidate selector returned invalid index %d for pool of %d", index, len(a.candidates))
	}
	return a.candidates[index].Run(ctx, opts)
}

// Close is a no-op because pipelineAgents owns and closes each concrete
// candidate exactly once.
func (a *reviewPoolAgent) Close() error { return nil }

func secureReviewCandidatePicker(candidateCount int) (int, error) {
	if candidateCount <= 0 {
		return 0, fmt.Errorf("candidate count must be positive")
	}
	selected, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(candidateCount)))
	if err != nil {
		return 0, err
	}
	return int(selected.Int64()), nil
}

func reviewCandidateReceipts(candidates []config.ReviewCandidate) []db.ReviewCandidateReceipt {
	receipts := make([]db.ReviewCandidateReceipt, len(candidates))
	for i, candidate := range candidates {
		receipts[i] = db.ReviewCandidateReceipt{
			Agent: string(candidate.Agent), Model: candidate.Model.Name, Vendor: candidate.Model.Vendor, Optional: candidate.Optional,
		}
	}
	return receipts
}

// SetReviewCandidateSeed installs a deterministic candidate selector. It is
// intended for repeatable tests and embedded simulations; normal runs use
// crypto/rand so independent daemon processes do not share a sequence.
func (e *Executor) SetReviewCandidateSeed(seed int64) {
	if e == nil {
		return
	}
	random := mathrand.New(mathrand.NewSource(seed))
	var mu sync.Mutex
	e.reviewCandidatePicker = func(candidateCount int) (int, error) {
		if candidateCount <= 0 {
			return 0, fmt.Errorf("candidate count must be positive")
		}
		mu.Lock()
		defer mu.Unlock()
		return random.Intn(candidateCount), nil
	}
}
