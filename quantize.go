package lipsync

import (
	"fmt"
	"math"
)

// QuantizeFrame maps a sample point to a frame with exact integer arithmetic.
// Rounding is half-up for direction=round. No floating-point accumulator is
// used anywhere in this conversion.
func QuantizeFrame(sample int64, sampleRate, fps int, direction string) (int64, error) {
	if sample < 0 {
		return 0, &DomainError{
			Code:       CodeRange,
			Field:      "sample",
			Sample:     int64Ptr(sample),
			FPS:        intPtr(fps),
			SampleRate: intPtr(sampleRate),
			Reason:     "negative sample",
			Message:    "sample must be non-negative",
		}
	}
	if sampleRate <= 0 || fps <= 0 {
		return 0, &DomainError{
			Code:       CodeRange,
			Field:      "sample_rate",
			Sample:     int64Ptr(sample),
			FPS:        intPtr(fps),
			SampleRate: intPtr(sampleRate),
			Reason:     "non-positive rate",
			Message:    "sample_rate and fps must be positive",
		}
	}
	if direction != "ceil" && direction != "floor" && direction != "round" {
		return 0, &DomainError{
			Code:    CodeSchema,
			Field:   "quantization.direction",
			Reason:  "unsupported direction",
			Message: "direction must be ceil, floor or round",
		}
	}
	if sample > math.MaxInt64/int64(fps) {
		return 0, &DomainError{
			Code:       CodeRange,
			Field:      "sample",
			Sample:     int64Ptr(sample),
			FPS:        intPtr(fps),
			SampleRate: intPtr(sampleRate),
			Reason:     "sample*fps overflow",
			Message:    "sample to frame conversion overflows int64",
		}
	}
	product := sample * int64(fps)
	sr := int64(sampleRate)
	switch direction {
	case "floor":
		return product / sr, nil
	case "ceil":
		if product > math.MaxInt64-(sr-1) {
			return 0, &DomainError{
				Code:       CodeRange,
				Field:      "sample",
				Sample:     int64Ptr(sample),
				FPS:        intPtr(fps),
				SampleRate: intPtr(sampleRate),
				Reason:     "ceiling adjustment overflow",
				Message:    "sample to frame conversion overflows int64",
			}
		}
		return (product + sr - 1) / sr, nil
	case "round":
		half := sr / 2
		if product > math.MaxInt64-half {
			return 0, &DomainError{
				Code:       CodeRange,
				Field:      "sample",
				Sample:     int64Ptr(sample),
				FPS:        intPtr(fps),
				SampleRate: intPtr(sampleRate),
				Reason:     "rounding adjustment overflow",
				Message:    "sample to frame conversion overflows int64",
			}
		}
		return (product + half) / sr, nil
	}
	return 0, fmt.Errorf("unreachable quantization direction")
}

type Boundary struct {
	DialogueID string `json:"dialogue_id"`
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Sample     int64  `json:"sample"`
	Frame      int64  `json:"frame"`
}

type QuantizedPhoneme struct {
	SegmentIndex int         `json:"segment_index"`
	PhonemeIndex int         `json:"phoneme_index"`
	ID           string      `json:"id"`
	StartSample  int64       `json:"start_sample"`
	EndSample    int64       `json:"end_sample"`
	StartFrame   int64       `json:"start_frame"`
	EndFrame     int64       `json:"end_frame"`
	Lip          LipCategory `json:"lip"`
}

type Timing struct {
	FrameCount int                `json:"frame_count"`
	Boundaries []Boundary         `json:"boundaries"`
	Phonemes   []QuantizedPhoneme `json:"phonemes"`
}

