package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"integritykernel/internal/checkpoint"
	"integritykernel/internal/core"
	"integritykernel/internal/forensics"
	"integritykernel/internal/loader"
	"integritykernel/internal/watermark"
)

type Envelope struct {
	Frame         int
	Type          string
	Key           string
	ContentHash   string
	ParamHash     string
	UpstreamKeys  []string
	ContextHashes []string
	Payload       any
	RawJSON       []byte
	Digest        string
	BatchSeq      int
}

type Options struct {
	Params     core.Params
	Region     *core.Rect
	Store      *checkpoint.Store
	CommitHook checkpoint.CommitHook
	Fault      func(frame int, evidenceType string) error
}

type Engine struct {
	seq    *loader.Sequence
	params core.Params
	region core.Rect
	planes []*forensics.LumaPlane
	store  *checkpoint.Store

	commitHook checkpoint.CommitHook
	fault      func(frame int, evidenceType string) error

	runMu sync.Mutex
	mu    sync.Mutex
	cache map[string]*Envelope

	failures      map[string]core.FailureRecord
	attempts      map[string]int
	invalidations []core.InvalidationRecord
	queueEvents   []core.QueueEvent

	nextBatchSeq int
}

type keyInput struct {
	AlgorithmVersion string         `json:"algorithm_version"`
	ContentHash      string         `json:"content_hash"`
	EvidenceType     string         `json:"evidence_type"`
	Region           *core.Rect     `json:"region,omitempty"`
	Params           map[string]any `json:"params"`
	ContextHashes    []string       `json:"context_hashes,omitempty"`
	UpstreamKeys     []string       `json:"upstream_keys,omitempty"`
}

func New(seq *loader.Sequence, opts Options) (*Engine, error) {
	params := opts.Params
	if params.SampleCols == 0 {
		params = core.DefaultParams()
	}
	if err := params.Validate(seq.Manifest.Width, seq.Manifest.Height); err != nil {
		return nil, err
	}
	region := seq.Manifest.WatermarkRegion
	if opts.Region != nil {
		region = *opts.Region
	}
	if !region.InFrame(seq.Manifest.Width, seq.Manifest.Height) {
		frame := 0
		return nil, &core.Error{Code: core.CodeMark, Message: "watermark region is outside frame", Frame: &frame, Field: "watermark_region"}
	}
	planes := make([]*forensics.LumaPlane, len(seq.Frames))
	for i, frame := range seq.Frames {
		planes[i] = forensics.BuildLuma(frame)
	}
	return &Engine{
		seq:          seq,
		params:       params,
		region:       region,
		planes:       planes,
		store:        opts.Store,
		commitHook:   opts.CommitHook,
		fault:        opts.Fault,
		cache:        map[string]*Envelope{},
		failures:     map[string]core.FailureRecord{},
		attempts:     map[string]int{},
		nextBatchSeq: 1,
	}, nil
}

func NodeID(frame int, evidenceType string) string {
	return fmt.Sprintf("%d:%s", frame, evidenceType)
}

func (e *Engine) Params() core.Params { return e.params }
func (e *Engine) Region() core.Rect   { return e.region }

func (e *Engine) SetFault(fault func(frame int, evidenceType string) error) {
	e.mu.Lock()
	e.fault = fault
	e.mu.Unlock()
}

