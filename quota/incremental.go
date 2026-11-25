package quota

import (
	"context"
	"fmt"
	"sort"
)

type EditKind string

const (
	EditNodeCapacity    EditKind = "node_capacity"
	EditTenantQuota     EditKind = "tenant_quota"
	EditTenantGuarantee EditKind = "tenant_guarantee"
	EditWeightEvent     EditKind = "weight_event"
)

type Edit struct {
	Kind      EditKind
	TenantID  string
	NodeID    string
	TimeMilli int64
	EventSeq  int
	Value     int64
}

type Invalidation struct {
	TimeMilli int64    `json:"time_millis"`
	Source    string   `json:"source"`
	Path      []string `json:"dependency_path"`
	Reused    bool     `json:"reused"`
}

type IncrementalResult struct {
	Timeline          *Timeline      `json:"timeline"`
	InvalidatedFrom   int64          `json:"invalidated_from_millis"`
	ReusedSteps       int            `json:"reused_steps"`
	RecomputedSteps   int            `json:"recomputed_steps"`
	Invalidation      []Invalidation `json:"invalidation"`
	Closure           []string       `json:"invalidation_closure"`
	FullEquivalent    bool           `json:"full_equivalent"`
	SourceProjectHash string         `json:"source_project_fingerprint"`
}

type LedgerDifference struct {
	TimeMilli int64  `json:"time_millis"`
	TenantID  string `json:"tenant_id"`
	Field     string `json:"field"`
	Before    int64  `json:"before"`
	After     int64  `json:"after"`
}

// ApplyEdit invalidates the suffix beginning at the edit's logical time. The
// prefix is reused verbatim. Because queue and guarantee state can propagate
// across later times, the closure extends to the end of the current timeline.
func ApplyEdit(ctx context.Context, p *Project, opts Options, prior *Timeline, edit Edit) (*IncrementalResult, error) {
	if prior == nil {
		return nil, NewError(CodeEditTarget, "apply_edit", "", "nil cached timeline")
	}
	oldFP, err := p.Fingerprint()
	if err != nil {
		return nil, err
	}
	if prior.ProjectFingerprint != "" && prior.ProjectFingerprint != oldFP {
		return nil, &BusinessError{Code: CodeVersionMismatch, Op: "apply_edit", Detail: "cached timeline belongs to a different project"}
	}
	edited, source, effectiveAt, err := applyProjectEdit(p, edit)
	if err != nil {
		return nil, err
	}
	if err := edited.Validate(); err != nil {
		return nil, err
	}
	cut := sort.Search(len(prior.Steps), func(i int) bool { return prior.Steps[i].TimeMilli >= effectiveAt })
	resumeState := NewState(edited)
	if cut > 0 {
		resumeState = prior.Steps[cut-1].State.Clone()
	}
	if cut == len(prior.Steps) && len(prior.Steps) > 0 {
		// An edit after the last observed time is observable only when the
		// edited project adds a later checkpoint. Keep current state.
		resumeState = prior.Steps[len(prior.Steps)-1].State.Clone()
		cut = len(prior.Steps)
	}
	suffix, err := Resume(ctx, edited, opts, resumeState)
	if err != nil {
		return nil, err
	}
	combinedSteps := make([]TimeStep, 0, len(prior.Steps)+len(suffix.Steps))
	combinedSteps = append(combinedSteps, prior.Steps[:cut]...)
	combinedSteps = append(combinedSteps, suffix.Steps...)
	fp, err := edited.Fingerprint()
	if err != nil {
		return nil, err
	}
	combined := &Timeline{ProjectFingerprint: fp, Steps: combinedSteps, FinalState: suffix.FinalState, CacheReused: cut > 0}
	if len(combinedSteps) > 0 {
		combined.FinalState = combinedSteps[len(combinedSteps)-1].State.Clone()
	}
	full, err := runWithEffectiveEdit(ctx, p, edited, effectiveAt, opts)
	if err != nil {
		return nil, err
	}
	if err := CompareTimelines(combined, full); err != nil {
		return nil, fmt.Errorf("incremental result diverged from full recomputation: %w", err)
	}

	invalidation := make([]Invalidation, 0, len(combinedSteps))
	for i := range combinedSteps {
		step := &combinedSteps[i]
		if i < cut {
			invalidation = append(invalidation, Invalidation{
				TimeMilli: step.TimeMilli, Source: "cache-reuse",
				Path: []string{"unchanged-prefix", "cached-step"}, Reused: true,
			})
			continue
		}
		invalidation = append(invalidation, Invalidation{
			TimeMilli: step.TimeMilli, Source: source,
			Path: dependencyPath(edited, edit, step), Reused: false,
		})
	}
	return &IncrementalResult{
		Timeline: combined, InvalidatedFrom: effectiveAt,
		ReusedSteps: cut, RecomputedSteps: len(combinedSteps) - cut,
		Invalidation: invalidation,
		Closure: []string{
			source + " -> ledger tenant/node state",
			"ledger tenant/node state -> waiting queue and guarantee reserve",
			"waiting queue and guarantee reserve -> all later admission decisions",
			fmt.Sprintf("recompute interval [%d,%d]ms -> full-equivalence checked", effectiveAt, lastTime(combinedSteps)),
		},
		FullEquivalent: true, SourceProjectHash: fp,
	}, nil
}

