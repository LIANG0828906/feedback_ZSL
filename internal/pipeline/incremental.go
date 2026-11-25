package pipeline

import (
	"context"
	"fmt"

	"battlekernel/internal/checkpoint"
	"battlekernel/internal/engine"
	"battlekernel/internal/loader"
	"battlekernel/internal/model"
)

type IncrementalReport struct {
	ProjectID           string               `json:"project_id"`
	TargetUnitID        string               `json:"target_unit_id"`
	CandidateStartRound int64                `json:"candidate_start_round"`
	FirstChangedRound   int64                `json:"first_changed_round,omitempty"`
	WindowSize          int                  `json:"window_size"`
	Invalidations       []model.Invalidation `json:"invalidations"`
	ReusedRounds        []int64              `json:"reused_rounds"`
	ShadowRounds        []int64              `json:"shadow_rounds"`
	RecomputedRounds    []int64              `json:"recomputed_rounds"`
	AtomicPeerRounds    []int64              `json:"atomic_peer_rounds"`
	RewrittenRounds     []int64              `json:"rewritten_rounds"`
	RewrittenWindows    []int                `json:"rewritten_windows"`
	Results             []model.RoundResult  `json:"results"`
	NoOp                bool                 `json:"no_op"`
}

func ApplyIncremental(ctx context.Context, edited *model.Project, store *checkpoint.Store, size int, targetUnit string, candidateStart int64, reason string) (*IncrementalReport, error) {
	size = normalizeWindowSize(size)
	oldRounds, _, err := store.LoadCommittedRounds(ctx, edited.ID)
	if err != nil {
		return nil, err
	}
	oldByRound := map[int64]model.RoundResult{}
	for _, round := range oldRounds {
		oldByRound[round.Round] = round
	}

	report := &IncrementalReport{ProjectID: edited.ID, TargetUnitID: targetUnit, CandidateStartRound: candidateStart, WindowSize: size}
	for _, round := range oldRounds {
		if round.Round < candidateStart {
			report.ReusedRounds = append(report.ReusedRounds, round.Round)
		}
	}

	startIndex := -1
	for i, spec := range edited.Rounds {
		if spec.Round >= candidateStart {
			startIndex = i
			break
		}
	}
	if startIndex < 0 {
		report.NoOp = true
		report.Results = oldRounds
		return report, nil
	}

	states := engine.InitialStateMap(edited)
	if startIndex > 0 {
		boundaryRound := edited.Rounds[startIndex-1].Round
		oldBoundary, ok := oldByRound[boundaryRound]
		if !ok {
			return nil, fmt.Errorf("missing persisted boundary round %d", boundaryRound)
		}
		states = model.StateMap(oldBoundary.States)
	}

	newByRound := map[int64]model.RoundResult{}
	affectedUnits := map[string]bool{targetUnit: true}
	changed := false

	for i := startIndex; i < len(edited.Rounds); i++ {
		spec := edited.Rounds[i]
		result, nextStates, err := engine.SettleRound(edited, spec, states)
		if err != nil {
			return nil, err
		}
		states = nextStates
		newByRound[spec.Round] = result
		report.ShadowRounds = append(report.ShadowRounds, spec.Round)

		oldResult, ok := oldByRound[spec.Round]
		if !ok {
			return nil, fmt.Errorf("missing persisted round %d", spec.Round)
		}
		diffs := diffRound(spec.Round, oldResult, result)
		if !changed {
			if len(diffs) == 0 {
				report.ReusedRounds = append(report.ReusedRounds, spec.Round)
				continue
			}
			changed = true
			report.FirstChangedRound = spec.Round
			for _, diff := range diffs {
				diff.Reason = reason + ": " + diff.Reason
				report.Invalidations = append(report.Invalidations, diff)
				affectedUnits[diff.UnitID] = true
			}
		} else {
			for unitID := range affectedUnits {
				report.Invalidations = append(report.Invalidations, model.Invalidation{
					Round: spec.Round, UnitID: unitID, Field: "future_round_propagation",
					Reason: "cooldown/resource/order dependency propagates after first changed round",
				})
			}
		}
		report.RecomputedRounds = append(report.RecomputedRounds, spec.Round)
	}

	if !changed {
		report.NoOp = true
		report.Results = oldRounds
		return report, nil
	}

	first := report.FirstChangedRound
	windowIndex := windowIndexForRound(edited, first, size)
	windowStart := edited.StartRound + int64((windowIndex-1)*size)
	if windowStart < edited.StartRound {
		windowStart = edited.StartRound
	}
	for round := windowStart; round < first; round++ {
		report.AtomicPeerRounds = append(report.AtomicPeerRounds, round)
	}

	if err := store.DeleteFromRound(ctx, edited.ID, windowStart); err != nil {
		return nil, err
	}

	var finalResults []model.RoundResult
	for _, old := range oldRounds {
		if old.Round < windowStart {
			finalResults = append(finalResults, old)
		}
	}

	var group []model.RoundResult
	flush := func(endRound int64) error {
		if len(group) == 0 {
			return nil
		}
		idx := windowIndexForRound(edited, group[0].Round, size)
		hash, err := canonicalJSONHash(group)
		if err != nil {
			return err
		}
		end := group[len(group)-1].Round
		offsets := engine.ConsumedReorderOffsets(edited, end)
		record := model.WindowRecord{
			WindowIndex: idx, StartRound: group[0].Round, EndRound: end, ContentHash: hash,
			FormatVersion: model.CanonicalFormatVersion, AlgorithmVersion: model.AlgorithmVersion,
			ReorderOffsets: offsets, RoundCount: len(group),
		}
		if err := store.CommitWindow(ctx, checkpoint.CommitWindowInput{ProjectID: edited.ID, Window: record, Rounds: group, ConsumedOffsets: offsets}); err != nil {
			return err
		}
		report.RewrittenWindows = append(report.RewrittenWindows, idx)
		for _, r := range group {
			report.RewrittenRounds = append(report.RewrittenRounds, r.Round)
		}
		finalResults = append(finalResults, group...)
		group = nil
		return nil
	}

	for _, spec := range edited.Rounds {
		if spec.Round < windowStart {
			continue
		}
		if spec.Round < first {
			old := oldByRound[spec.Round]
			group = append(group, old)
			continue
		}
		result := newByRound[spec.Round]
		group = append(group, result)
		relative := int(spec.Round - edited.StartRound)
		if (relative+1)%size == 0 || spec.Round == edited.Rounds[len(edited.Rounds)-1].Round {
			if err := flush(spec.Round); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(edited.Rounds[len(edited.Rounds)-1].Round); err != nil {
		return nil, err
	}
	report.Results = finalResults

	semanticHash, err := loader.SemanticHash(edited)
	if err != nil {
		return nil, err
	}
	if err := store.InitializeProject(ctx, edited, semanticHash); err != nil {
		return nil, err
	}
	return report, nil
}

func diffRound(round int64, old, next model.RoundResult) []model.Invalidation {
	var out []model.Invalidation
	oldQueue := map[string]model.QueueEntry{}
	newQueue := map[string]model.QueueEntry{}
	for _, q := range old.Queue {
		oldQueue[q.UnitID+"/"+q.SkillID] = q
	}
	for _, q := range next.Queue {
		newQueue[q.UnitID+"/"+q.SkillID] = q
	}
	for key, nq := range newQueue {
		oq, ok := oldQueue[key]
		if !ok || oq.Position != nq.Position || oq.Weight != nq.Weight || oq.TieOrdinal != nq.TieOrdinal {
			out = append(out, model.Invalidation{Round: round, UnitID: nq.UnitID, Field: "queue_position", Reason: fmt.Sprintf("old_position=%d new_position=%d", posOrZero(oldQueue, key), nq.Position)})
		}
	}
	oldStates := model.StateMap(old.States)
	for _, ns := range next.States {
		os := oldStates[ns.UnitID]
		if os.CooldownRemaining != ns.CooldownRemaining {
			out = append(out, model.Invalidation{Round: round, UnitID: ns.UnitID, Field: "cooldown_remaining", Reason: fmt.Sprintf("old=%d new=%d", os.CooldownRemaining, ns.CooldownRemaining)})
		}
		if os.Resource != ns.Resource {
			out = append(out, model.Invalidation{Round: round, UnitID: ns.UnitID, Field: "resource", Reason: fmt.Sprintf("old=%d new=%d", os.Resource, ns.Resource)})
		}
	}
	return out
}

func posOrZero(m map[string]model.QueueEntry, key string) int {
	if q, ok := m[key]; ok {
		return q.Position
	}
	return 0
}
