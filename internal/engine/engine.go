package engine

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"texlab/internal/apperr"
	"texlab/internal/convert"
	"texlab/internal/model"
	"texlab/internal/state"
)

type Options struct {
	Workers              int
	PixelBufferPerWorker int
	MaxCommits           int
	ForceSegments        []int
	Previous             *RunResult
	Resume               map[int]*SegmentCommit
	Sink                 CommitSink
}

type CommitSink interface {
	CommitSegment(*SegmentCommit) error
}

type ResourceLog struct {
	Segment int    `json:"segment"`
	Worker  int    `json:"worker"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail"`
}

type StageRecord struct {
	EntryID       string       `json:"entry_id"`
	EntryIndex    int          `json:"entry_index"`
	Entry         model.Entry  `json:"entry"`
	Segment       int          `json:"segment"`
	Image         model.Image  `json:"-"`
	CacheKey      string       `json:"cache_key"`
	InputHash     string       `json:"input_hash"`
	RegionHash    string       `json:"region_hash"`
	ContentHash   string       `json:"content_hash"`
	ParamHash     string       `json:"param_hash"`
	CoverageHash  string       `json:"coverage_hash"`
	Coverage      []model.Rect `json:"coverage"`
	UpstreamKey   string       `json:"upstream_key"`
	Reused        bool         `json:"reused"`
	CacheHit      bool         `json:"cache_hit"`
	Invalidated   bool         `json:"invalidated"`
	InvalidReason string       `json:"invalid_reason,omitempty"`
}

type SegmentCommit struct {
	Segment           model.Segment  `json:"segment"`
	Stages            []*StageRecord `json:"-"`
	Final             model.Image    `json:"-"`
	FinalHash         string         `json:"final_hash"`
	CommitSeq         int            `json:"commit_seq"`
	PlanFingerprint   string         `json:"plan_fingerprint"`
	SourceFingerprint string         `json:"source_fingerprint"`
	ParamVersion      int            `json:"param_version"`
	AlgorithmVersion  string         `json:"algorithm_version"`
	Reused            bool           `json:"reused"`
}

type RunResult struct {
	Schema            string                          `json:"schema"`
	Plan              model.Plan                      `json:"plan"`
	Source            model.Image                     `json:"-"`
	SourceFingerprint string                          `json:"source_fingerprint"`
	PlanFingerprint   string                          `json:"plan_fingerprint"`
	Revision          int                             `json:"revision"`
	AlgorithmVersion  string                          `json:"algorithm_version"`
	Segments          []*SegmentCommit                `json:"segments"`
	StageIndex        map[string]map[int]*StageRecord `json:"-"`
	Matrix            []state.Cell                    `json:"matrix"`
	CommitSequence    []int                           `json:"commit_sequence"`
	Logs              []ResourceLog                   `json:"logs"`
	Workers           int                             `json:"workers"`
	PixelBuffer       int                             `json:"pixel_buffer_per_worker"`
	CommittedEntries  int                             `json:"committed_entries"`
	TotalEntries      int                             `json:"total_entries"`
	CommittedSegments int                             `json:"committed_segments"`
	TotalSegments     int                             `json:"total_segments"`
	FailedSegments    int                             `json:"failed_segments"`
	CacheHits         int                             `json:"cache_hits"`
	ReusedCells       int                             `json:"reused_cells"`
	InvalidatedCells  int                             `json:"invalidated_cells"`
}

type segmentResult struct {
	segment     model.Segment
	stages      []*StageRecord
	final       model.Image
	failedEntry int
	err         error
	logs        []ResourceLog
}

func DefaultOptions() Options {
	return Options{Workers: 4, PixelBufferPerWorker: 262144}
}

