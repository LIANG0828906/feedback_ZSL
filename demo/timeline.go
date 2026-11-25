package demo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"alertkernel/engine"
	"alertkernel/sample"
)

func WriteTimeline(outDir string, bundle *sample.Bundle) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(outDir, "timeline.txt")
	var b strings.Builder
	fmt.Fprintf(&b, "alert kernel deterministic timeline seed=%d version=%s steps=%d step_ms=%d\n",
		bundle.Seed, bundle.Project.RuleVersion, bundle.Project.Clock.Steps, bundle.Project.Clock.StepMS)
	for _, frame := range bundle.Truth.Times {
		fmt.Fprintf(&b, "\n[t=%d at_ms=%d basis_after=%d]\n", frame.T, frame.AtMS, frame.BasisSeqAfter)
		for _, state := range frame.States {
			if state.State == engine.StateInactive && state.FireBasisNo == 0 && state.ResolveBasisNo == 0 {
				continue
			}
			fmt.Fprintf(&b, "  state %-10s %-10s key=%s entered=t%d duration=%dms basis=%s\n",
				state.RuleID, state.State, state.InstanceKey, state.EnteredT, state.DurationMS, state.BasisID)
		}
		if len(frame.Visible) > 0 {
			parts := []string{}
			for _, alert := range frame.Visible {
				parts = append(parts, fmt.Sprintf("%s/%s(%s)", alert.RuleID, alert.State, alert.InstanceKey))
			}
			fmt.Fprintf(&b, "  visible: %s\n", strings.Join(parts, ", "))
		}
		for _, decision := range frame.SuppressionDetails {
			fmt.Fprintf(&b, " 压制 %s key=%s primary=%s source=%s silence=%s window=[%d,%d]\n",
				decision.RuleID, decision.InstanceKey, decision.PrimaryReason, decision.SourceRuleID, decision.SilenceID, decision.StartT, decision.EndT)
		}
		for _, batch := range frame.Notifications {
			members := []string{}
			for _, member := range batch.Members {
				members = append(members, member.RuleID+":"+member.InstanceKey)
			}
			fmt.Fprintf(&b, "  notify receiver=%s route=%s dedup=%s members=[%s]\n",
				batch.Receiver, batch.RouteID, batch.DedupKey, strings.Join(members, ", "))
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func PrintEditSummaries(report *VerificationReport) {
	for _, edit := range report.EditChecks {
		fmt.Printf("- %s equal=%v reused=%v recomputed=%v diff_times=%v\n",
			edit.Name, edit.Comparison.Equal, edit.Invalidation.ReusedTimes,
			edit.Invalidation.RecomputedTimes, edit.Comparison.DifferentTimes)
	}
}
