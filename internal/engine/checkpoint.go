package engine

import (
	"errors"
	"fmt"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
	"replayaudit/internal/store"
)

// ErrInterrupted is the deterministic simulated-crash sentinel used to prove
// checkpoint resume behavior.
var ErrInterrupted = errors.New("simulated interruption after committed segment")

// CheckpointOptions controls interrupt/resume test hooks.
type CheckpointOptions struct {
	// InterruptAfter > 0 stops the run after that many segment commits in
	// this invocation, returning ErrInterrupted.
	InterruptAfter int
}

// ParamsTag is the stored parameter version: declared version plus a hash
// suffix so any parameter drift invalidates snapshots as well.
func ParamsTag(p model.Params) string {
	return fmt.Sprintf("%s#%s", p.ParamsVersion, p.Hash()[:12])
}

// RunCheckpointed settles segment-by-segment, durably committing each segment
// to SQLite. A compatible snapshot resumes at the next segment; any content
// hash / version mismatch has already purged all old segments.
func RunCheckpointed(r *model.Replay, p model.Params, st *store.Store, opt CheckpointOptions) (*model.Report, error) {
	contentHash := model.HashReplay(r)
	pv := ParamsTag(p)
	usable, err := st.CheckCompatible(r.ReplayID, contentHash, model.FormatVersion, pv, model.AlgoVersion)
	if err != nil {
		return nil, err
	}

	var prior []*model.EventResult
	state := model.NewState(r.Entities)
	startSeg := 0

	if usable {
		recs, _, lerr := st.LoadSegments(r.ReplayID)
		if lerr != nil {
			return nil, lerr
		}
		contiguous := true
		for i, rec := range recs {
			if rec.Segment != i {
				contiguous = false
				break
			}
		}
		if !contiguous {
			if rerr := st.ResetReplay(r.ReplayID); rerr != nil {
				return nil, rerr
			}
		} else if len(recs) > 0 {
			startSeg = len(recs)
			last := recs[len(recs)-1]
			state = last.State.Clone()
			for _, rec := range recs {
				prior = append(prior, rec.Results...)
			}
		}
	}

	flags, flagRecs, flagErr := ResolveFlags(r)
	order := CanonicalOrder(r.Events)
	decisions, mutexRecs, mutexErr := adjudicateMutex(r, order, flags)

	rep := &model.Report{
		ReplayID:      r.ReplayID,
		ContentHash:   contentHash,
		ParamsHash:    p.Hash(),
		FormatVersion: model.FormatVersion,
		AlgoVersion:   model.AlgoVersion,
		EventResults:  prior,
		Conflicts:     append(append([]model.ConflictRecord{}, flagRecs...), mutexRecs...),
	}
	sortConflictRecords(rep.Conflicts)

	if flagErr != nil {
		return rep, flagErr
	}
	if mutexErr != nil {
		return rep, mutexErr
	}

	maxHP, maxMP := map[string]int64{}, map[string]int64{}
	maxSeg := 0
	for _, en := range r.Entities {
		maxHP[en.ID] = en.MaxHP
		maxMP[en.ID] = en.MaxMP
	}
	for _, e := range order {
		if s := SegmentOf(e.At, p.SegmentUnits); s > maxSeg {
			maxSeg = s
		}
	}

	commitsThisRun := 0
	for seg := startSeg; seg <= maxSeg; seg++ {
		var segResults []*model.EventResult
		var segIDs []string
		for idx, e := range order {
			if SegmentOf(e.At, p.SegmentUnits) != seg {
				continue
			}
			oc := EvaluateOutcome(e, flags, decisions[e.ID])
			res := &model.EventResult{
				EventID: e.ID, EffectiveAt: e.At, OrderIndex: idx, Segment: seg,
				Status: oc.Status, Reason: oc.Reason,
				Effects: append([]model.Effect(nil), oc.Effects...), Attempt: 1,
			}
			if oc.Status != StatusFailed {
				next := state.Clone()
				if applyErr := applyEffects(&next, oc.Effects, maxHP, maxMP); applyErr != nil {
					res.Status = StatusFailed
					res.Reason = applyErr.Error()
					res.StateAfter = state.Clone()
				} else {
					state = next
					res.StateAfter = state.Clone()
				}
			} else {
				res.StateAfter = state.Clone()
			}
			res.ResultHash = hashResult(res)
			segResults = append(segResults, res)
			segIDs = append(segIDs, e.ID)
			rep.EventResults = append(rep.EventResults, res)
		}

		var syncIDs []string
		for _, s := range r.Sync {
			if SegmentOf(s.At, p.SegmentUnits) == seg {
				syncIDs = append(syncIDs, s.ID)
			}
		}
		var resultHashes []string
		for _, res := range segResults {
			resultHashes = append(resultHashes, res.ResultHash)
		}
		stateHash := state.Hash()
		sc := makeSegmentCommit(seg, seg, segIDs, syncIDs, resultHashes, stateHash)

		meta := store.Meta{
			ReplayID: r.ReplayID, ContentHash: contentHash,
			FormatVersion: model.FormatVersion, ParamsVersion: pv,
			AlgoVersion: model.AlgoVersion,
		}
		rec := store.SegmentRecord{SegmentCommit: sc, State: state.Clone(), Results: segResults}
		if err := st.CommitSegment(meta, rec); err != nil {
			return rep, err
		}
		commitsThisRun++
		if opt.InterruptAfter > 0 && commitsThisRun >= opt.InterruptAfter {
			return rep, ErrInterrupted
		}
	}

	return finalizeReport(rep, r, p, state)
}