func Run(p model.Plan, source model.Image, sourceFingerprint string, opts Options) (*RunResult, error) {
	if err := validateRunOptions(p, source, opts); err != nil {
		return nil, err
	}
	segments := model.BuildSegments(source.Width, source.Height, p.Segments)
	totalEntries := len(p.Entries) * len(segments)
	matrix := state.NewMatrix(len(p.Entries), len(segments), p.Revision, p.Revision)
	result := &RunResult{
		Schema: model.RunSchema, Plan: p.Clone(), Source: cloneImage(source),
		SourceFingerprint: sourceFingerprint, PlanFingerprint: model.PlanFingerprint(p),
		Revision: p.Revision, AlgorithmVersion: model.AlgorithmVersion,
		StageIndex:   make(map[string]map[int]*StageRecord),
		TotalEntries: totalEntries, TotalSegments: len(segments),
		Workers: opts.Workers, PixelBuffer: opts.PixelBufferPerWorker,
	}
	if result.Workers <= 0 {
		result.Workers = 4
	}
	if result.PixelBuffer <= 0 {
		result.PixelBuffer = 262144
	}
	loaded := opts.Resume
	force := map[int]bool{}
	for _, s := range opts.ForceSegments {
		if s < 0 || s >= len(segments) {
			return nil, apperr.At(apperr.EPara, 0, "force_segments", "segment %d is out of range", s)
		}
		force[s] = true
	}

	// Resume records are all-or-nothing at the caller boundary. Here they are
	// already validated, so populate the state matrix and final list first.
	commits := make([]*SegmentCommit, len(segments))
	for idx, c := range loaded {
		if idx < 0 || idx >= len(segments) || c == nil {
			continue
		}
		if c.PlanFingerprint != result.PlanFingerprint || c.AlgorithmVersion != model.AlgorithmVersion ||
			c.SourceFingerprint != sourceFingerprint || c.ParamVersion != p.Revision {
			return nil, &apperr.Error{
				Code: apperr.EVersion, Segment: idx, Field: "checkpoint",
				Version: p.Revision, Other: c.ParamVersion,
				Message: "resume segment version does not match current plan/source",
			}
		}
		if c.Segment.W != segments[idx].W || c.Segment.H != segments[idx].H {
			return nil, &apperr.Error{Code: apperr.EVersion, Segment: idx, Field: "segment", Message: "resume segment dimensions mismatch"}
		}
		commits[idx] = c
		for e := 1; e <= len(p.Entries); e++ {
			st := findStage(c.Stages, p.Entries[e-1].ID)
			if st == nil {
				return nil, &apperr.Error{Code: apperr.EVersion, EntryIndex: e, Segment: idx, Field: "stage", Message: "resume segment is missing an entry stage"}
			}
			if err := matrix.Transition(e, idx, state.Processing, p.Revision, "resume", nil); err != nil {
				return nil, err
			}
			snap := st.Entry.ParamSnapshot()
			if err := matrix.Transition(e, idx, state.Committed, p.Revision, "resume", &snap); err != nil {
				return nil, err
			}
			matrix.MarkReuse(e, idx, st.CacheKey, "resume", true, false)
		}
	}

	targets := make([]model.Segment, 0, len(segments))
	for _, seg := range segments {
		if _, ok := loaded[seg.Index]; ok {
			continue
		}
		targets = append(targets, seg)
	}
	if opts.MaxCommits > 0 && len(targets) > opts.MaxCommits {
		targets = targets[:opts.MaxCommits]
	}

	jobs := make(chan model.Segment)
	results := make(chan segmentResult, len(targets))
	var wg sync.WaitGroup
	var workerSeq atomic.Int64
	for w := 0; w < result.Workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for seg := range jobs {
				results <- processSegment(p, source, result.PlanFingerprint, sourceFingerprint, seg, opts, opts.Previous, force[seg.Index], matrix, workerID)
			}
		}(w)
	}
	go func() {
		for _, seg := range targets {
			workerSeq.Add(1)
			jobs <- seg
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	pending := map[int]*segmentResult{}
	nextTarget := 0
	commitNext := func(segIdx int) error {
		res, ok := pending[segIdx]
		if !ok {
			return nil
		}
		delete(pending, segIdx)
		for _, l := range res.logs {
			result.Logs = append(result.Logs, l)
		}
		if res.err != nil {
			result.FailedSegments++
			for e := 1; e <= len(p.Entries); e++ {
				cell := matrix.Get(e, segIdx)
				if cell.State == state.Processing {
					_ = matrix.Transition(e, segIdx, state.Failed, p.Revision, "failed", nil)
				}
			}
			return nil
		}
		commit := &SegmentCommit{
			Segment: res.segment, Stages: res.stages, Final: res.final,
			FinalHash: res.final.ContentHash(), CommitSeq: segIdx,
			PlanFingerprint: result.PlanFingerprint, SourceFingerprint: sourceFingerprint,
			ParamVersion: p.Revision, AlgorithmVersion: model.AlgorithmVersion,
		}
		if opts.Sink != nil {
			if err := opts.Sink.CommitSegment(commit); err != nil {
				for e := 1; e <= len(p.Entries); e++ {
					if cell := matrix.Get(e, segIdx); cell.State == state.Processing {
						_ = matrix.Transition(e, segIdx, state.Failed, p.Revision, "checkpoint_error", nil)
					}
				}
				return err
			}
		}
		for e, st := range res.stages {
			snap := st.Entry.ParamSnapshot()
			if err := matrix.Transition(e+1, segIdx, state.Committed, p.Revision, st.InvalidReason, &snap); err != nil {
				return err
			}
			matrix.MarkReuse(e+1, segIdx, st.CacheKey, st.InvalidReason, st.Reused, st.Invalidated)
		}
		commits[segIdx] = commit
		result.CommitSequence = append(result.CommitSequence, segIdx)
		return nil
	}

	for res := range results {
		pending[res.segment.Index] = &res
		for nextTarget < len(targets) {
			segIdx := targets[nextTarget].Index
			if _, ok := pending[segIdx]; !ok {
				break
			}
			if err := commitNext(segIdx); err != nil {
				return nil, err
			}
			nextTarget++
		}
	}
	// Results that arrived after a failed segment still need deterministic
	// commit traversal.
	for nextTarget < len(targets) {
		segIdx := targets[nextTarget].Index
		if err := commitNext(segIdx); err != nil {
			return nil, err
		}
		nextTarget++
	}

	for _, c := range commits {
		if c == nil {
			continue
		}
		result.Segments = append(result.Segments, c)
		for _, st := range c.Stages {
			m := result.StageIndex[st.EntryID]
			if m == nil {
				m = make(map[int]*StageRecord)
				result.StageIndex[st.EntryID] = m
			}
			m[st.Segment] = st
			if st.CacheHit {
				result.CacheHits++
			}
			if st.Reused {
				result.ReusedCells++
			}
			if st.Invalidated {
				result.InvalidatedCells++
			}
		}
	}
	sort.Slice(result.Segments, func(i, j int) bool { return result.Segments[i].Segment.Index < result.Segments[j].Segment.Index })
	result.CommitSequence = result.CommitSequence[:0]
	for _, c := range result.Segments {
		result.CommitSequence = append(result.CommitSequence, c.Segment.Index)
	}
	result.Matrix = matrix.Snapshot()
	for i := range result.Matrix {
		idx := result.Matrix[i].EntryIndex - 1
		if idx >= 0 && idx < len(result.Plan.Entries) {
			result.Matrix[i].EntryID = result.Plan.Entries[idx].ID
		}
	}
	committed, total, _ := matrix.Progress()
	result.CommittedEntries = committed
	result.TotalEntries = total
	result.CommittedSegments = len(result.Segments)
	if len(result.Logs) == 0 {
		result.Logs = []ResourceLog{}
	}
	return result, nil
}

func processSegment(p model.Plan, source model.Image, planFP, sourceFP string, seg model.Segment, opts Options, prev *RunResult, forced bool, matrix *state.Matrix, workerID int) segmentResult {
	res := segmentResult{segment: seg}
	if seg.Pixels > opts.PixelBufferPerWorker {
		res.logs = append(res.logs, ResourceLog{
			Segment: seg.Index, Worker: workerID, Reason: "buffer_banded",
			Detail: fmt.Sprintf("segment holds %d pixels; worker scratch limit %d, conversion proceeds in deterministic bands", seg.Pixels, opts.PixelBufferPerWorker),
		})
	}
	if prev == nil && opts.Resume == nil {
		res.logs = append(res.logs, ResourceLog{
			Segment: seg.Index, Worker: workerID, Reason: "cold_cache",
			Detail: "no prior stage snapshot; full segment transform",
		})
	}
	input := cropImage(source, model.Rect{ID: "segment", X: seg.X, Y: seg.Y, W: seg.W, H: seg.H})
	rootKey := model.HashParts("texlab-source-root-v1", sourceFP)
	upstreamKey := rootKey
	for eidx, entry := range p.Entries {
		if cell := matrix.Get(eidx+1, seg.Index); cell.State == state.Pending || cell.State == state.Failed {
			if err := matrix.Transition(eidx+1, seg.Index, state.Processing, p.Revision, "", nil); err != nil {
				res.failedEntry = eidx + 1
				res.err = err
				return res
			}
		}
		globalCoverage := p.Coverage(entry, seg)
		localCoverage := make([]model.Rect, 0, len(globalCoverage))
		for _, r := range globalCoverage {
			localCoverage = append(localCoverage, model.Rect{ID: r.ID, X: r.X - seg.X, Y: r.Y - seg.Y, W: r.W, H: r.H})
		}
		inputHash := input.ContentHash()
		regionHash := model.CoverageHash(input, localCoverage)
		coverageHash := model.CoverageSignature(localCoverage)
		paramHash := entry.ParamSignature()
		key := model.CacheKey(regionHash, entry.Index, paramHash, upstreamKey)

		var old *StageRecord
		if prev != nil {
			if m := prev.StageIndex[entry.ID]; m != nil {
				old = m[seg.Index]
			}
		}
		canReuse := !forced && old != nil && old.Entry.ParamSignature() == paramHash &&
			old.InputHash == inputHash && old.RegionHash == regionHash &&
			old.CoverageHash == coverageHash

		var out model.Image
		reused := false
		cacheHit := false
		invalidated := false
		reason := ""
		if canReuse {
			out = cloneImage(old.Image)
			reused = true
			cacheHit = old.CacheKey == key
			if cacheHit {
				reason = "reused"
			} else if old.EntryIndex != entry.Index {
				reason = "order_changed_reused"
			} else {
				reason = "cache_key_rebound"
			}
		} else if len(localCoverage) == 0 {
			out = cloneImage(input)
			reused = true
			reason = "outside_region_passthrough"
		} else {
			transformed, err := convert.TransformLimited(input, entry, localCoverage, opts.PixelBufferPerWorker)
			if err != nil {
				_ = matrix.Transition(eidx+1, seg.Index, state.Failed, p.Revision, "parameter_error", nil)
				res.failedEntry = eidx + 1
				res.err = err
				return res
			}
			out = transformed.Image
			invalidated = true
			reason = invalidationReason(old, entry, inputHash, forced)
		}
		st := &StageRecord{
			EntryID: entry.ID, EntryIndex: entry.Index, Entry: entry.ParamSnapshot(), Segment: seg.Index,
			Image: out, CacheKey: key, InputHash: inputHash, RegionHash: regionHash,
			ContentHash: out.ContentHash(), ParamHash: paramHash, CoverageHash: coverageHash,
			Coverage: localCoverage, UpstreamKey: upstreamKey, Reused: reused, CacheHit: cacheHit,
			Invalidated: invalidated, InvalidReason: reason,
		}
		res.stages = append(res.stages, st)
		input = out
		upstreamKey = key
	}
	res.final = cloneImage(input)
	return res
}

func invalidationReason(old *StageRecord, entry model.Entry, inputHash string, forced bool) string {
	if forced {
		return "retry"
	}
	if old == nil {
		return "cache_miss"
	}
	if old.Entry.ParamSignature() != entry.ParamSignature() {
		return "param_changed"
	}
	if old.EntryIndex != entry.Index {
		return "order_changed"
	}
	if old.InputHash != inputHash {
		return "upstream_changed"
	}
	return "cache_miss"
}

func validateRunOptions(p model.Plan, source model.Image, opts Options) error {
	if opts.Workers < 0 {
		return apperr.At(apperr.EPara, 0, "workers", "workers must be positive")
	}
	if opts.Workers > 64 {
		return apperr.At(apperr.EPara, 0, "workers", "workers must not exceed 64")
	}
	if opts.PixelBufferPerWorker < 0 {
		return apperr.At(apperr.EPara, 0, "pixel_buffer_per_worker", "pixel buffer must be positive")
	}
	if opts.PixelBufferPerWorker > 0 && opts.PixelBufferPerWorker < 16 {
		return apperr.At(apperr.EPara, 0, "pixel_buffer_per_worker", "pixel buffer must be at least 16 pixels")
	}
	if opts.MaxCommits < 0 {
		return apperr.At(apperr.EPara, 0, "max_commits", "max commits must be non-negative")
	}
	if len(p.Entries) == 0 {
		return apperr.At(apperr.EPlan, 0, "entries", "plan has no entries")
	}
	if source.Width <= 0 || source.Height <= 0 {
		return apperr.At(apperr.EPara, 0, "source", "source dimensions are invalid")
	}
	return nil
}

func findStage(stages []*StageRecord, entryID string) *StageRecord {
	for _, s := range stages {
		if s.EntryID == entryID {
			return s
		}
	}
	return nil
}

func cropImage(im model.Image, r model.Rect) model.Image {
	out := model.NewImage(r.W, r.H, im.Depth, im.Channels, im.Layout, im.Alpha)
	for y := 0; y < r.H; y++ {
		srcStart := ((r.Y+y)*im.Width + r.X) * 4
		copy(out.Pixels[y*r.W*4:(y*r.W+r.W)*4], im.Pixels[srcStart:srcStart+r.W*4])
	}
	return out
}

func cloneImage(im model.Image) model.Image {
	return im.Clone()
}

func CompareRuns(want, got *RunResult) error {
	if want == nil || got == nil {
		return apperr.New(apperr.EDiff, "both run results are required")
	}
	if want.PlanFingerprint != got.PlanFingerprint {
		return &apperr.Error{Code: apperr.EDiff, Field: "plan_fingerprint", Message: "plan fingerprints differ", Expected: nil, Actual: nil, Details: want.PlanFingerprint + " != " + got.PlanFingerprint}
	}
	if !equalInts(want.CommitSequence, got.CommitSequence) {
		return &apperr.Error{Code: apperr.EDiff, Field: "commit_sequence", Message: "segment commit sequences differ", Details: fmt.Sprintf("%v != %v", want.CommitSequence, got.CommitSequence)}
	}
	if len(want.Segments) != len(got.Segments) {
		return &apperr.Error{Code: apperr.EDiff, Field: "segments", Message: "segment counts differ"}
	}
	gotSegments := make(map[int]*SegmentCommit)
	for _, s := range got.Segments {
		gotSegments[s.Segment.Index] = s
	}
	for _, ws := range want.Segments {
		gs := gotSegments[ws.Segment.Index]
		if gs == nil {
			return &apperr.Error{Code: apperr.EDiff, Segment: ws.Segment.Index, Field: "segment", Message: "missing final segment"}
		}
		lastEntry := model.Entry{}
		if len(want.Plan.Entries) > 0 {
			lastEntry = want.Plan.Entries[len(want.Plan.Entries)-1]
		}
		if d := model.FirstImageDiff(ws.Final, gs.Final, lastEntry, ws.Segment.Index); d != nil {
			d.X += ws.Segment.X
			d.Y += ws.Segment.Y
			return diffError(d)
		}
		if ws.FinalHash != gs.FinalHash {
			return &apperr.Error{Code: apperr.EDiff, EntryIndex: len(want.Plan.Entries), Field: "final_hash", Segment: ws.Segment.Index, Message: "final segment hashes differ"}
		}
	}
	gotStage := got.StageIndex
	for entryID, bySeg := range want.StageIndex {
		for seg, a := range bySeg {
			b := gotStage[entryID][seg]
			if b == nil {
				return &apperr.Error{Code: apperr.EDiff, EntryIndex: a.EntryIndex, Field: "stage", Segment: seg, Message: "missing stage in compared run"}
			}
			if d := model.FirstImageDiff(a.Image, b.Image, a.Entry, seg); d != nil {
				if seg >= 0 && seg < len(got.Segments) {
					d.X += got.Segments[seg].Segment.X
					d.Y += got.Segments[seg].Segment.Y
				}
				return diffError(d)
			}
			if a.ContentHash != b.ContentHash {
				return &apperr.Error{Code: apperr.EDiff, EntryIndex: a.EntryIndex, Field: "content_hash", Segment: seg, Message: "stage content hashes differ"}
			}
		}
	}
	return nil
}

func (r *RunResult) AssembleFinal() model.Image {
	width, height := r.Source.Width, r.Source.Height
	if len(r.Segments) == 0 {
		return model.NewImage(width, height, r.Source.Depth, r.Source.Channels, r.Source.Layout, r.Source.Alpha)
	}
	first := r.Segments[0].Final
	out := model.NewImage(width, height, first.Depth, first.Channels, first.Layout, first.Alpha)
	for _, c := range r.Segments {
		copySegment(&out, c.Final, c.Segment)
	}
	return out
}

func (r *RunResult) AssembleEntry(entryID string) (model.Image, bool) {
	width, height := r.Source.Width, r.Source.Height
	bySeg := r.StageIndex[entryID]
	if len(bySeg) == 0 {
		return model.Image{}, false
	}
	var first model.Image
	found := false
	var segs []model.Segment
	for _, c := range r.Segments {
		if st := bySeg[c.Segment.Index]; st != nil {
			if !found {
				first = st.Image
				found = true
			}
			segs = append(segs, c.Segment)
		}
	}
	if !found {
		return model.Image{}, false
	}
	out := model.NewImage(width, height, first.Depth, first.Channels, first.Layout, first.Alpha)
	for _, seg := range segs {
		copySegment(&out, bySeg[seg.Index].Image, seg)
	}
	return out, true
}

func copySegment(dst *model.Image, src model.Image, seg model.Segment) {
	if src.Width != seg.W || src.Height != seg.H {
		return
	}
	for y := 0; y < seg.H; y++ {
		start := ((seg.Y+y)*dst.Width + seg.X) * 4
		copy(dst.Pixels[start:start+seg.W*4], src.Pixels[y*seg.W*4:(y*seg.W+seg.W)*4])
	}
}

func diffError(d *model.Diff) error {
	return &apperr.Error{
		Code: apperr.EDiff, EntryIndex: d.EntryIndex, Segment: d.Segment, Field: d.Field,
		Coordinate: &apperr.Point{X: d.X, Y: d.Y}, Expected: d.Expected, Actual: d.Actual,
		Message: fmt.Sprintf("first pixel mismatch at (%d,%d) in entry %d segment %d", d.X, d.Y, d.EntryIndex, d.Segment),
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
