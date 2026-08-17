package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const MaxRepairAttempts = 3

const (
	RepairResultAttempted       = "attempted"
	RepairResultResolved        = "resolved"
	RepairResultNoProgress      = "stopped_no_progress"
	RepairResultRepeatedFailure = "stopped_repeated_failure"
	RepairResultAttemptLimit    = "stopped_attempt_limit"
)

// RepairAudit is the content-free durable receipt for one repair decision.
// FailureFingerprint is a one-way hash; Result is a bounded enum above.
type RepairAudit struct {
	FailureFingerprint string
	Result             string
}

type RepairDecision struct {
	Attempt       bool
	AttemptNumber int
	Limit         int
	Message       string
	Audit         RepairAudit
}

// RepairProgress owns the retry budget and progress comparison for one step.
// The raw failure and worktree state are retained only in memory as hashes.
type RepairProgress struct {
	attempts                    int
	previousFailureFingerprint  string
	previousWorktreeFingerprint string
	audit                       RepairAudit
}

func NewRepairProgress(attempts int) *RepairProgress {
	return &RepairProgress{attempts: min(max(attempts, 0), MaxRepairAttempts)}
}

// RepairAttemptLimit clamps a configured automatic-repair budget to the hard
// product ceiling. A non-positive value keeps automatic repair disabled.
func RepairAttemptLimit(configured int) int {
	if configured <= 0 {
		return 0
	}
	return min(configured, MaxRepairAttempts)
}

func progressAwareRepairStep(step types.StepName) bool {
	switch step {
	case types.StepReview, types.StepBuild, types.StepTest, types.StepDocument, types.StepLint, types.StepCI:
		return true
	default:
		return false
	}
}

// Observe compares one substantive failure with the previous repair attempt
// without authorizing another attempt. This lets callers audit a surviving
// failure even when its action was rerouted away from automatic repair.
func (p *RepairProgress) Observe(ctx context.Context, workDir, rawFailure string) (RepairDecision, error) {
	decision, _, err := p.observe(ctx, workDir, rawFailure)
	return decision, err
}

func (p *RepairProgress) observe(ctx context.Context, workDir, rawFailure string) (RepairDecision, string, error) {
	if p == nil {
		return RepairDecision{}, "", fmt.Errorf("repair progress is unavailable")
	}
	failureFingerprint := repairFailureFingerprint(rawFailure)
	worktreeFingerprint, err := repairWorktreeFingerprint(ctx, workDir)
	if err != nil {
		return RepairDecision{}, "", err
	}
	decision := RepairDecision{Audit: RepairAudit{FailureFingerprint: failureFingerprint}}
	stop := func(result, message string) (RepairDecision, string, error) {
		decision.Audit.Result = result
		decision.Message = message
		p.audit = decision.Audit
		return decision, worktreeFingerprint, nil
	}
	if p.attempts > 0 && p.previousWorktreeFingerprint != "" && worktreeFingerprint == p.previousWorktreeFingerprint {
		return stop(RepairResultNoProgress, "repair stopped: worktree and HEAD made no content progress")
	}
	if p.attempts > 0 && p.previousFailureFingerprint != "" && failureFingerprint == p.previousFailureFingerprint {
		return stop(RepairResultRepeatedFailure, "repair stopped: normalized failure fingerprint repeated")
	}
	return decision, worktreeFingerprint, nil
}

// Next decides whether another repair may start. After the first attempt it
// requires both a changed Git content state and a new normalized failure.
func (p *RepairProgress) Next(ctx context.Context, workDir, rawFailure string, configuredLimit int) (RepairDecision, error) {
	limit := RepairAttemptLimit(configuredLimit)
	decision, worktreeFingerprint, err := p.observe(ctx, workDir, rawFailure)
	if err != nil {
		return RepairDecision{}, err
	}
	decision.Limit = limit
	if decision.Audit.Result != "" {
		return decision, nil
	}
	stop := func(result, message string) (RepairDecision, error) {
		decision.Audit.Result = result
		decision.Message = message
		p.audit = decision.Audit
		return decision, nil
	}
	if p.attempts >= limit {
		return stop(RepairResultAttemptLimit, fmt.Sprintf("repair stopped: maximum %d attempts reached", limit))
	}

	p.attempts++
	p.previousFailureFingerprint = decision.Audit.FailureFingerprint
	p.previousWorktreeFingerprint = worktreeFingerprint
	p.audit = RepairAudit{FailureFingerprint: decision.Audit.FailureFingerprint, Result: RepairResultAttempted}
	decision.Attempt = true
	decision.AttemptNumber = p.attempts
	decision.Audit = p.audit
	return decision, nil
}

