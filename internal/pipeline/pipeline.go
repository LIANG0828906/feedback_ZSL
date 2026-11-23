// Package pipeline orchestrates the full caption-timing chain:
// envelope detection, window derivation, character scheduling and
// error-report assembly. It implements the three execution paths that
// must agree bit-for-bit: realtime chunked processing with checkpoints,
// resumption from persisted checkpoints, and one-shot full recomputation.
package pipeline

import (
	"fmt"

	"captionx/internal/checkpoint"
	"captionx/internal/envelope"
	"captionx/internal/errs"
	"captionx/internal/loader"
	"captionx/internal/model"
	"captionx/internal/report"
	"captionx/internal/schedule"
	"captionx/internal/segment"
	"captionx/internal/window"
)

// Options controls a chunked run.
type Options struct {
	// Reading overrides the manifest reading parameters when non-nil.
	// Overrides only affect uncommitted chunks; committed chunks stay
	// frozen.
	Reading *model.ReadingParams
	// Store enables checkpoint persistence; nil disables it.
	Store *checkpoint.Store
	// Resume loads committed chunks from Store before processing.
	Resume bool
	// StopAfterCommits simulates interruption: the run stops (without
	// error) after committing this many chunks in this invocation.
	// Zero means run to completion.
	StopAfterCommits int
}

// Outcome is the result of a chunked run.
type Outcome struct {
	Result   *model.Result
	Machine  *segment.Machine
	Complete bool // false when interrupted by StopAfterCommits
}

func effectiveReading(p *loader.Project, override *model.ReadingParams) model.ReadingParams {
	if override != nil {
		return *override
	}
	return p.Manifest.Reading
}

// Status reports the chunk-state machine as persisted in the checkpoint
// store, without processing anything: committed chunks are replayed into
// the machine, everything else stays pending.
func Status(p *loader.Project, store *checkpoint.Store) (*segment.Machine, error) {
	m := &p.Manifest
	machine := segment.NewMachine(segment.ChunkCount(m.TotalSamples, m.SegmentChunkSamples))
	if store == nil {
		return machine, nil
	}
	_, recs, err := store.Load(p.ContentHash)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.ChunkIdx < 0 || r.ChunkIdx >= machine.Total() {
			continue
		}
		if err := machine.Transition(r.ChunkIdx, segment.Processing); err != nil {
			return nil, err
		}
		if err := machine.Transition(r.ChunkIdx, segment.Committed); err != nil {
			return nil, err
		}
	}
	return machine, nil
}

func detect(p *loader.Project) (*envelope.Detection, error) {
	return envelope.Detect(p.Audio.Samples, p.Manifest.Frame, p.Manifest.Segments)
}

// weightsFor selects the effective per-character weights of caption idx.
func weightsFor(reading model.ReadingParams, idx, nChars int) []int64 {
	if reading.HighlightMode == model.HighlightLinear {
		return schedule.LinearWeights(nChars)
	}
	return reading.Weights[idx]
}

