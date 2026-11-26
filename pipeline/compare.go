package pipeline

import (
	"fmt"
	"sort"

	"dialogueaudio/audioerr"
	"dialogueaudio/engine"
)

// Mismatch localizes the first difference between two runs.
type Mismatch struct {
	ClipID     string `json:"clip_id"`
	FrameIndex int    `json:"frame_index"`
	Field      string `json:"field"`
	Want       int64  `json:"want"`
	Got        int64  `json:"got"`
}

func (m *Mismatch) Error() string {
	return fmt.Sprintf("run mismatch clip=%s frame=%d field=%s want=%d got=%d",
		m.ClipID, m.FrameIndex, m.Field, m.Want, m.Got)
}

// CompareRuns checks commit order, frame samples, and calibration results. It
// returns the first mismatch in canonical clip/frame order.
func CompareRuns(want, got RunResult) *Mismatch {
	for i := 0; i < len(want.CommitOrder) && i < len(got.CommitOrder); i++ {
		if want.CommitOrder[i] != got.CommitOrder[i] {
			return &Mismatch{
				ClipID: want.CommitOrder[i], FrameIndex: -1, Field: "commit_order",
			}
		}
	}
	if len(want.CommitOrder) != len(got.CommitOrder) {
		return &Mismatch{FrameIndex: -1, Field: "commit_order_length",
			Want: int64(len(want.CommitOrder)), Got: int64(len(got.CommitOrder))}
	}
	clipIDs := unionClipIDs(want.Envelopes, got.Envelopes)
	for _, clipID := range clipIDs {
		wantEnv, wantOK := want.Envelopes[clipID]
		gotEnv, gotOK := got.Envelopes[clipID]
		if !wantOK || !gotOK {
			return &Mismatch{ClipID: clipID, FrameIndex: -1, Field: "clip_missing"}
		}
		if len(wantEnv.Frames) != len(gotEnv.Frames) {
			return &Mismatch{ClipID: clipID, FrameIndex: -1, Field: "frame_count",
				Want: int64(len(wantEnv.Frames)), Got: int64(len(gotEnv.Frames))}
		}
		for i := range wantEnv.Frames {
			if m := compareFrame(wantEnv.Frames[i], gotEnv.Frames[i]); m != nil {
				return m
			}
		}
		if m := compareInt(clipID, -1, "segment_energy", wantEnv.SegmentEnergy, gotEnv.SegmentEnergy); m != nil {
			return m
		}
		if m := compareInt(clipID, -1, "segment_envelope_q16", wantEnv.SegmentEnvelopeQ16, gotEnv.SegmentEnvelopeQ16); m != nil {
			return m
		}
		if m := compareInt(clipID, -1, "peak_offset", wantEnv.PeakOffset, gotEnv.PeakOffset); m != nil {
			return m
		}
	}
	for _, clipID := range clipIDs {
		wantCal, wantOK := want.Calibrations[clipID]
		gotCal, gotOK := got.Calibrations[clipID]
		if !wantOK || !gotOK {
			return &Mismatch{ClipID: clipID, FrameIndex: -1, Field: "calibration_missing"}
		}
		if m := compareInt(clipID, -1, "adjustment_gain_q16", wantCal.AdjustmentGainQ16, gotCal.AdjustmentGainQ16); m != nil {
			return m
		}
		if m := compareInt(clipID, -1, "applied_adjustment_db_q16", wantCal.AppliedAdjustmentDBQ16, gotCal.AppliedAdjustmentDBQ16); m != nil {
			return m
		}
		if m := compareInt(clipID, -1, "after_loudness_q16", wantCal.AfterLoudnessQ16, gotCal.AfterLoudnessQ16); m != nil {
			return m
		}
		if len(wantCal.Frames) != len(gotCal.Frames) {
			return &Mismatch{ClipID: clipID, FrameIndex: -1, Field: "calibrated_frame_count",
				Want: int64(len(wantCal.Frames)), Got: int64(len(gotCal.Frames))}
		}
		for i := range wantCal.Frames {
			if m := compareInt(clipID, wantCal.Frames[i].FrameIndex, "calibrated_energy",
				wantCal.Frames[i].AfterEnergy, gotCal.Frames[i].AfterEnergy); m != nil {
				return m
			}
			if m := compareInt(clipID, wantCal.Frames[i].FrameIndex, "calibrated_envelope_q16",
				wantCal.Frames[i].AfterEnvelopeQ16, gotCal.Frames[i].AfterEnvelopeQ16); m != nil {
				return m
			}
		}
	}
	return nil
}

func compareFrame(want, got engine.FrameEnvelope) *Mismatch {
	if want.FrameIndex != got.FrameIndex {
		return &Mismatch{ClipID: want.ClipID, FrameIndex: want.FrameIndex, Field: "frame_index",
			Want: int64(want.FrameIndex), Got: int64(got.FrameIndex)}
	}
	checks := []struct {
		field     string
		want, got int64
	}{
		{"raw_energy", want.RawEnergy, got.RawEnergy},
		{"session_energy", want.SessionEnergy, got.SessionEnergy},
		{"clip_gain_energy", want.ClipGainEnergy, got.ClipGainEnergy},
		{"weighted_energy", want.WeightedEnergy, got.WeightedEnergy},
		{"peak_offset", int64(want.PeakOffset), int64(got.PeakOffset)},
		{"envelope_q16", want.EnvelopeQ16, got.EnvelopeQ16},
	}
	for _, check := range checks {
		if m := compareInt(want.ClipID, want.FrameIndex, check.field, check.want, check.got); m != nil {
			return m
		}
	}
	return nil
}

func compareInt(clipID string, frameIndex int, field string, want, got int64) *Mismatch {
	if want == got {
		return nil
	}
	return &Mismatch{ClipID: clipID, FrameIndex: frameIndex, Field: field, Want: want, Got: got}
}

func unionClipIDs(a, b map[string]engine.ClipEnvelope) []string {
	set := make(map[string]bool, len(a)+len(b))
	for id := range a {
		set[id] = true
	}
	for id := range b {
		set[id] = true
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// AsError returns E_MISMATCH as the stable coded error.
func AsError(m *Mismatch) error {
	if m == nil {
		return nil
	}
	return audioerr.New(audioerr.CodeMismatch, m.Field, m.Error()).
		WithValues(map[string]any{
			"clip_id": m.ClipID, "frame_index": m.FrameIndex,
			"want": m.Want, "got": m.Got,
		})
}
