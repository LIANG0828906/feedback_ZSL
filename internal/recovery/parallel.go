package recovery

import (
	"sort"
	"sync"

	"enginelab.local/internal/model"
)

type Options struct {
	Budget          model.Budget
	BaselineItems   map[string]model.DataItem
	ScheduleOrder   []string
	ReverseSchedule bool
	CommitBatchSize int
}

func orderedSegmentIDs(p *model.Project, opts Options) []string {
	if len(opts.ScheduleOrder) > 0 {
		return append([]string(nil), opts.ScheduleOrder...)
	}
	ids := make([]string, 0, len(p.Segments))
	for _, seg := range p.SortedSegments() {
		ids = append(ids, seg.ID)
	}
	if opts.ReverseSchedule {
		for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
			ids[i], ids[j] = ids[j], ids[i]
		}
	}
	return ids
}

func RecoverOneShot(p *model.Project, opts Options) (*model.RecoveryResult, error) {
	if err := ValidateSequence(p); err != nil {
		return nil, err
	}
	if err := ValidateGraph(p); err != nil {
		return nil, err
	}
	budget, err := NormalizeBudget(opts.Budget)
	if err != nil {
		return nil, err
	}
	ranges := BuildRanges(p)
	order := orderedSegmentIDs(p, opts)

	result := &model.RecoveryResult{
		ProjectID:       p.ID,
		Budget:          budget,
		CommitBatchSize: opts.CommitBatchSize,
		ScheduleOrder:   order,
		Ranges:          ranges,
		SegmentMap:      map[string]*model.SegmentReport{},
	}
	if p.LatestCheckpoint != nil {
		result.CheckpointID = p.LatestCheckpoint.ID
	}

	replayIDs := []string{}
	for _, id := range order {
		if rng, ok := ranges[id]; ok && rng.Replay {
			replayIDs = append(replayIDs, id)
		}
	}
	capacity := budget.Workers + budget.QueueDepth
	rejected := map[string]bool{}
	var backpressureErr *model.Error
	if len(replayIDs) > capacity {
		for _, id := range replayIDs[capacity:] {
			rejected[id] = true
			diag := model.Diagnostic{
				Code:     model.EBackpressure,
				Severity: "error",
				Locator:  id,
				Message:  "segment not admitted because worker and queue budget is exhausted",
				Details:  map[string]any{"capacity": capacity, "workers": budget.Workers, "queue_depth": budget.QueueDepth},
			}
			result.Backpressure = append(result.Backpressure, diag)
			result.Diagnostics = append(result.Diagnostics, diag)
			result.SegmentMap[id] = &model.SegmentReport{SegmentID: id, State: model.StateRejected, Range: ranges[id], Diagnostics: []model.Diagnostic{diag}}
		}
		backpressureErr = model.NewError(model.EBackpressure, "$.budget",
			"%d replay segments exceed deterministic admission capacity %d", len(replayIDs), capacity).
			WithDetails(map[string]any{"replay_segments": len(replayIDs), "capacity": capacity})
		replayIDs = replayIDs[:capacity]
	}

	admission := NewAdmission(budget)
	sem := make(chan struct{}, budget.Workers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for _, id := range order {
		seg := p.Segment(id)
		if seg == nil {
			continue
		}
		rng := ranges[id]
		if !rng.Replay {
			rep, replayErr := ReplaySegment(seg, rng, budget)
			if replayErr != nil {
				errOnce.Do(func() { firstErr = replayErr })
			}
			mu.Lock()
			result.SegmentMap[id] = rep
			mu.Unlock()
			continue
		}
		if rejected[id] {
			continue
		}
		if admitErr := admission.TryAdmit(id); admitErr != nil {
			be := model.AsError(admitErr)
			diag := model.Diagnostic{Code: be.Code, Severity: "error", Locator: be.Locator, Message: be.Message, Details: be.Details}
			result.Backpressure = append(result.Backpressure, diag)
			result.Diagnostics = append(result.Diagnostics, diag)
			result.SegmentMap[id] = &model.SegmentReport{SegmentID: id, State: model.StateRejected, Range: rng, Diagnostics: []model.Diagnostic{diag}}
			errOnce.Do(func() { firstErr = admitErr })
			continue
		}
		wg.Add(1)
		go func(seg *model.Segment, rng model.ReplayRange) {
			defer wg.Done()
			defer admission.Release()
			sem <- struct{}{}
			defer func() { <-sem }()
			rep, replayErr := ReplaySegment(seg, rng, budget)
			if replayErr != nil {
				errOnce.Do(func() { firstErr = replayErr })
			}
			mu.Lock()
			if rep != nil {
				result.SegmentMap[seg.ID] = rep
			}
			mu.Unlock()
		}(seg, rng)
	}
	wg.Wait()

	for _, seg := range p.SortedSegments() {
		if rep := result.SegmentMap[seg.ID]; rep != nil {
			result.Segments = append(result.Segments, rep)
		}
	}
	baseline := opts.BaselineItems
	if baseline == nil {
		baseline = ComputeBaseline(p, p.LatestCheckpoint)
	}
	result.Items = AggregateItems(baseline, result.Segments, opts.CommitBatchSize)
	result.ItemMap = map[string]model.DataItem{}
	for _, item := range result.Items {
		result.ItemMap[item.Key] = item
	}
	txSet := map[string]bool{}
	abortSet := map[string]bool{}
	for _, rep := range result.Segments {
		for _, tx := range rep.CommittedTransactions {
			txSet[tx] = true
		}
		for _, tx := range rep.AbortedTransactions {
			abortSet[tx] = true
		}
		for _, d := range rep.Diagnostics {
			result.Diagnostics = append(result.Diagnostics, d)
		}
	}
	for tx := range txSet {
		result.CommittedTxIDs = append(result.CommittedTxIDs, tx)
	}
	for tx := range abortSet {
		result.AbortedTxIDs = append(result.AbortedTxIDs, tx)
	}
	sort.Strings(result.CommittedTxIDs)
	sort.Strings(result.AbortedTxIDs)
	if firstErr != nil {
		return result, firstErr
	}
	if backpressureErr != nil {
		return result, backpressureErr
	}
	return result, nil
}