// runWithEffectiveEdit performs a clean full recomputation while switching
// project parameters only at the edit's effective time. It is the oracle for
// incremental comparisons and avoids pretending a timed edit existed from the
// first checkpoint.
func runWithEffectiveEdit(ctx context.Context, original, edited *Project, effectiveAt int64, opts Options) (*Timeline, error) {
	opts, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	fp, err := edited.Fingerprint()
	if err != nil {
		return nil, err
	}
	state := NewState(original)
	times := distinctTimes(edited.Events, -1)
	steps := make([]TimeStep, 0, len(times))
	for _, at := range times {
		if err := ctx.Err(); err != nil {
			return &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}, err
		}
		active := original
		if at >= effectiveAt {
			active = edited
		}
		step, next, err := advanceTime(ctx, active, opts, state, at)
		if err != nil {
			return &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}, err
		}
		state = next
		steps = append(steps, step)
	}
	return &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}, nil
}

func applyProjectEdit(p *Project, edit Edit) (*Project, string, int64, error) {
	if edit.TimeMilli < 0 {
		return nil, "", 0, &BusinessError{Code: CodeEditTarget, Op: "apply_edit", TimeMilli: edit.TimeMilli, Detail: "negative effective time"}
	}
	cp := &Project{
		SchemaVersion: p.SchemaVersion, Seed: p.Seed,
		Tenants: append([]Tenant(nil), p.Tenants...),
		Nodes:   append([]Node(nil), p.Nodes...),
		Events:  append([]Event(nil), p.Events...),
	}
	for i := range cp.Nodes {
		cp.Nodes[i].AllowedTenants = append([]string(nil), p.Nodes[i].AllowedTenants...)
	}
	switch edit.Kind {
	case EditNodeCapacity:
		found := false
		for i := range cp.Nodes {
			if cp.Nodes[i].ID == edit.NodeID {
				if edit.Value <= 0 {
					return nil, "", 0, &BusinessError{Code: CodeQuotaRange, Op: "apply_edit", NodeID: edit.NodeID, Detail: "node capacity must be positive"}
				}
				cp.Nodes[i].CapacityMilli = edit.Value
				found = true
				break
			}
		}
		if !found {
			return nil, "", 0, &BusinessError{Code: CodeEditTarget, Op: "apply_edit", NodeID: edit.NodeID, Detail: "unknown node"}
		}
		return cp, fmt.Sprintf("edit:node_capacity:%s@%dms", edit.NodeID, edit.TimeMilli),
			edit.TimeMilli, nil
	case EditTenantQuota, EditTenantGuarantee:
		found := false
		for i := range cp.Tenants {
			if cp.Tenants[i].ID != edit.TenantID {
				continue
			}
			if edit.Value < 0 || edit.Value > RatioScaleBasisPoints {
				return nil, "", 0, &BusinessError{Code: CodeQuotaRange, Op: "apply_edit", TenantID: edit.TenantID, Detail: "quota ratio outside [0,10000]"}
			}
			if edit.Kind == EditTenantQuota {
				cp.Tenants[i].QuotaBP = edit.Value
				if cp.Tenants[i].GuaranteeBP > edit.Value {
					return nil, "", 0, &BusinessError{Code: CodeQuotaRange, Op: "apply_edit", TenantID: edit.TenantID, Detail: "guarantee would exceed quota"}
				}
			} else {
				if edit.Value > cp.Tenants[i].QuotaBP {
					return nil, "", 0, &BusinessError{Code: CodeQuotaRange, Op: "apply_edit", TenantID: edit.TenantID, Detail: "guarantee would exceed quota"}
				}
				cp.Tenants[i].GuaranteeBP = edit.Value
			}
			found = true
			break
		}
		if !found {
			return nil, "", 0, &BusinessError{Code: CodeEditTarget, Op: "apply_edit", TenantID: edit.TenantID, Detail: "unknown tenant"}
		}
		field := "tenant_quota"
		if edit.Kind == EditTenantGuarantee {
			field = "tenant_guarantee"
		}
		return cp, fmt.Sprintf("edit:%s:%s@%dms", field, edit.TenantID, edit.TimeMilli),
			edit.TimeMilli, nil
	case EditWeightEvent:
		for i := range cp.Events {
			ev := &cp.Events[i]
			if ev.Type != EventWeight {
				continue
			}
			if edit.EventSeq > 0 && ev.Seq != edit.EventSeq {
				continue
			}
			if edit.TenantID != "" && ev.TenantID != edit.TenantID {
				continue
			}
			if edit.TimeMilli != 0 && ev.TimeMilli != edit.TimeMilli {
				continue
			}
			if edit.Value <= 0 || edit.Value > MaxWeightPermille {
				return nil, "", 0, &BusinessError{Code: CodeQuotaRange, Op: "apply_edit", TenantID: ev.TenantID, TimeMilli: ev.TimeMilli,
					Detail: fmt.Sprintf("weight outside (0,%d]", MaxWeightPermille)}
			}
			ev.WeightPermille = edit.Value
			effectiveAt := ev.TimeMilli
			return cp, fmt.Sprintf("edit:weight_event:%s@seq=%d", ev.TenantID, ev.Seq), effectiveAt, nil
		}
		return nil, "", 0, &BusinessError{Code: CodeEditTarget, Op: "apply_edit", TenantID: edit.TenantID, TimeMilli: edit.TimeMilli, Detail: "weight event not found"}
	default:
		return nil, "", 0, &BusinessError{Code: CodeEditTarget, Op: "apply_edit", Detail: "unknown edit kind " + string(edit.Kind)}
	}
}

