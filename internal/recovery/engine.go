package recovery

import (
	"reflect"
	"sort"

	"enginelab.local/internal/model"
)

type Engine struct {
	Project        *model.Project
	Budget         model.Budget
	Cache          map[string]*model.SegmentReport
	Baseline       map[string]model.DataItem
	BaselineCPID   string
	AppliedEdits   []Edit
	LastResult     *model.RecoveryResult
	LastChanged    map[string][]model.InvalidationSource
	LastRecomputed []string
	LastReused     []string
}

func NewEngine(p *model.Project, budget model.Budget, baseline map[string]model.DataItem) (*Engine, error) {
	b, err := NormalizeBudget(budget)
	if err != nil {
		return nil, err
	}
	clone, err := CloneProject(p)
	if err != nil {
		return nil, err
	}
	if baseline == nil {
		baseline = ComputeBaseline(clone, clone.LatestCheckpoint)
	}
	e := &Engine{
		Project:     clone,
		Budget:      b,
		Cache:       map[string]*model.SegmentReport{},
		Baseline:    baseline,
		LastChanged: map[string][]model.InvalidationSource{},
	}
	if clone.LatestCheckpoint != nil {
		e.BaselineCPID = clone.LatestCheckpoint.ID
	}
	result, err := e.recompute(nil, nil)
	if err != nil {
		return nil, err
	}
	e.LastResult = result
	return e, nil
}

func (e *Engine) Apply(edit Edit) (*model.RecoveryResult, error) {
	oldGraph := BuildGraph(e.Project)
	if _, err := ApplyEdit(e.Project, edit); err != nil {
		return nil, err
	}
	if err := ValidateSequence(e.Project); err != nil {
		return nil, err
	}
	newGraph := BuildGraph(e.Project)
	if len(newGraph.Missing) > 0 || len(newGraph.Cycles) > 0 {
		if err := ValidateGraph(e.Project); err != nil {
			return nil, err
		}
	}

	changedID := edit.SegmentID
	if changedID == "" && edit.RecordID != "" {
		for _, seg := range e.Project.Segments {
			for _, rec := range seg.Records {
				if rec.ID == edit.RecordID {
					changedID = seg.ID
				}
			}
		}
	}
	sources := map[string][]model.InvalidationSource{}
	changedSet := map[string]ClosureNode{}
	addClosure := func(g *GraphReport, direct string, code model.ErrorCode) {
		closure := DependentsClosure(g, direct)
		for id, node := range closure {
			changedSet[id] = node
			source := model.InvalidationSource{
				Code:      code,
				EditID:    edit.ID,
				Kind:      edit.Type,
				SegmentID: direct,
				Path:      append([]string(nil), node.Path...),
			}
			if id == direct {
				source.Message = "directly edited segment"
			} else {
				source.Message = "dependency chain passes through edited segment"
			}
			sources[id] = append(sources[id], source)
		}
	}
	if changedID != "" {
		code := model.EStateConflict
		switch edit.Type {
		case EditTruncateSegment:
			code = model.ESegmentTruncated
		case EditAddCheckpoint:
			code = model.EOk
		case EditRewriteDependency:
			code = model.EStateConflict
		}
		addClosure(newGraph, changedID, code)
		if edit.Type == EditRewriteDependency {
			oldClosure := DependentsClosure(oldGraph, changedID)
			for id, node := range oldClosure {
				if _, ok := changedSet[id]; !ok {
					changedSet[id] = node
					sources[id] = append(sources[id], model.InvalidationSource{
						Code:      model.EStateConflict,
						EditID:    edit.ID,
						Kind:      edit.Type + ":old_edge",
						SegmentID: changedID,
						Path:      append([]string(nil), node.Path...),
						Message:   "old dependency edge required invalidation",
					})
				}
			}
		}
	}

	e.AppliedEdits = append(e.AppliedEdits, edit)
	result, err := e.recompute(changedSet, sources)
	if err != nil {
		return result, err
	}
	e.LastResult = result
	e.LastChanged = sources
	return result, nil
}

