package lipsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// FrameDependency makes the cache and invalidation inputs explicit. Each frame
// has timing, phoneme, and subtitle upstream keys; the final cache key also
// includes the dialogue content hash, frame index, quantization version,
// algorithm version, and all upstream keys.
type FrameDependency struct {
	DialogueID       string      `json:"dialogue_id"`
	Frame            int         `json:"frame"`
	FPS              int         `json:"fps"`
	SampleRate       int         `json:"sample_rate"`
	QDirection       string      `json:"q_direction"`
	QVersion         string      `json:"q_version"`
	AlgorithmVersion string      `json:"algorithm_version"`
	SegmentID        string      `json:"segment_id"`
	PhonemeID        string      `json:"phoneme_id"`
	PhonemeStart     int64       `json:"phoneme_start"`
	PhonemeEnd       int64       `json:"phoneme_end"`
	PhonemeLip       LipCategory `json:"phoneme_lip"`
	PrevBaseLip      LipCategory `json:"prev_base_lip"`
	BaseLip          LipCategory `json:"base_lip"`
	CharStart        int         `json:"char_start"`
	CharEnd          int         `json:"char_end"`
	CharWeight       int         `json:"char_weight"`
	TimingKey        string      `json:"timing_key"`
	PhonemeKey       string      `json:"phoneme_key"`
	SubtitleKey      string      `json:"subtitle_key"`
	ContentHash      string      `json:"content_hash"`
	DepHash          string      `json:"dep_hash"`
	CacheKey         string      `json:"cache_key"`
}

func hashParts(parts ...any) string {
	h := sha256.New()
	for _, part := range parts {
		b, _ := json.Marshal(part)
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func BuildFrameDependencies(p DialogueProject, timing Timing, schedule SubtitleSchedule, frames []FrameResult) ([]FrameDependency, error) {
	deps := make([]FrameDependency, len(frames))
	for f, frame := range frames {
		ph, _, ok := frameAt(timing, f)
		dep := FrameDependency{
			DialogueID:       p.DialogueID,
			Frame:            f,
			FPS:              p.FPS,
			SampleRate:       p.SampleRate,
			QDirection:       p.Quantization.Direction,
			QVersion:         p.Quantization.Version,
			AlgorithmVersion: p.AlgorithmVersion,
			PrevBaseLip:      LipCategory(""),
			BaseLip:          frame.BaseLip,
			CharStart:        frame.CharStart,
			CharEnd:          frame.CharEnd,
			CharWeight:       schedule.Frames[f].Weight,
			ContentHash:      "",
		}
		if f > 0 {
			dep.PrevBaseLip = frames[f-1].BaseLip
		}
		if ok {
			dep.SegmentID = p.Segments[ph.SegmentIndex].ID
			dep.PhonemeID = ph.ID
			dep.PhonemeStart = ph.StartSample
			dep.PhonemeEnd = ph.EndSample
			dep.PhonemeLip = ph.Lip
		}
		dep.TimingKey = hashParts("timing", p.FPS, p.SampleRate, p.TotalSamples(), p.Quantization.Direction, p.Quantization.Version, p.AlgorithmVersion, p.MinReadableFrames, p.LipTransitionTolerance)
		dep.PhonemeKey = hashParts("phoneme", dep.SegmentID, dep.PhonemeID, dep.PhonemeStart, dep.PhonemeEnd, dep.PhonemeLip, dep.PrevBaseLip)
		dep.SubtitleKey = hashParts("subtitle", dep.CharStart, dep.CharEnd, dep.CharWeight)
		dep.ContentHash = hashParts("frame-content", dep.TimingKey, dep.PhonemeKey, dep.SubtitleKey)
		dep.CacheKey = FrameCacheKey(dep)
		dep.DepHash = hashParts("dep", dep)
		deps[f] = dep
	}
	return deps, nil
}

func FrameCacheKey(dep FrameDependency) string {
	return hashParts("frame-cache/v1", dep.ContentHash, dep.Frame, dep.QVersion, dep.AlgorithmVersion, dep.TimingKey, dep.PhonemeKey, dep.SubtitleKey)
}

type Invalidation struct {
	DialogueID string   `json:"dialogue_id"`
	FrameStart int      `json:"frame_start"`
	FrameEnd   int      `json:"frame_end"`
	Reasons    []string `json:"reasons"`
	Fields     []string `json:"fields"`
}

type invalidationSignature struct {
	reasons string
	fields  string
}

func NormalizeInvalidations(in []Invalidation) []Invalidation {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].FrameStart != in[j].FrameStart {
			return in[i].FrameStart < in[j].FrameStart
		}
		return in[i].FrameEnd < in[j].FrameEnd
	})
	out := make([]Invalidation, 0, len(in))
	for _, invalidation := range in {
		reasons := uniqueSorted(invalidation.Reasons)
		fields := uniqueSorted(invalidation.Fields)
		if len(out) > 0 {
			last := &out[len(out)-1]
			lastSig := fmt.Sprintf("%s|%s|%s", last.DialogueID, strings.Join(last.Reasons, "\x00"), strings.Join(last.Fields, "\x00"))
			currentSig := fmt.Sprintf("%s|%s|%s", invalidation.DialogueID, strings.Join(reasons, "\x00"), strings.Join(fields, "\x00"))
			if last.FrameEnd == invalidation.FrameStart && lastSig == currentSig {
				last.FrameEnd = invalidation.FrameEnd
				continue
			}
		}
		invalidation.Reasons = reasons
		invalidation.Fields = fields
		out = append(out, invalidation)
	}
	return out
}

