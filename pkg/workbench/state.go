package workbench

import "fmt"

type SegmentSnapshot struct {
	Index            int           `json:"index"`
	X                int           `json:"x"`
	Y                int           `json:"y"`
	Width            int           `json:"width"`
	Height           int           `json:"height"`
	Fused            []float64     `json:"-"`
	Chroma           []float64     `json:"-"`
	Display          []uint8       `json:"-"`
	GamutMask        []uint8       `json:"-"`
	Peak             float64       `json:"peakRadiance"`
	Mean             float64       `json:"meanRadiance"`
	OutOfGamut       int           `json:"outOfGamut"`
	OutOfGamutSample []PixelCoord  `json:"outOfGamutSamples,omitempty"`
	Params           ParamSnapshot `json:"params"`
	BandMetaVersion  string        `json:"bandMetaVersion"`
	AlgorithmVersion string        `json:"algorithmVersion"`
	DownsampleFactor int           `json:"downsampleFactor"`
	Path             string        `json:"path"`
	ContentHash      string        `json:"contentHash"`
}

func (s *SegmentSnapshot) pixels() int {
	return s.Width * s.Height
}

type Segment struct {
	Index            int              `json:"index"`
	X                int              `json:"x"`
	Y                int              `json:"y"`
	Width            int              `json:"width"`
	Height           int              `json:"height"`
	State            string           `json:"state"`
	Dispatch         bool             `json:"-"`
	ParamVersion     string           `json:"paramVersion,omitempty"`
	DownsampleFactor int              `json:"downsampleFactor,omitempty"`
	Peak             float64          `json:"peakRadiance,omitempty"`
	ContentHash      string           `json:"contentHash,omitempty"`
	Error            *WorkbenchError  `json:"error,omitempty"`
	Snapshot         *SegmentSnapshot `json:"-"`
	StartedAt        string           `json:"startedAt,omitempty"`
	CommittedAt      string           `json:"committedAt,omitempty"`
}

func canTransition(from, to string) bool {
	switch from {
	case StatePending:
		return to == StateProcessing
	case StateProcessing:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StatePending
	case StateCommitted:
		return false
	default:
		return false
	}
}

func illegalTransition(index int, current, requested string) *WorkbenchError {
	e := fieldErr(CodeState, "state", current, requested, "illegal segment state transition")
	e.Segment = index
	return e
}

func transitionLocked(seg *Segment, to string) error {
	if !canTransition(seg.State, to) {
		return illegalTransition(seg.Index, seg.State, to)
	}
	seg.State = to
	return nil
}

type Progress struct {
	Committed int     `json:"committed"`
	Total     int     `json:"total"`
	Fraction  float64 `json:"fraction"`
}

func (e *Engine) Progress() Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.progressLocked()
}

func (e *Engine) progressLocked() Progress {
	p := Progress{Total: len(e.segments)}
	for _, seg := range e.segments {
		if seg.State == StateCommitted {
			p.Committed++
		}
	}
	if p.Total > 0 {
		p.Fraction = float64(p.Committed) / float64(p.Total)
	}
	return p
}

func (e *Engine) transition(index int, to string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if index < 0 || index >= len(e.segments) {
		return fieldErr(CodeState, "segment", index, "0.."+fmt.Sprint(len(e.segments)-1), "segment index is out of range")
	}
	seg := e.segments[index]
	if err := transitionLocked(seg, to); err != nil {
		return err
	}
	e.cond.Broadcast()
	return nil
}

// TransitionSegment exposes the state machine for diagnostics and tests.
// Committed segments cannot leave that state.
func (e *Engine) TransitionSegment(index int, to string) error {
	return e.transition(index, to)
}
