package engine

import (
	"sort"

	"chaosdiag/model"
)

func Run(p *model.Project) *model.Report {
	schedules := Schedule(p)
	active := ActiveMap(schedules)
	results := SimulateAll(p, active)
	ticks := make([]int, 0, len(schedules))
	for _, s := range schedules {
		ticks = append(ticks, s.Tick)
	}
	return &model.Report{
		ProjectID: p.ProjectID, ProjectVersion: p.ProjectVersion, Seed: p.Seed,
		Ticks: ticks, Schedules: schedules, Results: results,
	}
}

func SimulateAll(p *model.Project, activeByTick map[int]map[string]bool) []model.RequestResult {
	requests := append([]model.Request{}, p.Requests...)
	sort.SliceStable(requests, func(i, j int) bool { return requests[i].Sequence < requests[j].Sequence })
	out := make([]model.RequestResult, 0, len(requests))
	for _, req := range requests {
		active := activeByTick[req.Tick]
		if active == nil {
			active = map[string]bool{}
		}
		out = append(out, SimulateRequest(p, req, active))
	}
	return out
}

func SimulateRequest(p *model.Project, req model.Request, active map[string]bool) model.RequestResult {
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
		edge, edgeOK := p.EdgeByID(target.EdgeID)
		if !edgeOK {
			if found, ok := p.EdgeBetween(target.From, target.To); ok {
				edge = found
				edgeOK = true
			}
		}
		toSvc := FindService(p, target.To)
		base := toSvc.LatencyMS
		if edgeOK {
			base += edge.LatencyMS
		}

		matched := MatchHopInstructions(p, req.Tick, target, active)
		for _, ins := range matched {
			hitSet[ins.ID] = true
		}
		hop := model.HopResult{
			Index: hopIndex, EdgeID: target.EdgeID, From: target.From, To: target.To,
			StartMS: elapsed, BaseMS: base, MatchedInstructionIDs: InstructionIDs(matched),
			Adjudication: model.Adjudication{
				RankedInstructionIDs: InstructionIDs(matched),
				Rule:                 "priority_desc then declaration_sequence_asc; one winner per effect dimension",
			},
		}

		exempted := map[string]bool{}
		hits := ExemptionHits(p, target, matched)
		for _, hit := range hits {
			hop.ExemptionHits = append(hop.ExemptionHits, hit)
			exempted[hit.InstructionID] = true
			exemptSet[hit.InstructionID] = true
		}

		candidates := make([]model.Instruction, 0, len(matched))
		for _, ins := range matched {
			if !exempted[ins.ID] {
				candidates = append(candidates, ins)
			}
		}
		winners := Adjudicate(candidates)
		dimensions := make([]string, 0, len(winners))
		for dim := range winners {
			dimensions = append(dimensions, dim)
		}
		sort.Strings(dimensions)
		for _, dim := range dimensions {
			winner := winners[dim]
			hop.Adjudication.Decisions = append(hop.Adjudication.Decisions, model.EffectDecision{
				Dimension: dim, WinnerID: winner.ID, CandidateIDs: CandidateIDs(candidates, dim),
				Rule: "highest priority then earliest sequence",
			})
			hop.AppliedInstructionIDs = append(hop.AppliedInstructionIDs, winner.ID)
			appliedSet[winner.ID] = true
		}
		sort.Strings(hop.AppliedInstructionIDs)

		var delayMS, timeoutMS, repeatDelayMS int64
		var repeatCount, reorderDepth int
		var failureCode, failureID string
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
		deadline := result.BudgetMS
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
				result.Status = model.StatusFailure
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

func MatchHopInstructions(p *model.Project, tick int, target model.HopTarget, active map[string]bool) []model.Instruction {
	matched := []model.Instruction{}
	for _, ins := range p.Instructions {
		if active != nil && !active[ins.ID] {
			continue
		}
		if tick < ins.StartTick || tick > ins.EndTick {
			continue
		}
		if model.SelectorMatches(ins.Selector, target) {
			matched = append(matched, ins)
		}
	}
	RankInstructions(matched)
	return matched
}

func ExemptionHits(p *model.Project, target model.HopTarget, matched []model.Instruction) []model.ExemptionHit {
	hits := []model.ExemptionHit{}
	for _, ex := range p.Exemptions {
		if !model.SelectorMatches(ex.Selector, target) {
			continue
		}
		for _, ins := range matched {
			if ex.InstructionID != "" && ex.InstructionID != ins.ID {
				continue
			}
			hits = append(hits, model.ExemptionHit{
				ExemptionID: ex.ID, Source: ex.Source, InstructionID: ins.ID, Reason: ex.Reason,
			})
		}
	}
	return hits
}

func Adjudicate(candidates []model.Instruction) map[string]model.Instruction {
	RankInstructions(candidates)
	winners := map[string]model.Instruction{}
	for _, ins := range candidates {
		if _, exists := winners[ins.Kind]; !exists {
			winners[ins.Kind] = ins
		}
	}
	return winners
}

func RankInstructions(in []model.Instruction) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Priority != in[j].Priority {
			return in[i].Priority > in[j].Priority
		}
		return in[i].Sequence < in[j].Sequence
	})
}

func CandidateIDs(in []model.Instruction, kind string) []string {
	ids := []string{}
	for _, ins := range in {
		if ins.Kind == kind {
			ids = append(ids, ins.ID)
		}
	}
	return ids
}

func InstructionIDs(in []model.Instruction) []string {
	out := make([]string, 0, len(in))
	for _, ins := range in {
		out = append(out, ins.ID)
	}
	return out
}

func FindService(p *model.Project, name string) model.Service {
	for _, svc := range p.Services {
		if svc.Name == name {
			return svc
		}
	}
	return model.Service{Name: name}
}

func collectIDs(result *model.RequestResult, hit, applied, exempt map[string]bool) {
	result.HitInstructionIDs = sortedKeys(hit)
	result.AppliedInstructionIDs = sortedKeys(applied)
	result.ExemptionInstructionIDs = sortedKeys(exempt)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