func dependencyDifference(old, current FrameDependency) ([]string, []string) {
	reasons := make([]string, 0, 3)
	fields := make([]string, 0, 6)
	if old.TimingKey != current.TimingKey {
		reasons = append(reasons, "fps_or_sample_rate_changed")
		fields = append(fields, "fps", "sample_rate", "quantization.direction", "quantization.version", "min_readable_frames", "lip_transition_tolerance")
	}
	if old.PhonemeKey != current.PhonemeKey {
		if old.BaseLip != current.BaseLip || old.PhonemeLip != current.PhonemeLip || old.PrevBaseLip != current.PrevBaseLip {
			reasons = append(reasons, "lip_or_transition_change")
			fields = append(fields, "lip")
		}
		if old.PhonemeStart != current.PhonemeStart || old.PhonemeEnd != current.PhonemeEnd || old.PhonemeID != current.PhonemeID {
			reasons = append(reasons, "phoneme_interval_changed")
			fields = append(fields, "phoneme.start_sample", "phoneme.end_sample")
		}
		if old.SegmentID != current.SegmentID || old.PhonemeID != current.PhonemeID {
			reasons = append(reasons, "phoneme_identity_changed")
			fields = append(fields, "segment.id", "phoneme.id")
		}
	}
	if old.SubtitleKey != current.SubtitleKey {
		if old.CharStart != current.CharStart || old.CharEnd != current.CharEnd {
			reasons = append(reasons, "subtitle_schedule_shift")
			fields = append(fields, "char_start", "char_end")
		}
		if old.CharWeight != current.CharWeight {
			reasons = append(reasons, "subtitle_weight_changed")
			fields = append(fields, "subtitle.weights")
		}
	}
	reasons = uniqueSorted(reasons)
	fields = uniqueSorted(fields)
	return reasons, fields
}

