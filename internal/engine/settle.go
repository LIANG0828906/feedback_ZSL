package engine

import (
	"fmt"
	"math"
	"sort"

	"replayaudit/internal/errs"
	"replayaudit/internal/model"
)

// Status constants for event results / outcomes.
const (
	StatusSettled     = "settled"
	StatusSkippedCond = "skipped_cond"
	StatusSuppressed  = "suppressed"
	StatusFailed      = "failed"
)

// ResolveFlags adjudicates priority flag declarations. Deterministic rule:
// among declarations of one flag name, greatest Priority wins; if contradictory
// values remain tied at the top priority the decision is unadjudicable.
func ResolveFlags(r *model.Replay) (map[string]bool, []model.ConflictRecord, *errs.AuditError) {
	byName := map[string][]model.FlagDef{}
	for _, f := range r.Flags {
		byName[f.Name] = append(byName[f.Name], f)
	}
	values := map[string]bool{}
	var records []model.ConflictRecord
	var unresolved []string

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		defs := append([]model.FlagDef(nil), byName[name]...)
		sort.Slice(defs, func(i, j int) bool {
			if defs[i].Priority != defs[j].Priority {
				return defs[i].Priority > defs[j].Priority
			}
			return defs[i].ID < defs[j].ID
		})
		topPrio := defs[0].Priority
		var winners []model.FlagDef
		for _, d := range defs {
			if d.Priority == topPrio {
				winners = append(winners, d)
			}
		}
		vals := map[bool]bool{}
		for _, w := range winners {
			vals[w.Value] = true
		}
		if len(vals) == 2 {
			ids := make([]string, 0, len(winners))
			for _, w := range winners {
				ids = append(ids, w.ID)
			}
			sort.Strings(ids)
			records = append(records, model.ConflictRecord{
				Type:           "flag_contradiction",
				PrimaryEventID: ids[0],
				RelatedIDs:     ids,
				Decision:       "unresolved",
				Detail:         fmt.Sprintf("flag %q has contradictory top-priority declarations", name),
			})
			unresolved = append(unresolved, ids[0])
			continue
		}
		values[name] = winners[0].Value
		if len(defs) > 1 {
			records = append(records, model.ConflictRecord{
				Type:           "flag_contradiction",
				PrimaryEventID: winners[0].ID,
				RelatedIDs:     flagDefIDs(defs),
				Decision:       fmt.Sprintf("flag:%s=%t by priority %d", name, winners[0].Value, topPrio),
				Detail:         fmt.Sprintf("contradictory declarations for flag %q adjudicated by priority", name),
			})
		}
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return values, records, errs.New(errs.EConflict, "flags", unresolved[0],
			"unresolvable contradictory flag declarations")
	}
	return values, records, nil
}

func flagDefIDs(defs []model.FlagDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.ID)
	}
	sort.Strings(out)
	return out
}

