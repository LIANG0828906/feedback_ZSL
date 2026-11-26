package dialoguecal

import (
	"fmt"
	"sort"
)

const DefaultFrameWeight = 10000

type FrameFeature struct {
	DialogueID    string
	SegmentID     string
	Frame         int
	StartSample   int64
	EndSample     int64
	SampleCount   int
	Energy        int64
	ZeroCrossings int
	TransientPeak int
	Weight        int
}

type FrameCalibration struct {
	DialogueID         string
	SegmentID          string
	Frame              int
	Weight             int
	RawLoudness        int64
	GainAdjustment     int64
	CalibratedLoudness int64
}

type Interval struct {
	ID          string
	DialogueID  string
	SegmentID   string
	StartFrame  int
	EndFrame    int
	StartSample int64
	EndSample   int64
	Frames      []FrameSlot
}

type FrameSlot struct {
	Frame       int
	StartSample int64
	EndSample   int64
	Weight      int
}

type QuantizedPhonemeBoundary struct {
	DialogueID string
	SegmentID  string
	PhonemeID  string
	StartFrame int
	EndFrame   int
}

type IntervalResult struct {
	IntervalID  string
	DialogueID  string
	SegmentID   string
	StartFrame  int
	EndFrame    int
	StartSample int64
	EndSample   int64

	Features     []FrameFeature
	Calibrations []FrameCalibration

	MeasuredLoudness int64
	TargetLoudness   int64
	RequestedGain    int64
	AppliedGain      int64
	Reused           bool
	ReusedFeatures   bool

	FeatureKey        string
	CalibrationKey    string
	UpstreamKey       string
	ContentHash       string
	FeatureDigest     string
	CalibrationDigest string

	SourceVersion     SchemaVersion
	ConversionVersion string
	AlgorithmVersion  string
	ConsumedOffset    int64
	CommitSequence    int64
}

func FrameSampleWindow(frame, sampleRate, frameRate, sampleCount int64) (int64, int64, error) {
	if frame < 0 || sampleRate <= 0 || frameRate <= 0 {
		return 0, 0, &Error{Code: CodeCal, Field: "frame", OperandA: frame, OperandB: sampleRate, Message: "invalid frame or rates"}
	}
	startNum, err := checkedMul(frame, int64(sampleRate), "frame.startSample")
	if err != nil {
		return 0, 0, err
	}
	start, _ := divRound(startNum, int64(frameRate), RoundFloor)
	endNum, err := checkedMul(frame+1, int64(sampleRate), "frame.endSample")
	if err != nil {
		return 0, 0, err
	}
	end, _ := divRound(endNum, int64(frameRate), RoundFloor)
	if start < 0 {
		start = 0
	}
	if end > sampleCount {
		end = sampleCount
	}
	if end < start {
		end = start
	}
	return start, end, nil
}

func QuantizeSampleToFrame(sample int64, sampleRate, frameRate int, mode RoundMode) (int, error) {
	if sample < 0 || sampleRate <= 0 || frameRate <= 0 {
		return 0, &Error{Code: CodeCal, Field: "quantize", OperandA: sample, OperandB: int64(frameRate), Message: "invalid quantization operand"}
	}
	num, err := checkedMul(sample, int64(frameRate), "quantize.sampleFrame")
	if err != nil {
		return 0, err
	}
	q, _ := divRound(num, int64(sampleRate), mode)
	return int(q), nil
}

