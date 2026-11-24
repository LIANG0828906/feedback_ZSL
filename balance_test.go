package balance

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGenerateDecodeValidate(t *testing.T) {
	project := GenerateProject(42)
	if err := ValidateProject(project); err != nil {
		t.Fatalf("generated project failed validation: %v", err)
	}
	if !reflect.DeepEqual(project, GenerateProject(42)) {
		t.Fatal("same seed did not generate the same project")
	}
	if reflect.DeepEqual(project, GenerateProject(43)) {
		t.Fatal("different seeds unexpectedly generated identical projects")
	}
	data, err := MarshalProject(project)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeProject(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProject(decoded); err != nil {
		t.Fatal(err)
	}
	if ProjectContentHash(project) != ProjectContentHash(decoded) {
		t.Fatal("content hash changed after deterministic JSON round trip")
	}
	if ProjectContentHash(project) != ProjectContentHash(GenerateProject(42)) {
		t.Fatal("content hash is not deterministic")
	}
	withZeroConst := cloneProject(t, project)
	withZeroConst.Formulas = append(withZeroConst.Formulas, Formula{ID: "zero.const", Op: "const", Value: 0})
	zeroData, err := MarshalProject(withZeroConst)
	if err != nil {
		t.Fatal(err)
	}
	zeroDecoded, err := DecodeProject(zeroData)
	if err != nil {
		t.Fatalf("zero-valued const formula did not round trip: %v", err)
	}
	if err := ValidateProject(zeroDecoded); err != nil {
		t.Fatalf("zero-valued const formula is invalid after round trip: %v", err)
	}
	if len(project.Skills) == 0 || len(project.Builds) == 0 || len(project.Scenarios) != 2 || len(project.Plans) != 3 {
		t.Fatalf("unexpected generated sizes: skills=%d builds=%d scenarios=%d plans=%d",
			len(project.Skills), len(project.Builds), len(project.Scenarios), len(project.Plans))
	}
}

func TestValidationErrorCodesAndObjects(t *testing.T) {
	project := GenerateProject(42)

	missingFormula := cloneProject(t, project)
	missingFormula.Skills = missingFormula.Skills[1:]
	err := ValidateProject(missingFormula)
	assertCode(t, err, CodeSchema)
	if !strings.Contains(err.Error(), missingFormula.Builds[0].ID) && !strings.Contains(err.Error(), "rotation") {
		t.Fatalf("missing skill reference did not identify the offending object: %v", err)
	}

	cycle := cloneProject(t, project)
	for index := range cycle.Formulas {
		if cycle.Formulas[index].ID == "warrior.slash.scaled" {
			cycle.Formulas[index].Inputs = []string{"warrior.slash.growth", "warrior.slash.root"}
		}
	}
	err = ValidateProject(cycle)
	assertCode(t, err, CodeCycle)
	if appErr := AsAppError(err); appErr == nil || appErr.Field != "formula.dependency" || appErr.Object == "" {
		t.Fatalf("cycle error lacks object or field: %#v", appErr)
	}
	if report, runErr := Run(context.Background(), cycle, RunOptions{Mode: ModeFull}); report != nil || ErrorCode(runErr) != CodeCycle {
		t.Fatalf("invalid project entered settlement: report=%v err=%v", report, runErr)
	}

	outOfRange := cloneProject(t, project)
	outOfRange.Skills[0].GrowthCoefficient = 11 * FixedScale
	err = ValidateProject(outOfRange)
	assertCode(t, err, CodeRange)
	if appErr := AsAppError(err); appErr == nil || appErr.Object != outOfRange.Skills[0].ID {
		t.Fatalf("range error did not identify skill: %#v", appErr)
	}

	negativeBudget := cloneProject(t, project)
	negativeBudget.Budget.Slots = -1
	err = ValidateProject(negativeBudget)
	assertCode(t, err, CodeRange)
	if appErr := AsAppError(err); appErr == nil || appErr.Object != "budget.slots" {
		t.Fatalf("budget range error did not identify field: %#v", appErr)
	}

	rawData := projectAsMap(t, project)
	skills := rawData["skills"].([]any)
	delete(skills[0].(map[string]any), "formulaId")
	data, err := json.Marshal(rawData)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeProject(data)
	assertCode(t, err, CodeSchema)
	if appErr := AsAppError(err); appErr == nil || !strings.Contains(appErr.Object, "formulaId") {
		t.Fatalf("missing field error did not identify field: %#v", appErr)
	}

	unmappedBuild := cloneProject(t, project)
	unmappedBuild.Scenarios[0].LeftBuilds = []string{"duel_warrior"}
	unmappedBuild.Scenarios[0].RightBuilds = []string{"duel_warrior"}
	unmappedBuild.Scenarios[1].LeftBuilds = []string{"squad_guard"}
	err = ValidateProject(unmappedBuild)
	assertCode(t, err, CodeSchema)
}

func TestFixedSerialAndParallelMatchDeterminism(t *testing.T) {
	base := GenerateProject(42)
	base.Budget.MaxMatchesPerRound = 6
	base.Budget.MemoryBuffer = 6
	base.Budget.MaxRounds = 10
	base.Budget.TargetMatchesPerCombo = 8
	base.Budget.MaxTotalMatches = 36

	serialProject := cloneProject(t, base)
	serialProject.Budget.Slots = 1
	serial, err := Run(context.Background(), serialProject, RunOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	parallelProject := cloneProject(t, base)
	parallelProject.Budget.Slots = 4
	parallel, err := Run(context.Background(), parallelProject, RunOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareReports(serial, parallel); err != nil {
		t.Fatalf("serial and parallel reports differ: %v", err)
	}
	if serial.BudgetUsage.MaxSlotsUsed != 1 {
		t.Fatalf("serial run used %d slots", serial.BudgetUsage.MaxSlotsUsed)
	}
	if parallel.BudgetUsage.MaxSlotsUsed <= 1 || parallel.BudgetUsage.MaxSlotsUsed > 4 {
		t.Fatalf("parallel run peak slots = %d, want 2..4", parallel.BudgetUsage.MaxSlotsUsed)
	}

	second, err := Run(context.Background(), cloneProject(t, parallelProject), RunOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareReports(parallel, second); err != nil {
		t.Fatalf("repeat run is not deterministic: %v", err)
	}

	combo := findComboReport(t, parallel, "baseline", "duel")
	if len(combo.Matches) == 0 || len(combo.Matches[0].DamageLeft) == 0 {
		t.Fatal("missing per-round damage sequence")
	}
	for _, match := range combo.Matches {
		if match.Digest != buildMatchDigest(&match) {
			t.Fatalf("match %d digest mismatch", match.MatchIndex)
		}
	}
	revived := 0
	for _, planReport := range parallel.Plans {
		for _, combo := range planReport.Combos {
			for _, match := range combo.Matches {
				if match.RevivesLeft > 0 || match.RevivesRight > 0 {
					revived++
				}
			}
		}
	}
	if revived == 0 {
		t.Fatal("deterministic sample did not exercise death revival")
	}
}

func TestBudgetTradeoffAndConfidenceMonotonicity(t *testing.T) {
	project := GenerateProject(42)
	plan, err := BuildBudgetPlan(project)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RequestedTotal != 48 || plan.Capacity != 36 {
		t.Fatalf("unexpected budget totals requested=%d capacity=%d", plan.RequestedTotal, plan.Capacity)
	}
	allocated := 0
	reductions := 0
	for _, combo := range plan.Combos {
		allocated += combo.Allocated
		if combo.Reduction != nil {
			reductions++
			if combo.Reduction.Rule != PriorityFirst || combo.Reduction.Explanation == "" {
				t.Fatalf("reduction missing explainable rule: %#v", combo.Reduction)
			}
		}
	}
	if allocated != 36 || reductions != 2 {
		t.Fatalf("unexpected allocation/reduction counts: allocated=%d reductions=%d", allocated, reductions)
	}

	report, err := Run(context.Background(), project, RunOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	if report.BudgetUsage.AllocatedMatches != 36 || report.BudgetUsage.CompletedMatches != 36 {
		t.Fatalf("unexpected run budget usage: %#v", report.BudgetUsage)
	}
	for _, planReport := range report.Plans {
		for _, combo := range planReport.Combos {
			if err := ValidateCIWidths(combo); err != nil {
				t.Fatalf("CI width not monotonic for %s: %v", combo.Key(), err)
			}
			for _, round := range combo.Rounds {
				if round.ChangeSamples != round.AddedMatches {
					t.Fatalf("round change mismatch: %#v", round)
				}
			}
		}
	}
	previousWidth := int64(PPMSize + 1)
	for samples := int64(1); samples <= 100; samples++ {
		width := confidenceInterval(samples/2, samples).WidthPPM
		if width > previousWidth {
			t.Fatalf("CI width increased at %d samples: %d > %d", samples, width, previousWidth)
		}
		previousWidth = width
	}

	equalProject := cloneProject(t, project)
	equalProject.Budget.PriorityRule = PriorityEqual
	equalPlan, err := BuildBudgetPlan(equalProject)
	if err != nil {
		t.Fatal(err)
	}
	for _, combo := range equalPlan.Combos {
		if combo.Allocated != 6 {
			t.Fatalf("equal coverage allocation = %d, want 6", combo.Allocated)
		}
	}

	record := ReductionRecord{
		PlanID: "p", ScenarioID: "s", Round: 0, Rule: "priority_first",
		PolicyRule: "priority_first", Explanation: "same explanation",
		Requested: 10, Allocated: 4,
	}
	duplicate := record
	duplicate.Round = 1
	err = ValidateBudgetReductions([]ReductionRecord{record, duplicate})
	assertCode(t, err, CodeBudget)
	if appErr := AsAppError(err); appErr == nil || appErr.PlanID != "p" || appErr.ConflictRule != "priority_first" {
		t.Fatalf("budget conflict did not identify combination/rule: %#v", appErr)
	}

	tooSmall := cloneProject(t, project)
	tooSmall.Budget.MaxTotalMatches = 3
	_, err = BuildBudgetPlan(tooSmall)
	assertCode(t, err, CodeBudget)
	if appErr := AsAppError(err); appErr == nil || appErr.Object != "budget.coverage" {
		t.Fatalf("coverage error did not identify budget object: %#v", appErr)
	}
}

func TestStateMachineAndFrozenResults(t *testing.T) {
	project := GenerateProject(42)
	tracker := NewStateTracker(project)
	if err := tracker.TransitionPlan("baseline", PlanCommitted); err == nil {
		t.Fatal("illegal plan transition succeeded")
	} else {
		assertCode(t, err, CodeState)
	}
	if tracker.PlanState("baseline") != PlanPending {
		t.Fatal("illegal transition changed plan state")
	}
	if err := tracker.TransitionCombo("baseline", "duel", ComboCommitted); err == nil {
		t.Fatal("illegal combination transition succeeded")
	} else {
		assertCode(t, err, CodeState)
	}
	if tracker.ComboState("baseline", "duel") != ComboPending {
		t.Fatal("illegal transition changed combination state")
	}
	if err := tracker.TransitionCombo("baseline", "duel", ComboRunning); err != nil {
		t.Fatal(err)
	}
	payload := map[string]string{"summary": "frozen"}
	if err := tracker.CommitCombo("baseline", "duel", payload); err != nil {
		t.Fatal(err)
	}
	err := tracker.SetComboPayload("baseline", "duel", map[string]string{"summary": "rewritten"})
	assertCode(t, err, CodeState)
	stored, ok := tracker.ComboPayload("baseline", "duel")
	if !ok || !reflect.DeepEqual(stored, payload) {
		t.Fatal("committed payload was not frozen")
	}
	if err := tracker.TransitionPlan("baseline", PlanRunning); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TransitionPlan("baseline", PlanFailed); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TransitionPlan("baseline", PlanRunning); err != nil {
		t.Fatalf("failed plan retry should be legal: %v", err)
	}
	if err := tracker.CommitPlan("baseline", payload); err != nil {
		t.Fatal(err)
	}
	if tracker.PlanState("baseline") != PlanCommitted {
		t.Fatal("plan was not committed")
	}
}

func TestFaultInjectionCodesAndIsolation(t *testing.T) {
	project := GenerateProject(42)
	for _, kind := range []string{"unit", "cooldown", "resource"} {
		t.Run(kind, func(t *testing.T) {
			report, err := Run(context.Background(), cloneProject(t, project), RunOptions{
				Mode: ModeFull,
				Injection: Injection{
					Kind:       kind,
					PlanID:     "baseline",
					ScenarioID: "duel",
					MatchIndex: 1,
					Turn:       2,
				},
			})
			if report == nil {
				t.Fatal("fault injection returned no partial report")
			}
			want := map[string]string{"unit": CodeUnit, "cooldown": CodeCooldown, "resource": CodeResource}[kind]
			assertCode(t, err, want)
			appErr := AsAppError(err)
			if appErr == nil || appErr.PlanID != "baseline" || appErr.ScenarioID != "duel" || appErr.MatchIndex != 1 || appErr.Turn != 2 {
				t.Fatalf("fault error context is incomplete: %#v", appErr)
			}
			failed := findComboReport(t, report, "baseline", "duel")
			if failed.State != ComboFailed || failed.Failure == nil {
				t.Fatalf("failed combination not marked failed: %#v", failed)
			}
			if report.Progress.CommittedCombos == 0 {
				t.Fatal("failure polluted every other combination")
			}
			baseline := findPlanReport(t, report, "baseline")
			if baseline.State != PlanFailed {
				t.Fatalf("plan state = %s, want failed", baseline.State)
			}
		})
	}

	t.Run("internal retry", func(t *testing.T) {
		report, err := Run(context.Background(), cloneProject(t, project), RunOptions{
			Mode:       ModeFull,
			RetryLimit: 3,
			Injection: Injection{
				Kind:       "internal",
				PlanID:     "baseline",
				ScenarioID: "duel",
				MatchIndex: 1,
				Turn:       2,
			},
		})
		if err != nil {
			t.Fatalf("transient internal error was not retried: %v", err)
		}
		combo := findComboReport(t, report, "baseline", "duel")
		if combo.State != ComboCommitted || len(combo.Matches) == 0 || combo.Matches[0].Attempts != 2 {
			t.Fatalf("retry was not recorded without corrupting the combination: %#v", combo)
		}
	})

	t.Run("internal exhausted", func(t *testing.T) {
		report, err := Run(context.Background(), cloneProject(t, project), RunOptions{
			Mode:       ModeFull,
			RetryLimit: 1,
			Injection: Injection{
				Kind:       "internal",
				PlanID:     "baseline",
				ScenarioID: "duel",
				MatchIndex: 1,
				Turn:       2,
			},
		})
		assertCode(t, err, CodeInternal)
		if report == nil || report.Progress.CommittedCombos == 0 {
			t.Fatal("exhausted internal retry polluted other combinations")
		}
	})
}

func TestCheckpointResumeThreePathsAndInvalidation(t *testing.T) {
	project := GenerateProject(42)
	full, err := Run(context.Background(), project, RunOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	resumePath := filepath.Join(dir, "resume.json")
	partial, err := Run(context.Background(), project, RunOptions{
		Mode:             ModeRealtime,
		CheckpointPath:   resumePath,
		StopAfterCommits: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !partial.Partial {
		t.Fatal("stop-after run was not partial")
	}
	partialCheckpoint, err := ReadCheckpointFile(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(partialCheckpoint.Combos) != 3 {
		t.Fatalf("partial checkpoint has %d combinations, want 3", len(partialCheckpoint.Combos))
	}
	partialVerification, err := VerifyCheckpoint(project, partialCheckpoint)
	if err != nil || !partialVerification.OK {
		t.Fatalf("partial checkpoint failed independent verification: %v %#v", err, partialVerification)
	}

	resumed, err := Run(context.Background(), project, RunOptions{
		Mode:           ModeResume,
		CheckpointPath: resumePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Checkpoint.Loaded {
		t.Fatal("resume did not load the checkpoint")
	}
	if err := CompareReports(full, resumed); err != nil {
		t.Fatalf("resume differs from full run: %v", err)
	}
	if resumed.Progress.CommittedCombos != 6 || resumed.Progress.CommittedPlans != 3 {
		t.Fatalf("resume progress is incomplete: %#v", resumed.Progress)
	}

	realtimePath := filepath.Join(dir, "realtime.json")
	realtime, err := Run(context.Background(), project, RunOptions{
		Mode:           ModeRealtime,
		CheckpointPath: realtimePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareReports(full, realtime); err != nil {
		t.Fatalf("realtime differs from full run: %v", err)
	}

	checkpoint, err := ReadCheckpointFile(resumePath)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := VerifyCheckpoint(project, checkpoint)
	if err != nil || !verification.OK {
		t.Fatalf("resumed checkpoint failed independent verification: %v %#v", err, verification)
	}

	changed := cloneProject(t, project)
	changed.Seed += 100
	changedFull, err := Run(context.Background(), changed, RunOptions{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	changedResume, err := Run(context.Background(), changed, RunOptions{
		Mode:           ModeResume,
		CheckpointPath: resumePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changedResume.Checkpoint.Invalidated || changedResume.Checkpoint.InvalidReason == "" {
		t.Fatalf("mismatched checkpoint was not invalidated: %#v", changedResume.Checkpoint)
	}
	if err := CompareReports(changedFull, changedResume); err != nil {
		t.Fatalf("invalidated checkpoint mixed old results: %v", err)
	}

	stalePath := filepath.Join(dir, "stale.json")
	if _, err := Run(context.Background(), project, RunOptions{Mode: ModeRealtime, CheckpointPath: stalePath}); err != nil {
		t.Fatal(err)
	}
	_, injectErr := Run(context.Background(), project, RunOptions{
		Mode:           ModeRealtime,
		CheckpointPath: stalePath,
		Injection: Injection{
			Kind:       "unit",
			MatchIndex: 1,
			Turn:       2,
		},
	})
	assertCode(t, injectErr, CodeUnit)
	staleCheckpoint, err := ReadCheckpointFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(staleCheckpoint.Combos) != 0 {
		t.Fatalf("stale checkpoint was retained after a failed new run: %d combinations", len(staleCheckpoint.Combos))
	}
}

func TestIndependentVerifierAndFirstDifference(t *testing.T) {
	project := GenerateProject(42)
	dir := t.TempDir()
	checkpointPath := filepath.Join(dir, "checkpoint.json")
	report, err := Run(context.Background(), project, RunOptions{
		Mode:           ModeRealtime,
		CheckpointPath: checkpointPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := ReadCheckpointFile(checkpointPath)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := VerifyCheckpoint(project, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !verification.OK || verification.FirstPlanID != "" {
		t.Fatalf("valid checkpoint failed verification: %#v", verification)
	}

	tamperedCheckpoint := cloneJSON(t, checkpoint)
	key := ComboID{PlanID: "baseline", ScenarioID: "duel"}.Key()
	entry := tamperedCheckpoint.Combos[key]
	entry.Report.WinRatePPM++
	tamperedCheckpoint.Combos[key] = entry
	badVerification, err := VerifyCheckpoint(project, tamperedCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if badVerification.OK || badVerification.FirstPlanID != "baseline" || badVerification.FirstItem != "winRatePpm" {
		t.Fatalf("verifier did not identify first violation: %#v", badVerification)
	}

	tamperedDigest := cloneJSON(t, checkpoint)
	entry = tamperedDigest.Combos[key]
	entry.Report.Matches[0].DamageLeft[0]++
	tamperedDigest.Combos[key] = entry
	digestVerification, err := VerifyCheckpoint(project, tamperedDigest)
	if err != nil {
		t.Fatal(err)
	}
	if digestVerification.OK || digestVerification.FirstItem != "match.digest" {
		t.Fatalf("verifier did not detect digest tampering: %#v", digestVerification)
	}

	badBudget := cloneJSON(t, checkpoint)
	entry = badBudget.Combos[key]
	entry.Report.AllocatedMatches++
	badBudget.Combos[key] = entry
	budgetVerification, err := VerifyCheckpoint(project, badBudget)
	if err != nil {
		t.Fatal(err)
	}
	if budgetVerification.OK || budgetVerification.FirstItem != "budget.allocatedMatches" {
		t.Fatalf("verifier did not check budget consumption: %#v", budgetVerification)
	}

	modified := cloneJSON(t, report)
	firstPlan := modified.Plans[0]
	firstCombo := firstPlan.Combos[0]
	firstMatch := &firstCombo.Matches[0]
	if firstMatch.Winner == "left" {
		firstMatch.Winner = "right"
	} else {
		firstMatch.Winner = "left"
	}
	diff := CompareReports(report, modified)
	assertCode(t, diff, CodeDiff)
	appErr := AsAppError(diff)
	if appErr == nil || appErr.MatchIndex != firstMatch.MatchIndex || appErr.Field != "winner" || appErr.PlanID != firstPlan.PlanID || appErr.ScenarioID != firstCombo.ScenarioID {
		t.Fatalf("diff did not locate the first mismatch: %#v", appErr)
	}
}

func findComboReport(t *testing.T, report *Report, planID, scenarioID string) ComboReport {
	t.Helper()
	for _, plan := range report.Plans {
		if plan.PlanID != planID {
			continue
		}
		for _, combo := range plan.Combos {
			if combo.ScenarioID == scenarioID {
				return combo
			}
		}
	}
	t.Fatalf("combination %s/%s not found", planID, scenarioID)
	return ComboReport{}
}

func findPlanReport(t *testing.T, report *Report, planID string) PlanReport {
	t.Helper()
	for _, plan := range report.Plans {
		if plan.PlanID == planID {
			return plan
		}
	}
	t.Fatalf("plan %s not found", planID)
	return PlanReport{}
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %s, got nil", want)
	}
	if got := ErrorCode(err); got != want {
		t.Fatalf("error code = %s, want %s: %v", got, want, err)
	}
}

func cloneProject(t *testing.T, project *Project) *Project {
	t.Helper()
	return cloneJSON(t, project)
}

func cloneJSON[T any](t *testing.T, value *T) *T {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var cloned T
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return &cloned
}

func projectAsMap(t *testing.T, project *Project) map[string]any {
	t.Helper()
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
