package balance

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

type RunMode string

const (
	ModeFull     RunMode = "full"
	ModeRealtime RunMode = "realtime"
	ModeResume   RunMode = "resume"
)

type RunOptions struct {
	Mode             RunMode
	CheckpointPath   string
	StopAfterCommits int
	Injection        Injection
	RetryLimit       int
}

type matchJob struct {
	Key        string
	PlanID     string
	ScenarioID string
	MatchIndex int
	Round      int
}

type matchJobResult struct {
	Job    matchJob
	Report *MatchReport
	Err    error
}

type schedulerUsage struct {
	MaxSlotsUsed       int
	QueuedCount        int64
	QueueReasons       []QueueReason
	MemoryBackpressure int64
	PeakBuffer         int
}

type comboAccumulator struct {
	Budget  *BudgetCombo
	Matches []MatchReport
	Rounds  []RoundBudgetStat
	Failure *AppError
}

func Run(ctx context.Context, project *Project, options RunOptions) (*Report, error) {
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	if options.Mode == "" {
		options.Mode = ModeFull
	}
	if options.RetryLimit <= 0 {
		options.RetryLimit = 3
	}
	if options.Mode == ModeRealtime || options.Mode == ModeResume {
		if options.CheckpointPath == "" {
			return nil, &AppError{Code: CodeCheckpoint, Object: "checkpoint", Detail: "checkpoint path is required for realtime/resume mode"}
		}
	}
	budgetPlan, err := BuildBudgetPlan(project)
	if err != nil {
		return nil, err
	}
	prepared := make(map[string]*preparedPlan, len(project.Plans))
	for _, plan := range project.Plans {
		item, err := preparePlan(project, plan)
		if err != nil {
			return nil, err
		}
		prepared[plan.ID] = item
	}
	builds := make(map[string]Build, len(project.Builds))
	for _, build := range project.Builds {
		builds[build.ID] = build
	}
	scenarios := make(map[string]Scenario, len(project.Scenarios))
	for _, scenario := range project.Scenarios {
		scenarios[scenario.ID] = scenario
	}

	status := CheckpointStatus{Mode: options.Mode, Path: options.CheckpointPath}
	var checkpoint *Checkpoint
	loadedValidCheckpoint := false
	if options.Mode == ModeResume {
		loaded, invalidated, reason, err := LoadCheckpoint(options.CheckpointPath, project)
		if err != nil {
			return nil, err
		}
		if loaded != nil && !invalidated {
			if err := validateResumeCheckpoint(loaded, project, budgetPlan); err != nil {
				status.Invalidated = true
				status.InvalidReason = "checkpoint structure invalid: " + err.Error()
			} else {
				checkpoint = loaded
				status.Loaded = true
				loadedValidCheckpoint = true
			}
		} else {
			status.Invalidated = true
			status.InvalidReason = reason
		}
	}
	if checkpoint == nil {
		checkpoint = newCheckpoint(project)
	}
	var store *CheckpointStore
	if options.Mode == ModeRealtime || options.Mode == ModeResume {
		store = NewCheckpointStore(options.CheckpointPath, checkpoint)
		if !loadedValidCheckpoint {
			if err := store.Initialize(); err != nil {
				return nil, err
			}
		}
	}

	tracker := NewStateTracker(project)
	committed := make(map[string]ComboReport)
	for key, entry := range checkpoint.Combos {
		report := entry.Report
		if err := tracker.RestoreCommittedCombo(report.PlanID, report.ScenarioID, report); err != nil {
			return nil, err
		}
		committed[key] = report
	}
	for planID, entry := range checkpoint.Plans {
		if err := tracker.RestoreCommittedPlan(planID, entry); err != nil {
			return nil, err
		}
	}

	planIndex := make(map[string]int, len(project.Plans))
	for index, plan := range project.Plans {
		planIndex[plan.ID] = index
	}
	scenarioIndex := make(map[string]int, len(project.Scenarios))
	for index, scenario := range project.Scenarios {
		scenarioIndex[scenario.ID] = index
	}

	combos := make(map[string]*comboAccumulator, len(budgetPlan.Combos))
	uncommitted := make([]*BudgetCombo, 0)
	for _, combo := range budgetPlan.Combos {
		key := combo.Key()
		combos[key] = &comboAccumulator{Budget: combo}
		if _, ok := committed[key]; !ok {
			uncommitted = append(uncommitted, combo)
			if tracker.PlanState(combo.PlanID) == PlanPending {
				if err := tracker.TransitionPlan(combo.PlanID, PlanRunning); err != nil {
					return nil, err
				}
			}
			if err := tracker.TransitionCombo(combo.PlanID, combo.ScenarioID, ComboRunning); err != nil {
				return nil, err
			}
		}
	}

	roundJobs := buildRoundJobs(budgetPlan, committed)
	usage := schedulerUsage{}
	firstRuntimeError := error(nil)
	for round := range roundJobs {
		if len(roundJobs[round]) == 0 {
			continue
		}
		results, roundUsage, err := runJobs(ctx, roundJobs[round], project, prepared, builds, scenarios, options, budgetPlan)
		mergeUsage(&usage, roundUsage)
		if err != nil && firstRuntimeError == nil {
			firstRuntimeError = err
		}
		roundMatches := make(map[string]int)
		for _, result := range results {
			item := combos[result.Job.Key]
			if item == nil {
				continue
			}
			roundMatches[result.Job.Key]++
			if result.Err != nil {
				if item.Failure == nil {
					item.Failure = AsAppError(result.Err)
				}
				continue
			}
			item.Matches = append(item.Matches, *result.Report)
		}
		for _, combo := range budgetPlan.Combos {
			key := combo.Key()
			added := roundMatches[key]
			if added == 0 {
				continue
			}
			item := combos[key]
			sort.SliceStable(item.Matches, func(i, j int) bool {
				return item.Matches[i].MatchIndex < item.Matches[j].MatchIndex
			})
			wins, _, _ := summarizeMatches(item.Matches)
			ci := confidenceInterval(int64(wins), int64(len(item.Matches)))
			item.Rounds = append(item.Rounds, RoundBudgetStat{
				Round:             round + 1,
				AddedMatches:      added,
				CumulativeMatches: len(item.Matches),
				Wins:              wins,
				WinRatePPM:        ratePPM(int64(wins), int64(len(item.Matches))),
				CIWidthPPM:        ci.WidthPPM,
				ChangeSamples:     added,
			})
		}
		if err != nil {
			break
		}
	}

	reports := make(map[string]ComboReport, len(budgetPlan.Combos))
	for key, report := range committed {
		reports[key] = report
	}
	for _, combo := range uncommitted {
		key := combo.Key()
		item := combos[key]
		report := finalizeComboReport(item)
		if ciErr := ValidateCIWidths(report); ciErr != nil {
			report.Failure = &FailureInfo{Code: CodeBudget, Object: report.Key(), Detail: ciErr.Error()}
			if item.Failure == nil {
				item.Failure = AsAppError(ciErr)
			}
		}
		if item.Failure != nil {
			if err := tracker.TransitionCombo(combo.PlanID, combo.ScenarioID, ComboFailed); err != nil {
				return nil, err
			}
			report.State = ComboFailed
			if report.Failure == nil {
				report.Failure = &FailureInfo{
					Code:       item.Failure.Code,
					Object:     item.Failure.Object,
					MatchIndex: item.Failure.MatchIndex,
					Turn:       item.Failure.Turn,
					Detail:     item.Failure.Detail,
				}
			}
			if firstRuntimeError == nil {
				firstRuntimeError = item.Failure
			}
		}
		reports[key] = report
	}

	commitOrder := append([]*BudgetCombo(nil), uncommitted...)
	sort.SliceStable(commitOrder, func(i, j int) bool {
		if planIndex[commitOrder[i].PlanID] != planIndex[commitOrder[j].PlanID] {
			return planIndex[commitOrder[i].PlanID] < planIndex[commitOrder[j].PlanID]
		}
		return scenarioIndex[commitOrder[i].ScenarioID] < scenarioIndex[commitOrder[j].ScenarioID]
	})
	committedThisRun := 0
	for _, combo := range commitOrder {
		key := combo.Key()
		report := reports[key]
		if report.State == ComboFailed {
			continue
		}
		if options.StopAfterCommits > 0 && committedThisRun >= options.StopAfterCommits {
			break
		}
		if err := tracker.CommitCombo(combo.PlanID, combo.ScenarioID, report); err != nil {
			return nil, err
		}
		report.State = ComboCommitted
		reports[key] = report
		committedThisRun++
		if store != nil {
			if _, err := store.CommitCombo(report); err != nil {
				return nil, err
			}
		}
	}

	stopLimited := options.StopAfterCommits > 0 && committedThisRun >= options.StopAfterCommits
	for _, plan := range project.Plans {
		allCommitted := true
		anyFailed := false
		for _, scenarioID := range plan.TargetScenarios {
			report := reports[ComboID{PlanID: plan.ID, ScenarioID: scenarioID}.Key()]
			if report.State == ComboFailed {
				anyFailed = true
			}
			if report.State != ComboCommitted {
				allCommitted = false
			}
		}
		state := tracker.PlanState(plan.ID)
		if anyFailed {
			if state == PlanRunning {
				if err := tracker.TransitionPlan(plan.ID, PlanFailed); err != nil {
					return nil, err
				}
			}
			continue
		}
		if !allCommitted {
			if stopLimited && state == PlanPending {
				// A plan whose first combination was committed cannot remain
				// pending; this branch only guards unusual partial ordering.
				if err := tracker.TransitionPlan(plan.ID, PlanRunning); err != nil {
					return nil, err
				}
			}
			continue
		}
		if state == PlanCommitted {
			continue
		}
		if state == PlanPending {
			if err := tracker.TransitionPlan(plan.ID, PlanRunning); err != nil {
				return nil, err
			}
		}
		planReport := buildPlanReport(project, plan, reports, tracker)
		if err := tracker.CommitPlan(plan.ID, planReport); err != nil {
			return nil, err
		}
		planReport = buildPlanReport(project, plan, reports, tracker)
		if store != nil {
			if _, err := store.CommitPlan(planReport); err != nil {
				return nil, err
			}
		}
	}

	planReports := make([]PlanReport, 0, len(project.Plans))
	partial := false
	progress := Progress{TotalPlans: len(project.Plans), TotalCombos: len(budgetPlan.Combos)}
	for _, plan := range project.Plans {
		planState := tracker.PlanState(plan.ID)
		if planState == PlanPending || planState == PlanRunning {
			partial = true
		}
		planReport := buildPlanReport(project, plan, reports, tracker)
		if planReport.Committed {
			progress.CommittedPlans++
		}
		progress.CommittedCombos += planReport.CommittedCombos
		planReports = append(planReports, planReport)
	}
	for _, planReport := range planReports {
		for _, combo := range planReport.Combos {
			if combo.State != ComboCommitted {
				partial = true
			}
		}
	}
	sort.SliceStable(planReports, func(i, j int) bool {
		return planIndex[planReports[i].PlanID] < planIndex[planReports[j].PlanID]
	})
	applyThresholds(project, planReports)

	allocatedTotal := int64(0)
	requestedTotal := int64(0)
	completedTotal := int64(0)
	budgetRounds := 0
	for _, combo := range budgetPlan.Combos {
		requestedTotal += int64(combo.Requested)
		allocatedTotal += int64(combo.Allocated)
		report := reports[combo.Key()]
		completedTotal += int64(report.CompletedMatches)
		if len(report.Rounds) > budgetRounds {
			budgetRounds = len(report.Rounds)
		}
	}
	sort.SliceStable(usage.QueueReasons, func(i, j int) bool {
		return usage.QueueReasons[i].Reason < usage.QueueReasons[j].Reason
	})
	if usage.QueueReasons == nil {
		usage.QueueReasons = []QueueReason{}
	}

	if store != nil {
		status.CommitSequence = store.Snapshot().CommitSequence
	}
	report := &Report{
		SchemaVersion:    SchemaVersion,
		ProjectHash:      ProjectContentHash(project),
		AlgorithmVersion: project.AlgorithmVersion,
		BudgetVersion:    project.BudgetVersion,
		Mode:             options.Mode,
		BudgetUsage: BudgetUsage{
			Slots:                 budgetPlan.Slots,
			MemoryBufferPerWorker: budgetPlan.MemoryBuffer,
			RoundCapacity:         budgetPlan.RoundCapacity,
			RequestedMatches:      requestedTotal,
			AllocatedMatches:      allocatedTotal,
			CompletedMatches:      completedTotal,
			MaxSlotsUsed:          usage.MaxSlotsUsed,
			QueuedCount:           usage.QueuedCount,
			QueueReasons:          usage.QueueReasons,
			MemoryBackpressure:    usage.MemoryBackpressure,
			PeakBuffer:            usage.PeakBuffer,
			BudgetRounds:          budgetRounds,
		},
		Checkpoint: status,
		Progress:   progress,
		Plans:      planReports,
		Partial:    partial,
	}
	return report, firstRuntimeError
}

