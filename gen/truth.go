package gen

import (
	"sort"

	"chaosdiag/model"
)

type Truth struct {
	Seed             int64                 `json:"seed"`
	ProjectVersion   string                `json:"project_version"`
	Ticks            []int                 `json:"ticks"`
	Schedules        []model.TickSchedule  `json:"schedules"`
	Results          []model.RequestResult `json:"results"`
	ExpectedVerdicts map[string]string     `json:"expected_verdicts"`
}

func BuildTruth(p *model.Project) *Truth {
	ticks := make([]int, 0)
	for tick := 1; tick <= 12; tick++ {
		ticks = append(ticks, tick)
	}
	schedules := planTruthSchedule(p, ticks)
	activeByTick := map[int]map[string]bool{}
	for _, s := range schedules {
		m := map[string]bool{}
		for _, id := range s.Active {
			m[id] = true
		}
		activeByTick[s.Tick] = m
	}

	results := make([]model.RequestResult, 0, len(p.Requests))
	for _, req := range p.Requests {
		results = append(results, simulateTruthRequest(p, req, activeByTick[req.Tick]))
	}

	return &Truth{
		Seed: p.Seed, ProjectVersion: p.ProjectVersion, Ticks: ticks, Schedules: schedules, Results: results,
		ExpectedVerdicts: map[string]string{
			"R-01-BASE-AUTH":        model.VerdictMatch,
			"R-02-BASE-INV":         model.VerdictMatch,
			"R-03-LATENCY-SLO":      model.VerdictInjection,
			"R-04-INJECTED-FAIL":    model.VerdictInjection,
			"R-05-HOP-TIMEOUT":      model.VerdictInjection,
			"R-06-BUDGET-TIMEOUT":   model.VerdictInjection,
			"R-07-EXEMPT-CB":        model.VerdictMatch,
			"R-08-SERVICE-SELF":     model.VerdictServiceFailure,
			"R-09-BAD-EXPECTATION":  model.VerdictExpectationInvalid,
			"R-10-DUPLICATE":        model.VerdictInjection,
			"R-11-REORDER":          model.VerdictMatch,
			"R-12-QUEUED-PAY-FIRST": model.VerdictMatch,
			"R-13-RETRY-PAY":        model.VerdictMatch,
			"R-14-RETRY-INV":        model.VerdictMatch,
		},
	}
}

func planTruthSchedule(p *model.Project, ticks []int) []model.TickSchedule {
	active := map[string]bool{}
	usedBudget := int64(0)
	queue := []model.QueueItem{}
	queueSeq := 0
	out := []model.TickSchedule{}

	for _, tick := range ticks {
		schedule := model.TickSchedule{Tick: tick}
		released := []string{}
		for id := range active {
			ins := findInstruction(p, id)
			if ins != nil && ins.EndTick < tick {
				released = append(released, id)
			}
		}
		sortStrings(released)
		for _, id := range released {
			delete(active, id)
			usedBudget -= findInstruction(p, id).BudgetCostMS
		}
		schedule.Released = released

		retry := queue
		queue = []model.QueueItem{}
		stillQueued := []model.QueueItem{}
		for _, item := range retry {
			ins := findInstruction(p, item.InstructionID)
			if ins == nil {
				continue
			}
			violations := truthViolations(p, *ins, active, usedBudget)
			if len(violations) == 0 {
				active[ins.ID] = true
				usedBudget += ins.BudgetCostMS
				schedule.Admitted = append(schedule.Admitted, ins.ID)
			} else {
				queueSeq++
				item.Position = queueSeq
				item.ViolatedConstraints = violations
				item.ConstraintID = violations[0]
				stillQueued = append(stillQueued, item)
			}
		}
		queue = stillQueued

		submitted := []model.Instruction{}
		for _, ins := range p.Instructions {
			if ins.SubmittedTick == tick {
				submitted = append(submitted, ins)
				schedule.Submitted = append(schedule.Submitted, ins.ID)
			}
		}
		sort.SliceStable(submitted, func(i, j int) bool { return submitted[i].Sequence < submitted[j].Sequence })
		for _, ins := range submitted {
			if active[ins.ID] {
				continue
			}
			violations := truthViolations(p, ins, active, usedBudget)
			if len(violations) == 0 {
				active[ins.ID] = true
				usedBudget += ins.BudgetCostMS
				schedule.Admitted = append(schedule.Admitted, ins.ID)
			} else {
				queueSeq++
				item := model.QueueItem{
					InstructionID: ins.ID, Sequence: ins.Sequence, Position: queueSeq, SinceTick: tick,
					ConstraintID: violations[0], ViolatedConstraints: violations,
					Reason: "instruction exceeds deterministic scheduling guardrails",
				}
				queue = append(queue, item)
			}
		}

		for _, item := range queue {
			schedule.Queued = append(schedule.Queued, item)
		}
		activeIDs := make([]string, 0, len(active))
		for id := range active {
			activeIDs = append(activeIDs, id)
		}
		sortStrings(activeIDs)
		schedule.Active = activeIDs
		out = append(out, schedule)
	}
	return out
}

