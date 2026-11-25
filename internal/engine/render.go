package engine

import (
	"fmt"
	"sync"

	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

// RenderStats summarizes a render pass.
type RenderStats struct {
	Rendered    int `json:"rendered"`
	Reused      int `json:"reused"`
	Failed      int `json:"failed"`
	PeakBuffer  int `json:"peak_buffer"`
	CommitOrder []CommitEntry
}

// CommitEntry records one deterministic commit event.
type CommitEntry struct {
	GlyphID   string `json:"glyph_id"`
	SegmentID int    `json:"segment_id"`
	CacheKey  string `json:"cache_key"`
}

// FullRender rebuilds every glyph from an empty cache, honoring the segment
// upstream DAG and the bounded worker pool.
func (wb *Workbench) FullRender() (*RenderStats, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	// Reset all caches to guarantee a true from-scratch path.
	for _, rt := range wb.runtimes {
		rt.Records = map[int]*SegmentRecord{}
		rt.Mask = rasterNewMask(rt)
	}
	return wb.renderLocked(nil)
}

// RenderPending renders every segment that is not currently committed,
// reusing frozen cache entries for everything else (incremental path).
func (wb *Workbench) RenderPending(dirty map[string]map[int]bool) (*RenderStats, error) {
	wb.mu.Lock()
	defer wb.mu.Unlock()
	return wb.renderLocked(dirty)
}

func rasterNewMask(rt *GlyphRuntime) *raster.Mask {
	return raster.NewMask(rt.Frames)
}

// renderLocked runs the DAG scheduler. dirty == nil means render everything.
func (wb *Workbench) renderLocked(dirty map[string]map[int]bool) (*RenderStats, error) {
	stats := &RenderStats{}
	// Deterministic glyph order.
	glyphIDs := make([]string, 0, len(wb.runtimes))
	for id := range wb.runtimes {
		glyphIDs = append(glyphIDs, id)
	}
	sortStrings(glyphIDs)

	sem := make(chan struct{}, wb.pool.MaxWorkers)
	var wg sync.WaitGroup
	var firstErr error

	for _, glyphID := range glyphIDs {
		rt := wb.runtimes[glyphID]
		// Rebuild composite mask only from committed components when a
		// partial dirty set is supplied.
		if dirty != nil {
			rt.Mask = raster.NewMask(rt.Frames)
			for _, id := range rt.SegOrder {
				if rec := rt.Records[id]; rec != nil && rec.State == StateCommitted {
					if err := rt.Mask.MergeMax(rec.Mask); err != nil {
						return nil, err
					}
				}
			}
		}
		// DAG-aware scheduling with deterministic commits. Ready segments are
		// rendered in parallel (bounded by the semaphore), but publication into
		// the segment cache and the commit sequence happen in canonical order,
		// so the observable sequence is identical across runs and paths.
		pending := append([]int(nil), rt.SegOrder...)
		for len(pending) > 0 {
			if firstErr != nil {
				wg.Wait()
				return stats, firstErr
			}
			type jobResult struct {
				seg model.StrokeSegment
				rec *SegmentRecord
				err error
			}
			var readyIDs []int
			var remaining []int
			for _, segID := range pending {
				seg := findSeg(rt.Glyph, segID)
				// Reuse rule: only committed, clean segments are frozen.
				// Missing or pending segments are always (re)rendered, which
				// keeps the dependency graph schedulable after partial edits.
				clean := dirty == nil || !dirty[glyphID][segID]
				if clean {
					if rec := rt.Records[segID]; rec != nil && rec.State == StateCommitted {
						stats.Reused++
						stats.CommitOrder = append(stats.CommitOrder, CommitEntry{
							GlyphID: glyphID, SegmentID: segID, CacheKey: rec.CacheKey,
						})
						continue
					}
				}
				ready := true
				for _, up := range seg.Upstream {
					upRec := rt.Records[up]
					if upRec == nil || upRec.State != StateCommitted {
						ready = false
						break
					}
				}
				if !ready {
					remaining = append(remaining, segID)
					continue
				}
				readyIDs = append(readyIDs, segID)
			}
			if len(readyIDs) == 0 && len(remaining) > 0 {
				return stats, model.NewError(model.EStage, "dependency deadlock").
					WithLocation(glyphID, -1)
			}
			results := make([]jobResult, len(readyIDs))
			for idx, segID := range readyIDs {
				seg := findSeg(rt.Glyph, segID)
				sem <- struct{}{}
				wg.Add(1)
				go func(idx int, seg model.StrokeSegment) {
					defer wg.Done()
					defer func() { <-sem }()
					rec, err := wb.renderSegment(rt, seg)
					results[idx] = jobResult{seg: seg, rec: rec, err: err}
				}(idx, seg)
			}
			wg.Wait()
			// Publish deterministically in canonical order.
			for _, res := range results {
				if res.err != nil {
					if firstErr == nil {
						firstErr = res.err
					}
					stats.Failed++
					return stats, firstErr
				}
				rec := res.rec
				if err := Transition(rec.State, StateProcessing); err != nil {
					return stats, model.AttachLocation(err, rt.Glyph.ID, res.seg.ID)
				}
				rec.State = StateProcessing
				if err := Transition(rec.State, StateCommitted); err != nil {
					return stats, model.AttachLocation(err, rt.Glyph.ID, res.seg.ID)
				}
				rec.State = StateCommitted
				if buf := len(rec.Mask.Pix); buf > wb.peakBuffer {
					wb.peakBuffer = buf
				}
				if err := rt.Mask.MergeMax(rec.Mask); err != nil {
					return stats, err
				}
				rt.Records[res.seg.ID] = rec
				stats.Rendered++
				stats.CommitOrder = append(stats.CommitOrder, CommitEntry{
					GlyphID: rt.Glyph.ID, SegmentID: res.seg.ID, CacheKey: rec.CacheKey,
				})
			}
			pending = remaining
		}
	}
	wg.Wait()
	if firstErr != nil {
		return stats, firstErr
	}
	stats.PeakBuffer = wb.peakBuffer
	return stats, nil
}

// Compare renders a fresh full path and a dirty incremental path and checks
// pixel, density and commit-sequence equality.
func (wb *Workbench) Compare(dirty map[string]map[int]bool) (*DiffResult, error) {
	// Snapshot current records so the full render can restore them after.
	saved := wb.snapshotRecords()
	fullStats, err := wb.FullRender()
	if err != nil {
		return nil, err
	}
	fullMasks, fullDens, _ := wb.captureState()
	fullSeq := wb.canonicalSequence()

	// Restore the pre-compare state, then run the incremental path.
	wb.restoreRecords(saved)
	incStats, err := wb.RenderPending(dirty)
	if err != nil {
		return nil, err
	}
	incMasks, incDens, _ := wb.captureState()
	incSeq := wb.canonicalSequence()

	res := &DiffResult{OK: true, FirstSegmentID: -1}
	glyphIDs := make([]string, 0, len(fullMasks))
	for id := range fullMasks {
		glyphIDs = append(glyphIDs, id)
	}
	sortStrings(glyphIDs)
	for _, gid := range glyphIDs {
		eq, x, y := fullMasks[gid].Equal(incMasks[gid])
		if !eq {
			res.OK = false
			res.FirstGlyphID = gid
			res.FirstX, res.FirstY = x, y
			res.Message = fmt.Sprintf("first pixel divergence at (%d,%d)", x, y)
			// Locate the owning segment by scanning committed components.
			res.FirstSegmentID = wb.locateSegment(gid, x, y)
			res.PixelDifference = wb.countPixelDiff(fullMasks[gid], incMasks[gid])
			break
		}
	}
	for _, gid := range glyphIDs {
		for segID, dv := range fullDens[gid] {
			if len(incDens[gid][segID]) != len(dv) {
				res.OK = false
				res.DensityMismatch++
				continue
			}
			for i := range dv {
				if dv[i] != incDens[gid][segID][i] {
					res.DensityMismatch++
					if res.OK {
						res.OK = false
						res.FirstGlyphID = gid
						res.FirstSegmentID = segID
						res.Message = "density mismatch"
					}
				}
			}
		}
	}
	if len(fullSeq) != len(incSeq) {
		res.OK = false
		res.SequenceMismatch = true
	} else {
		for i := range fullSeq {
			if fullSeq[i] != incSeq[i] {
				res.OK = false
				res.SequenceMismatch = true
				break
			}
		}
	}
	if !res.OK {
		return res, model.NewError(model.EDiff, res.Message).
			WithLocation(res.FirstGlyphID, maxSeg(res.FirstSegmentID))
	}
	_ = fullStats
	_ = incStats
	return res, nil
}

func maxSeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func (wb *Workbench) locateSegment(glyphID string, x, y int) int {
	rt := wb.runtimes[glyphID]
	for _, id := range rt.SegOrder {
		rec := rt.Records[id]
		if rec == nil || rec.Mask == nil {
			continue
		}
		if rec.Mask.Pix[y*rec.Mask.Width+x] > 0 {
			return id
		}
	}
	return -1
}

func (wb *Workbench) countPixelDiff(a, b *raster.Mask) int {
	n := 0
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			n++
		}
	}
	return n
}