func buildRoundJobs(plan *BudgetPlan, committed map[string]ComboReport) [][]matchJob {
	rounds := make([][]matchJob, plan.MaxRounds)
	nextIndex := make(map[string]int, len(plan.Combos))
	for _, combo := range plan.Combos {
		nextIndex[combo.Key()] = 1
	}
	for round := 0; round < plan.MaxRounds; round++ {
		for _, combo := range plan.Combos {
			key := combo.Key()
			count := 0
			if round < len(combo.RoundCounts) {
				count = combo.RoundCounts[round]
			}
			for index := 0; index < count; index++ {
				matchIndex := nextIndex[key]
				nextIndex[key]++
				if _, isCommitted := committed[key]; isCommitted {
					continue
				}
				rounds[round] = append(rounds[round], matchJob{
					Key:        key,
					PlanID:     combo.PlanID,
					ScenarioID: combo.ScenarioID,
					MatchIndex: matchIndex,
					Round:      round + 1,
				})
			}
		}
	}
	return rounds
}

func runJobs(
	ctx context.Context,
	jobs []matchJob,
	project *Project,
	prepared map[string]*preparedPlan,
	builds map[string]Build,
	scenarios map[string]Scenario,
	options RunOptions,
	budgetPlan *BudgetPlan,
) ([]matchJobResult, schedulerUsage, error) {
	usage := schedulerUsage{}
	if len(jobs) == 0 {
		return nil, usage, nil
	}
	slots := budgetPlan.Slots
	if slots > len(jobs) {
		slots = len(jobs)
	}
	if slots <= 0 {
		return nil, usage, &AppError{Code: CodeBudget, Object: "budget.slots", Detail: "no simulation slots are available"}
	}
	results := make([]matchJobResult, 0, len(jobs))
	semaphore := make(chan struct{}, slots)
	resultCh := make(chan matchJobResult, slots*budgetPlan.MemoryBuffer)
	var (
		wg                 sync.WaitGroup
		active             int64
		maxActive          int64
		queued             int64
		memoryBackpressure int64
		peakBuffer         int64
	)
	ctxErr := error(nil)
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			ctxErr = err
			break
		}
		jobCopy := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
			default:
				atomic.AddInt64(&queued, 1)
				select {
				case semaphore <- struct{}{}:
				case <-ctx.Done():
					resultCh <- matchJobResult{Job: jobCopy, Err: ctx.Err()}
					return
				}
			}
			current := atomic.AddInt64(&active, 1)
			for {
				previous := atomic.LoadInt64(&maxActive)
				if current <= previous || atomic.CompareAndSwapInt64(&maxActive, previous, current) {
					break
				}
			}
			defer func() {
				atomic.AddInt64(&active, -1)
				<-semaphore
			}()
			if err := ctx.Err(); err != nil {
				resultCh <- matchJobResult{Job: jobCopy, Err: err}
				return
			}
			result := executeMatchJob(jobCopy, project, prepared, builds, scenarios, options)
			select {
			case resultCh <- result:
			default:
				atomic.AddInt64(&memoryBackpressure, 1)
				select {
				case resultCh <- result:
				case <-ctx.Done():
					resultCh <- matchJobResult{Job: jobCopy, Err: ctx.Err()}
				}
			}
			if buffered := atomic.LoadInt64(&peakBuffer); int64(len(resultCh)) > buffered {
				atomic.CompareAndSwapInt64(&peakBuffer, buffered, int64(len(resultCh)))
			}
		}()
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()
	for result := range resultCh {
		results = append(results, result)
	}
	usage.MaxSlotsUsed = int(maxActive)
	usage.QueuedCount = queued
	if queued > 0 {
		usage.QueueReasons = append(usage.QueueReasons, QueueReason{Reason: "all_simulation_slots_busy", Count: queued})
	}
	usage.MemoryBackpressure = memoryBackpressure
	if memoryBackpressure > 0 {
		usage.QueueReasons = append(usage.QueueReasons, QueueReason{Reason: "result_buffer_full", Count: memoryBackpressure})
	}
	usage.PeakBuffer = int(peakBuffer)
	return results, usage, ctxErr
}

