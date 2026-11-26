package recheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"dialoguefit/checkpoint"
	"dialoguefit/model"
)

type Violation struct {
	DialogueID   string `json:"dialogue_id"`
	PhonemeIndex int    `json:"phoneme_index,omitempty"`
	Field        string `json:"field"`
	Message      string `json:"message"`
}

type Report struct {
	OK                     bool       `json:"ok"`
	FirstViolation         *Violation `json:"first_violation,omitempty"`
	CheckedDialogues       int        `json:"checked_dialogues"`
	CheckedFrames          int        `json:"checked_frames"`
	CheckedStressMarks     int        `json:"checked_stress_marks"`
	CheckedSegments        int        `json:"checked_segments"`
	FingerprintsRecomputed int        `json:"fingerprints_recomputed"`
}

type phonemePayload struct {
	Index   int                   `json:"phoneme_index"`
	Frames  []model.Frame         `json:"frames"`
	Stress  *model.StressMark     `json:"stress,omitempty"`
	Segment *model.StretchSegment `json:"segment,omitempty"`
}

// Run independently verifies persisted segment payloads against the truth
// manifest. It deliberately does not import or invoke envelope/fit/engine.
func Run(ctx context.Context, store *checkpoint.Store, truth *model.TruthFile) Report {
	rep := Report{OK: true}
	truthByID := map[string]model.TruthDialogue{}
	for _, td := range truth.Dialogues {
		truthByID[td.DialogueID] = td
		recomputed := fingerprintTruth(td)
		rep.FingerprintsRecomputed++
		if recomputed != td.Fingerprint {
			return fail(rep, Violation{DialogueID: td.DialogueID, Field: "fingerprint", Message: "truth fingerprint mismatch"})
		}
	}

	records, err := store.LoadAllOrdered(ctx)
	if err != nil {
		return fail(rep, Violation{DialogueID: "*", Field: "checkpoint", Message: err.Error()})
	}
	byDialogue := map[string][]checkpoint.SegmentRecord{}
	order := make([]string, 0)
	for _, rec := range records {
		if _, ok := byDialogue[rec.DialogueID]; !ok {
			order = append(order, rec.DialogueID)
		}
		byDialogue[rec.DialogueID] = append(byDialogue[rec.DialogueID], rec)
	}

	for _, dialogueID := range order {
		td, ok := truthByID[dialogueID]
		if !ok {
			return fail(rep, Violation{DialogueID: dialogueID, Field: "truth.dialogue_id", Message: "persisted dialogue absent from truth"})
		}
		recs := byDialogue[dialogueID]
		rep.CheckedDialogues++
		if len(recs) != len(td.Stretched) {
			return fail(rep, Violation{DialogueID: dialogueID, Field: "segment_checkpoints", Message: fmt.Sprintf("segments=%d truth=%d", len(recs), len(td.Stretched))})
		}
		paramVersion := ""
		for pos, rec := range recs {
			if rec.PhonemeIndex != pos {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "phoneme_index", Message: "persisted segment order is not contiguous"})
			}
			if paramVersion == "" {
				paramVersion = rec.ParamVersion
			} else if rec.ParamVersion != paramVersion {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "param_version", Message: "mixed parameter versions in dialogue snapshot"})
			}
			if rec.ContentHash != td.ContentHash {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "content_hash", Message: "persisted content hash does not match truth"})
			}
			if rec.AlgorithmVersion != td.AlgorithmVersion {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "algorithm_version", Message: "algorithm version mismatch"})
			}
			if rec.TruthFingerprint != td.Fingerprint {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "truth_fingerprint", Message: "persisted truth fingerprint mismatch"})
			}

			var payload phonemePayload
			if err := json.Unmarshal([]byte(rec.PayloadJSON), &payload); err != nil {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "payload_json", Message: err.Error()})
			}
			for _, frame := range payload.Frames {
				if frame.Index < 0 || frame.Index >= len(td.FrameValues) {
					return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: fmt.Sprintf("frames[%d].index", frame.Index), Message: "frame index outside truth"})
				}
				rep.CheckedFrames++
				if frame.Value != td.FrameValues[frame.Index] {
					return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: fmt.Sprintf("frame_values[%d]", frame.Index),
						Message: fmt.Sprintf("persisted=%.17g truth=%.17g", frame.Value, td.FrameValues[frame.Index])})
				}
			}

			truthSegment := td.Stretched[pos]
			rep.CheckedSegments++
			if payload.Segment == nil {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "stretched", Message: "missing persisted segment"})
			}
			seg := payload.Segment
			if seg.StartSample != truthSegment.StartSample {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "stretched.start_sample", Message: fmt.Sprintf("%d != %d", seg.StartSample, truthSegment.StartSample)})
			}
			if seg.EndSample != truthSegment.EndSample {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "stretched.end_sample", Message: fmt.Sprintf("%d != %d", seg.EndSample, truthSegment.EndSample)})
			}
			if seg.HitCap != truthSegment.HitCap {
				return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "stretched.hit_cap", Message: "cap flag mismatch"})
			}

			if payload.Stress != nil {
				rep.CheckedStressMarks++
				truthStress := findTruthStress(td.Stress, pos)
				if truthStress == nil {
					return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "stress", Message: "persisted stress absent from truth"})
				}
				if payload.Stress.Sample != truthStress.Sample {
					return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "stress.sample", Message: fmt.Sprintf("%d != %d", payload.Stress.Sample, truthStress.Sample)})
				}
				if payload.Stress.AlignedSample != truthStress.AlignedSample {
					return fail(rep, Violation{DialogueID: dialogueID, PhonemeIndex: pos, Field: "stress.aligned_sample", Message: fmt.Sprintf("%d != %d", payload.Stress.AlignedSample, truthStress.AlignedSample)})
				}
			}
		}
	}
	return rep
}

func findTruthStress(marks []model.TruthStress, index int) *model.TruthStress {
	for i := range marks {
		if marks[i].Index == index {
			return &marks[i]
		}
	}
	return nil
}

func fingerprintTruth(td model.TruthDialogue) string {
	b, _ := json.Marshal(struct {
		FrameValues []float64              `json:"frame_values"`
		Stress      []model.TruthStress    `json:"stress"`
		Stretched   []model.StretchSegment `json:"stretched"`
	}{td.FrameValues, td.Stress, td.Stretched})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fail(rep Report, v Violation) Report {
	rep.OK = false
	rep.FirstViolation = &v
	return rep
}
