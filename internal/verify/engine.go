package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"screenverify/internal/model"
	"screenverify/internal/rverr"
	"screenverify/internal/store"
)

type NodeID struct {
	Frame int
	Type  EvidenceType
}

type Engine struct {
	set    *model.FrameSet
	params Params
	rt     RuntimeConfig
	store  *store.Store

	truthFP     string
	contentRoot string
	limiter     *BufferLimiter
	pool        *workerPool

	stateMu           sync.Mutex
	values            map[NodeID]any
	keys              map[NodeID]string
	upstreams         map[NodeID][]string
	replace           map[NodeID]string
	stats             Stats
	commits           []CommitItem
	invalidations     []Invalidation
	lastInvalidations []Invalidation
	failures          []FailureRecord

	faultMu    sync.Mutex
	faultArmed bool
	faultFrame int
	faultType  EvidenceType
}

func NewEngine(set *model.FrameSet, p Params, rt RuntimeConfig, st *store.Store) (*Engine, error) {
	t := set.Truth
	if err := p.Validate(t.Width, t.Height, t.FrameCount); err != nil {
		return nil, err
	}
	if err := rt.Validate(t.Width, t.Height); err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(set.Frames))
	for _, f := range set.Frames {
		hashes = append(hashes, f.ContentHash)
	}
	truthFP := t.TruthFingerprint
	if truthFP == "" {
		fp, err := model.FingerprintTruth(t)
		if err != nil {
			return nil, err
		}
		truthFP = fp
	}
	e := &Engine{
		set:         set,
		params:      p,
		rt:          rt,
		store:       st,
		truthFP:     truthFP,
		contentRoot: model.HashBytes("content-root-v1:", []byte(strings.Join(hashes, ","))),
		limiter:     NewBufferLimiter(rt.Parallelism, rt.BufferPixelsPerThread),
		pool:        newWorkerPool(rt.Parallelism),
		values:      map[NodeID]any{},
		keys:        map[NodeID]string{},
		upstreams:   map[NodeID][]string{},
		replace:     map[NodeID]string{},
		stats:       Stats{Parallelism: rt.Parallelism},
		faultArmed:  rt.FailureArmed,
		faultFrame:  rt.FailureFrame,
		faultType:   rt.FailureType,
	}
	return e, nil
}

func (e *Engine) Params() Params { return e.params }

func (e *Engine) ContentRoot() string { return e.contentRoot }

func (e *Engine) TruthFingerprint() string { return e.truthFP }

func (e *Engine) BatchCount() int {
	n := e.set.Truth.FrameCount
	return (n + e.params.BatchFrames - 1) / e.params.BatchFrames
}

func BatchID(index int) string {
	return fmt.Sprintf("initial-batch-%04d", index+1)
}

func ChangeBatchID(index int, name string) string {
	return fmt.Sprintf("change-%03d-%s", index+1, name)
}

func (e *Engine) EnsureIdentity(ctx context.Context, planHash string) (*RecoveryInfo, error) {
	expected := map[string]string{
		"algo_version":          model.AlgorithmVersion,
		"truth_fingerprint":     e.truthFP,
		"content_root":          e.contentRoot,
		"plan_hash":             planHash,
		"initial_param_version": ParamVersion(e.params),
	}
	discarded := false
	reason := ""
	for k, v := range expected {
		old, ok, err := e.store.GetRunMeta(ctx, k)
		if err != nil {
			return nil, err
		}
		if ok && old != v {
			discarded = true
			reason = "identity mismatch: " + k
			break
		}
	}
	if discarded {
		if err := e.store.ResetAll(ctx); err != nil {
			return nil, err
		}
	}
	for k, v := range expected {
		if err := e.store.SetRunMeta(ctx, k, v); err != nil {
			return nil, err
		}
	}
	return &RecoveryInfo{SnapshotDiscarded: discarded, Reason: reason}, nil
}