func makeSegmentCommit(seg, seq int, ids, syncIDs, resultHashes []string, stateHash string) model.SegmentCommit {
	segHash := model.HashOf(struct {
		Segment      int      `json:"segment"`
		EventIDs     []string `json:"event_ids"`
		ResultHashes []string `json:"result_hashes"`
		SyncIDs      []string `json:"sync_ids"`
		StateHash    string   `json:"state_hash"`
	}{seg, ids, resultHashes, syncIDs, stateHash})
	return model.SegmentCommit{
		Segment: seg, CommitSeq: seq, EventIDs: ids, SyncIDs: syncIDs,
		StateHash: stateHash, SegmentHash: segHash,
	}
}

// finalizeReport performs the shared sync-barrier check, winner decision and
// commit-sequence construction from assembled event results.
func finalizeReport(rep *model.Report, r *model.Replay, p model.Params, state model.State) (*model.Report, error) {
	syncs := append([]model.SyncPoint(nil), r.Sync...)
	// stable sort by time then id for determinism
	sortSync(syncs)
	var syncErrIDs []string
	for _, s := range syncs {
		var pending []string
		for _, res := range rep.EventResults {
			if res.EffectiveAt <= s.At && res.Status == StatusFailed {
				pending = append(pending, res.EventID)
			}
		}
		if len(pending) > 0 {
			for _, id := range pending {
				rep.Conflicts = append(rep.Conflicts, model.ConflictRecord{
					Type: "sync_pending", PrimaryEventID: id,
					RelatedIDs: []string{s.ID, id}, Decision: "blocked",
					Detail: fmt.Sprintf("forced sync %s at t=%d has unresolved event", s.ID, s.At),
				})
				syncErrIDs = append(syncErrIDs, id)
			}
		}
	}
	sortConflictRecords(rep.Conflicts)

	rep.Terminal = state.Clone()
	rep.Winner = decideWinner(r.Entities, &state)
	rep.Terminal.Winner = rep.Winner
	rep.Terminal.Ended = rep.Winner != ""
	rep.SyncPoints = syncs
	rep.CommitSequence = buildCommitSequence(rep, syncs, p, model.NewState(r.Entities).Hash())

	if len(syncErrIDs) > 0 {
		syncErrIDs = sortedUnique(syncErrIDs)
		return rep, errs.New(errs.EConflict, "sync", syncErrIDs[0],
			"forced sync point has unresolved/pending event")
	}
	return rep, nil
}

func sortSync(s []model.SyncPoint) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && (s[j].At < s[j-1].At || (s[j].At == s[j-1].At && s[j].ID < s[j-1].ID)); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
