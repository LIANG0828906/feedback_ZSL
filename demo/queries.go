package demo

import (
	"fmt"

	"alertkernel/engine"
	"alertkernel/model"
)

func stateAt(report *engine.RunReport, t int, ruleID string, subset model.Labels) *engine.State {
	if report == nil || t < 0 || t >= len(report.Times) {
		return nil
	}
	for i := range report.Times[t].States {
		state := &report.Times[t].States[i]
		if state.RuleID == ruleID && labelsContain(state.Labels, subset) {
			return state
		}
	}
	return nil
}

func observationAt(report *engine.RunReport, t int, ruleID string, subset model.Labels) *engine.Observation {
	if report == nil || t < 0 || t >= len(report.Times) {
		return nil
	}
	for i := range report.Times[t].Observations {
		obs := &report.Times[t].Observations[i]
		if obs.RuleID == ruleID && labelsContain(obs.Labels, subset) {
			return obs
		}
	}
	return nil
}

func visibleAt(report *engine.RunReport, t int, ruleID, instance string) bool {
	if report == nil || t < 0 || t >= len(report.Times) {
		return false
	}
	for _, alert := range report.Times[t].Visible {
		if alert.RuleID == ruleID && (alert.Labels["instance"] == instance || alert.Labels["host"] == instance) {
			return true
		}
	}
	return false
}

func suppressionAt(report *engine.RunReport, t int, ruleID string) *engine.SuppressionDecision {
	if report == nil || t < 0 || t >= len(report.Times) {
		return nil
	}
	for i := range report.Times[t].SuppressionDetails {
		decision := &report.Times[t].SuppressionDetails[i]
		if decision.RuleID == ruleID {
			return decision
		}
	}
	return nil
}

func batchAt(report *engine.RunReport, t int, alertname string) *engine.NotificationBatch {
	if report == nil || t < 0 || t >= len(report.Times) {
		return nil
	}
	for i := range report.Times[t].Notifications {
		batch := report.Times[t].Notifications[i]
		for _, member := range batch.Members {
			if member.Labels["alertname"] == alertname {
				return &batch
			}
		}
	}
	return nil
}

func countBatches(report *engine.RunReport) int {
	count := 0
	for _, frame := range report.Times {
		count += len(frame.Notifications)
	}
	return count
}

func batchContainsRuleAt(report *engine.RunReport, t int, ruleID string) bool {
	if report == nil || t < 0 || t >= len(report.Times) {
		return false
	}
	for _, batch := range report.Times[t].Notifications {
		for _, member := range batch.Members {
			if member.RuleID == ruleID {
				return true
			}
		}
	}
	return false
}

func labelsContain(got, subset model.Labels) bool {
	for key, value := range subset {
		if got[key] != value {
			return false
		}
	}
	return true
}

func stateDetail(state *engine.State) string {
	if state == nil {
		return "state not found"
	}
	return fmt.Sprintf("state=%s entered_t=%d duration_ms=%d basis=%s fire=%d resolve=%d",
		state.State, state.EnteredT, state.DurationMS, state.BasisID, state.FireBasisNo, state.ResolveBasisNo)
}

func obsDetail(obs *engine.Observation) string {
	if obs == nil {
		return "observation not found"
	}
	return fmt.Sprintf("has_data=%v matched=%v reason=%s sources=%d", obs.HasData, obs.Matched, obs.NoDataReason, len(obs.Sources))
}

func decisionDetail(decision *engine.SuppressionDecision) string {
	if decision == nil {
		return "decision not found"
	}
	return fmt.Sprintf("primary=%s suppressed=%v silenced=%v source=%s silence=%s window=[%d,%d]",
		decision.PrimaryReason, decision.Suppressed, decision.Silenced, decision.SourceRuleID, decision.SilenceID, decision.StartT, decision.EndT)
}

func batchDetail(batch *engine.NotificationBatch) string {
	if batch == nil {
		return "batch not found"
	}
	return fmt.Sprintf("key=%s receiver=%s route=%s members=%d", batch.DedupKey, batch.Receiver, batch.RouteID, len(batch.Members))
}