func (wb *Workbench) captureState() (map[string]*raster.Mask, map[string]map[int][]int, []CommitEntry) {
	masks := map[string]*raster.Mask{}
	dens := map[string]map[int][]int{}
	var seq []CommitEntry
	glyphIDs := make([]string, 0, len(wb.runtimes))
	for id := range wb.runtimes {
		glyphIDs = append(glyphIDs, id)
	}
	sortStrings(glyphIDs)
	for _, id := range glyphIDs {
		rt := wb.runtimes[id]
		masks[id] = rt.Mask.Clone()
		dens[id] = map[int][]int{}
		for _, sid := range rt.SegOrder {
			rec := rt.Records[sid]
			if rec == nil {
				continue
			}
			d := make([]int, len(rec.Densities))
			copy(d, rec.Densities)
			dens[id][sid] = d
			seq = append(seq, CommitEntry{GlyphID: id, SegmentID: sid, CacheKey: rec.CacheKey})
		}
	}
	return masks, dens, seq
}

// canonicalSequence returns the committed segment order independent of which
// path produced it: glyphs alphabetically, segments by their declared order.
func (wb *Workbench) canonicalSequence() []CommitEntry {
	glyphIDs := make([]string, 0, len(wb.runtimes))
	for id := range wb.runtimes {
		glyphIDs = append(glyphIDs, id)
	}
	sortStrings(glyphIDs)
	var seq []CommitEntry
	for _, id := range glyphIDs {
		rt := wb.runtimes[id]
		for _, sid := range rt.SegOrder {
			rec := rt.Records[sid]
			if rec == nil || rec.State != StateCommitted {
				continue
			}
			seq = append(seq, CommitEntry{GlyphID: id, SegmentID: sid, CacheKey: rec.CacheKey})
		}
	}
	return seq
}

func (wb *Workbench) snapshotRecords() map[string]map[int]*SegmentRecord {
	out := map[string]map[int]*SegmentRecord{}
	for id, rt := range wb.runtimes {
		out[id] = map[int]*SegmentRecord{}
		for sid, rec := range rt.Records {
			cp := *rec
			cp.Mask = rec.Mask.Clone()
			out[id][sid] = &cp
		}
	}
	return out
}

func (wb *Workbench) restoreRecords(saved map[string]map[int]*SegmentRecord) {
	for id, rt := range wb.runtimes {
		rt.Records = map[int]*SegmentRecord{}
		rt.Mask = raster.NewMask(rt.Frames)
		for sid, rec := range saved[id] {
			rt.Records[sid] = rec
			if rec.State == StateCommitted {
				_ = rt.Mask.MergeMax(rec.Mask)
			}
		}
	}
}

func findSeg(g *model.Glyph, id int) model.StrokeSegment {
	for _, s := range g.StrokeSegments {
		if s.ID == id {
			return s
		}
	}
	return model.StrokeSegment{}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
