package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"time"

	"gameintegrity/internal/analyze"
	"gameintegrity/internal/apperr"
	"gameintegrity/internal/checkpoint"
	"gameintegrity/internal/frameio"
	"gameintegrity/internal/hashx"
	"gameintegrity/internal/model"
	"gameintegrity/internal/scale"
	"gameintegrity/internal/segment"
	"gameintegrity/internal/workpool"
)

var ErrPaused = errors.New("verification paused")

type Options struct {
	Path               string
	Mode               string
	WorkDir            string
	StopAfterCommitted int
	BeforeSegment      func(context.Context) error
	Progress           func(committed, total int)
}

func Verify(ctx context.Context, ds *frameio.Dataset, cfg model.Config, opts Options) (*model.RunReport, error) {
	started := time.Now()
	if ds == nil {
		return nil, apperr.New(apperr.Decode, "dataset is nil")
	}
	if err := cfg.Validate(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount); err != nil {
		return nil, err
	}
	if opts.Path == "" {
		opts.Path = "full"
	}
	decision, err := scale.Decide(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.FrameCount, cfg.SamplingBudget, opts.Mode)
	if err != nil {
		return nil, err
	}
	segments, err := segment.Plan(ds.Manifest.FrameCount, frameDuration(ds.Manifest), cfg.SegmentSeconds)
	if err != nil {
		return nil, err
	}
	paramVersion := cfg.ParamVersion()
	var store *checkpoint.Store
	var committed []model.SegmentResult
	var invalidation *model.Invalidation
	if opts.Path == "realtime" || opts.Path == "resume" {
		workdir := opts.WorkDir
		if workdir == "" {
			workdir = filepath.Join("artifacts", "work", ds.Manifest.SampleID)
		}
		store = checkpoint.New(workdir)
		committed, invalidation, err = store.Load(ds.ContentHash, paramVersion, model.AlgorithmVersion)
		if err != nil {
			return nil, err
		}
	}
	if err := validateCommitted(segments, committed); err != nil {
		return nil, err
	}
	machine, err := segment.NewWithCommitted(segments, committed)
	if err != nil {
		return nil, err
	}
	if opts.Progress != nil {
		c, total := machine.Progress()
		opts.Progress(c, total)
	}
	pool := workpool.NewBufferPool(cfg.MaxPixelBufferBytes, ds.FrameBytes())
	if pool.Slots() < 2*cfg.History+1 {
		e := apperr.New(apperr.Para, "pixel buffer pool cannot hold one analysis working set")
		e.Field = "max_pixel_buffer_bytes"
		e.Expected = ds.FrameBytes() * int64(2*cfg.History+1)
		e.Actual = cfg.MaxPixelBufferBytes
		e.AvailableRange = fmt.Sprintf("%d..MaxInt64", ds.FrameBytes()*int64(2*cfg.History+1))
		return nil, e
	}

	all := make([]model.FrameResult, ds.Manifest.FrameCount)
	for i := 0; i < len(all); i++ {
		all[i].Frame = i
		all[i].TimestampUS = ds.Manifest.TimestampsUS[i]
		all[i].WatermarkOK = true
		all[i].WatermarkDecoded = i
		all[i].Verdict = ""
	}
	committedFrames := 0
	for _, seg := range committed {
		for i := range seg.Frames {
			fr := seg.Frames[i]
			if fr.Frame < 0 || fr.Frame >= len(all) || fr.Frame < seg.StartFrame || fr.Frame > seg.EndFrame {
				e := apperr.New(apperr.Diff, "checkpoint segment contains a frame outside its declared range")
				e.Field = "frame"
				e.Frame = fr.Frame
				e.Expected = []int{seg.StartFrame, seg.EndFrame}
				e.Actual = fr.Frame
				return nil, e
			}
			all[fr.Frame] = cloneFrame(fr)
			committedFrames++
		}
	}

	missingHashes := make([]int, 0, len(all)-committedFrames)
	for i := range all {
		if all[i].Summary == "" {
			missingHashes = append(missingHashes, i)
		}
	}
	if err := hashFrames(ctx, ds, cfg, pool, all, missingHashes); err != nil {
		return nil, err
	}
	for i := range all {
		if i == 0 {
			all[i].ChangedBlockCount = 0
			all[i].ChangedBlocks = nil
			continue
		}
		if len(all[i].Blocks) == 0 || len(all[i-1].Blocks) == 0 {
			continue
		}
		count := 0
		for b := range all[i].Blocks {
			changed := all[i].Blocks[b].Hash != all[i-1].Blocks[b].Hash
			all[i].Blocks[b].Changed = changed
			if changed {
				count++
			}
		}
		all[i].ChangedBlockCount = count
		all[i].ChangedBlocks = hashx.ChangedRefs(all[i].Blocks)
	}

	candidates := detectCandidates(all, cfg)
	refineSet := map[int]bool{}
	if decision.Mode == scale.ModeFine {
		for i := range all {
			refineSet[i] = true
		}
	} else {
		for _, i := range candidates {
			for d := -1; d <= 1; d++ {
				if i+d >= 0 && i+d < len(all) {
					refineSet[i+d] = true
				}
			}
		}
	}
	for i := range all {
		all[i].Candidate = false
		for _, c := range candidates {
			if c == i {
				all[i].Candidate = true
				break
			}
		}
		all[i].Screened = !refineSet[i]
	}

	processed := make([]bool, len(segments))
	for i := range committed {
		if committed[i].Index >= 0 && committed[i].Index < len(processed) {
			processed[committed[i].Index] = true
		}
	}
	paused := false
	for i, plan := range segments {
		if processed[i] {
			continue
		}
		if opts.BeforeSegment != nil {
			if err := opts.BeforeSegment(ctx); err != nil {
				if errors.Is(err, ErrPaused) {
					paused = true
					break
				}
				return nil, err
			}
		}
		if err := machine.Begin(i); err != nil {
			return nil, err
		}
		targets := make([]int, 0, plan.EndFrame-plan.StartFrame+1)
		for f := plan.StartFrame; f <= plan.EndFrame; f++ {
			if refineSet[f] {
				targets = append(targets, f)
			}
		}
		if err := refineFrames(ctx, ds, cfg, pool, all, targets); err != nil {
			_ = machine.Fail(i)
			return nil, err
		}
		for f := plan.StartFrame; f <= plan.EndFrame; f++ {
			analyze.Classify(&all[f], cfg)
		}
		segmentResult := model.SegmentResult{
			Index: i, StartFrame: plan.StartFrame, EndFrame: plan.EndFrame,
			State:  segment.StateCommitted,
			Frames: cloneFrames(all[plan.StartFrame : plan.EndFrame+1]),
		}
		segmentResult.Summary = hashx.SegmentSummary(segmentResult.Frames)
		if err := machine.Commit(i, segmentResult); err != nil {
			return nil, err
		}
		if store != nil {
			if err := store.Commit(ds.ContentHash, paramVersion, model.AlgorithmVersion, cfg, segmentResult); err != nil {
				return nil, err
			}
		}
		c, total := machine.Progress()
		if opts.Progress != nil {
			opts.Progress(c, total)
		}
		if opts.StopAfterCommitted > 0 && c >= opts.StopAfterCommitted {
			paused = true
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	for _, seg := range machine.CommittedResults() {
		for _, fr := range seg.Frames {
			if fr.Frame >= 0 && fr.Frame < len(all) {
				all[fr.Frame] = cloneFrame(fr)
			}
		}
	}
	report := &model.RunReport{
		SampleID: ds.Manifest.SampleID, Path: opts.Path, Mode: decision.Mode,
		AlgorithmVersion: model.AlgorithmVersion, ParamVersion: paramVersion,
		ContentHash: ds.ContentHash, Config: cfg, Scale: decision,
		Frames: all, Segments: machine.CommittedResults(),
		SegmentStates: machine.States(),
		Replacement:   replacementResult(all, &ds.Truth),
		Counts:        model.CountVerdicts(all), Invalidation: invalidation,
		Paused: paused,
	}
	report.CommittedSegments, report.TotalSegments = machine.Progress()
	report.Scale.CandidateFrames = candidates
	refined := make([]int, 0, len(refineSet))
	for i := range refineSet {
		refined = append(refined, i)
	}
	sort.Ints(refined)
	report.Scale.RefinedFrames = refined
	report.Scale.PoolWorkers = cfg.MaxWorkers
	report.Scale.PixelBufferBytes = cfg.MaxPixelBufferBytes
	report.Scale.PixelBufferSlots = pool.Slots()
	report.Scale.PixelBufferWaits = pool.Waits()
	report.DurationMS = time.Since(started).Milliseconds()
	if report.Paused {
		report.Counts = model.CountVerdicts(report.Frames)
	}
	return report, nil
}

func hashFrames(ctx context.Context, ds *frameio.Dataset, cfg model.Config, pool *workpool.BufferPool, all []model.FrameResult, indices []int) error {
	if len(indices) == 0 {
		return nil
	}
	return workpool.Run(ctx, cfg.MaxWorkers, len(indices), func(ctx context.Context, task int) error {
		index := indices[task]
		if err := pool.Acquire(ctx, 1); err != nil {
			return err
		}
		defer pool.Release(1)
		frame, err := ds.ReadFrame(index)
		if err != nil {
			return err
		}
		digest := hashx.DigestFrame(frame.Pixels, frame.Width, frame.Height, frame.Channels, cfg.BlockSize, 1)
		if len(digest.Blocks) != len(model.AllBlocks(frame.Width, frame.Height, cfg.BlockSize)) {
			return apperr.New(apperr.Decode, "block hash grid was generated inconsistently")
		}
		all[index].Summary = digest.Summary
		all[index].Blocks = digest.Blocks
		all[index].ContentHash = hashx.FrameContentHash(digest.Summary, fmt.Sprintf("%d", all[index].TimestampUS), index)
		return nil
	})
}

func refineFrames(ctx context.Context, ds *frameio.Dataset, cfg model.Config, pool *workpool.BufferPool, all []model.FrameResult, indices []int) error {
	if len(indices) == 0 {
		return nil
	}
	return workpool.Run(ctx, cfg.MaxWorkers, len(indices), func(ctx context.Context, task int) error {
		index := indices[task]
		prevStart := max(0, index-cfg.History)
		nextEnd := min(len(all)-1, index+cfg.History)
		buffers := 1 + (index - prevStart) + (nextEnd - index)
		if err := pool.Acquire(ctx, buffers); err != nil {
			return err
		}
		defer pool.Release(buffers)
		frame, err := ds.ReadFrame(index)
		if err != nil {
			return err
		}
		in := analyze.Input{
			Frame: frame.Pixels, Width: frame.Width, Height: frame.Height,
			Channels: frame.Channels, BlockSize: cfg.BlockSize,
			Shapes: ds.ShapesFor(index), BlockMetric: all[index].Blocks,
		}
		for i := prevStart; i < index; i++ {
			f, err := ds.ReadFrame(i)
			if err != nil {
				return err
			}
			in.Previous = append(in.Previous, analyze.Neighbor{Index: i, Pixels: f.Pixels})
		}
		for i := index + 1; i <= nextEnd; i++ {
			f, err := ds.ReadFrame(i)
			if err != nil {
				return err
			}
			in.Next = append(in.Next, analyze.Neighbor{Index: i, Pixels: f.Pixels})
		}
		metrics := analyze.Compute(in)
		all[index].ResidualEnergy = metrics.ResidualEnergy
		all[index].ResidualEnergyNext = metrics.ResidualEnergyNext
		all[index].ResidualScore = metrics.ResidualScore
		all[index].NoiseMean = metrics.NoiseMean
		all[index].NoiseDispersion = metrics.NoiseDispersion
		all[index].BBoxShiftPixels = metrics.BBoxShiftPixels
		all[index].Refined = true
		return nil
	})
}

func detectCandidates(all []model.FrameResult, cfg model.Config) []int {
	out := make([]int, 0)
	for i := 1; i < len(all); i++ {
		if len(all[i].Blocks) == 0 {
			continue
		}
		ratio := float64(all[i].ChangedBlockCount) / float64(len(all[i].Blocks))
		if ratio >= cfg.HashJumpRatio {
			out = append(out, i)
		}
	}
	if len(all) > 1 && len(all[1].Blocks) > 0 {
		ratio := float64(all[1].ChangedBlockCount) / float64(len(all[1].Blocks))
		if ratio >= cfg.HashJumpRatio {
			out = append([]int{0}, out...)
		}
	}
	return out
}

func replacementResult(frames []model.FrameResult, truth *model.Truth) model.ReplacementResult {
	detected := model.DetectReplacement(frames)
	expected := model.TruthInterval(truth)
	tolerance := 0
	if truth != nil {
		tolerance = truth.Tolerance
	}
	startDelta, endDelta := model.IntervalDelta(detected, expected)
	return model.ReplacementResult{
		Detected: detected, Truth: expected,
		StartDelta: startDelta, EndDelta: endDelta,
		WithinTolerance: startDelta <= tolerance && endDelta <= tolerance,
		ToleranceFrames: tolerance,
	}
}

func frameDuration(m model.Manifest) int64 {
	if m.FrameDuration > 0 {
		return m.FrameDuration
	}
	if len(m.TimestampsUS) > 1 {
		deltas := make([]int64, 0, len(m.TimestampsUS)-1)
		for i := 1; i < len(m.TimestampsUS); i++ {
			if d := m.TimestampsUS[i] - m.TimestampsUS[i-1]; d > 0 {
				deltas = append(deltas, d)
			}
		}
		if len(deltas) > 0 {
			sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
			return deltas[len(deltas)/2]
		}
	}
	return 100000
}

func validateCommitted(plan []segment.Segment, committed []model.SegmentResult) error {
	for i := range committed {
		r := committed[i]
		if i >= len(plan) {
			e := apperr.New(apperr.State, "checkpoint contains more segments than the current plan")
			e.Field = "segment_index"
			e.Expected = len(plan)
			e.Actual = len(committed)
			return e
		}
		if r.Index != i || r.StartFrame != plan[i].StartFrame || r.EndFrame != plan[i].EndFrame {
			e := apperr.New(apperr.State, "checkpoint segment boundaries do not match the current plan")
			e.Field = "segment_index"
			e.Expected = []int{i, plan[i].StartFrame, plan[i].EndFrame}
			e.Actual = []int{r.Index, r.StartFrame, r.EndFrame}
			return e
		}
		if len(r.Frames) != r.EndFrame-r.StartFrame+1 {
			e := apperr.New(apperr.Decode, "checkpoint segment does not contain every frame result")
			e.Field = "frames"
			e.Expected = r.EndFrame - r.StartFrame + 1
			e.Actual = len(r.Frames)
			return e
		}
	}
	return nil
}

func cloneFrames(in []model.FrameResult) []model.FrameResult {
	out := make([]model.FrameResult, len(in))
	for i := range in {
		out[i] = cloneFrame(in[i])
	}
	return out
}

func cloneFrame(in model.FrameResult) model.FrameResult {
	out := in
	out.Blocks = append([]model.BlockMetric(nil), in.Blocks...)
	out.ChangedBlocks = append([]model.BlockRef(nil), in.ChangedBlocks...)
	out.AbnormalBlocks = append([]model.BlockRef(nil), in.AbnormalBlocks...)
	out.Reasons = append([]string(nil), in.Reasons...)
	return out
}

func NearlyEqual(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return math.Abs(a-b) <= 1e-12
}