func dependencyPath(p *Project, edit Edit, step *TimeStep) []string {
	path := []string{"edit:" + string(edit.Kind)}
	if edit.NodeID != "" {
		path = append(path, "node:"+edit.NodeID)
	}
	if edit.TenantID != "" {
		path = append(path, "tenant:"+edit.TenantID)
	}
	for _, ev := range p.Events {
		if ev.TimeMilli != step.TimeMilli {
			continue
		}
		if edit.NodeID != "" && ev.NodeID == edit.NodeID {
			path = append(path, fmt.Sprintf("event:%d:%s", ev.Seq, ev.Type))
		}
		if edit.TenantID != "" && ev.TenantID == edit.TenantID {
			path = append(path, fmt.Sprintf("event:%d:%s", ev.Seq, ev.Type))
		}
	}
	path = append(path, "ledger", "queue", "admission", "node-occupancy")
	return path
}

func lastTime(steps []TimeStep) int64 {
	if len(steps) == 0 {
		return 0
	}
	return steps[len(steps)-1].TimeMilli
}

func DiffTimelines(before, after *Timeline) []LedgerDifference {
	if before == nil || after == nil {
		return nil
	}
	var out []LedgerDifference
	limit := len(before.Steps)
	if len(after.Steps) < limit {
		limit = len(after.Steps)
	}
	for i := 0; i < limit; i++ {
		b, a := before.Steps[i], after.Steps[i]
		if b.TimeMilli != a.TimeMilli {
			break
		}
		for j := range b.Tenants {
			bt, at := b.Tenants[j], a.Tenants[j]
			addDiff := func(field string, oldV, newV int64) {
				if oldV != newV {
					out = append(out, LedgerDifference{TimeMilli: a.TimeMilli, TenantID: at.TenantID, Field: field, Before: oldV, After: newV})
				}
			}
			addDiff("available_millicores", bt.AvailableMilli, at.AvailableMilli)
			addDiff("used_millicores", bt.UsedMilli, at.UsedMilli)
			addDiff("guarantee_remaining_millicores", bt.GuaranteeRemainingMilli, at.GuaranteeRemainingMilli)
			addDiff("quota_millicores", bt.QuotaMilli, at.QuotaMilli)
		}
	}
	return out
}