func executeMatchJob(
	job matchJob,
	project *Project,
	prepared map[string]*preparedPlan,
	builds map[string]Build,
	scenarios map[string]Scenario,
	options RunOptions,
) matchJobResult {
	planPreparation, ok := prepared[job.PlanID]
	if !ok {
		return matchJobResult{Job: job, Err: &AppError{Code: CodeUnit, Object: job.PlanID, PlanID: job.PlanID, ScenarioID: job.ScenarioID, Detail: "prepared plan is missing"}}
	}
	scenario, ok := scenarios[job.ScenarioID]
	if !ok {
		return matchJobResult{Job: job, Err: &AppError{Code: CodeUnit, Object: job.ScenarioID, PlanID: job.PlanID, ScenarioID: job.ScenarioID, Detail: "scenario is missing"}}
	}
	var lastErr error
	for attempt := 1; attempt <= options.RetryLimit; attempt++ {
		report, err := simulateMatch(project.Seed, builds, scenario, planPreparation, job.MatchIndex, options.Injection, attempt)
		if err == nil {
			return matchJobResult{Job: job, Report: report}
		}
		lastErr = err
		if ErrorCode(err) != CodeInternal || attempt == options.RetryLimit {
			break
		}
	}
	return matchJobResult{Job: job, Err: lastErr}
}

