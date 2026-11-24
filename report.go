package balance

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

type RNGProgress struct {
	MatchCursor int `json:"matchCursor"`
	TurnCount   int `json:"turnCount"`
}

type MatchReport struct {
	MatchIndex       int     `json:"matchIndex"`
	Attempts         int     `json:"attempts"`
	RoundsPlayed     int     `json:"roundsPlayed"`
	ActionTurns      int     `json:"actionTurns"`
	Winner           string  `json:"winner"`
	DamageLeft       []Fixed `json:"damageLeft"`
	DamageRight      []Fixed `json:"damageRight"`
	TotalLeft        Fixed   `json:"totalLeft"`
	TotalRight       Fixed   `json:"totalRight"`
	MinPossibleLeft  Fixed   `json:"minPossibleLeft"`
	MaxPossibleLeft  Fixed   `json:"maxPossibleLeft"`
	MinPossibleRight Fixed   `json:"minPossibleRight"`
	MaxPossibleRight Fixed   `json:"maxPossibleRight"`
	RevivesLeft      int     `json:"revivesLeft"`
	RevivesRight     int     `json:"revivesRight"`
	Digest           string  `json:"digest"`
}

type DamageSummary struct {
	MatchCount   int   `json:"matchCount"`
	ObservedMin  Fixed `json:"observedMin"`
	ObservedMax  Fixed `json:"observedMax"`
	ObservedMean Fixed `json:"observedMean"`
	TruthMin     Fixed `json:"truthMin"`
	TruthMax     Fixed `json:"truthMax"`
	TruthMatch   bool  `json:"truthMatch"`
}

type RoundBudgetStat struct {
	Round             int   `json:"round"`
	AddedMatches      int   `json:"addedMatches"`
	CumulativeMatches int   `json:"cumulativeMatches"`
	Wins              int   `json:"wins"`
	WinRatePPM        int64 `json:"winRatePpm"`
	CIWidthPPM        int64 `json:"ciWidthPpm"`
	ChangeSamples     int   `json:"changeSamples"`
}