// adjudicateMutex decides same-timestamp mutex groups. Rule: among condition
// active members at the same internal time, greatest Priority wins, ties
// resolved by lexicographically smallest event ID; losers are suppressed.
// Suppressing a Mandatory event is unadjudicable (E_CONFLICT).
func adjudicateMutex(r *model.Replay, order []*model.Event, flags map[string]bool) (map[string]string, []model.ConflictRecord, *errs.AuditError) {
	decisions := map[string]string{}
	var records []model.ConflictRecord
	var mandatoryBlocked []string

	byID := map[string]*model.Event{}
	for _, e := range order {
		byID[e.ID] = e
	}

	for _, g := range r.Mutex {
		members := make([]*model.Event, 0, len(g.EventIDs))
		for _, id := range g.EventIDs {
			e, ok := byID[id]
			if !ok {
				return decisions, records, errs.New(errs.ESchema, "mutex."+g.ID, id,
					"mutex group references unknown event")
			}
			members = append(members, e)
		}
		byTime := map[int64][]*model.Event{}
		var times []int64
		for _, e := range members {
			if e.Cond != nil {
				v := flags[e.Cond.Flag]
				if v != e.Cond.Want {
					continue // condition-inactive events are not candidates
				}
			}
			if _, ok := byTime[e.At]; !ok {
				times = append(times, e.At)
			}
			byTime[e.At] = append(byTime[e.At], e)
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		for _, t := range times {
			cand := append([]*model.Event(nil), byTime[t]...)
			if len(cand) < 2 {
				continue
			}
			sort.Slice(cand, func(i, j int) bool {
				if cand[i].Priority != cand[j].Priority {
					return cand[i].Priority > cand[j].Priority
				}
				return cand[i].ID < cand[j].ID
			})
			winner := cand[0]
			decisions[winner.ID] = "winner"
			for _, loser := range cand[1:] {
				decisions[loser.ID] = "suppressed"
				records = append(records, model.ConflictRecord{
					Type:           "mutex",
					PrimaryEventID: loser.ID,
					RelatedIDs:     []string{winner.ID, loser.ID},
					Decision:       fmt.Sprintf("suppress:%s -> winner:%s (priority %d vs %d)", loser.ID, winner.ID, loser.Priority, winner.Priority),
					Detail:         fmt.Sprintf("mutex group %s at t=%d adjudicated by priority then event id", g.ID, t),
				})
				if loser.Mandatory {
					mandatoryBlocked = append(mandatoryBlocked, loser.ID)
				}
			}
		}
	}
	if len(mandatoryBlocked) > 0 {
		sort.Strings(mandatoryBlocked)
		return decisions, records, errs.New(errs.EConflict, "mutex", mandatoryBlocked[0],
			"mandatory event cannot be suppressed by mutex adjudication")
	}
	return decisions, records, nil
}

// EvaluateOutcome is the pure, cacheable rule evaluation for one event.
func EvaluateOutcome(e *model.Event, flags map[string]bool, mutexDecision string) model.Outcome {
	if e.Fault {
		return model.Outcome{Status: StatusFailed, Reason: "injected settlement fault (atomic event exception)"}
	}
	if e.Cond != nil {
		v := flags[e.Cond.Flag]
		if v != e.Cond.Want {
			return model.Outcome{Status: StatusSkippedCond,
				Reason: fmt.Sprintf("condition %s==%t not met (flag=%t)", e.Cond.Flag, e.Cond.Want, v)}
		}
	}
	if mutexDecision == "suppressed" {
		return model.Outcome{Status: StatusSuppressed, Reason: "lost mutex adjudication"}
	}

	var fx []model.Effect
	switch e.Kind {
	case "damage":
		for _, t := range e.Targets {
			fx = append(fx, model.Effect{Key: "hp:" + t, Op: "add", Amount: -e.Amount})
		}
	case "heal":
		for _, t := range e.Targets {
			fx = append(fx, model.Effect{Key: "hp:" + t, Op: "add", Amount: e.Amount})
		}
	case "gain":
		for _, c := range e.Casters {
			fx = append(fx, model.Effect{Key: "mp:" + c, Op: "add", Amount: e.Amount})
		}
	case "spend":
		for _, c := range e.Casters {
			fx = append(fx, model.Effect{Key: "mp:" + c, Op: "add", Amount: -e.Amount})
		}
	}
	if e.Skill != "" && e.CooldownUnits > 0 {
		ready, ok := checkedAdd(e.At, e.CooldownUnits)
		if !ok {
			return model.Outcome{Status: StatusFailed, Reason: errs.New(errs.EOvf, "cooldown_units", e.ID,
				"cooldown ready-time overflow at=%d cd=%d", e.At, e.CooldownUnits).Error()}
		}
		for _, c := range e.Casters {
			fx = append(fx, model.Effect{Key: "cd:" + c + "|" + e.Skill, Op: "set", Amount: ready})
		}
	}
	return model.Outcome{Status: StatusSettled, Effects: fx}
}

func checkedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// SettleFull runs the complete deterministic kernel with a fresh outcome set.
func SettleFull(r *model.Replay, p model.Params) (*model.Report, error) {
	return settle(r, p, nil, nil)
}

// settle is the shared kernel. When outcomes is non-nil it is used as the
// cache source (incremental path); missing ids are evaluated safely.
func settle(r *model.Replay, p model.Params, outcomes map[string]model.Outcome, attempts map[string]int) (*model.Report, error) {
	contentHash := model.HashReplay(r)
	rep := &model.Report{
		ReplayID:      r.ReplayID,
		ContentHash:   contentHash,
		ParamsHash:    p.Hash(),
		FormatVersion: model.FormatVersion,
		AlgoVersion:   model.AlgoVersion,
	}

	flags, flagRecs, flagErr := ResolveFlags(r)
	rep.Conflicts = append(rep.Conflicts, flagRecs...)

	order := CanonicalOrder(r.Events)
	decisions, mutexRecs, mutexErr := adjudicateMutex(r, order, flags)
	rep.Conflicts = append(rep.Conflicts, mutexRecs...)
	sortConflictRecords(rep.Conflicts)

	if flagErr != nil {
		return rep, flagErr
	}
	if mutexErr != nil {
		return rep, mutexErr
	}

	if outcomes == nil {
		outcomes = map[string]model.Outcome{}
		for _, e := range order {
			outcomes[e.ID] = EvaluateOutcome(e, flags, decisions[e.ID])
		}
	}

	maxHP, maxMP := map[string]int64{}, map[string]int64{}
	for _, en := range r.Entities {
		maxHP[en.ID] = en.MaxHP
		maxMP[en.ID] = en.MaxMP
	}

	state := model.NewState(r.Entities)
	initialHash := state.Hash()
	failedBeforeSync := map[string]bool{}

	for idx, e := range order {
		oc, ok := outcomes[e.ID]
		if !ok {
			oc = EvaluateOutcome(e, flags, decisions[e.ID])
		}
		attempt := 1
		if attempts != nil {
			if a := attempts[e.ID]; a > 0 {
				attempt = a
			}
		}
		res := &model.EventResult{
			EventID:     e.ID,
			EffectiveAt: e.At,
			OrderIndex:  idx,
			Segment:     SegmentOf(e.At, p.SegmentUnits),
			Status:      oc.Status,
			Reason:      oc.Reason,
			Effects:     append([]model.Effect(nil), oc.Effects...),
			Attempt:     attempt,
		}

		if oc.Status == StatusFailed {
			failedBeforeSync[e.ID] = true
			res.StateAfter = state.Clone() // atomic: no partial mutation
		} else {
			next := state.Clone()
			applyErr := applyEffects(&next, oc.Effects, maxHP, maxMP)
			if applyErr != nil {
				res.Status = StatusFailed
				res.Reason = applyErr.Error()
				failedBeforeSync[e.ID] = true
				res.StateAfter = state.Clone()
			} else {
				state = next
				res.StateAfter = state.Clone()
			}
		}
		res.ResultHash = hashResult(res)
		rep.EventResults = append(rep.EventResults, res)
	}

	// Forced sync barriers: every event at or before the barrier must be
	// resolved; failed events are pending and make the barrier unpassable.
	syncs := append([]model.SyncPoint(nil), r.Sync...)
	sort.Slice(syncs, func(i, j int) bool { return syncs[i].At < syncs[j].At })
	var syncErrIDs []string
	for _, s := range syncs {
		var pending []string
		for _, res := range rep.EventResults {
			if res.EffectiveAt <= s.At && res.Status == StatusFailed {
				pending = append(pending, res.EventID)
			}
		}
		if len(pending) > 0 {
			sort.Strings(pending)
			for _, id := range pending {
				rep.Conflicts = append(rep.Conflicts, model.ConflictRecord{
					Type:           "sync_pending",
					PrimaryEventID: id,
					RelatedIDs:     []string{s.ID, id},
					Decision:       "blocked",
					Detail:         fmt.Sprintf("forced sync %s at t=%d has unresolved event", s.ID, s.At),
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
	rep.CommitSequence = buildCommitSequence(rep, syncs, p, initialHash)

	if len(syncErrIDs) > 0 {
		sort.Strings(syncErrIDs)
		return rep, errs.New(errs.EConflict, "sync", syncErrIDs[0],
			"forced sync point has unresolved/pending event")
	}
	return rep, nil
}

func applyEffects(state *model.State, fxs []model.Effect, maxHP, maxMP map[string]int64) error {
	for _, fx := range fxs {
		switch {
		case len(fx.Key) > 3 && fx.Key[:3] == "hp:":
			id := fx.Key[3:]
			if fx.Op == "set" {
				state.HP[id] = fx.Amount
			} else {
				v, ok := checkedAdd(state.HP[id], fx.Amount)
				if !ok {
					return errs.New(errs.EOvf, "state.hp", id, "hp arithmetic overflow")
				}
				state.HP[id] = clamp(v, 0, maxHP[id])
			}
		case len(fx.Key) > 3 && fx.Key[:3] == "mp:":
			id := fx.Key[3:]
			if fx.Op == "set" {
				state.MP[id] = fx.Amount
			} else {
				v, ok := checkedAdd(state.MP[id], fx.Amount)
				if !ok {
					return errs.New(errs.EOvf, "state.mp", id, "mp arithmetic overflow")
				}
				state.MP[id] = clamp(v, 0, maxMP[id])
			}
		case len(fx.Key) > 3 && fx.Key[:3] == "cd:":
			if fx.Op == "set" {
				state.Cooldown[fx.Key[3:]] = fx.Amount
			}
		}
	}
	return nil
}

func clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if hi > 0 && v > hi {
		return hi
	}
	return v
}

func decideWinner(entities []model.EntitySpec, s *model.State) string {
	sides := map[string][]string{}
	for _, en := range entities {
		sides[en.Side] = append(sides[en.Side], en.ID)
	}
	sideNames := make([]string, 0, len(sides))
	for sn := range sides {
		sideNames = append(sideNames, sn)
	}
	sort.Strings(sideNames)
	if len(sideNames) != 2 {
		return ""
	}
	dead := map[string]bool{}
	for sn, ids := range sides {
		allDead := true
		for _, id := range ids {
			if s.HP[id] > 0 {
				allDead = false
				break
			}
		}
		dead[sn] = allDead
	}
	a, b := sideNames[0], sideNames[1]
	switch {
	case dead[a] && dead[b]:
		return "draw"
	case dead[a]:
		return b
	case dead[b]:
		return a
	default:
		return ""
	}
}

func hashResult(res *model.EventResult) string {
	return model.HashOf(struct {
		EventID     string         `json:"event_id"`
		EffectiveAt int64          `json:"effective_at"`
		Status      string         `json:"status"`
		Reason      string         `json:"reason"`
		Effects     []model.Effect `json:"effects"`
		StateHash   string         `json:"state_hash"`
	}{res.EventID, res.EffectiveAt, res.Status, res.Reason, res.Effects, res.StateAfter.Hash()})
}

func buildCommitSequence(rep *model.Report, syncs []model.SyncPoint, p model.Params, initialHash string) []model.SegmentCommit {
	maxSeg := 0
	for _, res := range rep.EventResults {
		if res.Segment > maxSeg {
			maxSeg = res.Segment
		}
	}
	commits := make([]model.SegmentCommit, 0, maxSeg+1)
	ord := 0
	lastStateHash := initialHash
	for seg := 0; seg <= maxSeg; seg++ {
		var ids, resultHashes, syncIDs []string
		stateHash := lastStateHash
		for _, res := range rep.EventResults {
			if res.Segment == seg {
				ids = append(ids, res.EventID)
				resultHashes = append(resultHashes, res.ResultHash)
				stateHash = res.StateAfter.Hash()
			}
		}
		for _, s := range syncs {
			if SegmentOf(s.At, p.SegmentUnits) == seg {
				syncIDs = append(syncIDs, s.ID)
			}
		}
		segHash := model.HashOf(struct {
			Segment      int      `json:"segment"`
			EventIDs     []string `json:"event_ids"`
			ResultHashes []string `json:"result_hashes"`
			SyncIDs      []string `json:"sync_ids"`
			StateHash    string   `json:"state_hash"`
		}{seg, ids, resultHashes, syncIDs, stateHash})
		commits = append(commits, model.SegmentCommit{
			Segment: seg, CommitSeq: ord, EventIDs: ids, SyncIDs: syncIDs,
			StateHash: stateHash, SegmentHash: segHash,
		})
		lastStateHash = stateHash
		ord++
	}
	return commits
}

func sortConflictRecords(recs []model.ConflictRecord) {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].Type != recs[j].Type {
			return recs[i].Type < recs[j].Type
		}
		return recs[i].PrimaryEventID < recs[j].PrimaryEventID
	})
}