func (e *Engine) recompute(changed map[string]ClosureNode, sources map[string][]model.InvalidationSource) (*model.RecoveryResult, error) {
	if err := ValidateGraph(e.Project); err != nil {
		return nil, err
	}
	ranges := BuildRanges(e.Project)
	var oldRanges map[string]model.ReplayRange
	if e.LastResult != nil {
		oldRanges = e.LastResult.Ranges
	}
	e.LastRecomputed = nil
	e.LastReused = nil

	for _, seg := range e.Project.SortedSegments() {
		_, invalid := changed[seg.ID]
		cached, ok := e.Cache[seg.ID]
		if invalid || !ok || cached == nil || cached.CacheKey != SegmentCacheKey(seg) {
			localRange := FullSegmentRange(seg)
			rep, err := ReplaySegment(seg, localRange, e.Budget)
			if err != nil {
				return nil, err
			}
			rep.CacheStatus = model.CacheRecomputed
			rep.InvalidationSources = append([]model.InvalidationSource(nil), sources[seg.ID]...)
			e.Cache[seg.ID] = rep
			e.LastRecomputed = append(e.LastRecomputed, seg.ID)
		} else {
			cached.CacheStatus = model.CacheReused
			e.LastReused = append(e.LastReused, seg.ID)
		}
	}

	cp := e.Project.LatestCheckpoint
	if cp == nil {
		e.Baseline = map[string]model.DataItem{}
		e.BaselineCPID = ""
	} else if e.BaselineCPID != cp.ID {
		baseline := cloneDataItems(e.Baseline)
		var oldLSN uint64
		for _, oldCP := range e.Project.Checkpoints {
			if oldCP.ID == e.BaselineCPID {
				oldLSN = oldCP.LSN
			}
		}
		if e.BaselineCPID == "" || baseline == nil {
			baseline = ComputeBaseline(e.Project, cp)
		} else {
			for _, seg := range e.Project.SortedSegments() {
				rep := e.Cache[seg.ID]
				if rep == nil {
					continue
				}
				for _, w := range rep.Writes {
					if w.CommitLSN > oldLSN && w.CommitLSN <= cp.LSN && !w.Deleted {
						baseline[w.Key] = model.DataItem{Key: w.Key, Value: w.Value, Version: w.Version, LSN: w.CommitLSN, TxID: w.TxID, SegmentID: w.SegmentID}
					}
					if w.Deleted && w.CommitLSN > oldLSN && w.CommitLSN <= cp.LSN {
						delete(baseline, w.Key)
					}
				}
			}
		}
		e.Baseline = baseline
		e.BaselineCPID = cp.ID
	}

	result := &model.RecoveryResult{
		ProjectID:       e.Project.ID,
		Budget:          e.Budget,
		Ranges:          ranges,
		SegmentMap:      map[string]*model.SegmentReport{},
		ItemMap:         map[string]model.DataItem{},
		CacheRecomputed: append([]string(nil), e.LastRecomputed...),
		CacheReused:     append([]string(nil), e.LastReused...),
	}
	if cp != nil {
		result.CheckpointID = cp.ID
	}

	outputs := []*model.SegmentReport{}
	for _, seg := range e.Project.SortedSegments() {
		cached := e.Cache[seg.ID]
		rng := ranges[seg.ID]
		out := *cached
		out.Range = rng
		out.Diagnostics = append([]model.Diagnostic(nil), cached.Diagnostics...)
		segSources := append([]model.InvalidationSource(nil), sources[seg.ID]...)
		if old, ok := oldRanges[seg.ID]; ok && !reflect.DeepEqual(old, rng) {
			segSources = append(segSources, model.InvalidationSource{
				Code:      model.EOk,
				Kind:      "checkpoint_frontier",
				SegmentID: seg.ID,
				Message:   "replay range changed from checkpoint frontier; segment content cache was reused",
			})
		}
		out.InvalidationSources = segSources
		if _, invalid := changed[seg.ID]; invalid {
			out.CacheStatus = model.CacheRecomputed
		} else if len(changed) > 0 {
			out.CacheStatus = model.CacheReused
		}
		if rng.Replay {
			cpLSN := uint64(0)
			if cp != nil {
				cpLSN = cp.LSN
			}
			if cpLSN != 0 {
				scopedWrites := []model.Write{}
				for _, w := range cached.Writes {
					if w.CommitLSN > cpLSN {
						scopedWrites = append(scopedWrites, w)
					}
				}
				out.Writes = scopedWrites
			}
			if seg.Integrity != model.IntegrityComplete {
				out.State = model.StateDegraded
			} else {
				out.State = model.StateReplayed
			}
		} else {
			out.Writes = nil
			out.VisibleRecords = nil
			out.CommittedTransactions = nil
			out.AbortedTransactions = nil
			out.AppliedRecords = 0
			if seg.Integrity != model.IntegrityComplete {
				out.State = model.StateDegraded
			} else {
				out.State = model.StateSkipped
			}
		}
		outputs = append(outputs, &out)
		result.Segments = append(result.Segments, &out)
		result.SegmentMap[seg.ID] = &out
		for _, d := range out.Diagnostics {
			result.Diagnostics = append(result.Diagnostics, d)
		}
	}
	result.ScheduleOrder = orderedSegmentIDs(e.Project, Options{})
	result.Items = AggregateItems(e.Baseline, outputs, 0)
	for _, item := range result.Items {
		result.ItemMap[item.Key] = item
	}
	sort.Strings(result.CacheRecomputed)
	sort.Strings(result.CacheReused)
	committed := map[string]bool{}
	aborted := map[string]bool{}
	for _, rep := range outputs {
		if rep == nil || !rep.Range.Replay {
			continue
		}
		for _, tx := range rep.CommittedTransactions {
			committed[tx] = true
		}
		for _, tx := range rep.AbortedTransactions {
			aborted[tx] = true
		}
	}
	for tx := range committed {
		result.CommittedTxIDs = append(result.CommittedTxIDs, tx)
	}
	for tx := range aborted {
		result.AbortedTxIDs = append(result.AbortedTxIDs, tx)
	}
	sort.Strings(result.CommittedTxIDs)
	sort.Strings(result.AbortedTxIDs)
	e.LastRecomputed = result.CacheRecomputed
	e.LastReused = result.CacheReused
	return result, nil
}

func cloneDataItems(in map[string]model.DataItem) map[string]model.DataItem {
	out := make(map[string]model.DataItem, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
