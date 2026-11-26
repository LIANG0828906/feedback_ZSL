package dialogue

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
)

// FrameEnvelope is the deterministic per-frame result.
type FrameEnvelope struct {
	ID           string       `json:"id"`
	SegmentID    string       `json:"segment_id"`
	FrameIndex   int          `json:"frame_index"`
	StartSample  int64        `json:"start_sample"`
	EndSample    int64        `json:"end_sample"`
	Energy       int64        `json:"energy"`
	PeakPosition int          `json:"peak_position"`
	PeakSample   int16        `json:"peak_sample"`
	WeightedSum  int64        `json:"weighted_sum"`
	RawEnvelope  int64        `json:"raw_envelope"`
	Envelope     int64        `json:"envelope"`
	Truncations  []Truncation `json:"truncations,omitempty"`
}

type SegmentEnvelope struct {
	SegmentID   string          `json:"segment_id"`
	ContentHash string          `json:"content_hash"`
	Frames      []FrameEnvelope `json:"frames"`
	MaxRaw      int64           `json:"max_raw_envelope"`
	Truncations []Truncation    `json:"truncations,omitempty"`
}

func (e SegmentEnvelope) Digest() string {
	h := sha256.New()
	writeString(h, e.SegmentID)
	for _, frame := range e.Frames {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(frame.Envelope))
		_, _ = h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MeasureSegmentEnvelope measures one segment. The function is pure: its result
// depends only on the project, segment, and global frame geometry.
func MeasureSegmentEnvelope(project *Project, segment Segment) (*SegmentEnvelope, error) {
	if project == nil {
		return nil, newError(CodeSchema, "", "project", "project is nil")
	}
	if err := validateSegment(project, segment, "segment."+segment.ID, 0); err != nil {
		return nil, err
	}
	count := frameCount(segment, project.FrameLength, project.Hop)
	if count == 0 {
		return nil, newError(CodeSchema, project.ID, "frame_length", "segment is shorter than one frame")
	}
	weights := sampleWeights(segment)
	result := &SegmentEnvelope{
		SegmentID:   segment.ID,
		ContentHash: project.SignalHash(segment),
		Frames:      make([]FrameEnvelope, 0, count),
	}
	rawValues := make([]int64, 0, count)
	for index := 0; index < count; index++ {
		start := segment.StartSample + int64(index)*project.Hop
		end := start + project.FrameLength
		frame, err := measureFrame(project, segment.ID, index, start, end, weights)
		if err != nil {
			return nil, err
		}
		result.Frames = append(result.Frames, frame)
		rawValues = append(rawValues, frame.RawEnvelope)
	}
	for _, value := range rawValues {
		if value > result.MaxRaw {
			result.MaxRaw = value
		}
	}
	for i := range result.Frames {
		frame := &result.Frames[i]
		if result.MaxRaw == 0 {
			frame.Envelope = 0
			result.Truncations = append(result.Truncations, Truncation{
				Location: fmt.Sprintf("%s.frame[%d].normalize", segment.ID, i),
				Field:    "envelope", Operation: "normalize /0", OperandA: 0, OperandB: 0,
				RoundedValue: 0, Mode: "zero-segment",
			})
			continue
		}
		normalized, traces, err := ConvertScale(frame.RawEnvelope, result.MaxRaw, InternalScale, "envelope", fmt.Sprintf("%s.frame[%d].normalize", segment.ID, i))
		if err != nil {
			return nil, err
		}
		frame.Envelope = normalized
		frame.Truncations = append(frame.Truncations, traces...)
		result.Truncations = append(result.Truncations, traces...)
	}
	return result, nil
}

func measureFrame(project *Project, segmentID string, index int, start, end int64, weights []int64) (FrameEnvelope, error) {
	if start < 0 || end > int64(len(project.PCM)) || end <= start {
		return FrameEnvelope{}, newError(CodeSchema, project.ID, "frame", "frame sample range is outside PCM")
	}
	frame := FrameEnvelope{
		ID:          fmt.Sprintf("%s/frame-%04d", segmentID, index),
		SegmentID:   segmentID,
		FrameIndex:  index,
		StartSample: start,
		EndSample:   end,
	}
	var peakAbs int64
	for sampleIndex := start; sampleIndex < end; sampleIndex++ {
		sample := int64(project.PCM[sampleIndex])
		frame.Energy += sample * sample
		abs := sample
		if abs < 0 {
			abs = -abs
		}
		if abs > peakAbs {
			peakAbs = abs
			frame.PeakSample = int16(sample)
			frame.PeakPosition = int(sampleIndex - start)
		}
		weight := weights[sampleIndex-start]
		frame.WeightedSum += sample * sample * weight
	}
	frameLength := end - start
	mean, traces, err := truncDivMul(frame.WeightedSum, 1, frameLength, "weighted_mean", fmt.Sprintf("%s.frame[%d].weighted_mean", segmentID, index))
	if err != nil {
		return FrameEnvelope{}, err
	}
	frame.Truncations = append(frame.Truncations, traces...)
	mean, traces, err = truncDivMul(mean, 1, InternalScale, "weighted_mean_scaled", fmt.Sprintf("%s.frame[%d].weight_scale", segmentID, index))
	if err != nil {
		return FrameEnvelope{}, err
	}
	frame.Truncations = append(frame.Truncations, traces...)
	rms := isqrt(mean)
	rawEnvelope := rms * InternalScale / 32768
	if rms*InternalScale%32768 != 0 {
		frame.Truncations = append(frame.Truncations, Truncation{
			Location: fmt.Sprintf("%s.frame[%d].rms_scale", segmentID, index), Field: "raw_envelope",
			Operation: "*10000/32768", OperandA: rms, OperandB: 32768,
			RoundedValue: rawEnvelope, Remainder: rms * InternalScale % 32768,
			Residue: rms * InternalScale % 32768, Mode: "toward-zero",
		})
	}
	frame.RawEnvelope = rawEnvelope
	return frame, nil
}

func sampleWeights(segment Segment) []int64 {
	length := int(segment.Length())
	weights := make([]int64, length)
	for _, phoneme := range segment.Phonemes {
		weight := phoneme.Weight
		if phoneme.Kind == PhonemeSilence {
			weight = 0
		}
		if weight < 0 {
			weight = 0
		}
		start := int(phoneme.StartSample - segment.StartSample)
		end := int(phoneme.EndSample - segment.StartSample)
		if start < 0 {
			start = 0
		}
		if end > length {
			end = length
		}
		for i := start; i < end; i++ {
			weights[i] = weight
		}
	}
	return weights
}

// isqrt returns floor(sqrt(value)) using integer Newton iterations.
func isqrt(value int64) int64 {
	if value <= 0 {
		return 0
	}
	x := value
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + value/x) / 2
	}
	return x
}

