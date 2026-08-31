package eval

import (
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	matchExactID         = "exact-id"
	matchExactText       = "exact-text"
	matchLocation        = "location"
	matchContainment     = "containment"
	locationLineBand     = 3
	locationJaccardMin   = 0.5
	containmentMinTokens = 8
)

// Score is one candidate's finding-level confusion matrix against gold.
// Pending is unmatched candidate findings: queued, never punished as FP.
type Score struct {
	TruePositive      int
	TruePositiveExact int
	TruePositiveFuzzy int
	FalseNegative     int
	FalsePositive     int
	FalsePositiveGold int
	Pending           int
}

// ScoreCandidate matches a candidate finding list against recorded gold.
//
//   - TP: the candidate raises the same underlying issue as a true-issue gold
//     (human-accepted Fix, auto-fix that landed in a merged PR, a
//     human-added miss, or a confirmed post-PR miss)
//   - FN: the candidate misses a true-issue gold
//   - FP: only an explicit false-positive gold that the candidate still raised
//   - Pending: unmatched candidate findings, never inferred as invalid
//
// Matching is a documented cascade of strengths: exact-id, exact-text,
// nearby-line Jaccard, then gated containment. Assignment is globally optimal
// across all strengths, so neither input order nor a tier boundary can consume
// a candidate another gold needed. Headline recall uses the full cascade;
// exact vs fuzzy counts remain visible.
func ScoreCandidate(labels Labels, findingsJSON string) Score {
	candidate := parseFindingItems(findingsJSON)
	assigned := assignMatches(labels.Findings, candidate)
	used := make([]bool, len(candidate))
	var score Score
	for i, gold := range labels.Findings {
		if gold.Kind == GoldFalsePositive {
			score.FalsePositiveGold++
		}
		match := assigned[i]
		switch {
		case isTrueIssueGold(gold.Kind) && match.cand >= 0:
			score.TruePositive++
			if match.strength == matchExactID || match.strength == matchExactText {
				score.TruePositiveExact++
			} else {
				score.TruePositiveFuzzy++
			}
			used[match.cand] = true
		case isTrueIssueGold(gold.Kind):
			score.FalseNegative++
		case gold.Kind == GoldFalsePositive && match.cand >= 0:
			score.FalsePositive++
			used[match.cand] = true
		}
	}
	for i := range candidate {
		if !used[i] {
			score.Pending++
		}
	}
	return score
}

func parseFindingItems(raw string) []types.Finding {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return nil
	}
	return findings.Items
}

type assignedMatch struct {
	cand     int
	strength string
}

func assignMatches(golds []FindingGold, candidate []types.Finding) []assignedMatch {
	out := make([]assignedMatch, len(golds))
	for i := range out {
		out[i].cand = -1
	}
	if len(golds) == 0 || len(candidate) == 0 {
		return out
	}

	base := int64(len(golds)+len(candidate)) + 1
	weights := make([][]int64, len(golds))
	strengths := make([][]string, len(golds))
	for goldIndex, gold := range golds {
		weights[goldIndex] = make([]int64, len(candidate))
		strengths[goldIndex] = make([]string, len(candidate))
		for candidateIndex, finding := range candidate {
			for _, strength := range []string{matchExactID, matchExactText, matchLocation, matchContainment} {
				if matchAt(gold, finding, strength) {
					weights[goldIndex][candidateIndex] = matchWeight(strength, base)
					strengths[goldIndex][candidateIndex] = strength
					break
				}
			}
		}
	}
	for goldIndex, candidateIndex := range maxWeightAssignment(weights) {
		if candidateIndex < 0 || weights[goldIndex][candidateIndex] == 0 {
			continue
		}
		out[goldIndex] = assignedMatch{cand: candidateIndex, strength: strengths[goldIndex][candidateIndex]}
	}
	return out
}

// matchWeight makes the optimum lexicographic by strength. base is larger than
// the maximum number of pairs, so no combination of weaker matches can replace
// one stronger match.
func matchWeight(strength string, base int64) int64 {
	switch strength {
	case matchExactID, matchExactText:
		return base * base
	case matchLocation:
		return base
	case matchContainment:
		return 1
	default:
		return 0
	}
}

// maxWeightAssignment returns the candidate column assigned to each gold row,
// maximizing total weight. It transposes rectangular inputs because the
// Hungarian solver requires no more rows than columns.
func maxWeightAssignment(weights [][]int64) []int {
	rows, columns := len(weights), len(weights[0])
	if rows <= columns {
		return hungarianMinCost(negateWeights(weights))
	}

	transposed := make([][]int64, columns)
	for column := range transposed {
		transposed[column] = make([]int64, rows)
		for row := range weights {
			transposed[column][row] = -weights[row][column]
		}
	}
	candidateToGold := hungarianMinCost(transposed)
	goldToCandidate := make([]int, rows)
	for row := range goldToCandidate {
		goldToCandidate[row] = -1
	}
	for candidate, gold := range candidateToGold {
		if gold >= 0 {
			goldToCandidate[gold] = candidate
		}
	}
	return goldToCandidate
}