func (e *Engine) LoadCheckpoint() (string, *core.Error, error) {
	if e.store == nil {
		return "no_store", nil, nil
	}
	pending, hasPending, err := e.store.FirstUnfinishedBatch()
	if err != nil {
		return "", nil, err
	}
	contentHashes := map[int]string{}
	for i, frame := range e.seq.Frames {
		contentHashes[i] = frame.Hash
	}
	matches, reason, err := e.store.SnapshotMatches(e.seq.RootHash, e.params.EffectiveVersion(), core.AlgorithmVersion)
	if err != nil {
		return "", nil, err
	}
	if !matches {
		if err := e.store.Reset(reason); err != nil {
			return "", nil, err
		}
		e.nextBatchSeq = 1
		return reason, nil, nil
	}
	stored, err := e.store.LoadCurrent(e.seq.RootHash, e.params.EffectiveVersion(), core.AlgorithmVersion, contentHashes)
	if err != nil {
		return "", nil, err
	}
	for id, record := range stored {
		payload, err := decodePayload(record.Type, record.PayloadJSON)
		if err != nil {
			if resetErr := e.store.Reset("payload_decode_failure"); resetErr != nil {
				return "", nil, resetErr
			}
			return "payload_decode_failure", nil, nil
		}
		e.cache[id] = &Envelope{
			Frame: record.Frame, Type: record.Type, Key: record.CacheKey,
			ContentHash: record.ContentHash, ParamHash: record.ParamHash,
			Payload: payload, RawJSON: append([]byte(nil), record.PayloadJSON...),
			Digest: record.Digest, BatchSeq: record.BatchSeq,
		}
	}
	maxSeq, err := e.store.MaxBatchSeq()
	if err != nil {
		return "", nil, err
	}
	e.nextBatchSeq = maxSeq + 1
	var txnErr *core.Error
	if hasPending {
		e.nextBatchSeq = pending
		txnErr = core.WithBatch(core.NewError(core.CodeTxn, "first unfinished checkpoint batch after restart"), pending)
	}
	return "loaded", txnErr, nil
}