func finalizeComboReport(item *comboAccumulator) ComboReport {
	sort.SliceStable(item.Matches, func(i, j int) bool {
		return item.Matches[i].MatchIndex < item.Matches[j].MatchIndex
	})
	wins, losses, draws := summarizeMatches(item.Matches)
	turnCount := 0
	for _, match := range item.Matches {
		turnCount += match.ActionTurns
	}
	report := ComboReport{
		ComboID:          item.Budget.ComboID,
		State:            ComboRunning,
		RequestedMatches: item.Budget.Requested,
		AllocatedMatches: item.Budget.Allocated,
		CompletedMatches: len(item.Matches),
		Wins:             wins,
		Losses:           losses,
		Draws:            draws,
		WinRatePPM:       ratePPM(int64(wins), int64(len(item.Matches))),
		CI:               confidenceInterval(int64(wins), int64(len(item.Matches))),
		Damage:           summarizeDamage(item.Matches),
		Reduction:        item.Budget.Reduction,
		Rounds:           append([]RoundBudgetStat(nil), item.Rounds...),
		Matches:          append([]MatchReport(nil), item.Matches...),
		RNG: RNGProgress{
			MatchCursor: len(item.Matches),
			TurnCount:   turnCount,
		},
	}
	if item.Failure != nil {
		report.State = ComboFailed
		report.Failure = &FailureInfo{
			Code:       item.Failure.Code,
			Object:     item.Failure.Object,
			MatchIndex: item.Failure.MatchIndex,
			Turn:       item.Failure.Turn,
			Detail:     item.Failure.Detail,
		}
	} else if report.CompletedMatches != report.AllocatedMatches {
		item.Failure = &AppError{
			Code:       CodeInternal,
			Object:     "simulation",
			PlanID:     item.Budget.PlanID,
			ScenarioID: item.Budget.ScenarioID,
			Detail:     fmt.Sprintf("completed %d of %d allocated matches", report.CompletedMatches, report.AllocatedMatches),
		}
		report.State = ComboFailed
		report.Failure = &FailureInfo{
			Code:   CodeInternal,
			Object: "simulation",
			Detail: fmt.Sprintf("completed %d of %d allocated matches", report.CompletedMatches, report.AllocatedMatches),
		}
	}
	return report
}