func negateWeights(weights [][]int64) [][]int64 {
	out := make([][]int64, len(weights))
	for row, values := range weights {
		out[row] = make([]int64, len(values))
		for column, weight := range values {
			out[row][column] = -weight
		}
	}
	return out
}

// hungarianMinCost solves a rectangular minimum-cost assignment in O(n^2*m).
// It requires len(cost) <= len(cost[0]).
func hungarianMinCost(cost [][]int64) []int {
	rows := len(cost)
	if rows == 0 {
		return nil
	}
	columns := len(cost[0])
	const infinity = int64(1) << 62
	u := make([]int64, rows+1)
	v := make([]int64, columns+1)
	columnToRow := make([]int, columns+1)
	path := make([]int, columns+1)
	for row := 1; row <= rows; row++ {
		columnToRow[0] = row
		column := 0
		minimum := make([]int64, columns+1)
		used := make([]bool, columns+1)
		for i := range minimum {
			minimum[i] = infinity
		}
		for {
			used[column] = true
			currentRow := columnToRow[column]
			delta := infinity
			nextColumn := 0
			for candidateColumn := 1; candidateColumn <= columns; candidateColumn++ {
				if used[candidateColumn] {
					continue
				}
				current := cost[currentRow-1][candidateColumn-1] - u[currentRow] - v[candidateColumn]
				if current < minimum[candidateColumn] {
					minimum[candidateColumn] = current
					path[candidateColumn] = column
				}
				if minimum[candidateColumn] < delta {
					delta = minimum[candidateColumn]
					nextColumn = candidateColumn
				}
			}
			for candidateColumn := 0; candidateColumn <= columns; candidateColumn++ {
				if used[candidateColumn] {
					u[columnToRow[candidateColumn]] += delta
					v[candidateColumn] -= delta
				} else {
					minimum[candidateColumn] -= delta
				}
			}
			column = nextColumn
			if columnToRow[column] == 0 {
				break
			}
		}
		for column != 0 {
			previousColumn := path[column]
			columnToRow[column] = columnToRow[previousColumn]
			column = previousColumn
		}
	}
	rowToColumn := make([]int, rows)
	for row := range rowToColumn {
		rowToColumn[row] = -1
	}
	for column := 1; column <= columns; column++ {
		if columnToRow[column] > 0 {
			rowToColumn[columnToRow[column]-1] = column - 1
		}
	}
	return rowToColumn
}

func matchAt(gold FindingGold, finding types.Finding, strength string) bool {
	switch strength {
	case matchExactID:
		return gold.ID != "" && gold.ID == finding.ID
	case matchExactText:
		return exactTextMatch(gold, finding)
	case matchLocation:
		return locationMatch(gold, finding)
	case matchContainment:
		return containmentMatch(gold, finding)
	default:
		return false
	}
}

func exactTextMatch(gold FindingGold, finding types.Finding) bool {
	goldFile, goldDesc := normalizeIssue(gold.File, gold.Description)
	candFile, candDesc := normalizeIssue(finding.File, finding.Description)
	if goldFile == "" || candFile == "" || goldDesc == "" || candDesc == "" {
		return false
	}
	return goldFile == candFile && goldDesc == candDesc
}

func locationMatch(gold FindingGold, finding types.Finding) bool {
	goldFile, goldDesc := normalizeIssue(gold.File, gold.Description)
	candFile, candDesc := normalizeIssue(finding.File, finding.Description)
	if goldFile == "" || candFile == "" || goldDesc == "" || candDesc == "" {
		return false
	}
	if goldFile != candFile || gold.Line <= 0 || finding.Line <= 0 {
		return false
	}
	if absInt(gold.Line-finding.Line) > locationLineBand {
		return false
	}
	return tokenJaccard(goldDesc, candDesc) >= locationJaccardMin
}

func containmentMatch(gold FindingGold, finding types.Finding) bool {
	goldFile, goldDesc := normalizeIssue(gold.File, gold.Description)
	candFile, candDesc := normalizeIssue(finding.File, finding.Description)
	if goldFile == "" || candFile == "" || goldDesc == "" || candDesc == "" || goldFile != candFile {
		return false
	}
	if goldDesc == candDesc {
		return false
	}
	shorter, longer := goldDesc, candDesc
	if len(candDesc) < len(goldDesc) {
		shorter, longer = candDesc, goldDesc
	}
	if !strings.Contains(longer, shorter) {
		return false
	}
	return len(strings.Fields(shorter)) >= containmentMinTokens
}

func tokenJaccard(a, b string) float64 {
	left := uniqueTokens(a)
	right := uniqueTokens(b)
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	inter := 0
	for tok := range left {
		if right[tok] {
			inter++
		}
	}
	union := len(left) + len(right) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func uniqueTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.Fields(s) {
		out[tok] = true
	}
	return out
}

func normalizeIssue(file, description string) (string, string) {
	file = filepath.ToSlash(strings.TrimSpace(file))
	description = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(description))), " ")
	return file, description
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
