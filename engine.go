package dialoguecal

import (
	"context"
	"fmt"
	"sort"
)

type RunMode string

const (
	ModeFull     RunMode = "full"
	ModeResume   RunMode = "resume"
	ModeRealTime RunMode = "realtime"
)

type RunOptions struct {
	Mode             RunMode
	Session          string
	Injection        Injection
	PreviousProject  *Project
	StopAfterCommits int
}

type RunResult struct {
	Mode                   RunMode
	Session                string
	Intervals              []IntervalResult
	LogicalCommitOrder     []string
	PhysicalCommitOrder    []string
	ReusedIntervals        []string
	ReusedFeatureIntervals []string
	Invalidation           *InvalidationReport
	QueueEvents            []QueueEvent
	States                 StateSnapshot
}

type Engine struct {
	Store *Store
}

func NewEngine(store *Store) *Engine { return &Engine{Store: store} }

func injectInput(p *Project, inj Injection) (*Project, error) {
	if !inj.CorruptProject && !inj.InvertPhoneme && !inj.ContradictScale {
		return p, nil
	}
	cp := p.Clone()
	if inj.CorruptProject && len(cp.Dialogues) > 0 {
		cp.Dialogues[0].PCM.DeclaredSampleCount++
	}
	if inj.InvertPhoneme {
		found := false
		for di := range cp.Dialogues {
			for si := range cp.Dialogues[di].Segments {
				if len(cp.Dialogues[di].Segments[si].Phonemes) >= 2 {
					ph := &cp.Dialogues[di].Segments[si].Phonemes
					(*ph)[0].StartSample, (*ph)[1].StartSample = (*ph)[1].StartSample, (*ph)[0].StartSample
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}
	if inj.ContradictScale {
		cp.TargetLoudness = LoudnessCeiling + 1
	}
	return cp, nil
}

func checkpointVersionMatches(p *Project, r IntervalResult) bool {
	return r.SourceVersion == p.SourceVersion &&
		r.ConversionVersion == p.ConversionVersion &&
		r.AlgorithmVersion == p.AlgorithmVersion
}

func locateValidationError(p *Project, err error) error {
	ke := AsError(err)
	if ke == nil || p == nil {
		return err
	}
	var chosen *Segment
	var chosenDialogue *Dialogue
	for di := range p.Dialogues {
		d := &p.Dialogues[di]
		for si := range d.Segments {
			if ke.SegmentID == "" || d.Segments[si].ID == ke.SegmentID {
				chosen = &d.Segments[si]
				chosenDialogue = d
				break
			}
		}
		if chosen != nil {
			break
		}
	}
	if chosen != nil {
		if ke.SegmentID == "" {
			ke.SegmentID = chosen.ID
		}
		if !ke.HasFrame {
			frame, _ := QuantizeSampleToFrame(chosen.StartSample, chosenDialogue.PCM.SampleRate, p.FrameRateHz, RoundHalfAway)
			ke.Frame = frame
			ke.HasFrame = true
		}
	}
	return ke
}

func resultFeatureDigestValid(r IntervalResult) bool {
	return r.FeatureDigest != "" && r.FeatureDigest == FeatureDigest(r.Features)
}

func resultCalibrationDigestValid(r IntervalResult) bool {
	return r.CalibrationDigest != "" && r.CalibrationDigest == CalibrationDigest(r.Calibrations)
}

func groupFeatures(features []FrameFeature) map[string][]FrameFeature {
	out := make(map[string][]FrameFeature)
	for _, f := range features {
		key := SegmentKey(f.DialogueID, f.SegmentID)
		out[key] = append(out[key], f)
	}
	return out
}

// Run is the real-time/full/resume entry point. All three modes use the same
// deterministic frame builder, feature function, calibration solver and
// logical commit order. Resume is the only mode that adopts checkpoints.
func (e *Engine) Run(ctx context.Context, p *Project, opts RunOptions) (*RunResult, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	if opts.Mode == "" {
		opts.Mode = ModeFull
	}
	if opts.Session == "" {
		opts.Session = p.ID
	}
	working, err := injectInput(p, opts.Injection)
	if err != nil {
		return nil, err
	}
	if err := ValidateProject(working); err != nil {
		return nil, locateValidationError(working, err)
	}
	intervals, err := BuildIntervals(working)
	if err != nil {
		return nil, err
	}
	sm, err := NewStateMachine(working)
	if err != nil {
		return nil, err
	}
	for _, iv := range intervals {
		if err := sm.TransitionInterval(iv.ID, IntervalProcessing); err != nil {
			return nil, err
		}
		if err := sm.TransitionSegment(SegmentKey(iv.DialogueID, iv.SegmentID), SegmentExtracting); err != nil {
			return nil, err
		}
	}

	var persisted []StoredCheckpoint
	if e.Store != nil && (opts.Mode == ModeResume || opts.PreviousProject != nil) {
		persisted, err = e.Store.LoadCheckpoints(ctx, opts.Session)
		if err != nil {
			return nil, err
		}
	}
	if opts.PreviousProject != nil && persisted == nil {
		persisted = []StoredCheckpoint{}
	}
	persistedByID := make(map[string]StoredCheckpoint, len(persisted))
	for _, cp := range persisted {
		persistedByID[cp.Result.IntervalID] = cp
	}

	type planItem struct {
		interval     Interval
		keys         CacheKeys
		fullReuse    bool
		featureReuse bool
		checkpoint   *IntervalResult
	}
	plans := make([]planItem, 0, len(intervals))
	var needFeatures []Interval
	for _, iv := range intervals {
		keys, err := IntervalCacheKeys(working, iv)
		if err != nil {
			return nil, err
		}
		item := planItem{interval: iv, keys: keys}
		if opts.Mode == ModeResume {
			if cp, ok := persistedByID[iv.ID]; ok && checkpointVersionMatches(working, cp.Result) &&
				cp.Result.FeatureKey == keys.FeatureKey && cp.Result.ContentHash == keys.ContentHash &&
				resultFeatureDigestValid(cp.Result) {
				item.featureReuse = true
				item.checkpoint = &cp.Result
				if cp.Result.CalibrationKey == keys.CalibrationKey && resultCalibrationDigestValid(cp.Result) {
					item.fullReuse = true
				}
			}
		}
		if !item.featureReuse {
			needFeatures = append(needFeatures, iv)
		}
		plans = append(plans, item)
	}
	schedule := ScheduleOptionsFromProject(working)
	extracted, queueEvents, err := ExtractFrames(ctx, working, needFeatures, schedule, opts.Injection)
	if err != nil {
		if ke := AsError(err); ke != nil && ke.SegmentID != "" {
			for _, item := range plans {
				if item.interval.SegmentID == ke.SegmentID {
					_ = sm.FailInterval(item.interval.ID, err)
					_ = sm.FailSegment(SegmentKey(item.interval.DialogueID, item.interval.SegmentID), err)
					break
				}
			}
		}
		return &RunResult{Mode: opts.Mode, Session: opts.Session, QueueEvents: queueEvents, States: sm.Snapshot()}, err
	}
	featuresBySegment := groupFeatures(extracted)

	results := make([]IntervalResult, 0, len(plans))
	var reusedIDs []string
	var reusedFeatureIDs []string
	for _, item := range plans {
		if ctx.Err() != nil {
			err := &Error{Code: CodeCancel, SegmentID: item.interval.SegmentID,
				Frame: item.interval.StartFrame, HasFrame: true, Message: "run canceled during calibration"}
			_ = sm.FailInterval(item.interval.ID, err)
			_ = sm.FailSegment(SegmentKey(item.interval.DialogueID, item.interval.SegmentID), err)
			runResult := &RunResult{Mode: opts.Mode, Session: opts.Session, Intervals: results,
				LogicalCommitOrder: intervalIDs(results), QueueEvents: queueEvents, States: sm.Snapshot()}
			return runResult, err
		}
		iv := item.interval
		d := dialogueByID(working, iv.DialogueID)
		seg := segmentByID(d, iv.SegmentID)
		var features []FrameFeature
		if item.featureReuse {
			features = append([]FrameFeature(nil), item.checkpoint.Features...)
			reusedFeatureIDs = append(reusedFeatureIDs, iv.ID)
		} else {
			features = featuresBySegment[SegmentKey(iv.DialogueID, iv.SegmentID)]
		}
		if len(features) == 0 {
			err := &Error{Code: CodeStage, SegmentID: iv.SegmentID, Frame: iv.StartFrame, HasFrame: true, Message: "no features produced for interval"}
			_ = sm.FailInterval(iv.ID, err)
			_ = sm.FailSegment(SegmentKey(iv.DialogueID, iv.SegmentID), err)
			return &RunResult{Mode: opts.Mode, Session: opts.Session, QueueEvents: queueEvents, States: sm.Snapshot()}, err
		}
		var result IntervalResult
		if item.fullReuse {
			result = *item.checkpoint
			result.Reused = true
			result.ReusedFeatures = true
			reusedIDs = append(reusedIDs, iv.ID)
			segKey := SegmentKey(iv.DialogueID, iv.SegmentID)
			if err := sm.TransitionSegment(segKey, SegmentCalibrating); err != nil {
				return &RunResult{Mode: opts.Mode, Session: opts.Session, QueueEvents: queueEvents, States: sm.Snapshot()}, err
			}
			if err := sm.TransitionSegment(segKey, SegmentCommitted); err != nil {
				return &RunResult{Mode: opts.Mode, Session: opts.Session, QueueEvents: queueEvents, States: sm.Snapshot()}, err
			}
			if err := sm.TransitionInterval(iv.ID, IntervalCommitted); err != nil {
				return &RunResult{Mode: opts.Mode, Session: opts.Session, QueueEvents: queueEvents, States: sm.Snapshot()}, err
			}
		} else {
			cal, measured, requested, applied, err := CalibrateFeatures(working, *seg, features)
			if err != nil {
				_ = sm.FailInterval(iv.ID, err)
				_ = sm.FailSegment(SegmentKey(iv.DialogueID, iv.SegmentID), err)
				return &RunResult{Mode: opts.Mode, Session: opts.Session, QueueEvents: queueEvents, States: sm.Snapshot()}, err
			}
			consumed := iv.EndSample
			if len(iv.Frames) > 0 {
				consumed = iv.Frames[len(iv.Frames)-1].EndSample
			}
			result = IntervalResult{
				IntervalID: iv.ID, DialogueID: iv.DialogueID, SegmentID: iv.SegmentID,
				StartFrame: iv.StartFrame, EndFrame: iv.EndFrame,
				StartSample: iv.StartSample, EndSample: iv.EndSample,
				Features: features, Calibrations: cal,
				MeasuredLoudness: measured, TargetLoudness: working.TargetLoudness,
				RequestedGain: requested, AppliedGain: applied,
				FeatureKey: item.keys.FeatureKey, CalibrationKey: item.keys.CalibrationKey,
				UpstreamKey: item.keys.UpstreamKey, ContentHash: item.keys.ContentHash,
				FeatureDigest: FeatureDigest(features), CalibrationDigest: CalibrationDigest(cal),
				SourceVersion: working.SourceVersion, ConversionVersion: working.ConversionVersion,
				AlgorithmVersion: working.AlgorithmVersion, ConsumedOffset: consumed,
			}
			if err := sm.TransitionSegment(SegmentKey(iv.DialogueID, iv.SegmentID), SegmentCalibrating); err != nil {
				return &RunResult{Mode: opts.Mode, Session: opts.Session, QueueEvents: queueEvents, States: sm.Snapshot()}, err
			}
		}
		result.ReusedFeatures = item.featureReuse || result.Reused
		results = append(results, result)
	}

	var invalidation *InvalidationReport
	if opts.PreviousProject != nil {
		prevResults := make([]IntervalResult, 0, len(persisted))
		for _, cp := range persisted {
			prevResults = append(prevResults, cp.Result)
		}
		invalidation, err = PlanInvalidation(opts.PreviousProject, working, prevResults)
		if err != nil {
			return nil, err
		}
	}

	logicalOrder := make([]string, 0, len(results))
	for _, r := range results {
		logicalOrder = append(logicalOrder, r.IntervalID)
	}
	runResult := &RunResult{
		Mode: opts.Mode, Session: opts.Session, Intervals: results,
		LogicalCommitOrder: logicalOrder, ReusedIntervals: reusedIDs,
		ReusedFeatureIntervals: reusedFeatureIDs, Invalidation: invalidation,
		QueueEvents: queueEvents,
	}

	commitCount := 0
	for i := range results {
		if ctx.Err() != nil {
			runResult.States = sm.Snapshot()
			return runResult, &Error{
				Code: CodeCancel, Entity: "engine", SegmentID: results[i].SegmentID,
				Frame: results[i].StartFrame, HasFrame: true, Message: "run canceled before next checkpoint",
			}
		}
		r := &results[i]
		segKey := SegmentKey(r.DialogueID, r.SegmentID)
		needsCommit := opts.Mode != ModeResume || !r.Reused
		if needsCommit && e.Store != nil {
			seq, err := e.Store.CommitInterval(ctx, opts.Session, *r)
			if err != nil {
				_ = sm.FailInterval(r.IntervalID, err)
				_ = sm.FailSegment(segKey, err)
				runResult.States = sm.Snapshot()
				if ctx.Err() != nil {
					err = &Error{
						Code: CodeCancel, SegmentID: r.SegmentID, Frame: r.StartFrame, HasFrame: true,
						Message: "run canceled during checkpoint transaction",
					}
				}
				return runResult, err
			}
			r.CommitSequence = seq
			runResult.PhysicalCommitOrder = append(runResult.PhysicalCommitOrder, r.IntervalID)
			commitCount++
		}
		if !r.Reused {
			if err := sm.TransitionSegment(segKey, SegmentCommitted); err != nil {
				runResult.States = sm.Snapshot()
				return runResult, err
			}
			if err := sm.TransitionInterval(r.IntervalID, IntervalCommitted); err != nil {
				runResult.States = sm.Snapshot()
				return runResult, err
			}
		}
		if needsCommit && e.Store != nil {
			if opts.StopAfterCommits > 0 && commitCount >= opts.StopAfterCommits {
				runResult.States = sm.Snapshot()
				return runResult, &Error{
					Code: CodeCancel, Entity: "engine", SegmentID: r.SegmentID,
					Frame: r.EndFrame, HasFrame: true,
					Message: "run stopped after committed interval boundary",
				}
			}
		}
	}
	runResult.Intervals = results
	runResult.States = sm.Snapshot()
	return runResult, nil
}

func intervalIDs(results []IntervalResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.IntervalID)
	}
	return out
}

// CompareRunResults reports the first per-frame divergence using a stable
// dialogue/frame ordering and E_DIFF locators.
func CompareRunResults(a, b *RunResult) error {
	if a == nil || b == nil {
		return &Error{Code: CodeDiff, Message: "nil run result"}
	}
	af := flattenRunFrames(a)
	bf := flattenRunFrames(b)
	keys := make([]string, 0, len(af)+len(bf))
	seen := make(map[string]bool)
	for k := range af {
		seen[k] = true
		keys = append(keys, k)
	}
	for k := range bf {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fa, oka := af[k]
		fb, okb := bf[k]
		if !oka || !okb {
			return frameDiff(fa, fb)
		}
		if fa.feature.Energy != fb.feature.Energy ||
			fa.feature.ZeroCrossings != fb.feature.ZeroCrossings ||
			fa.feature.TransientPeak != fb.feature.TransientPeak ||
			fa.feature.Weight != fb.feature.Weight ||
			fa.calibrated != fb.calibrated {
			return frameDiff(fa, fb)
		}
	}
	if len(a.LogicalCommitOrder) != len(b.LogicalCommitOrder) {
		return &Error{Code: CodeDiff, Message: "logical commit sequence length differs"}
	}
	for i := range a.LogicalCommitOrder {
		if a.LogicalCommitOrder[i] != b.LogicalCommitOrder[i] {
			return &Error{
				Code: CodeDiff, FirstSegmentID: "commit-sequence", Message: fmt.Sprintf(
					"first commit sequence mismatch at %d: %s != %s",
					i, a.LogicalCommitOrder[i], b.LogicalCommitOrder[i]),
			}
		}
	}
	if a.Invalidation != nil && b.Invalidation != nil {
		if a.Invalidation.BeforeFingerprint != b.Invalidation.BeforeFingerprint ||
			a.Invalidation.AfterFingerprint != b.Invalidation.AfterFingerprint ||
			len(a.Invalidation.Items) != len(b.Invalidation.Items) ||
			len(a.Invalidation.Reused) != len(b.Invalidation.Reused) {
			return &Error{Code: CodeDiff, Message: "invalidation report differs"}
		}
		for i := range a.Invalidation.Items {
			if a.Invalidation.Items[i].IntervalID != b.Invalidation.Items[i].IntervalID ||
				a.Invalidation.Items[i].Reason != b.Invalidation.Items[i].Reason ||
				a.Invalidation.Items[i].StartFrame != b.Invalidation.Items[i].StartFrame ||
				a.Invalidation.Items[i].EndFrame != b.Invalidation.Items[i].EndFrame ||
				a.Invalidation.Items[i].FeatureReusable != b.Invalidation.Items[i].FeatureReusable {
				return &Error{Code: CodeDiff, Message: "invalidation item differs"}
			}
		}
		for i := range a.Invalidation.Reused {
			if a.Invalidation.Reused[i] != b.Invalidation.Reused[i] {
				return &Error{Code: CodeDiff, Message: "invalidated reuse set differs"}
			}
		}
	}
	return nil
}

type observedFrame struct {
	feature    FrameFeature
	calibrated int64
}

func flattenRunFrames(r *RunResult) map[string]observedFrame {
	out := make(map[string]observedFrame)
	for _, iv := range r.Intervals {
		cal := make(map[int]int64, len(iv.Calibrations))
		for _, c := range iv.Calibrations {
			cal[c.Frame] = c.CalibratedLoudness
		}
		for _, f := range iv.Features {
			out[fmt.Sprintf("%s/%08d", f.DialogueID, f.Frame)] = observedFrame{feature: f, calibrated: cal[f.Frame]}
		}
	}
	return out
}

func frameDiff(a, b observedFrame) error {
	f := a.feature
	if f.DialogueID == "" {
		f = b.feature
	}
	return &Error{
		Code: CodeDiff, FirstSegmentID: f.SegmentID, FirstFrame: f.Frame, HasFirstFrame: true,
		SegmentID: f.SegmentID, Frame: f.Frame, HasFrame: true,
		Message: "first per-frame feature/calibration mismatch",
	}
}