func buildPlanReport(project *Project, plan Plan, reports map[string]ComboReport, tracker *StateTracker) PlanReport {
	report := PlanReport{
		PlanID:      plan.ID,
		Priority:    plan.Priority,
		State:       tracker.PlanState(plan.ID),
		TotalCombos: len(plan.TargetScenarios),
		Combos:      make([]ComboReport, 0, len(plan.TargetScenarios)),
	}
	var allMatches []MatchReport
	for _, scenarioID := range plan.TargetScenarios {
		combo := reports[ComboID{PlanID: plan.ID, ScenarioID: scenarioID}.Key()]
		report.Combos = append(report.Combos, combo)
		report.RequestedMatches += combo.RequestedMatches
		report.AllocatedMatches += combo.AllocatedMatches
		report.CompletedMatches += combo.CompletedMatches
		report.Wins += combo.Wins
		report.Losses += combo.Losses
		report.Draws += combo.Draws
		if combo.State == ComboCommitted {
			report.CommittedCombos++
		}
		allMatches = append(allMatches, combo.Matches...)
	}
	report.Committed = report.State == PlanCommitted
	report.WinRatePPM = ratePPM(int64(report.Wins), int64(report.CompletedMatches))
	report.CI = confidenceInterval(int64(report.Wins), int64(report.CompletedMatches))
	report.Damage = summarizeDamage(allMatches)
	return report
}