func (e *Engine) Recover(ctx context.Context, planHash string) (*RecoveryInfo, error) {
	info, err := e.EnsureIdentity(ctx, planHash)
	if err != nil {
		return nil, err
	}
	checkpoints, err := e.store.Checkpoints(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range checkpoints {
		if c.AlgoVersion != model.AlgorithmVersion || c.ParamVersion != ParamVersion(e.params) ||
			c.TruthFP != e.truthFP || c.ContentRoot != e.contentRoot {
			if err := e.store.ResetAll(ctx); err != nil {
				return nil, err
			}
			info.SnapshotDiscarded = true
			info.Reason = "checkpoint parameter/algorithm/content version mismatch at seq " + fmt.Sprint(c.Seq)
			return info, nil
		}
	}
	items, err := e.store.BatchItems(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := e.store.Entries(ctx)
	if err != nil {
		return nil, err
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.values = map[NodeID]any{}
	e.keys = map[NodeID]string{}
	e.upstreams = map[NodeID][]string{}
	e.replace = map[NodeID]string{}
	for _, it := range items {
		e.stats.HitCount++
		e.commits = append(e.commits, CommitItem{
			Seq: it.Seq, BatchID: "", Ordinal: it.Ordinal,
			Frame: it.FrameNo, EvidenceType: EvidenceType(it.EvidenceType), CacheKey: it.CacheKey,
		})
	}
	checkpointBatch := map[int64]string{}
	for _, c := range checkpoints {
		checkpointBatch[c.Seq] = c.BatchID
	}
	for i := range e.commits {
		e.commits[i].BatchID = checkpointBatch[e.commits[i].Seq]
	}
	for _, st := range entries {
		t := EvidenceType(st.EvidenceType)
		v, err := decodePayload(t, st.PayloadJSON)
		if err != nil {
			return nil, rverr.Wrap(rverr.EStore, "decode stored evidence", err)
		}
		id := NodeID{Frame: st.FrameNo, Type: t}
		e.values[id] = v
		e.keys[id] = st.CacheKey
		var ups []string
		_ = json.Unmarshal([]byte(st.UpstreamKeys), &ups)
		e.upstreams[id] = ups
	}
	info.Recovered = len(checkpoints) > 0
	info.CompletedCheckpoints = len(checkpoints)
	info.LoadedEvidenceEntries = len(entries)
	return info, nil
}

func decodePayload(t EvidenceType, raw string) (any, error) {
	switch t {
	case EvWatermark:
		var v WatermarkEvidence
		return v, json.Unmarshal([]byte(raw), &v)
	case EvTimeline:
		var v TimelineEvidence
		return v, json.Unmarshal([]byte(raw), &v)
	case EvResidual:
		var v ResidualEvidence
		return v, json.Unmarshal([]byte(raw), &v)
	case EvNoise:
		var v NoiseEvidence
		return v, json.Unmarshal([]byte(raw), &v)
	case EvEdge:
		var v EdgeEvidence
		return v, json.Unmarshal([]byte(raw), &v)
	case EvDecision:
		var v DecisionEvidence
		return v, json.Unmarshal([]byte(raw), &v)
	default:
		return nil, fmt.Errorf("unknown evidence type %q", t)
	}
}

func (e *Engine) RunBatch(ctx context.Context, batchIndex int) ([]NodeID, error) {
	start := batchIndex * e.params.BatchFrames
	if start >= len(e.set.Frames) {
		return nil, rverr.New(rverr.EPara, "batch index out of range")
	}
	end := minInt(start+e.params.BatchFrames, len(e.set.Frames))
	rawTypes := []EvidenceType{EvWatermark, EvResidual, EvNoise, EvEdge}
	var errMu sync.Mutex
	var firstErr error
	for fi := start; fi < end; fi++ {
		frameIdx := fi
		for _, typ := range rawTypes {
			typ := typ
			e.pool.Go(func() {
				if err := e.computeRaw(ctx, frameIdx, typ); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			})
		}
	}
	e.pool.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	for fi := start; fi < end; fi++ {
		if err := e.computeTimeline(ctx, fi); err != nil {
			return nil, err
		}
	}
	for fi := start; fi < end; fi++ {
		idx := fi
		e.pool.Go(func() {
			if err := e.computeDecision(ctx, idx); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		})
	}
	e.pool.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	ids := make([]NodeID, 0, (end-start)*6)
	for frame := start + 1; frame <= end; frame++ {
		for _, typ := range allTypes() {
			ids = append(ids, NodeID{Frame: frame, Type: typ})
		}
	}
	_, err := e.CommitNodes(ctx, BatchID(batchIndex), "frame_batch", ids, len(ids), 0)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (e *Engine) computeRaw(ctx context.Context, idx int, typ EvidenceType) error {
	frame := e.set.Frames[idx]
	id := NodeID{Frame: frame.No, Type: typ}
	content := []string{frame.ContentHash}
	upstream := []string{SourceKey(frame)}
	var fn func() (any, error)

	switch typ {
	case EvWatermark:
		region := e.regionFor(idx)
		if !region.ValidFor(frame.Width, frame.Height) {
			return rverr.Frame(rverr.ERange, frame.No, "watermark_region", "watermark region is outside frame")
		}
		fn = func() (any, error) { return decodeWatermark(frame, region, e.params), nil }
	case EvResidual:
		content = e.contentRange(idx-e.params.ResidualWindow-1, idx)
		upstream = sourceKeys(e.set.Frames, idx-e.params.ResidualWindow-1, idx)
		fn = func() (any, error) { return residualEvidence(e.set.Frames, idx, e.params), nil }
	case EvNoise:
		fn = func() (any, error) { return noiseEvidence(frame, e.params), nil }
	case EvEdge:
		upstream = append(upstream, "truth:"+e.truthFP)
		fn = func() (any, error) { return edgeEvidence(e.set.Frames, e.set.Truth, idx), nil }
	default:
		return rverr.New(rverr.EPara, "not a raw evidence type: "+string(typ))
	}
	return e.resolveAndStore(ctx, id, content, upstream, fn)
}

func (e *Engine) computeTimeline(ctx context.Context, idx int) error {
	frame := e.set.Frames[idx]
	id := NodeID{Frame: frame.No, Type: EvTimeline}
	wm, err := e.watermark(frame.No)
	if err != nil {
		return err
	}
	upstream := []string{e.mustKey(NodeID{Frame: frame.No, Type: EvWatermark})}
	var prev *WatermarkEvidence
	if frame.No > 1 {
		pwm, err := e.watermark(frame.No - 1)
		if err == nil {
			prev = &pwm
			upstream = append(upstream, e.mustKey(NodeID{Frame: frame.No - 1, Type: EvWatermark}))
		}
	}
	fn := func() (any, error) { return buildTimeline(wm, prev), nil }
	return e.resolveAndStore(ctx, id, []string{frame.ContentHash}, upstream, fn)
}

func (e *Engine) computeDecision(ctx context.Context, idx int) error {
	frame := e.set.Frames[idx]
	id := NodeID{Frame: frame.No, Type: EvDecision}
	wm, err := e.watermark(frame.No)
	if err != nil {
		return err
	}
	tl, err := e.timeline(frame.No)
	if err != nil {
		return err
	}
	res, err := e.residual(frame.No)
	if err != nil {
		return err
	}
	noise, err := e.noise(frame.No)
	if err != nil {
		return err
	}
	edge, err := e.edge(frame.No)
	if err != nil {
		return err
	}
	upstream := []string{
		e.mustKey(NodeID{Frame: frame.No, Type: EvWatermark}),
		e.mustKey(NodeID{Frame: frame.No, Type: EvTimeline}),
		e.mustKey(NodeID{Frame: frame.No, Type: EvResidual}),
		e.mustKey(NodeID{Frame: frame.No, Type: EvNoise}),
		e.mustKey(NodeID{Frame: frame.No, Type: EvEdge}),
	}
	fn := func() (any, error) {
		return decide(frame, e.params, wm, tl, res, noise, edge), nil
	}
	return e.resolveAndStore(ctx, id, []string{frame.ContentHash}, upstream, fn)
}

func (e *Engine) resolveAndStore(ctx context.Context, id NodeID, content, upstream []string, fn func() (any, error)) error {
	key := CacheKey(content, id.Type, e.params, upstream)
	e.stateMu.Lock()
	e.keys[id] = key
	e.upstreams[id] = upstream
	if v, ok := e.values[id]; ok {
		e.stateMu.Unlock()
		_ = v
		return nil
	}
	e.stateMu.Unlock()

	stored, ok, err := e.store.Get(ctx, key)
	if err != nil {
		return rverr.Wrap(rverr.EStore, "read evidence cache", err)
	}
	if ok {
		v, err := decodePayload(id.Type, stored.PayloadJSON)
		if err != nil {
			return err
		}
		e.stateMu.Lock()
		e.values[id] = v
		e.stats.HitCount++
		e.stateMu.Unlock()
		return nil
	}

	if err := e.maybeFault(id); err != nil {
		e.recordFailure(ctx, id, key, upstream, err)
		return err
	}

	need := int64(e.set.Truth.Width * e.set.Truth.Height)
	lease, err := e.limiter.Acquire(ctx, need, id.Frame, id.Type, "evidence_pixel_scratch")
	if err != nil {
		return rverr.Wrap(rverr.EFault, "pixel buffer acquisition failed", err)
	}
	defer lease.Release()

	v, err := fn()
	if err != nil {
		e.recordFailure(ctx, id, key, upstream, err)
		return err
	}
	e.stateMu.Lock()
	e.values[id] = v
	e.stats.RecomputeCount++
	for i := len(e.failures) - 1; i >= 0; i-- {
		if e.failures[i].Frame == id.Frame && e.failures[i].EvidenceType == id.Type && !e.failures[i].Resolved {
			e.failures[i].Resolved = true
			break
		}
	}
	e.stateMu.Unlock()
	return nil
}

func (e *Engine) recordFailure(ctx context.Context, id NodeID, key string, upstream []string, err error) {
	code := string(rverr.CodeOf(err))
	if code == "" {
		code = string(rverr.EFault)
	}
	record := FailureRecord{Frame: id.Frame, EvidenceType: id.Type, Code: code, Message: err.Error()}
	e.stateMu.Lock()
	e.stats.FailureCount++
	e.failures = append(e.failures, record)
	e.stateMu.Unlock()
	_ = e.store.PutFailure(ctx, store.Entry{
		CacheKey: key, FrameNo: id.Frame, EvidenceType: string(id.Type),
		ContentHash: e.set.Frames[id.Frame-1].ContentHash, ParamVersion: ParamVersion(e.params),
		AlgoVersion: model.AlgorithmVersion, UpstreamKeys: upstream,
	}, code, err.Error())
}

func (e *Engine) maybeFault(id NodeID) error {
	e.faultMu.Lock()
	defer e.faultMu.Unlock()
	if e.faultArmed && e.faultFrame == id.Frame && e.faultType == id.Type {
		e.faultArmed = false
		return rverr.New(rverr.EFault, "injected transient failure for evidence retry test")
	}
	return nil
}

func (e *Engine) ArmFault(frame int, typ EvidenceType) {
	e.faultMu.Lock()
	e.faultArmed = true
	e.faultFrame = frame
	e.faultType = typ
	e.faultMu.Unlock()
}

func (e *Engine) RetryFailedBatch(ctx context.Context, batchIndex int) error {
	_, err := e.RunBatch(ctx, batchIndex)
	if err == nil {
		e.stateMu.Lock()
		e.stats.RetryCount++
		e.stateMu.Unlock()
	}
	return err
}

func (e *Engine) CommitNodes(ctx context.Context, batchID, kind string, ids []NodeID, recompute, hits int) (int64, error) {
	entries := make([]store.Entry, 0, len(ids))
	e.stateMu.Lock()
	for _, id := range ids {
		v, ok := e.values[id]
		if !ok {
			e.stateMu.Unlock()
			return 0, rverr.New(rverr.EFault, "missing evidence value before commit: "+fmt.Sprint(id))
		}
		key := e.keys[id]
		old := e.replace[id]
		entries = append(entries, store.Entry{
			CacheKey:     key,
			FrameNo:      id.Frame,
			EvidenceType: string(id.Type),
			ContentHash:  e.set.Frames[id.Frame-1].ContentHash,
			ParamVersion: ParamVersion(e.params),
			AlgoVersion:  model.AlgorithmVersion,
			UpstreamKeys: append([]string(nil), e.upstreams[id]...),
			Payload:      v,
			ReplacedKey:  old,
		})
		delete(e.replace, id)
	}
	e.stateMu.Unlock()

	b := store.Batch{
		BatchID: batchID, Kind: kind, ParamVersion: ParamVersion(e.params),
		AlgoVersion: model.AlgorithmVersion, TruthFP: e.truthFP, ContentRoot: e.contentRoot,
		ContentHash:    e.batchContentHash(ids),
		RecomputeCount: recompute, HitCount: hits, Entries: entries,
	}
	b.EvidenceDigest = store.DigestBatch(b)
	seq, err := e.store.SubmitBatch(ctx, b)
	if err != nil {
		return 0, rverr.Wrap(rverr.EStore, "commit checkpoint batch", err)
	}
	e.stateMu.Lock()
	for i, id := range ids {
		e.commits = append(e.commits, CommitItem{
			Seq: seq, BatchID: batchID, Ordinal: i, Frame: id.Frame,
			EvidenceType: id.Type, CacheKey: e.keys[id],
		})
	}
	e.stateMu.Unlock()
	return seq, nil
}

func (e *Engine) batchContentHash(ids []NodeID) string {
	seen := map[int]bool{}
	parts := []string{}
	for _, id := range sortedNodeIDs(ids) {
		if seen[id.Frame] {
			continue
		}
		seen[id.Frame] = true
		parts = append(parts, e.set.Frames[id.Frame-1].ContentHash)
	}
	return model.HashBytes("batch-content-v1:", []byte(strings.Join(parts, ",")))
}

func (e *Engine) regionFor(idx int) model.Rect {
	if e.params.WatermarkRegion != nil {
		return *e.params.WatermarkRegion
	}
	return e.set.Truth.Frames[idx].WatermarkRegion
}

func (e *Engine) mustKey(id NodeID) string {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.keys[id]
}

func (e *Engine) watermark(frame int) (WatermarkEvidence, error) {
	v, ok := e.get(NodeID{Frame: frame, Type: EvWatermark})
	if !ok {
		return WatermarkEvidence{}, rverr.Frame(rverr.EFault, frame, "watermark_decode", "missing watermark evidence")
	}
	return v.(WatermarkEvidence), nil
}

func (e *Engine) timeline(frame int) (TimelineEvidence, error) {
	v, ok := e.get(NodeID{Frame: frame, Type: EvTimeline})
	if !ok {
		return TimelineEvidence{}, rverr.Frame(rverr.EFault, frame, "timeline_continuity", "missing timeline evidence")
	}
	return v.(TimelineEvidence), nil
}

func (e *Engine) residual(frame int) (ResidualEvidence, error) {
	v, ok := e.get(NodeID{Frame: frame, Type: EvResidual})
	if !ok {
		return ResidualEvidence{}, rverr.Frame(rverr.EFault, frame, "residual_energy", "missing residual evidence")
	}
	return v.(ResidualEvidence), nil
}

func (e *Engine) noise(frame int) (NoiseEvidence, error) {
	v, ok := e.get(NodeID{Frame: frame, Type: EvNoise})
	if !ok {
		return NoiseEvidence{}, rverr.Frame(rverr.EFault, frame, "noise_variance", "missing noise evidence")
	}
	return v.(NoiseEvidence), nil
}

func (e *Engine) edge(frame int) (EdgeEvidence, error) {
	v, ok := e.get(NodeID{Frame: frame, Type: EvEdge})
	if !ok {
		return EdgeEvidence{}, rverr.Frame(rverr.EFault, frame, "edge_consistency", "missing edge evidence")
	}
	return v.(EdgeEvidence), nil
}

func (e *Engine) get(id NodeID) (any, bool) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	v, ok := e.values[id]
	return v, ok
}

func allTypes() []EvidenceType {
	return []EvidenceType{EvWatermark, EvTimeline, EvResidual, EvNoise, EvEdge, EvDecision}
}

func typeRank(t EvidenceType) int {
	for i, x := range allTypes() {
		if x == t {
			return i
		}
	}
	return 99
}

func sortedNodeIDs(ids []NodeID) []NodeID {
	out := append([]NodeID(nil), ids...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Frame != out[j].Frame {
			return out[i].Frame < out[j].Frame
		}
		return typeRank(out[i].Type) < typeRank(out[j].Type)
	})
	return out
}

func (e *Engine) contentRange(startIdx, endIdx int) []string {
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

func sourceKeys(frames []*model.Frame, start, end int) []string {
	if start < 0 {
		start = 0
	}
	if end >= len(frames) {
		end = len(frames) - 1
	}
	out := []string{}
	for i := start; i <= end; i++ {
		out = append(out, SourceKey(frames[i]))
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
