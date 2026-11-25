package dialoguecal

import (
	"fmt"
	"hash/fnv"
)

type EditKind string

const (
	EditGain      EditKind = "gain"
	EditTarget    EditKind = "target_loudness"
	EditFrameRate EditKind = "frame_rate"
	EditPhoneme   EditKind = "phoneme"
	EditSnapshot  EditKind = "snapshot_replace"
)

type Edit struct {
	Kind                EditKind `json:"kind"`
	SegmentID           string   `json:"segment_id,omitempty"`
	PhonemeID           string   `json:"phoneme_id,omitempty"`
	GainPermille        int      `json:"gain_permille,omitempty"`
	TargetLevelPermille int      `json:"target_level_permille,omitempty"`
	FramesPerSecond     int      `json:"frames_per_second,omitempty"`
	FrameShiftSamples   int      `json:"frame_shift_samples,omitempty"`
	FrameWindowSamples  int      `json:"frame_window_samples,omitempty"`
	Phoneme             *Phoneme `json:"phoneme,omitempty"`
	Reason              string   `json:"reason,omitempty"`
}

func CloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	cp := *p
	cp.PCM = append([]int16(nil), p.PCM...)
	cp.Segments = append([]Segment(nil), p.Segments...)
	cp.Phonemes = append([]Phoneme(nil), p.Phonemes...)
	return &cp
}