// QuantizePhonemeBoundaries exposes the declared half-up frame quantization
// used by the processing path. Boundaries are sorted by dialogue, segment and
// start frame.
func QuantizePhonemeBoundaries(p *Project) ([]QuantizedPhonemeBoundary, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	cp := p.Clone()
	cp.Sort()
	out := make([]QuantizedPhonemeBoundary, 0)
	for _, d := range cp.Dialogues {
		for _, seg := range d.Segments {
			for _, ph := range seg.Phonemes {
				sf, err := QuantizeSampleToFrame(ph.StartSample, d.PCM.SampleRate, p.FrameRateHz, RoundHalfAway)
				if err != nil {
					return nil, err
				}
				ef, err := QuantizeSampleToFrame(ph.EndSample, d.PCM.SampleRate, p.FrameRateHz, RoundHalfAway)
				if err != nil {
					return nil, err
				}
				if ef <= sf {
					ef = sf + 1
				}
				out = append(out, QuantizedPhonemeBoundary{
					DialogueID: d.ID, SegmentID: seg.ID, PhonemeID: ph.ID,
					StartFrame: sf, EndFrame: ef,
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DialogueID != out[j].DialogueID {
			return out[i].DialogueID < out[j].DialogueID
		}
		if out[i].SegmentID != out[j].SegmentID {
			return out[i].SegmentID < out[j].SegmentID
		}
		if out[i].StartFrame != out[j].StartFrame {
			return out[i].StartFrame < out[j].StartFrame
		}
		return out[i].PhonemeID < out[j].PhonemeID
	})
	return out, nil
}

func frameWeightFor(segment Segment, start, end int64) int {
	if len(segment.Phonemes) == 0 || end <= start {
		return DefaultFrameWeight
	}
	best := -1
	bestOverlap := int64(0)
	for i := range segment.Phonemes {
		ph := &segment.Phonemes[i]
		lo := ph.StartSample
		if lo < start {
			lo = start
		}
		hi := ph.EndSample
		if hi > end {
			hi = end
		}
		overlap := hi - lo
		if overlap > bestOverlap {
			bestOverlap = overlap
			best = i
		}
	}
	if best < 0 {
		return DefaultFrameWeight
	}
	return segment.Phonemes[best].Weight
}

// BuildIntervals turns sample-based segment boundaries into half-up quantized
// frame intervals. Intervals are sorted by dialogue ID and start frame.
func BuildIntervals(p *Project) ([]Interval, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	intervals := make([]Interval, 0)
	for di := range p.Dialogues {
		d := &p.Dialogues[di]
		segments := append([]Segment(nil), d.Segments...)
		sort.Slice(segments, func(i, j int) bool {
			if segments[i].StartSample == segments[j].StartSample {
				return segments[i].ID < segments[j].ID
			}
			return segments[i].StartSample < segments[j].StartSample
		})
		for si := range segments {
			seg := segments[si]
			sf, err := QuantizeSampleToFrame(seg.StartSample, d.PCM.SampleRate, p.FrameRateHz, RoundHalfAway)
			if err != nil {
				return nil, err
			}
			ef, err := QuantizeSampleToFrame(seg.EndSample, d.PCM.SampleRate, p.FrameRateHz, RoundHalfAway)
			if err != nil {
				return nil, err
			}
			if ef <= sf {
				ef = sf + 1
			}
			iv := Interval{
				ID:          fmt.Sprintf("%s/%s/%d-%d", d.ID, seg.ID, sf, ef),
				DialogueID:  d.ID,
				SegmentID:   seg.ID,
				StartFrame:  sf,
				EndFrame:    ef,
				StartSample: seg.StartSample,
				EndSample:   seg.EndSample,
				Frames:      make([]FrameSlot, 0, ef-sf),
			}
			for frame := sf; frame < ef; frame++ {
				start, end, err := FrameSampleWindow(int64(frame), int64(d.PCM.SampleRate), int64(p.FrameRateHz), int64(len(d.PCM.Samples)))
				if err != nil {
					return nil, err
				}
				iv.Frames = append(iv.Frames, FrameSlot{
					Frame: frame, StartSample: start, EndSample: end,
					Weight: frameWeightFor(seg, start, end),
				})
			}
			intervals = append(intervals, iv)
		}
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].DialogueID != intervals[j].DialogueID {
			return intervals[i].DialogueID < intervals[j].DialogueID
		}
		if intervals[i].StartFrame != intervals[j].StartFrame {
			return intervals[i].StartFrame < intervals[j].StartFrame
		}
		return intervals[i].SegmentID < intervals[j].SegmentID
	})
	for i := 1; i < len(intervals); i++ {
		if intervals[i].DialogueID == intervals[i-1].DialogueID &&
			intervals[i].StartFrame < intervals[i-1].EndFrame {
			return nil, &Error{
				Code: CodeOrder, Path: "segments", SegmentID: intervals[i].SegmentID,
				Frame: intervals[i].StartFrame, HasFrame: true,
				Message: "quantized frame intervals overlap",
			}
		}
	}
	return intervals, nil
}

func dialogueByID(p *Project, id string) *Dialogue {
	for i := range p.Dialogues {
		if p.Dialogues[i].ID == id {
			return &p.Dialogues[i]
		}
	}
	return nil
}

func segmentByID(d *Dialogue, id string) *Segment {
	for i := range d.Segments {
		if d.Segments[i].ID == id {
			return &d.Segments[i]
		}
	}
	return nil
}

// ComputeFrameFeatures is a pure function of the PCM and the frame window.
// No gain, cache state, worker count or previous result can affect it.
func ComputeFrameFeatures(dialogueID, segmentID string, pcm PCM, frame int, start, end int64, weight int) (FrameFeature, error) {
	if start < 0 || end > int64(len(pcm.Samples)) || end < start {
		return FrameFeature{}, &Error{
			Code: CodeDecode, Field: "pcm.samples", SegmentID: segmentID,
			Frame: frame, HasFrame: true, OperandA: start, OperandB: end,
			Message: "frame sample window outside PCM",
		}
	}
	var energy int64
	zcr := 0
	peakOffset := 0
	peakAbs := int64(0)
	prevSign := 0
	for i := start; i < end; i++ {
		s := int64(pcm.Samples[i])
		energy += s * s
		a := s
		if a < 0 {
			a = -a
		}
		if a > peakAbs {
			peakAbs = a
			peakOffset = int(i - start)
		}
		sign := 0
		if s > 0 {
			sign = 1
		} else if s < 0 {
			sign = -1
		}
		if sign != 0 {
			if prevSign != 0 && sign != prevSign {
				zcr++
			}
			prevSign = sign
		}
	}
	return FrameFeature{
		DialogueID: dialogueID, SegmentID: segmentID, Frame: frame,
		StartSample: start, EndSample: end, SampleCount: int(end - start),
		Energy: energy, ZeroCrossings: zcr, TransientPeak: peakOffset, Weight: weight,
	}, nil
}

func ComputeIntervalFeatures(p *Project, iv Interval) ([]FrameFeature, error) {
	d := dialogueByID(p, iv.DialogueID)
	if d == nil {
		return nil, &Error{Code: CodeSchema, Path: "dialogues", Message: "unknown dialogue " + iv.DialogueID}
	}
	features := make([]FrameFeature, 0, len(iv.Frames))
	for _, slot := range iv.Frames {
		f, err := ComputeFrameFeatures(iv.DialogueID, iv.SegmentID, d.PCM, slot.Frame, slot.StartSample, slot.EndSample, slot.Weight)
		if err != nil {
			return nil, err
		}
		features = append(features, f)
	}
	return features, nil
}

func FeatureDigest(features []FrameFeature) string {
	parts := make([]string, 0, len(features)+1)
	parts = append(parts, "frame-feature-v1")
	for _, f := range features {
		parts = append(parts, fmt.Sprintf("%s|%s|%d|%d|%d|%d|%d|%d|%d",
			f.DialogueID, f.SegmentID, f.Frame, f.StartSample, f.EndSample,
			f.Energy, f.ZeroCrossings, f.TransientPeak, f.Weight))
	}
	return HashStrings(parts...)
}

func CalibrationDigest(cal []FrameCalibration) string {
	parts := make([]string, 0, len(cal)+1)
	parts = append(parts, "frame-calibration-v1")
	for _, c := range cal {
		parts = append(parts, fmt.Sprintf("%s|%s|%d|%d|%d|%d|%d",
			c.DialogueID, c.SegmentID, c.Frame, c.Weight, c.RawLoudness,
			c.GainAdjustment, c.CalibratedLoudness))
	}
	return HashStrings(parts...)
}
