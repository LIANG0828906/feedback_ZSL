package reconcile

import (
	"fmt"

	"chaosdiag/engine"
	"chaosdiag/model"
)

func ReconcileAll(p *model.Project, actual []model.RequestResult) []model.Verdict {
	byID := map[string]model.RequestResult{}
	for _, r := range actual {
		byID[r.RequestID] = r
	}
	out := make([]model.Verdict, 0, len(p.Requests))
	for _, req := range p.Requests {
		actualResult := byID[req.ID]
		out = append(out, ReconcileOne(p, req, actualResult))
	}
	return out
}

func ReconcileOne(p *model.Project, req model.Request, actual model.RequestResult) model.Verdict {
	baseline := engine.SimulateRequest(p, req, map[string]bool{})
	verdict := model.Verdict{
		RequestID: req.ID, ExpectedStatus: req.Expected.Status, ActualStatus: actual.Status,
		ExpectedCode: req.Expected.Code, ActualCode: actual.Code,
	}

	baselineMatches := terminalMatches(baseline.Status, baseline.Code, req.Expected)
	if !baselineMatches {
		if baselineHasServiceFailure(baseline) {
			verdict.Classification = model.VerdictServiceFailure
			verdict.Consistent = false
			verdict.Reason = "declared service self-failure explains the mismatch before chaos instructions"
			verdict.DeviationInstructionIDs = serviceFailureSources(baseline)
			return verdict
		}
		verdict.Classification = model.VerdictExpectationInvalid
		verdict.Consistent = false
		verdict.Reason = "topology and deterministic baseline do not satisfy the declared expectation"
		verdict.DeviationMS = actual.TotalMS - baseline.TotalMS
		return verdict
	}

	instructionDeviations := buildDeviations(p, actual)
	verdict.Deviations = instructionDeviations
	sourceIDs := []string{}
	for _, d := range instructionDeviations {
		if d.InstructionID != "" {
			sourceIDs = append(sourceIDs, d.InstructionID)
		}
	}
	sourceIDs = uniqueSorted(sourceIDs)
	verdict.DeviationInstructionIDs = sourceIDs

	terminalOK := terminalMatches(actual.Status, actual.Code, req.Expected)
	latencyViolation := req.Expected.MaxTotalMS > 0 && actual.TotalMS > req.Expected.MaxTotalMS
	repeatCount := totalRepeats(actual)
	repeatViolation := repeatCount > req.Expected.MaxRepeats

	if !terminalOK || latencyViolation || repeatViolation {
		verdict.Classification = model.VerdictInjection
		verdict.Consistent = false
		switch {
		case latencyViolation:
			verdict.DeviationMS = actual.TotalMS - req.Expected.MaxTotalMS
			verdict.Reason = fmt.Sprintf("injection caused total %dms to exceed expected bound %dms", actual.TotalMS, req.Expected.MaxTotalMS)
		case repeatViolation:
			verdict.DeviationMS = actual.TotalMS - baseline.TotalMS
			verdict.Reason = fmt.Sprintf("injection caused %d duplicate calls, expected at most %d", repeatCount, req.Expected.MaxRepeats)
		default:
			verdict.DeviationMS = actual.TotalMS - baseline.TotalMS
			verdict.Reason = "injection changed terminal status or error code from the valid baseline expectation"
		}
		if len(sourceIDs) == 0 {
			verdict.DeviationInstructionIDs = actual.AppliedInstructionIDs
		}
		return verdict
	}

	verdict.Classification = model.VerdictMatch
	verdict.Consistent = true
	verdict.DeviationMS = actual.TotalMS - baseline.TotalMS
	if len(sourceIDs) > 0 {
		verdict.Reason = "terminal result matches; deterministic injection effects are traceable as non-terminal deviations"
	} else {
		verdict.Reason = "actual result matches expectation and no effective injection deviation remains"
	}
	return verdict
}

func terminalMatches(status, code string, expected model.ExpectedResult) bool {
	if status != expected.Status {
		return false
	}
	if expected.Code != "" && code != expected.Code {
		return false
	}
	return true
}

func baselineHasServiceFailure(r model.RequestResult) bool {
	for _, hop := range r.Hops {
		if len(hop.Source) >= 8 && hop.Source[:8] == "service:" {
			return true
		}
	}
	return false
}

func serviceFailureSources(r model.RequestResult) []string {
	out := []string{}
	for _, hop := range r.Hops {
		if len(hop.Source) >= 8 && hop.Source[:8] == "service:" {
			out = append(out, hop.Source)
		}
	}
	return uniqueSorted(out)
}

func totalRepeats(r model.RequestResult) int {
	total := 0
	for _, hop := range r.Hops {
		total += hop.RepeatCount
	}
	return total
}

func buildDeviations(p *model.Project, r model.RequestResult) []model.Deviation {
	out := []model.Deviation{}
	for _, hop := range r.Hops {
		for _, id := range hop.AppliedInstructionIDs {
			ins := engine.FindInstruction(p, id)
			if ins == nil {
				continue
			}
			d := model.Deviation{InstructionID: id, Kind: ins.Kind, Field: ins.Kind}
			switch ins.Kind {
			case model.KindDelay:
				d.Field = "delay_ms"
				d.AmountMS = ins.DelayMS
				d.Detail = fmt.Sprintf("declared delay=%dms; hop correction=%dms", ins.DelayMS, hop.CorrectionMS)
			case model.KindTimeout:
				d.Field = "timeout_ms"
				d.AmountMS = ins.TimeoutMS
				d.Detail = fmt.Sprintf("injected hop deadline=%dms; actual hop ended at %dms", ins.TimeoutMS, hop.EndMS)
			case model.KindDuplicate:
				d.Field = "repeat_count"
				d.AmountCount = ins.RepeatCount
				d.AmountMS = hop.AppliedRepeatMS
				d.Detail = fmt.Sprintf("%d duplicate calls added %dms", ins.RepeatCount, hop.AppliedRepeatMS)
			case model.KindReorder:
				d.Field = "reorder_depth"
				d.AmountCount = ins.ReorderDepth
				d.Detail = "deterministic reorder was adjudicated and recorded"
			case model.KindFailure:
				d.Field = "failure_code"
				d.Detail = "injected failure code " + ins.FailureCode
			}
			out = append(out, d)
		}
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