func QuantizeProject(p DialogueProject) (Timing, error) {
	if err := ValidateProject(p); err != nil {
		return Timing{}, err
	}
	boundaries := make([]Boundary, 0, 4+len(p.Segments)*4)
	phonemes := make([]QuantizedPhoneme, 0)
	for si, seg := range p.Segments {
		start, err := QuantizeFrame(seg.StartSample, p.SampleRate, p.FPS, p.Quantization.Direction)
		if err != nil {
			de, _ := asDomainError(err)
			if de != nil {
				de.DialogueID = p.DialogueID
				de.Field = fmt.Sprintf("segments[%d].start_sample", si)
			}
			return Timing{}, err
		}
		end, err := QuantizeFrame(seg.EndSample, p.SampleRate, p.FPS, p.Quantization.Direction)
		if err != nil {
			de, _ := asDomainError(err)
			if de != nil {
				de.DialogueID = p.DialogueID
				de.Field = fmt.Sprintf("segments[%d].end_sample", si)
			}
			return Timing{}, err
		}
		boundaries = append(boundaries,
			Boundary{DialogueID: p.DialogueID, Kind: "segment_start", ID: seg.ID, Sample: seg.StartSample, Frame: start},
			Boundary{DialogueID: p.DialogueID, Kind: "segment_end", ID: seg.ID, Sample: seg.EndSample, Frame: end},
		)
		for pi, ph := range seg.Phonemes {
			phStart, err := QuantizeFrame(ph.StartSample, p.SampleRate, p.FPS, p.Quantization.Direction)
			if err != nil {
				de, _ := asDomainError(err)
				if de != nil {
					de.DialogueID = p.DialogueID
					de.Field = fmt.Sprintf("segments[%d].phonemes[%d].start_sample", si, pi)
				}
				return Timing{}, err
			}
			phEnd, err := QuantizeFrame(ph.EndSample, p.SampleRate, p.FPS, p.Quantization.Direction)
			if err != nil {
				de, _ := asDomainError(err)
				if de != nil {
					de.DialogueID = p.DialogueID
					de.Field = fmt.Sprintf("segments[%d].phonemes[%d].end_sample", si, pi)
				}
				return Timing{}, err
			}
			if phEnd < phStart {
				return Timing{}, &DomainError{
					Code:       CodeRange,
					DialogueID: p.DialogueID,
					Field:      fmt.Sprintf("segments[%d].phonemes[%d]", si, pi),
					Frame:      intPtr(int(phStart)),
					Reason:     "quantized interval inverted",
					Message:    "quantized phoneme interval is inverted",
				}
			}
			boundaries = append(boundaries,
				Boundary{DialogueID: p.DialogueID, Kind: "phoneme_start", ID: seg.ID + "/" + ph.ID, Sample: ph.StartSample, Frame: phStart},
				Boundary{DialogueID: p.DialogueID, Kind: "phoneme_end", ID: seg.ID + "/" + ph.ID, Sample: ph.EndSample, Frame: phEnd},
			)
			phonemes = append(phonemes, QuantizedPhoneme{
				SegmentIndex: si,
				PhonemeIndex: pi,
				ID:           ph.ID,
				StartSample:  ph.StartSample,
				EndSample:    ph.EndSample,
				StartFrame:   phStart,
				EndFrame:     phEnd,
				Lip:          ph.Lip,
			})
		}
	}
	endFrame, err := QuantizeFrame(p.TotalSamples(), p.SampleRate, p.FPS, p.Quantization.Direction)
	if err != nil {
		de, _ := asDomainError(err)
		if de != nil {
			de.DialogueID = p.DialogueID
			de.Field = "segments[-1].end_sample"
		}
		return Timing{}, err
	}
	if endFrame <= 0 {
		return Timing{}, &DomainError{
			Code:       CodeRange,
			DialogueID: p.DialogueID,
			Field:      "segments[-1].end_sample",
			Sample:     int64Ptr(p.TotalSamples()),
			FPS:        intPtr(p.FPS),
			SampleRate: intPtr(p.SampleRate),
			Reason:     "zero frame duration",
			Message:    "dialogue duration quantizes to zero frames",
		}
	}
	if endFrame > int64(math.MaxInt) {
		return Timing{}, &DomainError{
			Code:       CodeRange,
			DialogueID: p.DialogueID,
			Field:      "frame_count",
			Sample:     int64Ptr(p.TotalSamples()),
			FPS:        intPtr(p.FPS),
			SampleRate: intPtr(p.SampleRate),
			Reason:     "frame count exceeds platform int",
			Message:    "quantized frame count is too large",
		}
	}
	return Timing{FrameCount: int(endFrame), Boundaries: boundaries, Phonemes: phonemes}, nil
}