func uniqueSorted(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func InvalidateDependencies(old, current []FrameDependency) []Invalidation {
	type dirtyFrame struct {
		frame   int
		reasons []string
		fields  []string
	}
	dirty := make([]dirtyFrame, 0)
	for f := 0; f < len(current); f++ {
		if f >= len(old) {
			dirty = append(dirty, dirtyFrame{frame: f, reasons: []string{"new_frame"}, fields: []string{"frame"}})
			continue
		}
		reasons, fields := dependencyDifference(old[f], current[f])
		if len(reasons) > 0 {
			dirty = append(dirty, dirtyFrame{frame: f, reasons: reasons, fields: fields})
		}
	}
	if len(old) > len(current) {
		dirty = append(dirty, dirtyFrame{
			frame:   -1,
			reasons: []string{"frame_removed"},
			fields:  []string{"frame"},
		})
	}
	out := make([]Invalidation, 0)
	for _, d := range dirty {
		if d.frame < 0 {
			out = append(out, Invalidation{
				DialogueID: currentDialogueID(current, old),
				FrameStart: len(current),
				FrameEnd:   len(old),
				Reasons:    d.reasons,
				Fields:     d.fields,
			})
			continue
		}
		sig := invalidationSignature{reasons: strings.Join(d.reasons, "\x00"), fields: strings.Join(d.fields, "\x00")}
		if len(out) > 0 {
			last := &out[len(out)-1]
			lastSig := invalidationSignature{reasons: strings.Join(last.Reasons, "\x00"), fields: strings.Join(last.Fields, "\x00")}
			if last.FrameEnd == d.frame && lastSig == sig {
				last.FrameEnd = d.frame + 1
				continue
			}
		}
		out = append(out, Invalidation{
			DialogueID: currentDialogueID(current, old),
			FrameStart: d.frame,
			FrameEnd:   d.frame + 1,
			Reasons:    d.reasons,
			Fields:     d.fields,
		})
	}
	return NormalizeInvalidations(out)
}

func currentDialogueID(current, old []FrameDependency) string {
	if len(current) > 0 {
		return current[0].DialogueID
	}
	if len(old) > 0 {
		return old[0].DialogueID
	}
	return ""
}

type DependencyNode struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Frames []int  `json:"frames,omitempty"`
}

type DependencyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type DependencyGraph struct {
	Nodes []DependencyNode `json:"nodes"`
	Edges []DependencyEdge `json:"edges"`
}

func BuildDependencyGraph(p DialogueProject, timing Timing, schedule SubtitleSchedule, frames []FrameResult) DependencyGraph {
	g := DependencyGraph{}
	addNode := func(id, kind, label string, frameList ...int) {
		g.Nodes = append(g.Nodes, DependencyNode{ID: id, Kind: kind, Label: label, Frames: frameList})
	}
	addEdge := func(from, to string) {
		g.Edges = append(g.Edges, DependencyEdge{From: from, To: to})
	}
	addNode("dialogue", "root", p.DialogueID)
	addNode("timing", "input", fmt.Sprintf("fps=%d sr=%d %s", p.FPS, p.SampleRate, p.Quantization.Direction))
	addEdge("timing", "dialogue")
	addNode("subtitle", "input", "subtitle weights")
	addEdge("subtitle", "dialogue")
	for si, seg := range p.Segments {
		segID := fmt.Sprintf("segment/%d", si)
		addNode(segID, "segment", seg.ID)
		addEdge(segID, "dialogue")
		for pi := range seg.Phonemes {
			phID := fmt.Sprintf("phoneme/%d/%d", si, pi)
			addNode(phID, "phoneme", seg.Phonemes[pi].ID)
			addEdge(phID, segID)
		}
	}
	for i, span := range schedule.Spans {
		id := fmt.Sprintf("subtitle/char/%d", i)
		addNode(id, "subtitle_char", fmt.Sprintf("char %d weight %d", i, span.Weight))
		addEdge(id, "subtitle")
	}
	for f, frame := range frames {
		frameID := fmt.Sprintf("frame/%d", f)
		addNode(frameID, "frame", frame.PhonemeID)
		addEdge("timing", frameID)
		if ph, _, ok := frameAt(timing, f); ok {
			addEdge(fmt.Sprintf("phoneme/%d/%d", ph.SegmentIndex, ph.PhonemeIndex), frameID)
		}
		addEdge(fmt.Sprintf("subtitle/char/%d", frame.CharStart), frameID)
	}
	return g
}