func decodePayload(evidenceType string, data []byte) (any, error) {
	var payload any
	switch evidenceType {
	case core.EvidenceWatermark:
		payload = &core.WatermarkEvidence{}
	case core.EvidenceResidual:
		payload = &core.ResidualEvidence{}
	case core.EvidenceNoise:
		payload = &core.NoiseEvidence{}
	case core.EvidenceVerdict:
		payload = &core.VerdictEvidence{}
	default:
		return nil, fmt.Errorf("unknown evidence type %s", evidenceType)
	}
	if err := json.Unmarshal(data, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (e *Engine) ApplyParams(next core.Params) ([]core.InvalidationRecord, error) {
	if err := next.Validate(e.seq.Manifest.Width, e.seq.Manifest.Height); err != nil {
		return nil, err
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.mu.Lock()
	old := e.params
	oldRegion := e.region
	e.params = next
	recs := e.invalidateForChangeLocked(old, next, oldRegion, e.region)
	e.invalidations = recs
	e.mu.Unlock()
	return recs, nil
}

func (e *Engine) CorrectRegion(region core.Rect) ([]core.InvalidationRecord, error) {
	if !region.InFrame(e.seq.Manifest.Width, e.seq.Manifest.Height) {
		frame := 0
		return nil, &core.Error{Code: core.CodeMark, Message: "corrected watermark region is outside frame", Frame: &frame, Field: "watermark_region"}
	}
	e.runMu.Lock()
	defer e.runMu.Unlock()
	e.mu.Lock()
	old := e.region
	e.region = region
	oldParams := e.params
	recs := e.invalidateForChangeLocked(oldParams, oldParams, old, region)
	e.invalidations = recs
	e.mu.Unlock()
	return recs, nil
}

func (e *Engine) invalidateForChangeLocked(oldParams, newParams core.Params, oldRegion, newRegion core.Rect) []core.InvalidationRecord {
	direct := map[string][]int{}
	reasons := map[string]string{}
	for _, evidenceType := range core.EvidenceTypes {
		oldRelevant := relevantParams(evidenceType, oldParams)
		newRelevant := relevantParams(evidenceType, newParams)
		changed := changedMapFields(oldRelevant, newRelevant)
		if evidenceType == core.EvidenceWatermark && oldRegion != newRegion {
			changed = append(changed, "watermark_region")
		}
		if len(changed) > 0 {
			sort.Strings(changed)
			reasons[evidenceType] = "param:" + joinComma(changed)
			for frame := 0; frame < e.seq.Manifest.FrameCount; frame++ {
				id := NodeID(frame, evidenceType)
				if _, ok := e.cache[id]; ok {
					direct[evidenceType] = append(direct[evidenceType], frame)
				}
			}
		}
	}

	invalidated := map[string]core.InvalidationRecord{}
	for evidenceType, frames := range direct {
		for _, frame := range frames {
			id := NodeID(frame, evidenceType)
			delete(e.cache, id)
			delete(e.failures, id)
			invalidated[id] = core.InvalidationRecord{Frame: frame, Type: evidenceType, Reason: reasons[evidenceType]}
		}
	}

	for frame := 0; frame < e.seq.Manifest.FrameCount; frame++ {
		verdictID := NodeID(frame, core.EvidenceVerdict)
		if _, ok := invalidated[verdictID]; ok {
			continue
		}
		upstreamReason := ""
		for _, upstreamType := range []string{core.EvidenceWatermark, core.EvidenceResidual, core.EvidenceNoise} {
			if _, ok := invalidated[NodeID(frame, upstreamType)]; ok {
				upstreamReason = "upstream:" + upstreamType
				break
			}
		}
		if upstreamReason != "" {
			if _, cached := e.cache[verdictID]; cached {
				delete(e.cache, verdictID)
				invalidated[verdictID] = core.InvalidationRecord{Frame: frame, Type: core.EvidenceVerdict, Reason: upstreamReason}
			}
		}
	}

	out := make([]core.InvalidationRecord, 0, len(invalidated))
	for _, record := range invalidated {
		out = append(out, record)
	}
	return core.SortedInvalidations(out)
}

func relevantParams(evidenceType string, p core.Params) map[string]any {
	switch evidenceType {
	case core.EvidenceWatermark:
		return map[string]any{
			"sample_cols": p.SampleCols, "sample_rows": p.SampleRows,
			"watermark_threshold": p.WatermarkThreshold, "confidence_threshold": p.ConfidenceThreshold,
			"search_radius": p.SearchRadius, "alignment_tolerance": p.AlignmentTolerance,
		}
	case core.EvidenceResidual:
		return map[string]any{
			"residual_window": p.ResidualWindow, "residual_z_scale": p.ResidualZScale,
		}
	case core.EvidenceNoise:
		return map[string]any{
			"block_size": p.BlockSize, "noise_window": p.NoiseWindow,
			"noise_z_threshold": p.NoiseZThreshold, "edge_bins": p.EdgeBins,
			"edge_anomaly_threshold": p.EdgeAnomalyThreshold,
		}
	case core.EvidenceVerdict:
		return map[string]any{
			"watermark_weight": p.WatermarkWeight, "residual_weight": p.ResidualWeight,
			"noise_weight": p.NoiseWeight, "verdict_threshold": p.VerdictThreshold,
		}
	}
	return map[string]any{}
}

func changedMapFields(oldMap, newMap map[string]any) []string {
	changed := []string{}
	keys := map[string]struct{}{}
	for k := range oldMap {
		keys[k] = struct{}{}
	}
	for k := range newMap {
		keys[k] = struct{}{}
	}
	for k := range keys {
		if core.HashCanonical(oldMap[k]) != core.HashCanonical(newMap[k]) {
			changed = append(changed, k)
		}
	}
	return changed
}

func joinComma(values []string) string {
	out := ""
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

func (e *Engine) Run() error {
	e.runMu.Lock()
	defer e.runMu.Unlock()
	if err := e.params.Validate(e.seq.Manifest.Width, e.seq.Manifest.Height); err != nil {
		return err
	}

	missingRaw := map[string]struct{}{}
	missingVerdicts := map[string]struct{}{}
	for _, item := range core.CanonicalEvidenceOrder(e.seq.Manifest.FrameCount) {
		id := NodeID(item.Frame, item.Type)
		e.mu.Lock()
		env, ok := e.cache[id]
		e.mu.Unlock()
		if ok {
			expectedKey, _, keyErr := e.expectedKey(item.Frame, item.Type)
			if keyErr == nil && env.Key == expectedKey {
				continue
			}
			e.mu.Lock()
			delete(e.cache, id)
			e.mu.Unlock()
		}
		if item.Type == core.EvidenceVerdict {
			missingVerdicts[id] = struct{}{}
		} else {
			missingRaw[id] = struct{}{}
		}
	}

	computed := map[string]*Envelope{}
	var computedMu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, e.params.MaxParallel)

	for id := range missingRaw {
		frame, evidenceType := mustParseID(id)
		e.acquireSlot(frame, evidenceType, slots)
		wg.Add(1)
		go func(frame int, evidenceType string) {
			defer wg.Done()
			defer func() { <-slots }()
			env, err := e.safeCompute(frame, evidenceType, computed, &computedMu)
			if err != nil {
				e.recordFailure(frame, evidenceType, err)
				return
			}
			computedMu.Lock()
			computed[NodeID(frame, evidenceType)] = env
			computedMu.Unlock()
			e.mu.Lock()
			delete(e.failures, NodeID(frame, evidenceType))
			e.mu.Unlock()
		}(frame, evidenceType)
	}
	wg.Wait()

	for id := range missingVerdicts {
		frame, _ := mustParseID(id)
		if !e.upstreamsReady(frame, computed, &computedMu) {
			e.recordFailure(frame, core.EvidenceVerdict, core.NewError(core.CodeInternal, "upstream evidence is unavailable"))
			continue
		}
		e.acquireSlot(frame, core.EvidenceVerdict, slots)
		wg.Add(1)
		go func(frame int) {
			defer wg.Done()
			defer func() { <-slots }()
			env, err := e.safeCompute(frame, core.EvidenceVerdict, computed, &computedMu)
			if err != nil {
				e.recordFailure(frame, core.EvidenceVerdict, err)
				return
			}
			computedMu.Lock()
			computed[NodeID(frame, core.EvidenceVerdict)] = env
			computedMu.Unlock()
			e.mu.Lock()
			delete(e.failures, NodeID(frame, core.EvidenceVerdict))
			e.mu.Unlock()
		}(frame)
	}
	wg.Wait()

	ordered := []*Envelope{}
	for _, item := range core.CanonicalEvidenceOrder(e.seq.Manifest.FrameCount) {
		if env, ok := computed[NodeID(item.Frame, item.Type)]; ok {
			ordered = append(ordered, env)
		}
	}

	for start := 0; start < len(ordered); start += e.params.BatchSize {
		end := start + e.params.BatchSize
		if end > len(ordered) {
			end = len(ordered)
		}
		chunk := ordered[start:end]
		if err := e.commitChunk(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) safeCompute(frame int, evidenceType string, staged map[string]*Envelope, stagedMu *sync.Mutex) (env *Envelope, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = core.NewError(core.CodeInternal, "evidence computation panic: %v", recovered)
		}
	}()
	e.mu.Lock()
	fault := e.fault
	e.mu.Unlock()
	if fault != nil {
		if faultErr := fault(frame, evidenceType); faultErr != nil {
			return nil, faultErr
		}
	}
	pixelEstimate := 8 * e.seq.Manifest.Width * e.seq.Manifest.Height
	if pixelEstimate > e.params.MaxPixelsPerWorker {
		field := "max_pixels_per_worker"
		return nil, core.WithField(core.NewError(core.CodeResource, "estimated pixel buffers %d exceed per-worker cap %d", pixelEstimate, e.params.MaxPixelsPerWorker), field)
	}
	return e.compute(frame, evidenceType, staged, stagedMu)
}

func (e *Engine) acquireSlot(frame int, evidenceType string, slots chan struct{}) {
	select {
	case slots <- struct{}{}:
		return
	default:
		e.mu.Lock()
		e.queueEvents = append(e.queueEvents, core.QueueEvent{Frame: frame, Type: evidenceType, Reason: "pixel_buffer_exhausted_waiting_in_queue"})
		e.mu.Unlock()
		slots <- struct{}{}
	}
}

func (e *Engine) recordFailure(frame int, evidenceType string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := NodeID(frame, evidenceType)
	e.attempts[id]++
	code := core.CodeInternal
	message := err.Error()
	if kernelErr, ok := core.AsError(err); ok {
		code = kernelErr.Code
		message = kernelErr.Message
	}
	e.failures[id] = core.FailureRecord{
		Frame: frame, Type: evidenceType, Code: code, Message: message, Attempts: e.attempts[id],
	}
}

func (e *Engine) upstreamsReady(frame int, computed map[string]*Envelope, computedMu *sync.Mutex) bool {
	for _, evidenceType := range []string{core.EvidenceWatermark, core.EvidenceResidual, core.EvidenceNoise} {
		id := NodeID(frame, evidenceType)
		computedMu.Lock()
		if _, ok := computed[id]; ok {
			computedMu.Unlock()
			continue
		}
		computedMu.Unlock()
		e.mu.Lock()
		_, cached := e.cache[id]
		_, failed := e.failures[id]
		e.mu.Unlock()
		if failed || !cached {
			return false
		}
	}
	return true
}

func (e *Engine) commitChunk(chunk []*Envelope) error {
	if e.store == nil {
		e.mu.Lock()
		for _, env := range chunk {
			e.cache[NodeID(env.Frame, env.Type)] = env
		}
		e.mu.Unlock()
		return nil
	}
	seq := e.nextBatchSeq
	items := make([]checkpoint.Item, 0, len(chunk))
	for _, env := range chunk {
		items = append(items, checkpoint.Item{
			Frame: env.Frame, Type: env.Type, CacheKey: env.Key,
			ContentHash: env.ContentHash, ParamHash: env.ParamHash,
			AlgorithmVersion: core.AlgorithmVersion, Digest: env.Digest,
			PayloadJSON: env.RawJSON,
		})
	}
	firstFrame, lastFrame := chunk[0].Frame, chunk[len(chunk)-1].Frame
	if err := e.store.BeginBatch(seq, "evidence", firstFrame, lastFrame, e.params.EffectiveVersion(), core.AlgorithmVersion); err != nil {
		return core.WithBatch(core.NewError(core.CodeTxn, "begin checkpoint batch %d: %v", seq, err), seq)
	}
	paramsJSON := string(core.CanonicalJSON(e.params))
	if err := e.store.CommitBatch(seq, "evidence", items, e.seq.RootHash, e.params.EffectiveVersion(), core.AlgorithmVersion, paramsJSON, e.commitHook); err != nil {
		return err
	}
	e.mu.Lock()
	for _, env := range chunk {
		env.BatchSeq = seq
		e.cache[NodeID(env.Frame, env.Type)] = env
	}
	e.mu.Unlock()
	e.nextBatchSeq++
	return nil
}

func (e *Engine) compute(frame int, evidenceType string, staged map[string]*Envelope, stagedMu *sync.Mutex) (*Envelope, error) {
	contentHash := e.seq.Frames[frame].Hash
	var payload any
	var upstream []string
	var context []string

	switch evidenceType {
	case core.EvidenceWatermark:
		wm, err := watermark.Decode(e.seq.Frames[frame].Image, frame, e.region, e.params)
		if err != nil {
			return nil, err
		}
		payload = wm
	case core.EvidenceResidual:
		context = e.contextHashes(frame, e.params.ResidualWindow)
		payloadValue := forensics.AnalyzeResidual(frame, e.seq.Frames, e.planes, e.seq.Manifest.WatermarkRegion, e.params)
		payload = &payloadValue
	case core.EvidenceNoise:
		context = e.contextHashes(frame, e.params.NoiseWindow)
		payloadValue := forensics.AnalyzeNoise(frame, e.planes, e.seq.Manifest, e.params)
		payload = &payloadValue
	case core.EvidenceVerdict:
		wm, residual, noise, upstreamKeys, err := e.upstreamPayloads(frame, staged, stagedMu)
		if err != nil {
			return nil, err
		}
		upstream = upstreamKeys
		verdict := forensics.Judge(frame, wm, *residual, *noise, e.params)
		payload = &verdict
	default:
		return nil, core.NewError(core.CodeInternal, "unknown evidence type %s", evidenceType)
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	paramHash := core.HashCanonical(relevantParams(evidenceType, e.params))[:16]
	key := core.HashCanonical(keyInput{
		AlgorithmVersion: core.AlgorithmVersion,
		ContentHash:      contentHash,
		EvidenceType:     evidenceType,
		Region:           regionForKey(evidenceType, e.region),
		Params:           relevantParams(evidenceType, e.params),
		ContextHashes:    context,
		UpstreamKeys:     upstream,
	})
	digest := core.SHA256Hex(raw)
	return &Envelope{
		Frame: frame, Type: evidenceType, Key: key, ContentHash: contentHash,
		ParamHash: paramHash, UpstreamKeys: upstream, ContextHashes: context,
		Payload: payload, RawJSON: raw, Digest: digest,
	}, nil
}

func regionForKey(evidenceType string, region core.Rect) *core.Rect {
	if evidenceType == core.EvidenceWatermark {
		r := region
		return &r
	}
	return nil
}

func (e *Engine) contextHashes(frame, window int) []string {
	start := frame - window
	if start < 0 {
		start = 0
	}
	out := []string{}
	for i := start; i < frame; i++ {
		out = append(out, e.seq.Frames[i].Hash)
	}
	return out
}

func (e *Engine) upstreamPayloads(frame int, staged map[string]*Envelope, stagedMu *sync.Mutex) (*core.WatermarkEvidence, *core.ResidualEvidence, *core.NoiseEvidence, []string, error) {
	types := []string{core.EvidenceWatermark, core.EvidenceResidual, core.EvidenceNoise}
	keys := make([]string, 0, len(types))
	var wm *core.WatermarkEvidence
	var residual *core.ResidualEvidence
	var noise *core.NoiseEvidence
	for _, evidenceType := range types {
		e.mu.Lock()
		env := e.cache[NodeID(frame, evidenceType)]
		e.mu.Unlock()
		if env == nil && staged != nil {
			stagedMu.Lock()
			env = staged[NodeID(frame, evidenceType)]
			stagedMu.Unlock()
		}
		if env == nil {
			return nil, nil, nil, nil, core.NewError(core.CodeInternal, "missing upstream %s for frame %d", evidenceType, frame)
		}
		keys = append(keys, env.Key)
		switch evidenceType {
		case core.EvidenceWatermark:
			wm = env.Payload.(*core.WatermarkEvidence)
		case core.EvidenceResidual:
			residual = env.Payload.(*core.ResidualEvidence)
		case core.EvidenceNoise:
			noise = env.Payload.(*core.NoiseEvidence)
		}
	}
	return wm, residual, noise, keys, nil
}

func (e *Engine) expectedKey(frame int, evidenceType string) (string, []string, error) {
	contentHash := e.seq.Frames[frame].Hash
	var upstream []string
	var context []string
	if evidenceType == core.EvidenceResidual {
		context = e.contextHashes(frame, e.params.ResidualWindow)
	}
	if evidenceType == core.EvidenceNoise {
		context = e.contextHashes(frame, e.params.NoiseWindow)
	}
	if evidenceType == core.EvidenceVerdict {
		_, _, _, keys, err := e.upstreamPayloads(frame, nil, nil)
		if err != nil {
			return "", nil, err
		}
		upstream = keys
	}
	key := core.HashCanonical(keyInput{
		AlgorithmVersion: core.AlgorithmVersion,
		ContentHash:      contentHash,
		EvidenceType:     evidenceType,
		Region:           regionForKey(evidenceType, e.region),
		Params:           relevantParams(evidenceType, e.params),
		ContextHashes:    context,
		UpstreamKeys:     upstream,
	})
	return key, upstream, nil
}

func (e *Engine) BuildReport() *core.Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	frameCount := e.seq.Manifest.FrameCount
	report := &core.Report{
		RootHash: e.seq.RootHash, ParamVersion: e.params.EffectiveVersion(),
		AlgorithmVersion: core.AlgorithmVersion,
		Frames:           make([]core.FrameReport, frameCount),
		CommitSequence:   []core.CommitRecord{},
		Invalidations:    append([]core.InvalidationRecord(nil), e.invalidations...),
		QueueEvents:      append([]core.QueueEvent(nil), e.queueEvents...),
		Failures:         []core.FailureRecord{},
	}
	for _, failure := range e.failures {
		report.Failures = append(report.Failures, failure)
	}
	sort.Slice(report.Failures, func(i, j int) bool {
		if report.Failures[i].Frame != report.Failures[j].Frame {
			return report.Failures[i].Frame < report.Failures[j].Frame
		}
		return report.Failures[i].Type < report.Failures[j].Type
	})

	watermarks := make([]*core.WatermarkEvidence, frameCount)
	verdicts := make([]core.VerdictEvidence, 0, frameCount)
	for frame := 0; frame < frameCount; frame++ {
		frameReport := core.FrameReport{Frame: frame, ContentHash: e.seq.Frames[frame].Hash}
		for typeIndex, evidenceType := range core.EvidenceTypes {
			env := e.cache[NodeID(frame, evidenceType)]
			if env == nil {
				continue
			}
			upstreamKeys := env.UpstreamKeys
			if evidenceType == core.EvidenceVerdict && len(upstreamKeys) == 0 {
				for _, upstreamType := range []string{core.EvidenceWatermark, core.EvidenceResidual, core.EvidenceNoise} {
					if upstream := e.cache[NodeID(frame, upstreamType)]; upstream != nil {
						upstreamKeys = append(upstreamKeys, upstream.Key)
					}
				}
			}
			report.CommitSequence = append(report.CommitSequence, core.CommitRecord{
				Ordinal: frame*4 + typeIndex + 1, Frame: frame, Type: evidenceType,
				CacheKey: env.Key, Digest: env.Digest, UpstreamKeys: upstreamKeys,
			})
			switch evidenceType {
			case core.EvidenceWatermark:
				payload := env.Payload.(*core.WatermarkEvidence)
				frameReport.Watermark = payload
				watermarks[frame] = payload
			case core.EvidenceResidual:
				frameReport.Residual = env.Payload.(*core.ResidualEvidence)
			case core.EvidenceNoise:
				frameReport.Noise = env.Payload.(*core.NoiseEvidence)
			case core.EvidenceVerdict:
				payload := env.Payload.(*core.VerdictEvidence)
				frameReport.Verdict = payload
				verdicts = append(verdicts, *payload)
			}
		}
		report.Frames[frame] = frameReport
	}
	nonNilWatermarks := make([]*core.WatermarkEvidence, 0, frameCount)
	for _, wm := range watermarks {
		if wm != nil {
			nonNilWatermarks = append(nonNilWatermarks, wm)
		}
	}
	if len(nonNilWatermarks) == frameCount {
		report.Timeline = forensics.BuildTimeline(watermarks)
	}
	if len(verdicts) == frameCount {
		report.Intervals = forensics.LocateIntervals(verdicts)
	}
	return report
}

func mustParseID(id string) (int, string) {
	for i := 0; i < len(id); i++ {
		if id[i] == ':' {
			frame := 0
			for j := 0; j < i; j++ {
				frame = frame*10 + int(id[j]-'0')
			}
			return frame, id[i+1:]
		}
	}
	return -1, id
}