func truthViolations(p *model.Project, ins model.Instruction, active map[string]bool, usedBudget int64) []string {
	violations := []string{}
	targetSvc := truthTargetService(p, ins)
	serviceActive := 0
	globalActive := 0
	for id := range active {
		globalActive++
		other := findInstruction(p, id)
		if other != nil && truthTargetService(p, *other) == targetSvc {
			serviceActive++
		}
	}
	if cap := p.ServiceConcurrency(targetSvc); serviceActive >= cap {
		violations = append(violations, "CONCURRENCY-SERVICE:"+targetSvc)
	}
	if globalActive >= p.Limits.GlobalConcurrent {
		violations = append(violations, "CONCURRENCY-GLOBAL")
	}
	if usedBudget+ins.BudgetCostMS > p.AdmissionTimeoutBudgetMS {
		violations = append(violations, "TIMEOUT-BUDGET-ADMISSION")
	}
	return violations
}

func truthTargetService(p *model.Project, ins model.Instruction) string {
	if ins.Selector.Kind == model.TargetService {
		return ins.Selector.Service
	}
	if edge, ok := p.EdgeByID(ins.Selector.EdgeID); ok {
		return edge.To
	}
	return ins.Selector.To
}

func simulateTruthRequest(p *model.Project, req model.Request, active map[string]bool) model.RequestResult {
	result := model.RequestResult{
		RequestID: req.ID, Sequence: req.Sequence, Tick: req.Tick, BudgetMS: p.RequestBudget(req),
		Status: model.StatusCompleted,
	}
	elapsed := int64(0)
	hitSet := map[string]bool{}
	appliedSet := map[string]bool{}
	exemptSet := map[string]bool{}

	for hopIndex := 0; hopIndex+1 < len(req.Path); hopIndex++ {
		target, ok := p.HopTarget(req, hopIndex)
		if !ok {
			result.Status = model.StatusFailure
			result.Code = "E_INVALID_PATH"
			break
		}
		edge, _ := p.EdgeByID(target.EdgeID)
		toSvc := findService(p, target.To)
		base := edge.LatencyMS + toSvc.LatencyMS

		matched := []model.Instruction{}
		for _, ins := range p.Instructions {
			if !active[ins.ID] {
				continue
			}
			if req.Tick < ins.StartTick || req.Tick > ins.EndTick {
				continue
			}
			if model.SelectorMatches(ins.Selector, target) {
				matched = append(matched, ins)
				hitSet[ins.ID] = true
			}
		}
		sortInstructionsRanked(matched)

		hop := model.HopResult{
			Index: hopIndex, EdgeID: target.EdgeID, From: target.From, To: target.To,
			StartMS: elapsed, BaseMS: base, MatchedInstructionIDs: instructionIDs(matched),
			Adjudication: model.Adjudication{
				RankedInstructionIDs: instructionIDs(matched),
				Rule:                 "priority_desc then declaration_sequence_asc; one winner per effect dimension",
			},
		}

		exemptMatched := map[string]bool{}
		for _, ex := range p.Exemptions {
			if !model.SelectorMatches(ex.Selector, target) {
				continue
			}
			for _, ins := range matched {
				if ex.InstructionID != "" && ex.InstructionID != ins.ID {
					continue
				}
				exemptMatched[ins.ID] = true
				exemptSet[ins.ID] = true
				hop.ExemptionHits = append(hop.ExemptionHits, model.ExemptionHit{
					ExemptionID: ex.ID, Source: ex.Source, InstructionID: ins.ID, Reason: ex.Reason,
				})
			}
		}

		applied := []model.Instruction{}
		for _, ins := range matched {
			if !exemptMatched[ins.ID] {
				applied = append(applied, ins)
			}
		}
		winners := winnersByDimension(applied)
		for dimension, winner := range winners {
			hop.Adjudication.Decisions = append(hop.Adjudication.Decisions, model.EffectDecision{
				Dimension: dimension, WinnerID: winner.ID, CandidateIDs: candidateIDs(applied, dimension),
				Rule: "highest priority then earliest sequence",
			})
			hop.AppliedInstructionIDs = append(hop.AppliedInstructionIDs, winner.ID)
			appliedSet[winner.ID] = true
		}
		sortStrings(hop.AppliedInstructionIDs)
		sortDecisions(hop.Adjudication.Decisions)

		var delayMS, timeoutMS, repeatDelayMS int64
		var repeatCount, reorderDepth int
		failureCode := ""
		failureID := ""
		if w, ok := winners[model.KindDelay]; ok {
			delayMS = w.DelayMS
			hop.AppliedDelayMS = delayMS
		}
		if w, ok := winners[model.KindTimeout]; ok {
			timeoutMS = w.TimeoutMS
			hop.TimeoutMS = timeoutMS
		}
		if w, ok := winners[model.KindDuplicate]; ok {
			repeatCount = w.RepeatCount
			repeatDelayMS = int64(w.RepeatCount) * w.RepeatDelayMS
			hop.RepeatCount = repeatCount
			hop.AppliedRepeatMS = repeatDelayMS
		}
		if w, ok := winners[model.KindReorder]; ok {
			reorderDepth = w.ReorderDepth
			hop.ReorderDepth = reorderDepth
		}
		if w, ok := winners[model.KindFailure]; ok {
			failureCode = w.FailureCode
			failureID = w.ID
		}

		natural := base + delayMS + repeatDelayMS
		hop.NaturalMS = natural
		naturalEnd := elapsed + natural
		budgetDeadline := result.BudgetMS
		deadline := budgetDeadline
		timeoutSource := "constraint:TIMEOUT-BUDGET-REQUEST"
		timeoutCode := "CHAOS_REQUEST_BUDGET_TIMEOUT"
		if timeoutMS > 0 {
			hopDeadline := elapsed + timeoutMS
			if hopDeadline <= deadline {
				deadline = hopDeadline
				timeoutSource = "instruction:" + winners[model.KindTimeout].ID
				timeoutCode = "CHAOS_INJECTED_HOP_TIMEOUT"
			}
		}

		if failureID != "" && naturalEnd <= deadline {
			hop.EndMS = naturalEnd
			hop.CorrectionMS = naturalEnd - elapsed - base
			hop.Status = model.StatusFailure
			hop.Code = failureCode
			hop.Source = "instruction:" + failureID
			result.Hops = append(result.Hops, hop)
			result.CompletedHops = hopIndex
			result.Status = model.StatusFailure
			result.Code = failureCode
			result.TotalMS = hop.EndMS
			collectIDs(&result, hitSet, appliedSet, exemptSet)
			return result
		}

		if naturalEnd <= deadline {
			hop.EndMS = naturalEnd
			hop.CorrectionMS = naturalEnd - elapsed - base
			hop.Status = model.StatusCompleted
			if toSvc.SelfErrorCode != "" {
				hop.Status = model.StatusFailure
				hop.Code = toSvc.SelfErrorCode
				hop.Source = "service:" + toSvc.Name
				result.Hops = append(result.Hops, hop)
				result.CompletedHops = hopIndex
				result.Status = model.StatusFailure
				result.Code = toSvc.SelfErrorCode
				result.TotalMS = hop.EndMS
				collectIDs(&result, hitSet, appliedSet, exemptSet)
				return result
			}
			result.Hops = append(result.Hops, hop)
			elapsed = hop.EndMS
			continue
		}

		hop.EndMS = deadline
		hop.CorrectionMS = deadline - elapsed - base
		hop.Status = model.StatusTimeout
		hop.Code = timeoutCode
		hop.Source = timeoutSource
		result.Hops = append(result.Hops, hop)
		result.CompletedHops = hopIndex
		result.Status = model.StatusTimeout
		result.Code = timeoutCode
		result.TotalMS = deadline
		collectIDs(&result, hitSet, appliedSet, exemptSet)
		return result
	}

	result.TotalMS = elapsed
	result.CompletedHops = len(result.Hops)
	collectIDs(&result, hitSet, appliedSet, exemptSet)
	return result
}

