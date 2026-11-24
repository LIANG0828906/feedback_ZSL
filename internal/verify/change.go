package verify

import (
	"context"

	"screenverify/internal/model"
	"screenverify/internal/rverr"
)

type ChangeResult struct {
	BatchID         string         `json:"batch_id"`
	Field           string         `json:"field"`
	OldParamVersion string         `json:"old_param_version"`
	NewParamVersion string         `json:"new_param_version"`
	Invalidations   []Invalidation `json:"invalidations"`
	RecomputeCount  int            `json:"recompute_count"`
	HitCount        int            `json:"hit_count"`
	CheckpointSeq   int64          `json:"checkpoint_seq"`
}

func (e *Engine) ApplyParameterChange(ctx context.Context, changeIndex int, id, field string, next Params) (*ChangeResult, error) {
	if err := next.Validate(e.set.Truth.Width, e.set.Truth.Height, e.set.Truth.FrameCount); err != nil {
		return nil, err
	}
	batchID := ChangeBatchID(changeIndex, id)
	oldVersion := ParamVersion(e.params)
	invalidations, invalidIDs, err := e.planInvalidations(next, field)
	if err != nil {
		return nil, err
	}

	e.stateMu.Lock()
	for _, nid := range invalidIDs {
		e.replace[nid] = e.keys[nid]
		delete(e.values, nid)
	}
	e.params = next
	e.stats.HitCount += e.set.Truth.FrameCount*len(allTypes()) - len(invalidIDs)
	e.stateMu.Unlock()

	rawInvalid := map[EvidenceType]bool{EvWatermark: true, EvResidual: true, EvNoise: true, EvEdge: true}
	var rawIDs []NodeID
	var timelineIDs []NodeID
	var decisionIDs []NodeID
	for _, nid := range invalidIDs {
		switch nid.Type {
		case EvTimeline:
			timelineIDs = append(timelineIDs, nid)
		case EvDecision:
			decisionIDs = append(decisionIDs, nid)
		default:
			if rawInvalid[nid.Type] {
				rawIDs = append(rawIDs, nid)
			}
		}
	}

	pool := newWorkerPool(e.rt.Parallelism)
	var errMu = newErrorMutex()
	for _, nid := range rawIDs {
		nid := nid
		pool.Go(func() {
			idx := nid.Frame - 1
			if err := e.computeRaw(ctx, idx, nid.Type); err != nil {
				errMu.set(err)
			}
		})
	}
	pool.Wait()
	if err := errMu.get(); err != nil {
		return nil, err
	}

	for _, nid := range timelineIDs {
		if err := e.computeTimeline(ctx, nid.Frame-1); err != nil {
			return nil, err
		}
	}
	for _, nid := range decisionIDs {
		nid := nid
		pool.Go(func() {
			if err := e.computeDecision(ctx, nid.Frame-1); err != nil {
				errMu.set(err)
			}
		})
	}
	pool.Wait()
	if err := errMu.get(); err != nil {
		return nil, err
	}

	seq, err := e.CommitNodes(ctx, batchID, "parameter_change", invalidIDs, len(invalidIDs),
		e.set.Truth.FrameCount*len(allTypes())-len(invalidIDs))
	if err != nil {
		return nil, err
	}

	e.stateMu.Lock()
	e.invalidations = append(e.invalidations, invalidations...)
	e.lastInvalidations = invalidations
	e.stateMu.Unlock()

	return &ChangeResult{
		BatchID: batchID, Field: field, OldParamVersion: oldVersion, NewParamVersion: ParamVersion(next),
		Invalidations: invalidations, RecomputeCount: len(invalidIDs),
		HitCount: e.set.Truth.FrameCount*len(allTypes()) - len(invalidIDs), CheckpointSeq: seq,
	}, nil
}

type errorMutex struct {
	mu  chan struct{}
	err error
}

func newErrorMutex() *errorMutex { return &errorMutex{mu: make(chan struct{}, 1)} }

func (m *errorMutex) set(err error) {
	m.mu <- struct{}{}
	if m.err == nil {
		m.err = err
	}
	<-m.mu
}

func (m *errorMutex) get() error {
	m.mu <- struct{}{}
	err := m.err
	<-m.mu
	return err
}

