package dialoguecal

import (
	"fmt"
)

func validateRange(code Code, field string, value, low, high int64, segmentID string) error {
	if value < low || value > high {
		return &Error{
			Code: code, Field: field, SegmentID: segmentID,
			OperandA: value, OperandB: low,
			Message: fmt.Sprintf("value outside declared range [%d,%d]", low, high),
		}
	}
	return nil
}

// ValidateProject enforces all source invariants before a segment is allowed
// into the processing state machine.
func ValidateProject(p *Project) error {
	if p == nil {
		return &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	if _, ok := ParseSchemaVersion(int(p.SourceVersion)); !ok {
		return &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: "unknown source schema version"}
	}
	if p.ID == "" {
		return &Error{Code: CodeSchema, Path: "$.projectId", Message: "required field is missing"}
	}
	if p.FrameRateHz <= 0 {
		return &Error{Code: CodeCal, Field: "frameRateHz", OperandA: int64(p.FrameRateHz), Message: "frame rate must be positive"}
	}
	if p.SampleRate <= 0 {
		return &Error{Code: CodeCal, Field: "sampleRate", OperandA: int64(p.SampleRate), Message: "sample rate must be positive"}
	}
	frameSamples := p.SampleRate / p.FrameRateHz
	if frameSamples <= 0 {
		return &Error{Code: CodeCal, Field: "frameRateHz", OperandA: int64(p.FrameRateHz), OperandB: int64(p.SampleRate), Message: "frame rate is incompatible with sample rate"}
	}
	if p.MaxParallelism < 1 {
		return &Error{Code: CodeCal, Field: "maxParallelism", OperandA: int64(p.MaxParallelism), Message: "parallelism budget must be at least one"}
	}
	if p.WorkerBufferSamples < frameSamples {
		return &Error{Code: CodeCal, Field: "workerBufferSamples", OperandA: int64(p.WorkerBufferSamples), OperandB: int64(frameSamples), Message: "worker sample buffer is smaller than one frame"}
	}
	if p.QueueBudget < 1 {
		return &Error{Code: CodeCal, Field: "queueBudget", OperandA: int64(p.QueueBudget), Message: "queue budget must be at least one"}
	}
	if err := validateRange(CodeCal, "targetLoudness", p.TargetLoudness, LoudnessFloor, LoudnessCeiling, ""); err != nil {
		return err
	}
	if err := validateRange(CodeCal, "sessionGain", p.SessionGain, GainFloor, GainCeiling, ""); err != nil {
		return err
	}
	if err := validateRange(CodeCal, "gainUpperBound", p.GainUpperBound, 0, GainCeiling, ""); err != nil {
		return err
	}
	if err := validateRange(CodeCal, "tolerance", p.Tolerance, 0, 100000, ""); err != nil {
		return err
	}
	if p.FrameEnergyLimit <= 0 {
		return &Error{Code: CodeCal, Field: "frameEnergyLimit", OperandA: p.FrameEnergyLimit, Message: "per-frame energy limit must be positive"}
	}
	if len(p.Dialogues) == 0 {
		return &Error{Code: CodeSchema, Path: "$.dialogues", Message: "required field is missing or empty"}
	}
	seenDialogues := make(map[string]struct{}, len(p.Dialogues))
	for di := range p.Dialogues {
		d := &p.Dialogues[di]
		if d.ID == "" {
			return &Error{Code: CodeSchema, Path: fmt.Sprintf("$.dialogues[%d].id", di), Message: "required field is missing"}
		}
		if _, exists := seenDialogues[d.ID]; exists {
			return &Error{Code: CodeSchema, Path: "dialogues", Field: "id", Message: "duplicate dialogue ID " + d.ID}
		}
		seenDialogues[d.ID] = struct{}{}
		if d.PCM.Encoding != "pcm_s16le" {
			return &Error{Code: CodeDecode, Path: fmt.Sprintf("dialogues.%s.pcm.encoding", d.ID), Message: "only pcm_s16le is supported"}
		}
		if d.PCM.Channels != 1 {
			return &Error{Code: CodeDecode, Path: fmt.Sprintf("dialogues.%s.pcm.channels", d.ID), Message: "only mono PCM is supported"}
		}
		if d.PCM.SampleRate != p.SampleRate {
			return &Error{Code: CodeCal, Field: "sampleRate", OperandA: int64(d.PCM.SampleRate), OperandB: int64(p.SampleRate), Message: "dialogue sample rate differs from project sample rate"}
		}
		if d.PCM.DeclaredSampleCount != int64(len(d.PCM.Samples)) {
			return &Error{
				Code: CodeDecode, Path: fmt.Sprintf("dialogues.%s.pcm.sampleCount", d.ID),
				OperandA: d.PCM.DeclaredSampleCount, OperandB: int64(len(d.PCM.Samples)),
				Message: "declared sample count does not match PCM payload",
			}
		}
		if len(d.Segments) == 0 {
			return &Error{Code: CodeSchema, Path: fmt.Sprintf("dialogues.%s.segments", d.ID), SegmentID: d.ID, Message: "required field is missing or empty"}
		}
		seenSegments := make(map[string]struct{}, len(d.Segments))
		var prevSegEnd int64
		for si := range d.Segments {
			seg := &d.Segments[si]
			if seg.ID == "" {
				return &Error{Code: CodeSchema, Path: fmt.Sprintf("dialogues.%s.segments[%d].id", d.ID, si), SegmentID: d.ID, Message: "required field is missing"}
			}
			if _, exists := seenSegments[seg.ID]; exists {
				return &Error{Code: CodeSchema, Path: "segments", Field: "id", SegmentID: seg.ID, Message: "duplicate segment ID"}
			}
			seenSegments[seg.ID] = struct{}{}
			seg.DialogueID = d.ID
			if seg.StartSample < 0 || seg.EndSample <= seg.StartSample || seg.EndSample > int64(len(d.PCM.Samples)) {
				return &Error{
					Code: CodeOrder, Path: fmt.Sprintf("dialogues.%s.segments.%s", d.ID, seg.ID),
					SegmentID: seg.ID, OperandA: seg.StartSample, OperandB: seg.EndSample,
					Message: "invalid segment sample interval",
				}
			}
			if si > 0 && seg.StartSample < prevSegEnd {
				return &Error{
					Code: CodeOrder, Path: fmt.Sprintf("dialogues.%s.segments", d.ID),
					SegmentID: seg.ID, OperandA: seg.StartSample, OperandB: prevSegEnd,
					Message: "segment intervals overlap",
				}
			}
			prevSegEnd = seg.EndSample
			if err := validateRange(CodeCal, "segments.gain", seg.Gain, GainFloor, GainCeiling, seg.ID); err != nil {
				return err
			}
			if seg.Gain > p.GainUpperBound {
				return &Error{
					Code: CodeCal, Field: "segments.gain", SegmentID: seg.ID,
					OperandA: seg.Gain, OperandB: p.GainUpperBound,
					Message: "declared segment gain exceeds project gain ceiling",
				}
			}
			if len(seg.Phonemes) == 0 {
				return &Error{Code: CodeSchema, Path: fmt.Sprintf("segments.%s.phonemes", seg.ID), SegmentID: seg.ID, Message: "required field is missing or empty"}
			}
			seenPhonemes := make(map[string]struct{}, len(seg.Phonemes))
			var prevEnd int64 = -1
			var prevStart int64 = -1
			for pi := range seg.Phonemes {
				ph := &seg.Phonemes[pi]
				if ph.ID == "" || ph.Class == "" {
					return &Error{Code: CodeSchema, Path: fmt.Sprintf("segments.%s.phonemes[%d]", seg.ID, pi), SegmentID: seg.ID, Message: "required phoneme field is missing"}
				}
				if _, exists := seenPhonemes[ph.ID]; exists {
					return &Error{Code: CodeSchema, Path: "phonemes", Field: "id", SegmentID: seg.ID, Message: "duplicate phoneme ID"}
				}
				seenPhonemes[ph.ID] = struct{}{}
				if ph.StartSample < seg.StartSample || ph.EndSample > seg.EndSample || ph.EndSample <= ph.StartSample {
					return &Error{
						Code: CodeOrder, Path: fmt.Sprintf("segments.%s.phonemes.%s", seg.ID, ph.ID),
						SegmentID: seg.ID, OperandA: ph.StartSample, OperandB: ph.EndSample,
						Message: "phoneme interval outside segment or inverted",
					}
				}
				if prevEnd >= 0 && (ph.StartSample < prevEnd || ph.StartSample <= prevStart) {
					return &Error{
						Code: CodeOrder, Path: fmt.Sprintf("segments.%s.phonemes", seg.ID),
						SegmentID: seg.ID, OperandA: ph.StartSample, OperandB: prevEnd,
						Message: "phoneme intervals overlap or are not strictly increasing",
					}
				}
				if ph.Weight <= 0 || ph.Weight > 50000 {
					return &Error{
						Code: CodeCal, Field: "phonemes.weight", SegmentID: seg.ID,
						OperandA: int64(ph.Weight), OperandB: 50000,
						Message: "phoneme loudness weight outside fixed range",
					}
				}
				prevEnd = ph.EndSample
				prevStart = ph.StartSample
			}
		}
	}
	intervals, err := BuildIntervals(p)
	if err != nil {
		return err
	}
	for _, iv := range intervals {
		fs, err := ComputeIntervalFeatures(p, iv)
		if err != nil {
			return err
		}
		for _, f := range fs {
			if f.Energy > p.FrameEnergyLimit {
				return &Error{
					Code: CodeCal, Field: "frameEnergyLimit", SegmentID: f.SegmentID,
					Frame: f.Frame, HasFrame: true,
					OperandA: f.Energy, OperandB: p.FrameEnergyLimit,
					Message: "frame energy exceeds declared limit",
				}
			}
		}
	}
	return nil
}