// processCaption schedules one caption inside its window and computes
// its measured error terms.
func processCaption(p *loader.Project, det *envelope.Detection, reading model.ReadingParams,
	idx int, win model.Window) (model.CaptionResult, model.CaptionErrorTerms, error) {
	m := &p.Manifest
	c := m.Captions[idx]
	nCh := p.CharCounts[idx]
	w := weightsFor(reading, idx, nCh)
	sched, err := schedule.Schedule(c.ID, win, w)
	if err != nil {
		return model.CaptionResult{}, model.CaptionErrorTerms{}, err
	}
	var truth model.CaptionTruth
	for _, t := range m.Truth.Captions {
		if t.CaptionID == c.ID {
			truth = t
			break
		}
	}

	var bq int64
	if c.ExplicitWindow == nil {
		for si, s := range m.Segments {
			if s.ID == c.SegmentID {
				bq = det.Segments[si].Start.QuantError + det.Segments[si].End.QuantError
				break
			}
		}
	}
	terms := model.CaptionErrorTerms{
		CaptionID:         c.ID,
		BoundaryQuant:     bq,
		WindowDeviation:   abs64(win.Start-truth.Start) + abs64(win.End-truth.End),
		SchedResidual:     sched.ResidualNum / sched.ResidualDen,
		DurationDeviation: abs64(win.Len() - (truth.End - truth.Start)),
	}
	res := model.CaptionResult{
		CaptionID:  c.ID,
		Window:     win,
		CharStarts: sched.Starts,
		CharEnds:   sched.Ends,
		SegmentIdx: segment.ChunkOf(win.Start, m.SegmentChunkSamples),
	}
	return res, terms, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// assembleReport builds the cumulative report from per-caption terms in
// caption order. Bound enforcement (E_BOUND) applies only when the run
// uses the manifest's own reading parameters, because ground truth is
// defined for exactly those parameters.
func assembleReport(p *loader.Project, terms []model.CaptionErrorTerms,
	results []model.CaptionResult, enforce bool) (*model.ErrorReport, error) {
	var totalDerived int64
	for _, r := range results {
		totalDerived += r.Window.Len()
	}
	return report.Assemble(terms, p.CharCounts, p.Manifest.Frame.FrameHop, p.Manifest.ToleranceSamples,
		totalDerived, p.Manifest.Truth.TotalReadSamples, enforce)
}

// pendingCaption is a derived caption not yet consumed by a chunk.
type pendingCaption struct {
	idx       int
	res       model.CaptionResult
	terms     model.CaptionErrorTerms
	decisions []model.Decision
}

// RunFull recomputes the whole project in one shot, ignoring any
// checkpoint state.
func RunFull(p *loader.Project, override *model.ReadingParams) (*model.Result, error) {
	reading := effectiveReading(p, override)
	det, err := detect(p)
	if err != nil {
		return nil, err
	}
	m := &p.Manifest
	deriver, err := window.NewDeriver(&window.Input{
		Captions: m.Captions, CharCounts: p.CharCounts, Segments: m.Segments,
		Detection: det, Reading: reading, SampleRate: m.Frame.SampleRate,
		Total: m.TotalSamples,
	})
	if err != nil {
		return nil, err
	}
	res := &model.Result{ProjectID: m.ProjectID, Frame: m.Frame}
	for _, s := range det.Segments {
		res.Boundaries = append(res.Boundaries, s.Start, s.End)
	}
	var terms []model.CaptionErrorTerms
	for i := range m.Captions {
		win, err := deriver.Step(i)
		if err != nil {
			return nil, err
		}
		cr, ct, err := processCaption(p, det, reading, i, win)
		if err != nil {
			return nil, err
		}
		res.Captions = append(res.Captions, cr)
		terms = append(terms, ct)
	}
	res.Decisions = deriver.Decisions
	rep, err := assembleReport(p, terms, res.Captions, override == nil)
	if err != nil {
		return nil, err
	}
	res.Report = *rep
	return res, nil
}

// RunChunked executes the realtime path (and, with Resume, the
// checkpoint-resume path): chunks advance through the state machine in
// order, each commit is persisted atomically, and committed chunks are
// frozen - reading-parameter overrides only affect uncommitted chunks.
func RunChunked(p *loader.Project, opts Options) (*Outcome, error) {
	reading := effectiveReading(p, opts.Reading)
	m := &p.Manifest
	det, err := detect(p)
	if err != nil {
		return nil, err
	}
	nChunks := segment.ChunkCount(m.TotalSamples, m.SegmentChunkSamples)
	machine := segment.NewMachine(nChunks)

	committed := map[int]*checkpoint.ChunkRecord{}
	var persistedOrder []int
	if opts.Resume && opts.Store != nil {
		_, recs, err := opts.Store.Load(p.ContentHash)
		if err != nil {
			return nil, err
		}
		for i := range recs {
			r := recs[i]
			committed[r.ChunkIdx] = &r
			persistedOrder = append(persistedOrder, r.ChunkIdx)
		}
	}

	deriver, err := window.NewDeriver(&window.Input{
		Captions: m.Captions, CharCounts: p.CharCounts, Segments: m.Segments,
		Detection: det, Reading: reading, SampleRate: m.Frame.SampleRate,
		Total: m.TotalSamples,
	})
	if err != nil {
		return nil, err
	}

	nCaps := len(m.Captions)
	capIdxByID := map[int]int{}
	for i, c := range m.Captions {
		capIdxByID[c.ID] = i
	}
	capResults := make([]model.CaptionResult, nCaps)
	capTerms := make([]model.CaptionErrorTerms, nCaps)
	done := make([]bool, nCaps)
	var decisions []model.Decision

	capIdx := 0
	var carry *pendingCaption
	commitsThisRun := 0
	out := &Outcome{Machine: machine}

	for chunk := 0; chunk < nChunks; chunk++ {
		if rec, ok := committed[chunk]; ok {
			// Replay frozen committed state.
			if err := machine.Transition(chunk, segment.Processing); err != nil {
				return nil, err
			}
			if err := machine.Transition(chunk, segment.Committed); err != nil {
				return nil, err
			}
			for _, cr := range rec.Captions {
				idx := capIdxByID[cr.CaptionID]
				capResults[idx] = cr
				done[idx] = true
				deriver.Advance(m.Captions[idx].SegmentID, cr.Window)
			}
			for _, ct := range rec.ErrorTerms {
				capTerms[capIdxByID[ct.CaptionID]] = ct
			}
			decisions = append(decisions, rec.Decisions...)
			capIdx += len(rec.Captions)
			continue
		}

		if err := machine.Transition(chunk, segment.Processing); err != nil {
			return nil, err
		}
		rec := checkpoint.ChunkRecord{ChunkIdx: chunk}
		for capIdx < nCaps {
			if carry == nil {
				before := len(deriver.Decisions)
				win, err := deriver.Step(capIdx)
				if err != nil {
					_ = machine.Transition(chunk, segment.Failed)
					return nil, err
				}
				cr, ct, err := processCaption(p, det, reading, capIdx, win)
				if err != nil {
					_ = machine.Transition(chunk, segment.Failed)
					return nil, err
				}
				carry = &pendingCaption{
					idx: capIdx, res: cr, terms: ct,
					decisions: append([]model.Decision(nil), deriver.Decisions[before:]...),
				}
			}
			if segment.ChunkOf(carry.res.Window.Start, m.SegmentChunkSamples) > chunk {
				break // belongs to a later chunk
			}
			capResults[capIdx] = carry.res
			capTerms[capIdx] = carry.terms
			done[capIdx] = true
			rec.Captions = append(rec.Captions, carry.res)
			rec.ErrorTerms = append(rec.ErrorTerms, carry.terms)
			rec.Decisions = append(rec.Decisions, carry.decisions...)
			decisions = append(decisions, carry.decisions...)
			capIdx++
			carry = nil
		}
		if err := machine.Transition(chunk, segment.Committed); err != nil {
			return nil, err
		}
		if opts.Store != nil {
			persistedOrder = append(persistedOrder, chunk)
			snap := checkpoint.Snapshot{ContentHash: p.ContentHash, Committed: persistedOrder}
			if err := opts.Store.Commit(rec, snap); err != nil {
				return nil, err
			}
		}
		commitsThisRun++
		if opts.StopAfterCommits > 0 && commitsThisRun >= opts.StopAfterCommits {
			out.Complete = false
			return out, nil
		}
	}

	if carry != nil {
		return nil, errs.State(fmt.Sprintf("caption %d escapes the last chunk", carry.res.CaptionID))
	}
	for i := range done {
		if !done[i] {
			return nil, errs.State(fmt.Sprintf("caption %d was never scheduled", m.Captions[i].ID))
		}
	}

	res := &model.Result{ProjectID: m.ProjectID, Frame: m.Frame}
	for _, s := range det.Segments {
		res.Boundaries = append(res.Boundaries, s.Start, s.End)
	}
	res.Captions = capResults
	res.Decisions = decisions
	rep, err := assembleReport(p, capTerms, capResults, opts.Reading == nil)
	if err != nil {
		return nil, err
	}
	res.Report = *rep
	out.Result = res
	out.Complete = true
	return out, nil
}

// Compare verifies that two runs produced identical windows, character
// schedules, decision sequences and error statistics. The first
// divergence is reported: decision-sequence mismatches yield E_DECIDE,
// everything else E_DIFF with caption id, character index and field.
func Compare(a, b *model.Result) error {
	if len(a.Decisions) != len(b.Decisions) {
		return errs.Decide(fmt.Sprintf("decision count %d vs %d", len(a.Decisions), len(b.Decisions)))
	}
	for i := range a.Decisions {
		if a.Decisions[i] != b.Decisions[i] {
			return errs.Decide(fmt.Sprintf("decision %d differs: %+v vs %+v", i, a.Decisions[i], b.Decisions[i]))
		}
	}
	if len(a.Captions) != len(b.Captions) {
		return errs.Diff(-1, -1, "captions", fmt.Sprintf("count %d vs %d", len(a.Captions), len(b.Captions)))
	}
	for i := range a.Captions {
		ca, cb := a.Captions[i], b.Captions[i]
		id := ca.CaptionID
		if ca.Window != cb.Window {
			return errs.Diff(id, -1, "window", fmt.Sprintf("%+v vs %+v", ca.Window, cb.Window))
		}
		if len(ca.CharStarts) != len(cb.CharStarts) {
			return errs.Diff(id, -1, "charStarts", "length mismatch")
		}
		for j := range ca.CharStarts {
			if ca.CharStarts[j] != cb.CharStarts[j] {
				return errs.Diff(id, j, "charStarts",
					fmt.Sprintf("%d vs %d", ca.CharStarts[j], cb.CharStarts[j]))
			}
			if ca.CharEnds[j] != cb.CharEnds[j] {
				return errs.Diff(id, j, "charEnds",
					fmt.Sprintf("%d vs %d", ca.CharEnds[j], cb.CharEnds[j]))
			}
		}
		ta, tb := a.Report.Captions[i], b.Report.Captions[i]
		if ta != tb {
			return errs.Diff(id, -1, "errorTerms", fmt.Sprintf("%+v vs %+v", ta, tb))
		}
	}
	if a.Report.TotalDurationDeviation != b.Report.TotalDurationDeviation {
		return errs.Diff(-1, -1, "totalDurationDeviation",
			fmt.Sprintf("%d vs %d", a.Report.TotalDurationDeviation, b.Report.TotalDurationDeviation))
	}
	return nil
}
