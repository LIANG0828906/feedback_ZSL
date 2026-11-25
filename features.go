package dialogue

import (
	"fmt"
)

// FrameBoundary returns the integer sample at the start of a frame. Frame
// boundaries are floor(n * sample_rate / fps), making every quantization
// remainder explicit and reproducible.
func FrameBoundary(frameNo, sampleRate, fps int) int64 {
	if fps <= 0 {
		return 0
	}
	return int64(frameNo) * int64(sampleRate) / int64(fps)
}

// FrameOfSample returns the logical frame containing a global sample.
func FrameOfSample(sample int64, sampleRate, fps int) int {
	if fps <= 0 || sampleRate <= 0 {
		return 0
	}
	return int(sample * int64(fps) / int64(sampleRate))
}

// FrameAttribution traces one sample's frame assignment across frame-rate edits.
type FrameAttribution struct {
	Sample     int64 `json:"sample"`
	OldFPS     int   `json:"old_fps"`
	OldFrameNo int   `json:"old_frame_no"`
	NewFPS     int   `json:"new_fps"`
	NewFrameNo int   `json:"new_frame_no"`
	Changed    bool  `json:"changed"`
}

func AttributeSampleChange(sample int64, oldFPS, newFPS, sampleRate int) FrameAttribution {
	oldFrame := FrameOfSample(sample, sampleRate, oldFPS)
	newFrame := FrameOfSample(sample, sampleRate, newFPS)
	return FrameAttribution{
		Sample:     sample,
		OldFPS:     oldFPS,
		OldFrameNo: oldFrame,
		NewFPS:     newFPS,
		NewFrameNo: newFrame,
		Changed:    oldFrame != newFrame,
	}
}

// QuantizeClip maps clip sample boundaries to an inclusive-exclusive frame
// interval. Start rounds down and end rounds up.
func QuantizeClip(project *Project, clip *Clip) []FrameInterval {
	first := FrameOfSample(clip.StartSample, project.SampleRate, project.FrameRate)
	endSample := clip.StartSample + int64(len(clip.Samples))
	lastExclusive := int((endSample*int64(project.FrameRate) + int64(project.SampleRate) - 1) /
		int64(project.SampleRate))
	if lastExclusive <= first {
		lastExclusive = first + 1
	}
	out := make([]FrameInterval, 0, lastExclusive-first)
	for no := first; no < lastExclusive; no++ {
		start := FrameBoundary(no, project.SampleRate, project.FrameRate)
		end := FrameBoundary(no+1, project.SampleRate, project.FrameRate)
		rangeID := fmt.Sprintf("%s:f%05d:%d-%d", clip.ID, no, start, end)
		out = append(out, FrameInterval{
			ClipID:           clip.ID,
			FrameNo:          no,
			RangeID:          rangeID,
			FrameStartSample: start,
			FrameEndSample:   end,
		})
	}
	return out
}

// FrameBufferCap is the declared per-worker sample buffer ceiling.
const FrameBufferCap = 4096