func mergeUsage(destination *schedulerUsage, source schedulerUsage) {
	if source.MaxSlotsUsed > destination.MaxSlotsUsed {
		destination.MaxSlotsUsed = source.MaxSlotsUsed
	}
	destination.QueuedCount += source.QueuedCount
	destination.MemoryBackpressure += source.MemoryBackpressure
	if source.PeakBuffer > destination.PeakBuffer {
		destination.PeakBuffer = source.PeakBuffer
	}
	reasonCounts := make(map[string]int64)
	for _, reason := range destination.QueueReasons {
		reasonCounts[reason.Reason] += reason.Count
	}
	for _, reason := range source.QueueReasons {
		reasonCounts[reason.Reason] += reason.Count
	}
	destination.QueueReasons = destination.QueueReasons[:0]
	reasons := make([]string, 0, len(reasonCounts))
	for reason := range reasonCounts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		destination.QueueReasons = append(destination.QueueReasons, QueueReason{Reason: reason, Count: reasonCounts[reason]})
	}
}

func validateResumeCheckpoint(checkpoint *Checkpoint, project *Project, budgetPlan *BudgetPlan) error {
	if checkpoint.CommitSequence < 0 {
		return fmt.Errorf("negative commit sequence")
	}
	maxCommit := int64(0)
	for key, entry := range checkpoint.Combos {
		report := entry.Report
		if key != report.Key() {
			return fmt.Errorf("combination key mismatch for %s", key)
		}
		combo := budgetPlan.ByKey[key]
		if combo == nil {
			return fmt.Errorf("checkpoint contains unknown combination %s", key)
		}
		if report.State != ComboCommitted {
			return fmt.Errorf("checkpoint combination %s is not committed", key)
		}
		if report.RequestedMatches != combo.Requested || report.AllocatedMatches != combo.Allocated {
			return fmt.Errorf("checkpoint combination %s has mismatched budget", key)
		}
		if report.CompletedMatches != len(report.Matches) || report.AllocatedMatches != report.CompletedMatches {
			return fmt.Errorf("checkpoint combination %s has incomplete matches", key)
		}
		wins, losses, draws := summarizeMatches(report.Matches)
		if report.Wins != wins || report.Losses != losses || report.Draws != draws {
			return fmt.Errorf("checkpoint combination %s has mismatched win counts", key)
		}
		if report.WinRatePPM != ratePPM(int64(wins), int64(len(report.Matches))) {
			return fmt.Errorf("checkpoint combination %s has mismatched win rate", key)
		}
		if report.CI != confidenceInterval(int64(wins), int64(len(report.Matches))) {
			return fmt.Errorf("checkpoint combination %s has mismatched confidence interval", key)
		}
		for index, match := range report.Matches {
			if match.MatchIndex != index+1 {
				return fmt.Errorf("checkpoint combination %s has unordered match indices", key)
			}
			if match.Digest != buildMatchDigest(&match) {
				return fmt.Errorf("checkpoint combination %s has invalid match digest", key)
			}
		}
		if entry.CommitSeq <= 0 {
			return fmt.Errorf("checkpoint combination %s has invalid commit sequence", key)
		}
		if entry.CommitSeq > maxCommit {
			maxCommit = entry.CommitSeq
		}
	}
	for planID, entry := range checkpoint.Plans {
		plan := findPlan(project, planID)
		if plan == nil {
			return fmt.Errorf("checkpoint contains unknown plan %s", planID)
		}
		if entry.State != PlanCommitted || entry.PlanID != planID {
			return fmt.Errorf("checkpoint plan %s is not committed", planID)
		}
		for _, scenarioID := range plan.TargetScenarios {
			key := ComboID{PlanID: planID, ScenarioID: scenarioID}.Key()
			if _, ok := checkpoint.Combos[key]; !ok {
				return fmt.Errorf("committed plan %s is missing combination %s", planID, scenarioID)
			}
		}
		if entry.CommitSeq <= 0 {
			return fmt.Errorf("checkpoint plan %s has invalid commit sequence", planID)
		}
		if entry.CommitSeq > maxCommit {
			maxCommit = entry.CommitSeq
		}
	}
	if maxCommit > checkpoint.CommitSequence {
		return fmt.Errorf("checkpoint commit sequence is behind records")
	}
	verification, err := VerifyCheckpoint(project, checkpoint)
	if err != nil {
		return err
	}
	if !verification.OK {
		return fmt.Errorf("independent verification failed first at plan=%s item=%s", verification.FirstPlanID, verification.FirstItem)
	}
	return nil
}

func findPlan(project *Project, planID string) *Plan {
	for index := range project.Plans {
		if project.Plans[index].ID == planID {
			return &project.Plans[index]
		}
	}
	return nil
}

func PlanDamageDigest(report PlanReport) string {
	var buf []byte
	buf = append(buf, report.PlanID...)
	for _, combo := range report.Combos {
		buf = append(buf, combo.Key()...)
		for _, match := range combo.Matches {
			buf = append(buf, match.Digest...)
		}
	}
	return sha256Hex(buf)
}

func planRNGProgress(report PlanReport) RNGProgress {
	progress := RNGProgress{}
	for _, combo := range report.Combos {
		progress.MatchCursor += combo.RNG.MatchCursor
		progress.TurnCount += combo.RNG.TurnCount
	}
	return progress
}
