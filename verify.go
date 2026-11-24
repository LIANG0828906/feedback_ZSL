package balance

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

type Violation struct {
	PlanID     string `json:"planId"`
	ScenarioID string `json:"scenarioId,omitempty"`
	Item       string `json:"item"`
	Detail     string `json:"detail"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
}

type VerifyBudgetSummary struct {
	RequestedMatches int64 `json:"requestedMatches"`
	AllocatedMatches int64 `json:"allocatedMatches"`
	CompletedMatches int64 `json:"completedMatches"`
	MaxTotalMatches  int   `json:"maxTotalMatches"`
	CommittedCombos  int   `json:"committedCombos"`
	CommittedPlans   int   `json:"committedPlans"`
}

type VerificationReport struct {
	SchemaVersion    string              `json:"schemaVersion"`
	OK               bool                `json:"ok"`
	ProjectHash      string              `json:"projectHash"`
	AlgorithmVersion string              `json:"algorithmVersion"`
	BudgetVersion    string              `json:"budgetVersion"`
	CommitSequence   int64               `json:"commitSequence"`
	CheckedCombos    int                 `json:"checkedCombos"`
	CheckedPlans     int                 `json:"checkedPlans"`
	Budget           VerifyBudgetSummary `json:"budget"`
	FirstPlanID      string              `json:"firstPlanId,omitempty"`
	FirstItem        string              `json:"firstItem,omitempty"`
	Violations       []Violation         `json:"violations"`
}

func ReadCheckpointFile(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &AppError{Code: CodeIO, Object: path, Detail: err.Error()}
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, &AppError{Code: CodeCheckpoint, Object: path, Field: "json", Detail: err.Error()}
	}
	if checkpoint.Combos == nil {
		checkpoint.Combos = make(map[string]ComboCheckpoint)
	}
	if checkpoint.Plans == nil {
		checkpoint.Plans = make(map[string]PlanCheckpoint)
	}
	return &checkpoint, nil
}

// VerifyCheckpoint is deliberately independent of scheduler execution. It
// recomputes all derived statistics from the persisted match records and the
// project manifest.
func VerifyCheckpoint(project *Project, checkpoint *Checkpoint) (*VerificationReport, error) {
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	result := &VerificationReport{
		SchemaVersion:    SchemaVersion,
		ProjectHash:      ProjectContentHash(project),
		AlgorithmVersion: project.AlgorithmVersion,
		BudgetVersion:    project.BudgetVersion,
		Violations:       make([]Violation, 0),
	}
	if checkpoint == nil {
		result.addViolation("_checkpoint", "", "checkpoint.presence", "checkpoint is nil", "present", "nil")
		result.finish()
		return result, nil
	}
	result.CommitSequence = checkpoint.CommitSequence
	result.Budget.MaxTotalMatches = project.Budget.MaxTotalMatches
	if checkpoint.SchemaVersion != SchemaVersion {
		result.addViolation("_checkpoint", "", "schemaVersion", "schema version mismatch", SchemaVersion, checkpoint.SchemaVersion)
	}
	if checkpoint.ProjectHash != ProjectContentHash(project) {
		result.addViolation("_checkpoint", "", "projectHash", "project content hash mismatch", ProjectContentHash(project), checkpoint.ProjectHash)
	}
	if checkpoint.BudgetHash != BudgetHash(project.Budget) {
		result.addViolation("_checkpoint", "", "budgetHash", "budget parameter hash mismatch", BudgetHash(project.Budget), checkpoint.BudgetHash)
	}
	if checkpoint.BudgetVersion != project.BudgetVersion {
		result.addViolation("_checkpoint", "", "budgetVersion", "budget version mismatch", project.BudgetVersion, checkpoint.BudgetVersion)
	}
	if checkpoint.AlgorithmVersion != project.AlgorithmVersion {
		result.addViolation("_checkpoint", "", "algorithmVersion", "algorithm version mismatch", project.AlgorithmVersion, checkpoint.AlgorithmVersion)
	}
	if len(result.Violations) > 0 {
		result.finish()
		return result, nil
	}

	budgetPlan, err := BuildBudgetPlan(project)
	if err != nil {
		return nil, err
	}
	if checkpoint.CommitSequence < 0 {
		result.addViolation("_checkpoint", "", "commitSequence", "negative commit sequence", ">= 0", fmt.Sprint(checkpoint.CommitSequence))
	}

	sequences := make(map[int64]string)
	planOrder := make(map[string]int, len(project.Plans))
	for index, plan := range project.Plans {
		planOrder[plan.ID] = index
	}
	scenarioOrder := make(map[string]int, len(project.Scenarios))
	for index, scenario := range project.Scenarios {
		scenarioOrder[scenario.ID] = index
	}

	keys := make([]string, 0, len(checkpoint.Combos))
	for key := range checkpoint.Combos {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		left := checkpoint.Combos[keys[i]].Report
		right := checkpoint.Combos[keys[j]].Report
		if planOrder[left.PlanID] != planOrder[right.PlanID] {
			return planOrder[left.PlanID] < planOrder[right.PlanID]
		}
		if left.PlanID != right.PlanID {
			return left.PlanID < right.PlanID
		}
		if scenarioOrder[left.ScenarioID] != scenarioOrder[right.ScenarioID] {
			return scenarioOrder[left.ScenarioID] < scenarioOrder[right.ScenarioID]
		}
		return left.ScenarioID < right.ScenarioID
	})

	for _, key := range keys {
		entry := checkpoint.Combos[key]
		report := entry.Report
		result.CheckedCombos++
		result.Budget.RequestedMatches += int64(report.RequestedMatches)
		result.Budget.AllocatedMatches += int64(report.AllocatedMatches)
		result.Budget.CompletedMatches += int64(report.CompletedMatches)
		result.Budget.CommittedCombos++
		if key != report.Key() {
			result.addViolation(report.PlanID, report.ScenarioID, "combo.key", "combination key mismatch", report.Key(), key)
		}
		expected := budgetPlan.ByKey[key]
		if expected == nil {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.combo", "checkpoint contains an unknown combination", "known combination", key)
			continue
		}
		verifyComboBudgetAndStats(result, &entry, expected)
		if entry.CommitSeq <= 0 || entry.CommitSeq > checkpoint.CommitSequence {
			result.addViolation(report.PlanID, report.ScenarioID, "commitSeq", "combination commit sequence is out of range", fmt.Sprintf("1..%d", checkpoint.CommitSequence), fmt.Sprint(entry.CommitSeq))
		}
		if previous, exists := sequences[entry.CommitSeq]; exists {
			result.addViolation(report.PlanID, report.ScenarioID, "commitSeq.unique", "duplicate commit sequence", "unique", "shared with "+previous)
		} else {
			sequences[entry.CommitSeq] = report.Key()
		}
	}

	planIDs := make([]string, 0, len(checkpoint.Plans))
	for planID := range checkpoint.Plans {
		planIDs = append(planIDs, planID)
	}
	sort.SliceStable(planIDs, func(i, j int) bool {
		if planOrder[planIDs[i]] != planOrder[planIDs[j]] {
			return planOrder[planIDs[i]] < planOrder[planIDs[j]]
		}
		return planIDs[i] < planIDs[j]
	})
	for _, planID := range planIDs {
		entry := checkpoint.Plans[planID]
		result.CheckedPlans++
		result.Budget.CommittedPlans++
		plan := findPlan(project, planID)
		if plan == nil {
			result.addViolation(planID, "", "plan.id", "checkpoint contains an unknown plan", "known plan", planID)
			continue
		}
		if entry.State != PlanCommitted || entry.PlanID != planID {
			result.addViolation(planID, "", "plan.state", "plan checkpoint is not committed", "committed", string(entry.State))
		}
		if entry.CommitSeq <= 0 || entry.CommitSeq > checkpoint.CommitSequence {
			result.addViolation(planID, "", "plan.commitSeq", "plan commit sequence is out of range", fmt.Sprintf("1..%d", checkpoint.CommitSequence), fmt.Sprint(entry.CommitSeq))
		}
		if previous, exists := sequences[entry.CommitSeq]; exists {
			result.addViolation(planID, "", "plan.commitSeq.unique", "duplicate commit sequence", "unique", "shared with "+previous)
		} else {
			sequences[entry.CommitSeq] = planID
		}
		verifyPlanCheckpoint(result, project, checkpoint, plan, entry)
	}

	if project.Budget.MaxTotalMatches > 0 && result.Budget.CompletedMatches > int64(project.Budget.MaxTotalMatches) {
		result.addViolation("_budget", "", "budget.completedMatches", "completed match count exceeds max total budget", fmt.Sprint(project.Budget.MaxTotalMatches), fmt.Sprint(result.Budget.CompletedMatches))
	}
	if result.Budget.AllocatedMatches > budgetPlan.Capacity {
		result.addViolation("_budget", "", "budget.allocatedMatches", "allocated match count exceeds computed capacity", fmt.Sprint(budgetPlan.Capacity), fmt.Sprint(result.Budget.AllocatedMatches))
	}
	reductions := make([]ReductionRecord, 0)
	for _, entry := range checkpoint.Combos {
		if entry.Report.Reduction != nil {
			reductions = append(reductions, *entry.Report.Reduction)
		}
	}
	if err := ValidateBudgetReductions(reductions); err != nil {
		appErr := AsAppError(err)
		planID := "_budget"
		item := "budget.reduction"
		if appErr != nil && appErr.PlanID != "" {
			planID = appErr.PlanID
			item = "budget.reduction.round"
		}
		result.addViolation(planID, "", item, err.Error(), "one explained reduction per rule change", "conflicting reduction records")
	}
	result.finish()
	return result, nil
}

func verifyComboBudgetAndStats(result *VerificationReport, entry *ComboCheckpoint, expected *BudgetCombo) {
	report := entry.Report
	if report.State != ComboCommitted {
		result.addViolation(report.PlanID, report.ScenarioID, "state", "checkpoint combination is not committed", "committed", string(report.State))
	}
	if report.RequestedMatches != expected.Requested {
		result.addViolation(report.PlanID, report.ScenarioID, "budget.requestedMatches", "requested match count differs from budget plan", fmt.Sprint(expected.Requested), fmt.Sprint(report.RequestedMatches))
	}
	if report.AllocatedMatches != expected.Allocated {
		result.addViolation(report.PlanID, report.ScenarioID, "budget.allocatedMatches", "allocated match count differs from budget plan", fmt.Sprint(expected.Allocated), fmt.Sprint(report.AllocatedMatches))
	}
	if report.CompletedMatches != len(report.Matches) {
		result.addViolation(report.PlanID, report.ScenarioID, "budget.completedMatches", "completed count differs from persisted matches", fmt.Sprint(len(report.Matches)), fmt.Sprint(report.CompletedMatches))
	}
	if report.CompletedMatches != report.AllocatedMatches {
		result.addViolation(report.PlanID, report.ScenarioID, "budget.allocatedMatches", "committed combination did not complete its allocation", fmt.Sprint(report.AllocatedMatches), fmt.Sprint(report.CompletedMatches))
	}
	if expected.Reduction == nil && report.Reduction != nil {
		result.addViolation(report.PlanID, report.ScenarioID, "budget.reduction", "unexpected reduction record", "none", report.Reduction.Rule)
	}
	if expected.Reduction != nil {
		if report.Reduction == nil {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.reduction", "missing reduction record", expected.Reduction.Explanation, "nil")
		} else if report.Reduction.Allocated != expected.Allocated || report.Reduction.Requested != expected.Requested ||
			report.Reduction.Rule != expected.Reduction.Rule || report.Reduction.Explanation == "" {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.reduction", "reduction record does not match the declared budget policy", expected.Reduction.Explanation, report.Reduction.Explanation)
		}
	}
	wins, losses, draws := summarizeMatches(report.Matches)
	if wins != report.Wins {
		result.addViolation(report.PlanID, report.ScenarioID, "wins", "win count mismatch", fmt.Sprint(wins), fmt.Sprint(report.Wins))
	}
	if losses != report.Losses {
		result.addViolation(report.PlanID, report.ScenarioID, "losses", "loss count mismatch", fmt.Sprint(losses), fmt.Sprint(report.Losses))
	}
	if draws != report.Draws {
		result.addViolation(report.PlanID, report.ScenarioID, "draws", "draw count mismatch", fmt.Sprint(draws), fmt.Sprint(report.Draws))
	}
	rate := ratePPM(int64(wins), int64(len(report.Matches)))
	if report.WinRatePPM != rate {
		result.addViolation(report.PlanID, report.ScenarioID, "winRatePpm", "win rate mismatch", fmt.Sprint(rate), fmt.Sprint(report.WinRatePPM))
	}
	ci := confidenceInterval(int64(wins), int64(len(report.Matches)))
	if report.CI != ci {
		result.addViolation(report.PlanID, report.ScenarioID, "confidenceInterval", "confidence interval mismatch", fmt.Sprintf("%+v", ci), fmt.Sprintf("%+v", report.CI))
	}
	damage := summarizeDamage(report.Matches)
	if !sameDamageSummary(damage, report.Damage) {
		result.addViolation(report.PlanID, report.ScenarioID, "damage", "damage statistics mismatch", fmt.Sprintf("%+v", damage), fmt.Sprintf("%+v", report.Damage))
	}
	expectedWins := 0
	turnCount := 0
	for index, match := range report.Matches {
		if match.MatchIndex != index+1 {
			result.addViolation(report.PlanID, report.ScenarioID, "match.index", "match indices are not stable and contiguous", fmt.Sprint(index+1), fmt.Sprint(match.MatchIndex))
		}
		if match.RevivesLeft < 0 || match.RevivesRight < 0 {
			result.addViolation(report.PlanID, report.ScenarioID, "match.revives", "revive count is negative", ">= 0", fmt.Sprintf("%d/%d", match.RevivesLeft, match.RevivesRight))
		}
		if match.Digest != buildMatchDigest(&match) {
			result.addViolation(report.PlanID, report.ScenarioID, "match.digest", "stored match digest does not match persisted damage sequence", buildMatchDigest(&match), match.Digest)
		}
		var totalLeft, totalRight Fixed
		for _, value := range match.DamageLeft {
			totalLeft += value
		}
		for _, value := range match.DamageRight {
			totalRight += value
		}
		if totalLeft != match.TotalLeft {
			result.addViolation(report.PlanID, report.ScenarioID, "match.totalLeft", "left damage sequence total mismatch", fmt.Sprint(totalLeft), fmt.Sprint(match.TotalLeft))
		}
		if totalRight != match.TotalRight {
			result.addViolation(report.PlanID, report.ScenarioID, "match.totalRight", "right damage sequence total mismatch", fmt.Sprint(totalRight), fmt.Sprint(match.TotalRight))
		}
		if match.Winner == "left" {
			expectedWins++
		}
		turnCount += match.ActionTurns
	}
	if report.RNG.MatchCursor != len(report.Matches) {
		result.addViolation(report.PlanID, report.ScenarioID, "rngProgress.matchCursor", "RNG match cursor mismatch", fmt.Sprint(len(report.Matches)), fmt.Sprint(report.RNG.MatchCursor))
	}
	if report.RNG.TurnCount != turnCount {
		result.addViolation(report.PlanID, report.ScenarioID, "rngProgress.turnCount", "RNG turn progress mismatch", fmt.Sprint(turnCount), fmt.Sprint(report.RNG.TurnCount))
	}
	if expectedWins != wins {
		result.addViolation(report.PlanID, report.ScenarioID, "wins.recomputed", "winner fields disagree with win count", fmt.Sprint(expectedWins), fmt.Sprint(wins))
	}
	previousSamples := 0
	previousWidth := int64(PPMSize + 1)
	for index, roundStat := range report.Rounds {
		if roundStat.Round <= 0 {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.round", "round number is not positive", "> 0", fmt.Sprint(roundStat.Round))
		}
		if index > 0 && roundStat.Round <= report.Rounds[index-1].Round {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.round.order", "round order is not increasing", "increasing", fmt.Sprint(roundStat.Round))
		}
		if roundStat.CumulativeMatches != previousSamples+roundStat.AddedMatches {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.round.addedMatches", "round match delta mismatch", fmt.Sprint(previousSamples+roundStat.AddedMatches), fmt.Sprint(roundStat.CumulativeMatches))
		}
		expectedWidth := confidenceInterval(int64(roundStat.Wins), int64(roundStat.CumulativeMatches)).WidthPPM
		if roundStat.CIWidthPPM != expectedWidth {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.round.ciWidthPpm", "round confidence interval width mismatch", fmt.Sprint(expectedWidth), fmt.Sprint(roundStat.CIWidthPPM))
		}
		if roundStat.CIWidthPPM > previousWidth {
			result.addViolation(report.PlanID, report.ScenarioID, "budget.round.ciWidthPpm.monotonic", "confidence interval width increased", "non-increasing", fmt.Sprint(roundStat.CIWidthPPM))
		}
		previousSamples = roundStat.CumulativeMatches
		previousWidth = roundStat.CIWidthPPM
	}
	if len(report.Rounds) > 0 && previousSamples != report.CompletedMatches {
		result.addViolation(report.PlanID, report.ScenarioID, "budget.round.cumulativeMatches", "final cumulative round samples differ from completed matches", fmt.Sprint(report.CompletedMatches), fmt.Sprint(previousSamples))
	}
}

func verifyPlanCheckpoint(result *VerificationReport, project *Project, checkpoint *Checkpoint, plan *Plan, entry PlanCheckpoint) {
	comboKeys := make(map[string]struct{}, len(plan.TargetScenarios))
	requested := 0
	allocated := 0
	completed := 0
	wins := 0
	losses := 0
	draws := 0
	committedCombos := 0
	for _, scenarioID := range plan.TargetScenarios {
		key := ComboID{PlanID: plan.ID, ScenarioID: scenarioID}.Key()
		comboKeys[key] = struct{}{}
		comboEntry, ok := checkpoint.Combos[key]
		if !ok {
			result.addViolation(plan.ID, scenarioID, "plan.combo", "committed plan is missing a combination", "present", "missing")
			continue
		}
		combo := comboEntry.Report
		requested += combo.RequestedMatches
		allocated += combo.AllocatedMatches
		completed += combo.CompletedMatches
		wins += combo.Wins
		losses += combo.Losses
		draws += combo.Draws
		committedCombos++
	}
	if entry.CommittedCombos != committedCombos {
		result.addViolation(plan.ID, "", "plan.committedCombos", "plan committed combination count mismatch", fmt.Sprint(committedCombos), fmt.Sprint(entry.CommittedCombos))
	}
	if entry.RequestedMatches != requested {
		result.addViolation(plan.ID, "", "plan.requestedMatches", "plan requested match total mismatch", fmt.Sprint(requested), fmt.Sprint(entry.RequestedMatches))
	}
	if entry.AllocatedMatches != allocated {
		result.addViolation(plan.ID, "", "plan.allocatedMatches", "plan allocated match total mismatch", fmt.Sprint(allocated), fmt.Sprint(entry.AllocatedMatches))
	}
	if entry.CompletedMatches != completed {
		result.addViolation(plan.ID, "", "plan.completedMatches", "plan completed match total mismatch", fmt.Sprint(completed), fmt.Sprint(entry.CompletedMatches))
	}
	if entry.Wins != wins || entry.Losses != losses || entry.Draws != draws {
		result.addViolation(plan.ID, "", "plan.wins", "plan win/loss/draw totals mismatch", fmt.Sprintf("%d/%d/%d", wins, losses, draws), fmt.Sprintf("%d/%d/%d", entry.Wins, entry.Losses, entry.Draws))
	}
	rate := ratePPM(int64(wins), int64(completed))
	if entry.WinRatePPM != rate {
		result.addViolation(plan.ID, "", "plan.winRatePpm", "plan win rate mismatch", fmt.Sprint(rate), fmt.Sprint(entry.WinRatePPM))
	}
	if len(entry.ComboKeys) != len(comboKeys) {
		result.addViolation(plan.ID, "", "plan.comboKeys", "plan combination key count mismatch", fmt.Sprint(len(comboKeys)), fmt.Sprint(len(entry.ComboKeys)))
	} else {
		for _, key := range entry.ComboKeys {
			if _, ok := comboKeys[key]; !ok {
				result.addViolation(plan.ID, "", "plan.comboKeys", "plan contains an unexpected combination key", "target combination", key)
				break
			}
		}
	}
	expectedCI := confidenceInterval(int64(wins), int64(completed))
	if entry.CI != expectedCI {
		result.addViolation(plan.ID, "", "plan.confidenceInterval", "plan confidence interval mismatch", fmt.Sprintf("%+v", expectedCI), fmt.Sprintf("%+v", entry.CI))
	}
	expectedDigest := planDigestFromCheckpoint(plan, checkpoint)
	if entry.DamageDigest != expectedDigest {
		result.addViolation(plan.ID, "", "plan.damageDigest", "plan damage digest mismatch", expectedDigest, entry.DamageDigest)
	}
	expectedRNG := RNGProgress{}
	var allMatches []MatchReport
	for _, scenarioID := range plan.TargetScenarios {
		comboEntry, ok := checkpoint.Combos[ComboID{PlanID: plan.ID, ScenarioID: scenarioID}.Key()]
		if ok {
			expectedRNG.MatchCursor += comboEntry.Report.RNG.MatchCursor
			expectedRNG.TurnCount += comboEntry.Report.RNG.TurnCount
			allMatches = append(allMatches, comboEntry.Report.Matches...)
		}
	}
	if entry.RNG != expectedRNG {
		result.addViolation(plan.ID, "", "plan.rngProgress", "plan RNG progress mismatch", fmt.Sprintf("%+v", expectedRNG), fmt.Sprintf("%+v", entry.RNG))
	}
	expectedDamage := summarizeDamage(allMatches)
	if !sameDamageSummary(expectedDamage, entry.Damage) {
		result.addViolation(plan.ID, "", "plan.damage", "plan damage summary mismatch", fmt.Sprintf("%+v", expectedDamage), fmt.Sprintf("%+v", entry.Damage))
	}
}

func planDigestFromCheckpoint(plan *Plan, checkpoint *Checkpoint) string {
	var buf []byte
	buf = append(buf, plan.ID...)
	for _, scenarioID := range plan.TargetScenarios {
		key := ComboID{PlanID: plan.ID, ScenarioID: scenarioID}.Key()
		buf = append(buf, key...)
		entry, ok := checkpoint.Combos[key]
		if !ok {
			continue
		}
		for _, match := range entry.Report.Matches {
			buf = append(buf, match.Digest...)
		}
	}
	return sha256Hex(buf)
}

func sameDamageSummary(a, b DamageSummary) bool {
	return a.MatchCount == b.MatchCount &&
		a.ObservedMin == b.ObservedMin &&
		a.ObservedMax == b.ObservedMax &&
		a.ObservedMean == b.ObservedMean &&
		a.TruthMin == b.TruthMin &&
		a.TruthMax == b.TruthMax &&
		a.TruthMatch == b.TruthMatch
}

func (r *VerificationReport) addViolation(planID, scenarioID, item, detail, expected, actual string) {
	if len(r.Violations) >= 100 {
		return
	}
	r.Violations = append(r.Violations, Violation{
		PlanID:     planID,
		ScenarioID: scenarioID,
		Item:       item,
		Detail:     detail,
		Expected:   expected,
		Actual:     actual,
	})
	if r.FirstPlanID == "" {
		r.FirstPlanID = planID
		r.FirstItem = item
	}
}

func (r *VerificationReport) finish() {
	r.OK = len(r.Violations) == 0
	if r.OK {
		r.FirstPlanID = ""
		r.FirstItem = ""
	}
	if r.Violations == nil {
		r.Violations = []Violation{}
	}
}