func (e *Engine) planInvalidations(next Params, field string) ([]Invalidation, []NodeID, error) {
	n := e.set.Truth.FrameCount
	newKeys := map[NodeID]string{}
	newUp := map[NodeID][]string{}

	for frameNo := 1; frameNo <= n; frameNo++ {
		idx := frameNo - 1
		f := e.set.Frames[idx]
		region := next.WatermarkRegion
		if region == nil {
			r := e.set.Truth.Frames[idx].WatermarkRegion
			region = &r
		}
		if !region.ValidFor(f.Width, f.Height) {
			return nil, nil, rverr.Frame(rverr.ERange, frameNo, "watermark_region", "corrected watermark region is outside frame")
		}
		wmContent := []string{f.ContentHash}
		wmUp := []string{SourceKey(f)}
		wmID := NodeID{frameNo, EvWatermark}
		newKeys[wmID] = CacheKey(wmContent, EvWatermark, next, wmUp)
		newUp[wmID] = wmUp

		resContent := e.contentRangeWith(next.ResidualWindow, idx-next.ResidualWindow-1, idx)
		resUp := sourceKeys(e.set.Frames, idx-next.ResidualWindow-1, idx)
		resID := NodeID{frameNo, EvResidual}
		newKeys[resID] = CacheKey(resContent, EvResidual, next, resUp)
		newUp[resID] = resUp

		noiseID := NodeID{frameNo, EvNoise}
		newKeys[noiseID] = CacheKey([]string{f.ContentHash}, EvNoise, next, []string{SourceKey(f)})
		newUp[noiseID] = []string{SourceKey(f)}

		edgeUp := []string{SourceKey(f), "truth:" + e.truthFP}
		edgeID := NodeID{frameNo, EvEdge}
		newKeys[edgeID] = CacheKey([]string{f.ContentHash}, EvEdge, next, edgeUp)
		newUp[edgeID] = edgeUp
	}

	for frameNo := 1; frameNo <= n; frameNo++ {
		wmID := NodeID{frameNo, EvWatermark}
		up := []string{newKeys[wmID]}
		if frameNo > 1 {
			up = append(up, newKeys[NodeID{frameNo - 1, EvWatermark}])
		}
		id := NodeID{frameNo, EvTimeline}
		newKeys[id] = CacheKey([]string{e.set.Frames[frameNo-1].ContentHash}, EvTimeline, next, up)
		newUp[id] = up
	}
	for frameNo := 1; frameNo <= n; frameNo++ {
		up := []string{
			newKeys[NodeID{frameNo, EvWatermark}],
			newKeys[NodeID{frameNo, EvTimeline}],
			newKeys[NodeID{frameNo, EvResidual}],
			newKeys[NodeID{frameNo, EvNoise}],
			newKeys[NodeID{frameNo, EvEdge}],
		}
		id := NodeID{frameNo, EvDecision}
		newKeys[id] = CacheKey([]string{e.set.Frames[frameNo-1].ContentHash}, EvDecision, next, up)
		newUp[id] = up
	}

	invalidKeyType := map[string]EvidenceType{}
	var ids []NodeID
	for frameNo := 1; frameNo <= n; frameNo++ {
		for _, typ := range allTypes() {
			id := NodeID{frameNo, typ}
			oldKey := e.keys[id]
			if oldKey == "" || oldKey != newKeys[id] {
				ids = append(ids, id)
				invalidKeyType[newKeys[id]] = typ
			}
		}
	}
	ids = sortedNodeIDs(ids)

	out := make([]Invalidation, 0, len(ids))
	for _, id := range ids {
		reason := invalidReason(id.Type, field, newUp[id], invalidKeyType)
		out = append(out, Invalidation{
			Frame: id.Frame, EvidenceType: id.Type, CacheKey: e.keys[id], Reason: reason,
		})
	}
	return out, ids, nil
}

func (e *Engine) contentRangeWith(window, startIdx, endIdx int) []string {
	if startIdx < 0 {
		startIdx = 0
	}
	if endIdx >= len(e.set.Frames) {
		endIdx = len(e.set.Frames) - 1
	}
	out := []string{}
	for i := startIdx; i <= endIdx; i++ {
		out = append(out, e.set.Frames[i].ContentHash)
	}
	return out
}

func invalidReason(typ EvidenceType, field string, upstream []string, invalidNewKeys map[string]EvidenceType) string {
	if directFieldDependency(typ, field) {
		return "param:" + field
	}
	for _, key := range upstream {
		if upType, ok := invalidNewKeys[key]; ok {
			return "upstream:" + string(upType)
		}
	}
	return "cache_key_changed"
}

func directFieldDependency(typ EvidenceType, field string) bool {
	switch field {
	case "block_size":
		return typ == EvNoise
	case "residual_window":
		return typ == EvResidual
	case "grid_x", "grid_y", "watermark_region", "watermark_threshold":
		return typ == EvWatermark
	case "tamper_threshold", "residual_z_threshold", "noise_z_threshold", "edge_threshold":
		return typ == EvDecision
	default:
		return false
	}
}

var _ = model.AlgorithmVersion