// Resolved records that the latest attempted repair cleared its target.
func (p *RepairProgress) Resolved() RepairAudit {
	if p == nil || p.attempts == 0 {
		return RepairAudit{}
	}
	p.audit = RepairAudit{FailureFingerprint: p.previousFailureFingerprint, Result: RepairResultResolved}
	return p.audit
}

func (p *RepairProgress) Audit() RepairAudit {
	if p == nil {
		return RepairAudit{}
	}
	return p.audit
}

func repairFailureFingerprint(raw string) string {
	normalized := normalizeRepairFailure(raw)
	sum := sha256.Sum256([]byte(normalized))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type repairFindingIdentity struct {
	Severity    string `json:"severity"`
	File        string `json:"file,omitempty"`
	Description string `json:"description"`
	ReviewScope string `json:"review_scope,omitempty"`
}

func normalizeRepairFailure(raw string) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil || len(findings.Items) == 0 {
		return strings.Join(strings.Fields(raw), " ")
	}
	identities := make([]string, 0, len(findings.Items))
	for _, finding := range findings.Items {
		identity := repairFindingIdentity{
			Severity:    strings.ToLower(strings.Join(strings.Fields(finding.Severity), " ")),
			File:        filepath.ToSlash(strings.TrimSpace(finding.File)),
			Description: strings.Join(strings.Fields(finding.Description), " "),
			ReviewScope: strings.ToLower(strings.Join(strings.Fields(finding.ReviewScope), " ")),
		}
		encoded, _ := json.Marshal(identity)
		identities = append(identities, string(encoded))
	}
	sort.Strings(identities)
	return strings.Join(identities, "\n")
}

// repairWorktreeFingerprint hashes Git-observable content, never mtimes. HEAD
// and tree identity cover commits/rebases; porcelain status and the tracked
// binary diff cover index/worktree state; untracked file bytes are streamed in.
func repairWorktreeFingerprint(ctx context.Context, workDir string) (string, error) {
	h := sha256.New()
	for _, args := range [][]string{
		{"rev-parse", "--verify", "HEAD"},
		{"rev-parse", "--verify", "HEAD^{tree}"},
		{"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=none"},
		{"diff", "--binary", "--no-ext-diff", "HEAD", "--"},
	} {
		output, err := gitutil.RunRaw(ctx, workDir, args...)
		if err != nil {
			return "", fmt.Errorf("fingerprint repair worktree with git %s: %w", strings.Join(args, " "), err)
		}
		writeFingerprintField(h, output)
	}

	untracked, err := gitutil.RunRaw(ctx, workDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", fmt.Errorf("list untracked repair files: %w", err)
	}
	for _, pathBytes := range bytes.Split(untracked, []byte{0}) {
		if len(pathBytes) == 0 {
			continue
		}
		path := string(pathBytes)
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("fingerprint repair worktree: unsafe untracked path %q", path)
		}
		writeFingerprintField(h, pathBytes)
		if err := hashRepairFile(h, filepath.Join(workDir, filepath.FromSlash(path))); err != nil {
			return "", fmt.Errorf("fingerprint untracked repair file %q: %w", path, err)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func writeFingerprintField(h hash.Hash, value []byte) {
	_, _ = fmt.Fprintf(h, "%d:", len(value))
	_, _ = h.Write(value)
}

func hashRepairFile(h hash.Hash, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	writeFingerprintField(h, []byte(info.Mode().String()))
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		writeFingerprintField(h, []byte(target))
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("unsupported file mode %s", info.Mode())
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	contentHash := sha256.New()
	if _, err := io.Copy(contentHash, file); err != nil {
		return err
	}
	writeFingerprintField(h, contentHash.Sum(nil))
	return nil
}
