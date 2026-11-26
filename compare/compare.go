package compare

import (
	"fmt"

	"dialoguefit/model"
)

type Mismatch struct {
	OK               bool   `json:"ok"`
	DialogueID       string `json:"dialogue_id,omitempty"`
	PhonemeIndex     int    `json:"phoneme_index,omitempty"`
	Field            string `json:"field,omitempty"`
	Message          string `json:"message,omitempty"`
	ComparedFrames   int    `json:"compared_frames"`
	ComparedStress   int    `json:"compared_stress_marks"`
	ComparedSegments int    `json:"compared_segments"`
}

func OK(frames, stress, segments int) Mismatch {
	return Mismatch{OK: true, PhonemeIndex: -1, ComparedFrames: frames, ComparedStress: stress, ComparedSegments: segments}
}

func RunsEqual(expected, actual []*model.DialogueResult) Mismatch {
	frameCount, stressCount, segmentCount := counts(expected)
	if len(expected) != len(actual) {
		return Mismatch{DialogueID: "*", PhonemeIndex: -1, Field: "dialogues",
			Message:        fmt.Sprintf("dialogue count expected=%d actual=%d", len(expected), len(actual)),
			ComparedFrames: frameCount, ComparedStress: stressCount, ComparedSegments: segmentCount}
	}
	for di := range expected {
		a := expected[di]
		b := actual[di]
		if a.DialogueID != b.DialogueID {
			return fail(a.DialogueID, -1, "dialogue_id", fmt.Sprintf("expected order id=%s actual=%s", a.DialogueID, b.DialogueID))
		}
		if m := compareDialogue(a, b); !m.OK {
			m.ComparedFrames = frameCount
			m.ComparedStress = stressCount
			m.ComparedSegments = segmentCount
			return m
		}
	}
	return OK(frameCount, stressCount, segmentCount)
}

func compareDialogue(a, b *model.DialogueResult) Mismatch {
	if len(a.PhonemeResults) != len(b.PhonemeResults) {
		return fail(a.DialogueID, -1, "phoneme_results", fmt.Sprintf("length %d != %d", len(a.PhonemeResults), len(b.PhonemeResults)))
	}
	for i := range a.PhonemeResults {
		pa, pb := a.PhonemeResults[i], b.PhonemeResults[i]
		idx := pa.Index
		if pa.Index != pb.Index {
			return fail(a.DialogueID, idx, "phoneme_index", fmt.Sprintf("index %d != %d", pa.Index, pb.Index))
		}
		if len(pa.Frames) != len(pb.Frames) {
			return fail(a.DialogueID, idx, "frames", fmt.Sprintf("frame count %d != %d", len(pa.Frames), len(pb.Frames)))
		}
		for fi := range pa.Frames {
			fa, fb := pa.Frames[fi], pb.Frames[fi]
			if fa.Index != fb.Index || fa.StartSample != fb.StartSample || fa.CenterSample != fb.CenterSample || fa.EndSample != fb.EndSample {
				return fail(a.DialogueID, idx, fmt.Sprintf("frames[%d].timeline", fi), "frame timeline differs")
			}
			if fa.Value != fb.Value {
				return fail(a.DialogueID, idx, fmt.Sprintf("frames[%d].value", fi), fmt.Sprintf("frame %d value %.17g != %.17g", fa.Index, fa.Value, fb.Value))
			}
		}
		if (pa.Stress == nil) != (pb.Stress == nil) {
			return fail(a.DialogueID, idx, "stress", "stress presence differs")
		}
		if pa.Stress != nil {
			if pa.Stress.Sample != pb.Stress.Sample {
				return fail(a.DialogueID, idx, "stress.sample", fmt.Sprintf("%d != %d", pa.Stress.Sample, pb.Stress.Sample))
			}
			if pa.Stress.AlignedSample != pb.Stress.AlignedSample {
				return fail(a.DialogueID, idx, "stress.aligned_sample", fmt.Sprintf("%d != %d", pa.Stress.AlignedSample, pb.Stress.AlignedSample))
			}
		}
		if (pa.Segment == nil) != (pb.Segment == nil) {
			return fail(a.DialogueID, idx, "segment", "segment presence differs")
		}
		if pa.Segment != nil {
			sa, sb := pa.Segment, pb.Segment
			if sa.StartSample != sb.StartSample {
				return fail(a.DialogueID, idx, "stretched.start_sample", fmt.Sprintf("%d != %d", sa.StartSample, sb.StartSample))
			}
			if sa.EndSample != sb.EndSample {
				return fail(a.DialogueID, idx, "stretched.end_sample", fmt.Sprintf("%d != %d", sa.EndSample, sb.EndSample))
			}
			if sa.StretchFactor != sb.StretchFactor {
				return fail(a.DialogueID, idx, "stretch_factor", fmt.Sprintf("%.17g != %.17g", sa.StretchFactor, sb.StretchFactor))
			}
			if sa.HitCap != sb.HitCap {
				return fail(a.DialogueID, idx, "hit_cap", fmt.Sprintf("%v != %v", sa.HitCap, sb.HitCap))
			}
		}
	}
	if len(a.CommitSequence) != len(b.CommitSequence) {
		return fail(a.DialogueID, -1, "commit_sequence", "commit sequence length differs")
	}
	for i := range a.CommitSequence {
		if a.CommitSequence[i] != b.CommitSequence[i] {
			return fail(a.DialogueID, -1, "commit_sequence["+itoa(i)+"]", a.CommitSequence[i]+" != "+b.CommitSequence[i])
		}
	}
	return Mismatch{OK: true, PhonemeIndex: -1}
}

func counts(results []*model.DialogueResult) (frames, stress, segments int) {
	for _, r := range results {
		if r == nil {
			continue
		}
		frames += len(r.Frames)
		stress += len(r.Stress)
		segments += len(r.Segments)
	}
	return
}

func fail(dialogueID string, phonemeIndex int, field, message string) Mismatch {
	return Mismatch{OK: false, DialogueID: dialogueID, PhonemeIndex: phonemeIndex, Field: field, Message: message}
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