// ExtractFrame computes fixed-point energy, zero-crossing rate and peak data.
// If buffer is supplied it must have at least the frame length; this is the
// worker's bounded sample buffer.
func ExtractFrame(project *Project, clip *Clip, interval FrameInterval, buffer []int16) (FrameFeature, error) {
	frameLen := int(interval.FrameEndSample - interval.FrameStartSample)
	if frameLen <= 0 {
		return FrameFeature{}, parameterError(clip.ID, "frame_interval", "non-positive frame length")
	}
	if frameLen > FrameBufferCap {
		return FrameFeature{}, &Error{
			Code:     CodeOvf,
			Message:  "frame sample buffer exceeds declared cap",
			ClipID:   clip.ID,
			Field:    "sample_buffer_cap",
			Operands: []int64{int64(frameLen), FrameBufferCap},
		}
	}
	if buffer == nil {
		buffer = make([]int16, frameLen)
	}
	if len(buffer) < frameLen {
		return FrameFeature{}, &Error{
			Code:     CodePara,
			Message:  "per-thread sample buffer is too small",
			ClipID:   clip.ID,
			Field:    "sample_buffer_cap",
			Operands: []int64{int64(len(buffer)), int64(frameLen)},
		}
	}
	frame := buffer[:frameLen]
	realSamples := 0
	for j := 0; j < frameLen; j++ {
		globalSample := interval.FrameStartSample + int64(j)
		clipIndex := globalSample - clip.StartSample
		if clipIndex >= 0 && clipIndex < int64(len(clip.Samples)) {
			frame[j] = clip.Samples[clipIndex]
			realSamples++
		} else {
			frame[j] = 0
		}
	}

	truncations := make([]Truncation, 0, 4)
	var sumSquares int64
	crossings := 0
	peakAbs := 0
	peakIndex := -1
	for i, sample := range frame {
		v := int64(sample)
		sumSquares += v * v
		if i > 0 {
			prev := frame[i-1]
			if (prev >= 0 && sample < 0) || (prev < 0 && sample >= 0) {
				crossings++
			}
		}
		abs := int(sample)
		if abs < 0 {
			abs = -abs
		}
		if abs > peakAbs {
			peakAbs = abs
			peakIndex = i
		}
	}

	scaledSquares, ok := checkedMulPositive(sumSquares, EnergyScale)
	if !ok {
		return FrameFeature{}, overflowError("energy_fixed", sumSquares, EnergyScale)
	}
	energyDen := int64(frameLen) * 32768 * 32768
	energy, tr := resolveRounding(scaledSquares, energyDen, RoundFloor,
		"energy_fixed", "sum(sample^2)*1e6/(N*32768^2)", interval.FrameStartSample)
	truncations = append(truncations, tr)

	zcrNumerator, ok := checkedMulPositive(int64(crossings), BasisScale)
	if !ok {
		return FrameFeature{}, overflowError("zcr_bp", int64(crossings), BasisScale)
	}
	zcr, tr := resolveRounding(zcrNumerator, int64(frameLen-1), RoundFloor,
		"zcr_bp", "crossings*10000/(N-1)", interval.FrameStartSample)
	truncations = append(truncations, tr)

	feature := FrameFeature{
		ClipID:           clip.ID,
		FrameNo:          interval.FrameNo,
		RangeID:          interval.RangeID,
		FrameStartSample: interval.FrameStartSample,
		FrameEndSample:   interval.FrameEndSample,
		EvaluatedSamples: frameLen,
		EnergyFixed:      energy,
		RMSMicro:         IntSqrt(energy * LinearScale),
		ZCRBP:            zcr,
		PeakAmp:          0,
		PeakSample:       -1,
		PeakOffset:       -1,
		Transient:        false,
		Truncations:      truncations,
	}
	if peakIndex >= 0 && peakAbs > 0 {
		feature.PeakOffset = peakIndex
		feature.PeakAmp = int(frame[peakIndex])
		if feature.PeakAmp < 0 {
			feature.PeakAmp = -feature.PeakAmp
		}
		feature.PeakSample = interval.FrameStartSample + int64(peakIndex)
		feature.Transient = peakAbs >= TransientThreshold
	}
	_ = realSamples
	return feature, nil
}

// ValidatePCM enforces declared PCM length and overlap-independent decode
// integrity. Interval overlap is checked separately because it is E_OVER.
func ValidatePCM(clip *Clip) error {
	if clip == nil {
		return newError(CodeDecode, "nil clip")
	}
	if clip.DeclaredSampleCount > 0 && clip.DeclaredSampleCount > len(clip.Samples) {
		return &Error{
			Code:     CodeDecode,
			Message:  "audio payload is truncated relative to declared sample count",
			ClipID:   clip.ID,
			Field:    "declared_sample_count",
			Operands: []int64{int64(clip.DeclaredSampleCount), int64(len(clip.Samples))},
		}
	}
	if len(clip.Samples) == 0 {
		return &Error{Code: CodeDecode, Message: "clip has no PCM samples", ClipID: clip.ID, Field: "samples"}
	}
	return nil
}

// ValidateOverlap returns E_OVER when two clips occupy overlapping timelines.
func ValidateOverlap(clips []*Clip) error {
	for i := 0; i < len(clips); i++ {
		for j := i + 1; j < len(clips); j++ {
			a, b := clips[i], clips[j]
			aEnd := a.StartSample + int64(len(a.Samples))
			bEnd := b.StartSample + int64(len(b.Samples))
			if a.StartSample < bEnd && b.StartSample < aEnd {
				return &Error{
					Code:    CodeOver,
					Message: "clip sample intervals overlap",
					ClipID:  a.ID,
					Field:   "clips",
					Details: map[string]any{
						"clip_ids": []string{a.ID, b.ID},
						"ranges": [][2]int64{
							{a.StartSample, aEnd},
							{b.StartSample, bEnd},
						},
					},
				}
			}
		}
	}
	return nil
}