// MeasureProjectEnvelopes computes all segments in project order.
func MeasureProjectEnvelopes(project *Project) ([]*SegmentEnvelope, error) {
	segments := project.Segments()
	results := make([]*SegmentEnvelope, len(segments))
	for i, segment := range segments {
		envelope, err := MeasureSegmentEnvelope(project, segment)
		if err != nil {
			return nil, err
		}
		results[i] = envelope
	}
	return results, nil
}

// MeasureProjectEnvelopesParallel computes segments concurrently but writes by
// segment index, so scheduling cannot affect result ordering or values.
func MeasureProjectEnvelopesParallel(project *Project) ([]*SegmentEnvelope, error) {
	segments := project.Segments()
	results := make([]*SegmentEnvelope, len(segments))
	errs := make([]error, len(segments))
	type indexedSegment struct {
		index   int
		segment Segment
	}
	jobs := make(chan indexedSegment)
	done := make(chan struct{})
	workers := 4
	for worker := 0; worker < workers; worker++ {
		go func() {
			for job := range jobs {
				results[job.index], errs[job.index] = MeasureSegmentEnvelope(project, job.segment)
			}
			done <- struct{}{}
		}()
	}
	for i, segment := range segments {
		jobs <- indexedSegment{index: i, segment: segment}
	}
	close(jobs)
	for worker := 0; worker < workers; worker++ {
		<-done
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func findPhonemeAt(segment Segment, sample int64) (Phoneme, bool) {
	index := sort.Search(len(segment.Phonemes), func(i int) bool {
		return segment.Phonemes[i].EndSample > sample
	})
	if index < len(segment.Phonemes) && segment.Phonemes[index].StartSample <= sample {
		return segment.Phonemes[index], true
	}
	return Phoneme{}, false
}
