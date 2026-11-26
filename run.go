package maskqa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

var errStopAfter = errors.New("stop after configured commits")

func (s *Service) Run(ctx context.Context, opts RunOptions) (*RunReport, error) {
	if err := s.Params.Validate(); err != nil {
		return nil, err
	}
	report := &RunReport{Mode: opts.Mode, Params: s.Params, StopAfterCommits: opts.StopAfterCommits}
	committed := 0
	var firstErr error
	for _, sample := range s.Dataset.Samples {
		sr, err := s.runSample(ctx, sample, opts, &committed, report)
		if errors.Is(err, errStopAfter) {
			report.Incomplete = true
			break
		}
		if sr != nil {
			report.SampleReports = append(report.SampleReports, sr)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	commits, err := s.Store.ListCheckpoints(ctx, "")
	if err != nil && firstErr == nil {
		firstErr = err
	}
	report.CommitSequence = make([]CommitRef, 0, len(commits))
	for _, cp := range commits {
		report.CommitSequence = append(report.CommitSequence, CommitRef{
			Seq: cp.Seq, SampleID: cp.SampleID, BlockX: cp.BlockX, BlockY: cp.BlockY,
			BlockID: fmt.Sprintf("b%02d_%02d", cp.BlockX, cp.BlockY), Stage: cp.Stage,
			CacheKey: cp.CacheKey, ContentHash: cp.ContentHash, ParamVersion: cp.ParamVersion,
		})
	}
	report.SchedulerMetrics = s.Scheduler.Metrics()
	report.WaitRecords = append([]WaitRecord(nil), report.SchedulerMetrics.WaitRecords...)
	if items, err := s.Store.ListInvalidations(ctx, ""); err == nil {
		report.Invalidations = items
	}
	return report, firstErr
}

func checkpointUsable(cp *Checkpoint, key, paramVersion string) bool {
	return cp != nil && cp.CacheKey == key && cp.ParamVersion == paramVersion && cp.AlgoVersion == AlgoVersion &&
		HashBytes("maskqa-content-v1", []byte(cp.OutputJSON)) == cp.ContentHash
}

func (s *Service) runSample(ctx context.Context, sample *Sample, opts RunOptions, committed *int, report *RunReport) (*SampleRunReport, error) {
	blocks := sample.Blocks
	n := len(blocks)
	sr := &SampleRunReport{SampleID: sample.ID, BlockCount: n, InitialComponents: ComputeComponents(sample.Truth.Mask)}
	keys := map[Stage][]string{}
	for _, st := range Stages {
		keys[st] = make([]string, n)
	}
	var reportMu sync.Mutex

	runUnit := func(i int, st Stage, fn func() error) error {
		b := blocks[i]
		ref := NodeRef{SampleID: sample.ID, Block: b, Stage: st}
		if err := s.Store.Transition(ctx, sample.ID, b, st, StatePending, ""); err != nil {
			return err
		}
		if opts.InjectFailureNode != "" && opts.InjectFailureNode == ref.Key() {
			injected := fmt.Errorf("injected stage failure for %s", ref.Key())
			_ = s.Store.Transition(ctx, sample.ID, b, st, StateProcessing, "")
			_ = s.Store.Transition(ctx, sample.ID, b, st, StateFailed, injected.Error())
			reportMu.Lock()
			sr.Failed = append(sr.Failed, ref)
			reportMu.Unlock()
			return injected
		}
		if err := s.Store.Transition(ctx, sample.ID, b, st, StateProcessing, ""); err != nil {
			return err
		}
		if err := fn(); err != nil {
			_ = s.Store.Transition(ctx, sample.ID, b, st, StateFailed, err.Error())
			reportMu.Lock()
			sr.Failed = append(sr.Failed, ref)
			reportMu.Unlock()
			return err
		}
		return nil
	}

	persist := func(i int, st Stage, out any, summary map[string]any, radius []RadiusDecision, key, pv string) error {
		outputJSON, hash, err := ContentHash(out)
		if err != nil {
			return err
		}
		summaryJSON, _ := jsonMarshalMap(summary)
		radiusToWrite := radius
		if radiusToWrite == nil {
			radiusToWrite = []RadiusDecision{}
		}
		radiusJSON, _ := jsonMarshalMap(radiusToWrite)
		b := blocks[i]
		_, err = s.Store.CommitCheckpoint(ctx, Checkpoint{
			SampleID: sample.ID, BlockX: b.IndexX, BlockY: b.IndexY, Stage: st,
			SummaryJSON: summaryJSON, RadiusJSON: radiusJSON, CacheKey: key,
			ContentHash: hash, ParamVersion: pv, OutputJSON: outputJSON,
		})
		if err != nil {
			return err
		}
		sr.Recomputed = append(sr.Recomputed, NodeRef{SampleID: sample.ID, Block: b, Stage: st})
		*committed++
		if opts.StopAfterCommits > 0 && *committed >= opts.StopAfterCommits {
			return errStopAfter
		}
		return nil
	}

	gradientOuts := make([]*GradientOutput, n)
	var jobs []Job
	jobIndex := map[int]bool{}
	for i, b := range blocks {
		pv := StageParamVersion(s.Params, StageGradient)
		inputHash := BlockInputHash(sample, b)
		key := MakeCacheKey(sample.ID, b, StageGradient, inputHash, "", pv, nil)
		keys[StageGradient][i] = key
		cp, err := s.Store.LatestCheckpoint(ctx, sample.ID, b, StageGradient)
		if err != nil {
			return sr, err
		}
		if checkpointUsable(cp, key, pv) {
			out, err := DecodeCheckpoint[GradientOutput](cp)
			if err == nil {
				gradientOuts[i] = out
				sr.Reused = append(sr.Reused, NodeRef{SampleID: sample.ID, Block: b, Stage: StageGradient})
				continue
			}
		}
		idx := i
		jobIndex[i] = true
		jobs = append(jobs, Job{Block: b, Stage: StageGradient, Pixels: b.Width * b.Height, Run: func([]byte) error {
			return runUnit(idx, StageGradient, func() error {
				out, err := ComputeGradientBlock(sample, s.Params, blocks[idx])
				if err != nil {
					return err
				}
				gradientOuts[idx] = out
				return nil
			})
		}})
	}
	results := s.Scheduler.Run(ctx, jobs)
	var firstErr error
	for ji, res := range results {
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
		_ = ji
	}
	for i := range blocks {
		if !jobIndex[i] || gradientOuts[i] == nil {
			continue
		}
		g := gradientOuts[i]
		err := persist(i, StageGradient, g, map[string]any{"max_magnitude": maxIntSlice(g.Magnitude), "trace_pixels": len(g.Traces)}, nil, keys[StageGradient][i], StageParamVersion(s.Params, StageGradient))
		if errors.Is(err, errStopAfter) {
			return sr, err
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return sr, firstErr
	}

	mag := NewIntField(sample.Width, sample.Height)
	for i, b := range blocks {
		for ly := 0; ly < b.Height; ly++ {
			for lx := 0; lx < b.Width; lx++ {
				mag.Set(b.OriginX+lx, b.OriginY+ly, gradientOuts[i].Magnitude[ly*b.Width+lx])
			}
		}
	}
	threshold, err := ComputeThreshold(mag.Values, s.Params)
	if err != nil {
		return sr, err
	}
	thresholdKey := "global-quantile-" + HashCanonical(threshold)[:16]

	edgeOuts := make([]*EdgeOutput, n)
	jobs = nil
	jobIndex = map[int]bool{}
	for i, b := range blocks {
		pv := StageParamVersion(s.Params, StageEdge)
		up := []string{keys[StageGradient][i], thresholdKey}
		key := MakeCacheKey(sample.ID, b, StageEdge, BlockInputHash(sample, b), "", pv, up)
		keys[StageEdge][i] = key
		cp, _ := s.Store.LatestCheckpoint(ctx, sample.ID, b, StageEdge)
		if checkpointUsable(cp, key, pv) {
			if out, err := DecodeCheckpoint[EdgeOutput](cp); err == nil {
				edgeOuts[i] = out
				sr.Reused = append(sr.Reused, NodeRef{SampleID: sample.ID, Block: b, Stage: StageEdge})
				continue
			}
		}
		idx := i
		jobIndex[i] = true
		jobs = append(jobs, Job{Block: b, Stage: StageEdge, Pixels: b.Width * b.Height, Run: func([]byte) error {
			return runUnit(idx, StageEdge, func() error {
				out, err := ComputeEdgeBlock(mag, threshold, s.Params, blocks[idx])
				if err != nil {
					return err
				}
				edgeOuts[idx] = out
				return nil
			})
		}})
	}
	for _, res := range s.Scheduler.Run(ctx, jobs) {
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
	}
	for i := range blocks {
		if !jobIndex[i] || edgeOuts[i] == nil {
			continue
		}
		e := edgeOuts[i]
		if err := persist(i, StageEdge, e, map[string]any{"threshold": e.Threshold, "band_pixels": maskCount(e.Band)}, nil, keys[StageEdge][i], StageParamVersion(s.Params, StageEdge)); err != nil {
			if errors.Is(err, errStopAfter) {
				return sr, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return sr, firstErr
	}
	edgeFull := AssembleMask(sample.Width, sample.Height, blocks, coresFromEdge(edgeOuts))

	plan, err := BuildRadiusPlan(sample, edgeFull, s.Params)
	if err != nil {
		return sr, err
	}
	radiusOuts := make([]*RadiusOutput, n)
	radiusHashByBlock := make([]string, n)
	for i, b := range blocks {
		ro, err := ComputeRadiusBlock(plan, b, s.Params)
		if err != nil {
			return sr, err
		}
		radiusOuts[i] = ro
		radiusHashByBlock[i] = HashCanonical(ro.Decisions)
	}
	jobs = nil
	jobIndex = map[int]bool{}
	for i, b := range blocks {
		pv := StageParamVersion(s.Params, StageRadius)
		key := MakeCacheKey(sample.ID, b, StageRadius, BlockInputHash(sample, b), radiusHashByBlock[i], pv, []string{keys[StageEdge][i], "global-radius-plan-" + HashCanonical(plan.Decisions)[:16]})
		keys[StageRadius][i] = key
		cp, _ := s.Store.LatestCheckpoint(ctx, sample.ID, b, StageRadius)
		if checkpointUsable(cp, key, pv) {
			if out, err := DecodeCheckpoint[RadiusOutput](cp); err == nil {
				radiusOuts[i] = out
				sr.Reused = append(sr.Reused, NodeRef{SampleID: sample.ID, Block: b, Stage: StageRadius})
				continue
			}
		}
		idx := i
		jobIndex[i] = true
		jobs = append(jobs, Job{Block: b, Stage: StageRadius, Pixels: b.Width * b.Height, Run: func([]byte) error {
			return runUnit(idx, StageRadius, func() error {
				out, err := ComputeRadiusBlock(plan, blocks[idx], s.Params)
				if err != nil {
					return err
				}
				radiusOuts[idx] = out
				return nil
			})
		}})
	}
	for _, res := range s.Scheduler.Run(ctx, jobs) {
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
	}
	for i := range blocks {
		if !jobIndex[i] || radiusOuts[i] == nil {
			continue
		}
		r := radiusOuts[i]
		if err := persist(i, StageRadius, r, map[string]any{"segments": len(r.Decisions), "field_bytes": len(r.Field)}, r.Decisions, keys[StageRadius][i], StageParamVersion(s.Params, StageRadius)); err != nil {
			if errors.Is(err, errStopAfter) {
				return sr, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return sr, firstErr
	}

	closedFull := AdaptiveClose(sample.Rough, plan.Field)
	closeOuts := make([]*CloseOutput, n)
	for i, b := range blocks {
		core := ExtractCore(closedFull, b)
		closeOuts[i] = &CloseOutput{Block: b, Mask: core, Changes: XORCount(ExtractCore(sample.Rough, b), core), KernelAreas: map[int]int{}}
		for r := s.Params.RadiusMin; r <= s.Params.RadiusMax; r++ {
			closeOuts[i].KernelAreas[r] = len(DiskOffsets(r))
		}
	}
	jobs = nil
	jobIndex = map[int]bool{}
	for i, b := range blocks {
		pv := StageParamVersion(s.Params, StageClose)
		key := MakeCacheKey(sample.ID, b, StageClose, BlockInputHash(sample, b), radiusHashByBlock[i], pv, []string{keys[StageRadius][i]})
		keys[StageClose][i] = key
		cp, _ := s.Store.LatestCheckpoint(ctx, sample.ID, b, StageClose)
		if checkpointUsable(cp, key, pv) {
			if out, err := DecodeCheckpoint[CloseOutput](cp); err == nil && out.Mask != nil {
				closeOuts[i] = out
				sr.Reused = append(sr.Reused, NodeRef{SampleID: sample.ID, Block: b, Stage: StageClose})
				continue
			}
		}
		idx := i
		jobIndex[i] = true
		jobs = append(jobs, Job{Block: b, Stage: StageClose, Pixels: b.Width * b.Height, Run: func(buf []byte) error {
			return runUnit(idx, StageClose, func() error {
				if len(buf) < blocks[idx].Width*blocks[idx].Height {
					return fmt.Errorf("worker buffer below block pixels")
				}
				core := ExtractCore(closedFull, blocks[idx])
				closeOuts[idx].Mask = core
				return nil
			})
		}})
	}
	for _, res := range s.Scheduler.Run(ctx, jobs) {
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
	}
	for i := range blocks {
		if !jobIndex[i] || closeOuts[i] == nil {
			continue
		}
		c := closeOuts[i]
		if err := persist(i, StageClose, c, map[string]any{"changes": c.Changes, "kernel_areas": c.KernelAreas}, radiusOuts[i].Decisions, keys[StageClose][i], StageParamVersion(s.Params, StageClose)); err != nil {
			if errors.Is(err, errStopAfter) {
				return sr, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return sr, firstErr
	}
	closeAssembly := AssembleMask(sample.Width, sample.Height, blocks, coresFromClose(closeOuts))
	allCloseKey := "global-close-assembly-" + HashCanonical(keys[StageClose])[:16]

	openOuts := make([]*OpenOutput, n)
	jobs = nil
	jobIndex = map[int]bool{}
	for i, b := range blocks {
		pv := StageParamVersion(s.Params, StageOpen)
		up := []string{keys[StageClose][i], keys[StageRadius][i], allCloseKey}
		key := MakeCacheKey(sample.ID, b, StageOpen, BlockInputHash(sample, b), radiusHashByBlock[i], pv, up)
		keys[StageOpen][i] = key
		cp, _ := s.Store.LatestCheckpoint(ctx, sample.ID, b, StageOpen)
		if checkpointUsable(cp, key, pv) {
			if out, err := DecodeCheckpoint[OpenOutput](cp); err == nil && out.Mask != nil {
				openOuts[i] = out
				sr.Reused = append(sr.Reused, NodeRef{SampleID: sample.ID, Block: b, Stage: StageOpen})
				continue
			}
		}
		idx := i
		jobIndex[i] = true
		jobs = append(jobs, Job{Block: b, Stage: StageOpen, Pixels: b.Width * b.Height, Run: func([]byte) error {
			return runUnit(idx, StageOpen, func() error {
				out, err := RunOpenIterations(sample.Rough, closeAssembly, plan.Field, s.Params, blocks[idx])
				if out != nil {
					openOuts[idx] = out
				}
				return err
			})
		}})
	}
	for _, res := range s.Scheduler.Run(ctx, jobs) {
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
	}
	for i := range blocks {
		if !jobIndex[i] || openOuts[i] == nil || !openOuts[i].Converged {
			continue
		}
		o := openOuts[i]
		summary := map[string]any{"converged": o.Converged, "min_changes": o.MinChanges, "rounds": o.Rounds}
		if err := persist(i, StageOpen, o, summary, radiusOuts[i].Decisions, keys[StageOpen][i], StageParamVersion(s.Params, StageOpen)); err != nil {
			if errors.Is(err, errStopAfter) {
				return sr, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return sr, firstErr
	}
	openAssembly := AssembleMask(sample.Width, sample.Height, blocks, coresFromOpen(openOuts))

	composeOuts := make([]*ComposeOutput, n)
	jobs = nil
	jobIndex = map[int]bool{}
	for i, b := range blocks {
		pv := StageParamVersion(s.Params, StageCompose)
		up := []string{keys[StageOpen][i], keys[StageClose][i], "global-open-assembly-" + HashCanonical(keys[StageOpen])[:16]}
		key := MakeCacheKey(sample.ID, b, StageCompose, BlockInputHash(sample, b), radiusHashByBlock[i], pv, up)
		keys[StageCompose][i] = key
		cp, _ := s.Store.LatestCheckpoint(ctx, sample.ID, b, StageCompose)
		if checkpointUsable(cp, key, pv) {
			if out, err := DecodeCheckpoint[ComposeOutput](cp); err == nil && out.Mask != nil {
				composeOuts[i] = out
				sr.Reused = append(sr.Reused, NodeRef{SampleID: sample.ID, Block: b, Stage: StageCompose})
				continue
			}
		}
		idx := i
		jobIndex[i] = true
		jobs = append(jobs, Job{Block: b, Stage: StageCompose, Pixels: b.Width * b.Height, Run: func([]byte) error {
			return runUnit(idx, StageCompose, func() error {
				out, err := ComposeBlock(openAssembly, closeAssembly, sample.Rough, s.Params, blocks[idx])
				if err != nil {
					return err
				}
				composeOuts[idx] = out
				return nil
			})
		}})
	}
	for _, res := range s.Scheduler.Run(ctx, jobs) {
		if res.Err != nil && firstErr == nil {
			firstErr = res.Err
		}
	}
	for i := range blocks {
		if !jobIndex[i] || composeOuts[i] == nil {
			continue
		}
		c := composeOuts[i]
		if err := persist(i, StageCompose, c, map[string]any{"pixels": maskCount(c.Mask)}, radiusOuts[i].Decisions, keys[StageCompose][i], StageParamVersion(s.Params, StageCompose)); err != nil {
			if errors.Is(err, errStopAfter) {
				return sr, err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return sr, firstErr
	}

	final := AssembleMask(sample.Width, sample.Height, blocks, coresFromCompose(composeOuts))
	sr.Final = final
	sr.Components = ComputeComponents(final)
	errorReport, err := BuildErrorReport(sample, final, gradientOuts, edgeOuts, radiusOuts, openOuts, composeOuts, s.Params, opts.IncludePixels)
	if err != nil {
		return sr, err
	}
	sr.ErrorReport = errorReport
	return sr, nil
}

func jsonMarshalMap(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func maxIntSlice(v []int) int {
	m := 0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func maskCount(m *Mask) int {
	if m == nil {
		return 0
	}
	n := 0
	for _, v := range m.Data {
		if v != 0 {
			n++
		}
	}
	return n
}

func coresFromEdge(in []*EdgeOutput) []*Mask {
	out := make([]*Mask, len(in))
	for i, v := range in {
		if v != nil {
			out[i] = v.Band
		}
	}
	return out
}
func coresFromClose(in []*CloseOutput) []*Mask {
	out := make([]*Mask, len(in))
	for i, v := range in {
		if v != nil {
			out[i] = v.Mask
		}
	}
	return out
}
func coresFromOpen(in []*OpenOutput) []*Mask {
	out := make([]*Mask, len(in))
	for i, v := range in {
		if v != nil {
			out[i] = v.Mask
		}
	}
	return out
}
func coresFromCompose(in []*ComposeOutput) []*Mask {
	out := make([]*Mask, len(in))
	for i, v := range in {
		if v != nil {
			out[i] = v.Mask
		}
	}
	return out
}