func collectIDs(result *model.RequestResult, hit, applied, exempt map[string]bool) {
	result.HitInstructionIDs = sortedKeys(hit)
	result.AppliedInstructionIDs = sortedKeys(applied)
	result.ExemptionInstructionIDs = sortedKeys(exempt)
}

func winnersByDimension(applied []model.Instruction) map[string]model.Instruction {
	out := map[string]model.Instruction{}
	for _, ins := range applied {
		dim := ins.Kind
		if _, exists := out[dim]; !exists {
			out[dim] = ins
		}
	}
	return out
}

func candidateIDs(applied []model.Instruction, kind string) []string {
	ids := []string{}
	for _, ins := range applied {
		if ins.Kind == kind {
			ids = append(ids, ins.ID)
		}
	}
	return ids
}

func sortInstructionsRanked(in []model.Instruction) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Priority != in[j].Priority {
			return in[i].Priority > in[j].Priority
		}
		return in[i].Sequence < in[j].Sequence
	})
}

func sortDecisions(in []model.EffectDecision) {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Dimension < in[j].Dimension })
}

func instructionIDs(in []model.Instruction) []string {
	out := make([]string, 0, len(in))
	for _, ins := range in {
		out = append(out, ins.ID)
	}
	return out
}

func findInstruction(p *model.Project, id string) *model.Instruction {
	for i := range p.Instructions {
		if p.Instructions[i].ID == id {
			return &p.Instructions[i]
		}
	}
	return nil
}

func findService(p *model.Project, name string) model.Service {
	for _, svc := range p.Services {
		if svc.Name == name {
			return svc
		}
	}
	return model.Service{Name: name}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) { sort.Strings(s) }