type FailureInfo struct {
	Code       string `json:"code"`
	Object     string `json:"object,omitempty"`
	MatchIndex int    `json:"matchIndex,omitempty"`
	Turn       int    `json:"turn,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type ComboReport struct {
	ComboID
	State            ComboState         `json:"state"`
	RequestedMatches int                `json:"requestedMatches"`
	AllocatedMatches int                `json:"allocatedMatches"`
	CompletedMatches int                `json:"completedMatches"`
	Wins             int                `json:"wins"`
	Losses           int                `json:"losses"`
	Draws            int                `json:"draws"`
	WinRatePPM       int64              `json:"winRatePpm"`
	CI               ConfidenceInterval `json:"confidenceInterval"`
	Damage           DamageSummary      `json:"damage"`
	Reduction        *ReductionRecord   `json:"reduction,omitempty"`
	Rounds           []RoundBudgetStat  `json:"rounds"`
	Matches          []MatchReport      `json:"matches"`
	RNG              RNGProgress        `json:"rngProgress"`
	Failure          *FailureInfo       `json:"failure,omitempty"`
}

type ThresholdResult struct {
	ID     string `json:"id"`
	Metric string `json:"metric"`
	Actual int64  `json:"actual"`
	Min    int64  `json:"min"`
	Max    int64  `json:"max"`
	Passed bool   `json:"passed"`
}

type PlanReport struct {
	PlanID           string             `json:"planId"`
	Priority         int                `json:"priority"`
	State            PlanState          `json:"state"`
	Committed        bool               `json:"committed"`
	TotalCombos      int                `json:"totalCombos"`
	CommittedCombos  int                `json:"committedCombos"`
	RequestedMatches int                `json:"requestedMatches"`
	AllocatedMatches int                `json:"allocatedMatches"`
	CompletedMatches int                `json:"completedMatches"`
	Wins             int                `json:"wins"`
	Losses           int                `json:"losses"`
	Draws            int                `json:"draws"`
	WinRatePPM       int64              `json:"winRatePpm"`
	CI               ConfidenceInterval `json:"confidenceInterval"`
	Damage           DamageSummary      `json:"damage"`
	Thresholds       []ThresholdResult  `json:"thresholds"`
	Combos           []ComboReport      `json:"combos"`
}

type QueueReason struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type BudgetUsage struct {
	Slots                 int           `json:"slots"`
	MemoryBufferPerWorker int           `json:"memoryBufferPerWorker"`
	RoundCapacity         int           `json:"roundCapacity"`
	RequestedMatches      int64         `json:"requestedMatches"`
	AllocatedMatches      int64         `json:"allocatedMatches"`
	CompletedMatches      int64         `json:"completedMatches"`
	MaxSlotsUsed          int           `json:"maxSlotsUsed"`
	QueuedCount           int64         `json:"queuedCount"`
	QueueReasons          []QueueReason `json:"queueReasons"`
	MemoryBackpressure    int64         `json:"memoryBackpressure"`
	PeakBuffer            int           `json:"peakBuffer"`
	BudgetRounds          int           `json:"budgetRounds"`
}

type CheckpointStatus struct {
	Mode           RunMode `json:"mode"`
	Path           string  `json:"path,omitempty"`
	Loaded         bool    `json:"loaded"`
	Invalidated    bool    `json:"invalidated"`
	InvalidReason  string  `json:"invalidReason,omitempty"`
	CommitSequence int64   `json:"commitSequence"`
}

type Report struct {
	SchemaVersion    string           `json:"schemaVersion"`
	ProjectHash      string           `json:"projectHash"`
	AlgorithmVersion string           `json:"algorithmVersion"`
	BudgetVersion    string           `json:"budgetVersion"`
	Mode             RunMode          `json:"mode"`
	BudgetUsage      BudgetUsage      `json:"budgetUsage"`
	Checkpoint       CheckpointStatus `json:"checkpoint"`
	Progress         Progress         `json:"progress"`
	Plans            []PlanReport     `json:"plans"`
	Partial          bool             `json:"partial"`
}

type Progress struct {
	CommittedPlans  int `json:"committedPlans"`
	TotalPlans      int `json:"totalPlans"`
	CommittedCombos int `json:"committedCombos"`
	TotalCombos     int `json:"totalCombos"`
}

func buildMatchDigest(match *MatchReport) string {
	var buf []byte
	buf = appendInt64(buf, int64(match.MatchIndex))
	buf = appendInt64(buf, int64(match.RoundsPlayed))
	buf = appendInt64(buf, int64(match.ActionTurns))
	buf = append(buf, match.Winner...)
	buf = appendInt64(buf, int64(match.TotalLeft))
	buf = appendInt64(buf, int64(match.TotalRight))
	buf = appendInt64(buf, int64(match.RevivesLeft))
	buf = appendInt64(buf, int64(match.RevivesRight))
	for _, value := range match.DamageLeft {
		buf = appendInt64(buf, int64(value))
	}
	buf = append(buf, 0xff)
	for _, value := range match.DamageRight {
		buf = appendInt64(buf, int64(value))
	}
	return sha256Hex(buf)
}

func appendInt64(buf []byte, value int64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], uint64(value))
	return append(buf, tmp[:]...)
}

func summarizeDamage(matches []MatchReport) DamageSummary {
	if len(matches) == 0 {
		return DamageSummary{TruthMatch: true}
	}
	summary := DamageSummary{
		MatchCount:  len(matches),
		ObservedMin: matches[0].TotalLeft,
		ObservedMax: matches[0].TotalLeft,
		TruthMin:    matches[0].MinPossibleLeft,
		TruthMax:    matches[0].MaxPossibleLeft,
		TruthMatch:  true,
	}
	var total Fixed
	for _, match := range matches {
		if match.TotalLeft < summary.ObservedMin {
			summary.ObservedMin = match.TotalLeft
		}
		if match.TotalLeft > summary.ObservedMax {
			summary.ObservedMax = match.TotalLeft
		}
		if match.MinPossibleLeft < summary.TruthMin {
			summary.TruthMin = match.MinPossibleLeft
		}
		if match.MaxPossibleLeft > summary.TruthMax {
			summary.TruthMax = match.MaxPossibleLeft
		}
		if match.TotalLeft < match.MinPossibleLeft || match.TotalLeft > match.MaxPossibleLeft {
			summary.TruthMatch = false
		}
		total += match.TotalLeft
	}
	summary.ObservedMean = total / Fixed(len(matches))
	return summary
}

func summarizeMatches(matches []MatchReport) (wins, losses, draws int) {
	for _, match := range matches {
		switch match.Winner {
		case "left":
			wins++
		case "right":
			losses++
		default:
			draws++
		}
	}
	return wins, losses, draws
}

func ValidateCIWidths(report ComboReport) error {
	previousWidth := int64(PPMSize + 1)
	previousSamples := -1
	for _, round := range report.Rounds {
		if round.CumulativeMatches < previousSamples {
			return &AppError{
				Code:         CodeBudget,
				Object:       report.Key(),
				PlanID:       report.PlanID,
				ScenarioID:   report.ScenarioID,
				ConflictRule: "sample_monotonic",
				Field:        "rounds.samples",
				Detail:       "cumulative sample count decreased",
			}
		}
		if round.CIWidthPPM > previousWidth {
			return &AppError{
				Code:         CodeBudget,
				Object:       report.Key(),
				PlanID:       report.PlanID,
				ScenarioID:   report.ScenarioID,
				ConflictRule: "ci_width_monotonic",
				Field:        "rounds.ciWidthPpm",
				Detail:       "confidence interval width increased as samples increased",
			}
		}
		previousWidth = round.CIWidthPPM
		previousSamples = round.CumulativeMatches
	}
	return nil
}

func CompareReports(expected, actual *Report) error {
	if expected == nil || actual == nil {
		return &AppError{Code: CodeDiff, Field: "report", Detail: "nil report"}
	}
	expectedPlans := planMap(expected)
	actualPlans := planMap(actual)
	planIDs := unionKeys(expectedPlans, actualPlans)
	for _, planID := range planIDs {
		leftPlan, leftOK := expectedPlans[planID]
		rightPlan, rightOK := actualPlans[planID]
		if leftOK != rightOK {
			return diffError(planID, "", 0, "plan.presence", fmt.Sprint(leftOK), fmt.Sprint(rightOK))
		}
		leftCombos := comboMap(leftPlan)
		rightCombos := comboMap(rightPlan)
		comboKeys := unionKeys(leftCombos, rightCombos)
		for _, key := range comboKeys {
			leftCombo, okLeft := leftCombos[key]
			rightCombo, okRight := rightCombos[key]
			if okLeft != okRight {
				scenarioID := leftCombo.ScenarioID
				if !okLeft {
					scenarioID = rightCombo.ScenarioID
				}
				return diffError(planID, scenarioID, 0, "combo.presence", fmt.Sprint(okLeft), fmt.Sprint(okRight))
			}
			if err := compareCombo(leftCombo, rightCombo); err != nil {
				return err
			}
		}
	}
	return nil
}

func compareCombo(expected, actual ComboReport) error {
	checks := []struct {
		field string
		left  string
		right string
	}{
		{"state", string(expected.State), string(actual.State)},
		{"requestedMatches", fmt.Sprint(expected.RequestedMatches), fmt.Sprint(actual.RequestedMatches)},
		{"allocatedMatches", fmt.Sprint(expected.AllocatedMatches), fmt.Sprint(actual.AllocatedMatches)},
		{"completedMatches", fmt.Sprint(expected.CompletedMatches), fmt.Sprint(actual.CompletedMatches)},
		{"wins", fmt.Sprint(expected.Wins), fmt.Sprint(actual.Wins)},
		{"losses", fmt.Sprint(expected.Losses), fmt.Sprint(actual.Losses)},
		{"draws", fmt.Sprint(expected.Draws), fmt.Sprint(actual.Draws)},
		{"winRatePpm", fmt.Sprint(expected.WinRatePPM), fmt.Sprint(actual.WinRatePPM)},
		{"confidenceInterval.lowerPpm", fmt.Sprint(expected.CI.LowerPPM), fmt.Sprint(actual.CI.LowerPPM)},
		{"confidenceInterval.upperPpm", fmt.Sprint(expected.CI.UpperPPM), fmt.Sprint(actual.CI.UpperPPM)},
		{"confidenceInterval.widthPpm", fmt.Sprint(expected.CI.WidthPPM), fmt.Sprint(actual.CI.WidthPPM)},
		{"damage.observedMin", fmt.Sprint(expected.Damage.ObservedMin), fmt.Sprint(actual.Damage.ObservedMin)},
		{"damage.observedMax", fmt.Sprint(expected.Damage.ObservedMax), fmt.Sprint(actual.Damage.ObservedMax)},
		{"damage.observedMean", fmt.Sprint(expected.Damage.ObservedMean), fmt.Sprint(actual.Damage.ObservedMean)},
		{"damage.truthMin", fmt.Sprint(expected.Damage.TruthMin), fmt.Sprint(actual.Damage.TruthMin)},
		{"damage.truthMax", fmt.Sprint(expected.Damage.TruthMax), fmt.Sprint(actual.Damage.TruthMax)},
		{"damage.truthMatch", fmt.Sprint(expected.Damage.TruthMatch), fmt.Sprint(actual.Damage.TruthMatch)},
		{"rounds", fmt.Sprint(expected.Rounds), fmt.Sprint(actual.Rounds)},
	}
	for _, check := range checks {
		if check.left != check.right {
			return diffError(expected.PlanID, expected.ScenarioID, 0, check.field, check.left, check.right)
		}
	}
	leftMatches := matchMap(expected.Matches)
	rightMatches := matchMap(actual.Matches)
	indices := unionIntKeys(leftMatches, rightMatches)
	for _, index := range indices {
		left, leftOK := leftMatches[index]
		right, rightOK := rightMatches[index]
		if leftOK != rightOK {
			return diffError(expected.PlanID, expected.ScenarioID, index, "match.presence", fmt.Sprint(leftOK), fmt.Sprint(rightOK))
		}
		if left.Winner != right.Winner {
			return diffError(expected.PlanID, expected.ScenarioID, index, "winner", left.Winner, right.Winner)
		}
		if left.RoundsPlayed != right.RoundsPlayed {
			return diffError(expected.PlanID, expected.ScenarioID, index, "roundsPlayed", fmt.Sprint(left.RoundsPlayed), fmt.Sprint(right.RoundsPlayed))
		}
		if left.ActionTurns != right.ActionTurns {
			return diffError(expected.PlanID, expected.ScenarioID, index, "actionTurns", fmt.Sprint(left.ActionTurns), fmt.Sprint(right.ActionTurns))
		}
		if left.RevivesLeft != right.RevivesLeft {
			return diffError(expected.PlanID, expected.ScenarioID, index, "revivesLeft", fmt.Sprint(left.RevivesLeft), fmt.Sprint(right.RevivesLeft))
		}
		if left.RevivesRight != right.RevivesRight {
			return diffError(expected.PlanID, expected.ScenarioID, index, "revivesRight", fmt.Sprint(left.RevivesRight), fmt.Sprint(right.RevivesRight))
		}
		for turn := 0; turn < len(left.DamageLeft) || turn < len(right.DamageLeft); turn++ {
			var leftValue, rightValue Fixed
			if turn < len(left.DamageLeft) {
				leftValue = left.DamageLeft[turn]
			}
			if turn < len(right.DamageLeft) {
				rightValue = right.DamageLeft[turn]
			}
			if turn >= len(left.DamageLeft) || turn >= len(right.DamageLeft) || leftValue != rightValue {
				return diffError(expected.PlanID, expected.ScenarioID, index, fmt.Sprintf("damageLeft[%d]", turn), fmt.Sprint(leftValue), fmt.Sprint(rightValue))
			}
		}
		for turn := 0; turn < len(left.DamageRight) || turn < len(right.DamageRight); turn++ {
			var leftValue, rightValue Fixed
			if turn < len(left.DamageRight) {
				leftValue = left.DamageRight[turn]
			}
			if turn < len(right.DamageRight) {
				rightValue = right.DamageRight[turn]
			}
			if turn >= len(left.DamageRight) || turn >= len(right.DamageRight) || leftValue != rightValue {
				return diffError(expected.PlanID, expected.ScenarioID, index, fmt.Sprintf("damageRight[%d]", turn), fmt.Sprint(leftValue), fmt.Sprint(rightValue))
			}
		}
		if left.Digest != right.Digest {
			return diffError(expected.PlanID, expected.ScenarioID, index, "digest", left.Digest, right.Digest)
		}
	}
	return nil
}

func diffError(planID, scenarioID string, matchIndex int, field, expected, actual string) *AppError {
	return &AppError{
		Code:       CodeDiff,
		Object:     planID + "/" + scenarioID,
		PlanID:     planID,
		ScenarioID: scenarioID,
		MatchIndex: matchIndex,
		Field:      field,
		Expected:   expected,
		Actual:     actual,
		Detail:     "first deterministic difference",
	}
}

func planMap(report *Report) map[string]PlanReport {
	out := make(map[string]PlanReport, len(report.Plans))
	for _, plan := range report.Plans {
		out[plan.PlanID] = plan
	}
	return out
}

func comboMap(plan PlanReport) map[string]ComboReport {
	out := make(map[string]ComboReport, len(plan.Combos))
	for _, combo := range plan.Combos {
		out[combo.Key()] = combo
	}
	return out
}

func matchMap(matches []MatchReport) map[int]MatchReport {
	out := make(map[int]MatchReport, len(matches))
	for _, match := range matches {
		out[match.MatchIndex] = match
	}
	return out
}

func unionKeys[V any](left, right map[string]V) []string {
	seen := make(map[string]struct{}, len(left)+len(right))
	for key := range left {
		seen[key] = struct{}{}
	}
	for key := range right {
		seen[key] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func unionIntKeys[V any](left, right map[int]V) []int {
	seen := make(map[int]struct{}, len(left)+len(right))
	for key := range left {
		seen[key] = struct{}{}
	}
	for key := range right {
		seen[key] = struct{}{}
	}
	keys := make([]int, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func thresholdValue(plan *PlanReport, metric string) int64 {
	switch metric {
	case "win_rate_ppm":
		return plan.WinRatePPM
	case "avg_damage":
		return int64(plan.Damage.ObservedMean)
	case "max_ci_width_ppm":
		maxWidth := int64(0)
		for _, combo := range plan.Combos {
			if combo.CI.WidthPPM > maxWidth {
				maxWidth = combo.CI.WidthPPM
			}
		}
		return maxWidth
	case "completed_matches":
		return int64(plan.CompletedMatches)
	default:
		return 0
	}
}

func applyThresholds(project *Project, plans []PlanReport) {
	planIndex := make(map[string]int, len(plans))
	for index := range plans {
		planIndex[plans[index].PlanID] = index
	}
	for _, threshold := range project.Thresholds {
		indices := make([]int, 0)
		if threshold.PlanID != "" {
			if index, ok := planIndex[threshold.PlanID]; ok {
				indices = append(indices, index)
			}
		} else {
			for index := range plans {
				indices = append(indices, index)
			}
		}
		for _, index := range indices {
			actual := thresholdValue(&plans[index], threshold.Metric)
			plans[index].Thresholds = append(plans[index].Thresholds, ThresholdResult{
				ID:     threshold.ID,
				Metric: threshold.Metric,
				Actual: actual,
				Min:    threshold.Min,
				Max:    threshold.Max,
				Passed: actual >= threshold.Min && actual <= threshold.Max,
			})
		}
	}
}

func sortedPlans(plans []PlanReport) []PlanReport {
	sort.SliceStable(plans, func(i, j int) bool {
		return plans[i].PlanID < plans[j].PlanID
	})
	for index := range plans {
		sort.SliceStable(plans[index].Combos, func(i, j int) bool {
			return plans[index].Combos[i].ScenarioID < plans[index].Combos[j].ScenarioID
		})
	}
	return plans
}

func queueReasonsText(reasons []QueueReason) string {
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", reason.Reason, reason.Count))
	}
	return strings.Join(parts, ", ")
}
