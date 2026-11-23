// Package window derives per-caption display windows from detected
// envelope boundaries and reading-assist parameters, and enforces the
// minimum readable duration with a deterministic, auditable trade-off.
//
// Derivation is sequential in caption order: the Deriver carries the
// per-segment packing cursor and the previous window, so chunked
// processing, resumed processing and one-shot recomputation all produce
// identical layouts as long as captions are visited in order.
package window

import (
	"fmt"

	"captionx/internal/envelope"
	"captionx/internal/errs"
	"captionx/internal/model"
)

// Input carries everything needed to derive one project's windows.
type Input struct {
	Captions   []model.CaptionManifest
	CharCounts []int // rune count per caption, parallel to Captions
	Segments   []model.SegmentManifest
	Detection  *envelope.Detection // parallel to Segments
	Reading    model.ReadingParams
	SampleRate int64
	Total      int64 // total timeline samples
}

// ceilDivRat computes ceil(a*num/den) for positive values.
func ceilDivRat(a, num, den int64) int64 {
	n := a * num
	return (n + den - 1) / den
}

// DesiredLen is the display duration for nChars characters at the speech
// rate (chars/sec = rateNum/rateDen), in samples.
func DesiredLen(nChars int64, r model.ReadingParams, sampleRate int64) int64 {
	return ceilDivRat(nChars*sampleRate*r.SpeechRateDen, 1, r.SpeechRateNum)
}

// MinReadableLen is the minimum readable duration for nChars characters
// at the readability ceiling (chars/sec = maxNum/maxDen), in samples.
func MinReadableLen(nChars int64, r model.ReadingParams, sampleRate int64) int64 {
	return ceilDivRat(nChars*sampleRate*r.MaxReadRateDen, 1, r.MaxReadRateNum)
}

// Deriver derives windows one caption at a time, in caption order.
type Deriver struct {
	in        *Input
	segIdx    map[int]int
	cursor    map[int]int64
	prev      model.Window
	hasPrev   bool
	Decisions []model.Decision
}

// NewDeriver validates the input and returns a fresh deriver.
func NewDeriver(in *Input) (*Deriver, error) {
	if len(in.Captions) != len(in.CharCounts) {
		return nil, errs.Schema(-1, "captions", "char counts not parallel to captions")
	}
	d := &Deriver{
		in:     in,
		segIdx: map[int]int{},
		cursor: map[int]int64{},
	}
	for i, s := range in.Segments {
		d.segIdx[s.ID] = i
	}
	return d, nil
}

// Advance replays an already-finalised window (e.g. loaded from a
// checkpoint) into the deriver state without re-deriving it.
func (d *Deriver) Advance(segmentID int, win model.Window) {
	d.cursor[segmentID] = win.End
	d.prev = win
	d.hasPrev = true
}

// Step derives the window of caption idx (calls must be in order).
func (d *Deriver) Step(idx int) (model.Window, error) {
	in := d.in
	c := in.Captions[idx]
	si, ok := d.segIdx[c.SegmentID]
	if !ok {
		return model.Window{}, errs.Schema(c.ID, "segmentId", "unknown segment")
	}
	det := in.Detection.Segments[si]
	qStart, qEnd := det.Start.Quantized, det.End.Quantized
	nCh := int64(in.CharCounts[idx])

	var win model.Window
	if c.ExplicitWindow != nil {
		win = *c.ExplicitWindow
	} else {
		anchor := qStart
		if cur, ok := d.cursor[c.SegmentID]; ok && cur > anchor {
			anchor = cur
		}
		end := anchor + DesiredLen(nCh, in.Reading, in.SampleRate)
		if end > qEnd {
			end = qEnd
		}
		win = model.Window{Start: anchor, End: end}
	}

	win = d.enforceMinDuration(idx, win, qStart, qEnd)
	d.Advance(c.SegmentID, win)
	return win, nil
}

// enforceMinDuration extends win into reclaimable gaps when it is shorter
// than the minimum readable duration, applying the deterministic
// trade-off when the gaps do not suffice. Every trade-off is appended to
// the decision log with caption id, means and reason.
func (d *Deriver) enforceMinDuration(idx int, win model.Window, qStart, qEnd int64) model.Window {
	in := d.in
	c := in.Captions[idx]
	nCh := int64(in.CharCounts[idx])
	minLen := MinReadableLen(nCh, in.Reading, in.SampleRate)
	need := minLen - win.Len()
	if need <= 0 {
		return win
	}

	// Reclaimable gap after the previous caption on the timeline.
	var left int64
	if d.hasPrev {
		if g := win.Start - d.prev.End; g > 0 {
			left = g
		}
	} else if win.Start > 0 {
		left = win.Start
	}
	// Reclaimable gap after this segment: silence up to the next
	// segment start (or the timeline end), plus the unused tail inside
	// the segment itself.
	right := in.Total - qEnd
	si := d.segIdx[c.SegmentID]
	if si+1 < len(in.Segments) {
		right = in.Segments[si+1].Start - qEnd
	}
	if tail := qEnd - win.End; tail > 0 {
		right += tail
	}

	takeLeft := need
	if takeLeft > left {
		takeLeft = left
	}
	takeRight := need - takeLeft
	if takeRight > right {
		takeRight = right
	}
	gained := takeLeft + takeRight
	if gained > 0 {
		before := win.Len()
		win.Start -= takeLeft
		win.End += takeRight
		d.Decisions = append(d.Decisions, model.Decision{
			CaptionID: c.ID,
			Kind:      model.DecisionExtend,
			Reason: fmt.Sprintf(
				"window %d < min readable %d; extended left=%d right=%d (reclaimable left=%d right=%d)",
				before, minLen, takeLeft, takeRight, left, right),
		})
	}
	if win.Len() >= minLen {
		return win
	}

	// Deterministic trade-off: prefer the whole-segment window when it
	// satisfies the minimum; otherwise compress character dwell.
	whole := qEnd - qStart
	if whole >= minLen && whole > win.Len() {
		d.Decisions = append(d.Decisions, model.Decision{
			CaptionID: c.ID,
			Kind:      model.DecisionWholeSegment,
			Reason: fmt.Sprintf(
				"gap extension insufficient (%d < %d); whole segment window %d satisfies minimum",
				win.Len(), minLen, whole),
		})
		return model.Window{Start: qStart, End: qEnd}
	}
	d.Decisions = append(d.Decisions, model.Decision{
		CaptionID: c.ID,
		Kind:      model.DecisionCompressDwell,
		Reason: fmt.Sprintf(
			"gap extension insufficient (%d < %d) and whole segment %d too short; compressing character dwell",
			win.Len(), minLen, whole),
	})
	return win
}

// Derive computes every caption window in one shot; it is exactly the
// sequential Deriver applied to all captions in order.
func Derive(in *Input) ([]model.Window, []model.Decision, error) {
	d, err := NewDeriver(in)
	if err != nil {
		return nil, nil, err
	}
	wins := make([]model.Window, len(in.Captions))
	for i := range in.Captions {
		w, err := d.Step(i)
		if err != nil {
			return nil, nil, err
		}
		wins[i] = w
	}
	return wins, d.Decisions, nil
}