func ApplyEdits(base *Project, edits ...Edit) (*Project, error) {
	p := CloneProject(base)
	for _, edit := range edits {
		reason := edit.Reason
		if reason == "" {
			reason = string(edit.Kind)
		}
		switch edit.Kind {
		case EditGain:
			seg := segmentByID(p, edit.SegmentID)
			if seg == nil {
				return nil, newError(CodeFrameBounds, "edit.segment_id", "unknown segment %q", edit.SegmentID)
			}
			seg.GainPermille = edit.GainPermille
		case EditTarget:
			seg := segmentByID(p, edit.SegmentID)
			if seg == nil {
				return nil, newError(CodeFrameBounds, "edit.segment_id", "unknown segment %q", edit.SegmentID)
			}
			seg.TargetLevelPermille = edit.TargetLevelPermille
		case EditFrameRate:
			p.FramesPerSecond = edit.FramesPerSecond
			if edit.FrameShiftSamples > 0 {
				p.FrameShiftSamples = edit.FrameShiftSamples
			} else if edit.FramesPerSecond > 0 && p.SampleRateHz > 0 {
				p.FrameShiftSamples = p.SampleRateHz / edit.FramesPerSecond
			}
			if edit.FrameWindowSamples > 0 {
				p.FrameWindowSamples = edit.FrameWindowSamples
			}
		case EditPhoneme:
			if edit.Phoneme == nil {
				return nil, newError(CodeInvalidInjection, "edit.phoneme", "phoneme edit requires a replacement")
			}
			replacement := *edit.Phoneme
			if edit.PhonemeID != "" {
				replacement.ID = edit.PhonemeID
			}
			idx := -1
			for i := range p.Phonemes {
				if p.Phonemes[i].ID == replacement.ID {
					idx = i
					break
				}
			}
			if idx >= 0 {
				p.Phonemes[idx] = replacement
			} else {
				p.Phonemes = append(p.Phonemes, replacement)
			}
		default:
			return nil, newError(CodeInvalidInjection, "edit.kind", "unknown edit kind %q", edit.Kind)
		}
		_ = reason
	}
	if err := NormalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func ContentHash(p *Project) string {
	h := fnv.New128a()
	fmt.Fprintf(h, "%s|%s|%d|%d|%d|%d|%d|", p.ProjectID, p.SchemaVersion, p.Seed, p.SampleRateHz, p.FramesPerSecond, p.FrameShiftSamples, p.FrameWindowSamples)
	for _, s := range p.Segments {
		fmt.Fprintf(h, "S:%s:%d:%d:%d:%d:", s.ID, s.StartSample, s.EndSample, s.GainPermille, s.TargetLevelPermille)
	}
	for _, ph := range p.Phonemes {
		fmt.Fprintf(h, "P:%s:%d:%d:%s:%d:%d:", ph.ID, ph.StartSample, ph.EndSample, ph.Class, ph.F0StartHzQ8, ph.WeightPermille)
	}
	for _, sample := range p.PCM {
		fmt.Fprintf(h, "%d,", sample)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

type segmentFrameInfo struct {
	startGlobal int
	count       int
}

func segmentFrameMap(frames []Frame) map[string]segmentFrameInfo {
	info := map[string]segmentFrameInfo{}
	for _, f := range frames {
		v := info[f.SegmentID]
		if v.count == 0 {
			v.startGlobal = f.GlobalIndex
		}
		v.count++
		info[f.SegmentID] = v
	}
	return info
}

func framesOverlappingSampleRange(frames []Frame, startSample, endSample int) map[string]FrameRange {
	ranges := map[string]FrameRange{}
	for _, f := range frames {
		if f.StartSample < endSample && f.EndSample > startSample {
			r := ranges[f.SegmentID]
			if r.SegmentID == "" {
				r = FrameRange{SegmentID: f.SegmentID, Start: f.IndexInSegment, End: f.IndexInSegment + 1}
			} else {
				if f.IndexInSegment < r.Start {
					r.Start = f.IndexInSegment
				}
				if f.IndexInSegment+1 > r.End {
					r.End = f.IndexInSegment + 1
				}
			}
			ranges[f.SegmentID] = r
		}
	}
	return ranges
}

// RecomputeWithEdits applies edits to a clone and reuses frame cache entries
// outside explicitly constructed propagation closures.
func (e *Engine) RecomputeWithEdits(edits ...Edit) (*RecomputeReport, error) {
	newProject, err := ApplyEdits(e.project, edits...)
	if err != nil {
		return nil, err
	}
	timingChanged := false
	for _, edit := range edits {
		if edit.Kind == EditFrameRate {
			timingChanged = true
		}
	}
	newFrames, err := BuildFrames(newProject)
	if err != nil {
		return nil, err
	}
	if timingChanged || len(newFrames) != len(e.frames) {
		fresh, err := NewEngine(newProject)
		if err != nil {
			return nil, err
		}
		report, err := fresh.ComputeAll()
		if err != nil {
			return nil, err
		}
		closure := InvalidationClosure{
			ID: "closure-all-frame-plan", Source: "frame_rate_or_plan_change", Code: CodeFrameRateRange,
			Reason: "frame timing/plan changed; every frame coordinate depends on it",
		}
		for segID, info := range segmentFrameMap(newFrames) {
			r := FrameRange{SegmentID: segID, Start: 0, End: info.count}
			closure.DirectRanges = append(closure.DirectRanges, r)
			closure.ClosureRanges = append(closure.ClosureRanges, r)
		}
		report.Closures = []InvalidationClosure{closure}
		e.project = fresh.project
		e.frames = fresh.frames
		e.cache = fresh.cache
		e.revision++
		return &report, nil
	}

	infos := segmentFrameMap(newFrames)
	closureStart := map[string]int{}
	closureIDsBySeg := map[string][]string{}
	sourcesByClosure := map[string][]string{}
	var closures []InvalidationClosure

	mergeClosure := func(closure InvalidationClosure, startBySeg map[string]int) {
		closures = append(closures, closure)
		for segID, start := range startBySeg {
			if current, ok := closureStart[segID]; !ok || start < current {
				closureStart[segID] = start
			}
			id := closure.ID
			closureIDsBySeg[segID] = append(closureIDsBySeg[segID], id)
			sourcesByClosure[id] = append(sourcesByClosure[id], closure.Source)
		}
	}

	for i, edit := range edits {
		id := fmt.Sprintf("closure-%d-%s", i+1, edit.Kind)
		switch edit.Kind {
		case EditGain, EditTarget:
			info, ok := infos[edit.SegmentID]
			if !ok {
				return nil, newError(CodeFrameBounds, "edit.segment_id", "segment %q has no frames", edit.SegmentID)
			}
			r := FrameRange{SegmentID: edit.SegmentID, Start: 0, End: info.count}
			closure := InvalidationClosure{ID: id, Source: fmt.Sprintf("edit:%s:%s", edit.Kind, edit.SegmentID), DirectRanges: []FrameRange{r}, ClosureRanges: []FrameRange{r}, Reason: "segment calibration parameter feeds every frame; state propagates to segment tail"}
			mergeClosure(closure, map[string]int{edit.SegmentID: 0})
		case EditPhoneme:
			startSample, endSample := 0, 0
			if edit.Phoneme != nil {
				startSample, endSample = edit.Phoneme.StartSample, edit.Phoneme.EndSample
			}
			if old := phonemeByID(e.project, edit.PhonemeID); old != nil {
				if edit.Phoneme == nil {
					startSample, endSample = old.StartSample, old.EndSample
				} else {
					startSample = minInt(old.StartSample, edit.Phoneme.StartSample)
					endSample = maxInt(old.EndSample, edit.Phoneme.EndSample)
				}
			}
			direct := framesOverlappingSampleRange(newFrames, startSample, endSample)
			starts := map[string]int{}
			closure := InvalidationClosure{ID: id, Source: fmt.Sprintf("edit:phoneme:%s", edit.PhonemeID), Reason: "annotation/weight feeds frame target and labels; calibration carry propagates to each affected segment tail"}
			for _, r := range direct {
				closure.DirectRanges = append(closure.DirectRanges, r)
				starts[r.SegmentID] = r.Start
				info := infos[r.SegmentID]
				closure.ClosureRanges = append(closure.ClosureRanges, FrameRange{SegmentID: r.SegmentID, Start: r.Start, End: info.count})
			}
			if len(direct) == 0 {
				return nil, newError(CodeFrameBounds, "edit.phoneme_id", "phoneme edit range [%d,%d) overlaps no frame", startSample, endSample)
			}
			mergeClosure(closure, starts)
		}
	}

	nextCache := map[int]cacheEntry{}
	for k, v := range e.cache {
		nextCache[k] = v
	}
	results := make([]FrameResult, len(newFrames))
	recomputed := []int{}
	reused := []int{}
	var prev *calibrationCarry
	currentSegment := ""
	for _, frame := range newFrames {
		if frame.SegmentID != currentSegment {
			currentSegment = frame.SegmentID
			prev = nil
		}
		start, hasClosure := closureStart[frame.SegmentID]
		if !hasClosure || frame.IndexInSegment < start {
			old, ok := e.cache[frame.GlobalIndex]
			if !ok {
				return nil, &BusinessError{Code: CodeCheckpointOrder, FrameIndex: frame.GlobalIndex, SegmentID: frame.SegmentID, Detail: "missing reusable cache entry outside invalidation closure"}
			}
			result := old.result
			result.Frame = frame
			result.Provenance.CacheState = "reused"
			result.Provenance.ClosureIDs = nil
			result.Provenance.Sources = []string{fmt.Sprintf("cache:revision:%d", e.revision)}
			results[frame.GlobalIndex] = result
			reused = append(reused, frame.GlobalIndex)
			nextCache[frame.GlobalIndex] = old
			continue
		}

		if frame.IndexInSegment == start && start > 0 {
			previous, ok := e.cache[frame.GlobalIndex-1]
			if !ok {
				return nil, &BusinessError{Code: CodeCheckpointOrder, FrameIndex: frame.GlobalIndex - 1, Detail: "missing predecessor carry for closure"}
			}
			carry := previous.carry
			prev = &carry
		}
		provenance := Provenance{
			Revision: e.revision + 1, CacheState: "computed",
			ClosureIDs: closureIDsBySeg[frame.SegmentID],
			Sources:    sourcesForSeg(closureIDsBySeg[frame.SegmentID], sourcesByClosure),
		}
		result, carry, err := computeFrameResult(newProject, frame, e.revision+1, provenance, prev)
		if err != nil {
			return nil, err
		}
		results[frame.GlobalIndex] = result
		recomputed = append(recomputed, frame.GlobalIndex)
		nextCache[frame.GlobalIndex] = cacheEntry{result: result, carry: carry}
		prev = &carry
	}

	e.project = newProject
	e.frames = newFrames
	e.cache = nextCache
	e.revision++
	return &RecomputeReport{Revision: e.revision, Results: results, Closures: closures, RecomputedSeqs: recomputed, ReusedSeqs: reused}, nil
}

func sourcesForSeg(ids []string, byID map[string][]string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		for _, source := range byID[id] {
			if !seen[source] {
				out = append(out, source)
				seen[source] = true
			}
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ReplaceSnapshot discards all frame cache when the supplied snapshot version
// differs from the currently loaded version.
func (e *Engine) ReplaceSnapshot(p *Project, version string) (*RecomputeReport, InvalidationClosure, error) {
	if version == "" {
		version = ContentHash(p)
	}
	oldVersion := e.snapshotVersion
	if oldVersion == "" {
		oldVersion = ContentHash(e.project)
		e.snapshotVersion = oldVersion
	}
	closure := InvalidationClosure{ID: "closure-snapshot-replace", Source: "snapshot_replace", Code: CodeSnapshotVersion, Reason: "whole-project snapshot version changed; all frame cache entries are not applicable"}
	if version == e.snapshotVersion {
		report, err := e.ComputeAll()
		return &report, closure, err
	}
	fresh, err := NewEngine(p)
	if err != nil {
		return nil, closure, err
	}
	fresh.snapshotVersion = version
	report, err := fresh.ComputeAll()
	if err != nil {
		return nil, closure, err
	}
	for segID, info := range segmentFrameMap(fresh.frames) {
		r := FrameRange{SegmentID: segID, Start: 0, End: info.count}
		closure.DirectRanges = append(closure.DirectRanges, r)
		closure.ClosureRanges = append(closure.ClosureRanges, r)
	}
	e.project = fresh.project
	e.frames = fresh.frames
	e.cache = fresh.cache
	e.revision = fresh.revision
	e.snapshotVersion = version
	report.Closures = []InvalidationClosure{closure}
	return &report, closure, nil
}
