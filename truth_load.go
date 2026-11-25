package lipsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func LoadTruthsFile(path string) ([]Truth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &DomainError{Code: CodeSchema, Field: "truths_file", Message: "cannot read truth file", Cause: err.Error()}
	}
	return LoadTruths(bytes.NewReader(data))
}

// LoadTruths reads the generator truth manifest. Truth is observation data,
// so its structure and internal frame ordering are validated independently of
// runtime caches.
func LoadTruths(r io.Reader) ([]Truth, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "cannot read truth input", Cause: err.Error()}
	}
	var root json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "invalid truth JSON", Cause: err.Error()}
	}
	var raws []json.RawMessage
	switch firstNonSpace(root) {
	case '[':
		if err := json.Unmarshal(root, &raws); err != nil {
			return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "invalid truth array", Cause: err.Error()}
		}
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(root, &obj); err != nil {
			return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "invalid truth object", Cause: err.Error()}
		}
		raw, ok := obj["truths"]
		if !ok {
			return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "missing field truths"}
		}
		if err := json.Unmarshal(raw, &raws); err != nil {
			return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "truths must be an array", Cause: err.Error()}
		}
	default:
		return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "expected an object or array"}
	}
	if len(raws) == 0 {
		return nil, &DomainError{Code: CodeSchema, Field: "truths", Message: "at least one truth record is required"}
	}
	out := make([]Truth, 0, len(raws))
	seen := map[string]bool{}
	for i, raw := range raws {
		path := fmt.Sprintf("truths[%d]", i)
		obj, err := object(raw, path, "")
		if err != nil {
			return nil, err
		}
		id := ""
		if idRaw, ok := obj["dialogue_id"]; ok {
			_ = json.Unmarshal(idRaw, &id)
		}
		if err := requireKeys(obj, path, id, "dialogue_id", "fps", "frame_count", "frames",
			"lip_switches", "transitions", "content_hash", "quantization_version", "algorithm_version"); err != nil {
			return nil, err
		}
		frames, err := array(obj["frames"], path+".frames", id)
		if err != nil {
			return nil, err
		}
		for fi, frameRaw := range frames {
			framePath := fmt.Sprintf("%s.frames[%d]", path, fi)
			frameObj, err := object(frameRaw, framePath, id)
			if err != nil {
				return nil, err
			}
			if err := requireKeys(frameObj, framePath, id, "frame", "char_start", "char_end",
				"base_lip", "lip", "transition", "phoneme_id", "segment_id"); err != nil {
				return nil, err
			}
		}
		var truth Truth
		if err := json.Unmarshal(raw, &truth); err != nil {
			field := path
			if te, ok := err.(*json.UnmarshalTypeError); ok && te.Field != "" {
				field = path + "." + te.Field
			}
			return nil, &DomainError{Code: CodeSchema, DialogueID: id, Field: field, Message: "truth field has the wrong type", Cause: err.Error()}
		}
		if err := ValidateTruth(truth); err != nil {
			return nil, err
		}
		if seen[truth.DialogueID] {
			return nil, &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Field: path + ".dialogue_id", Message: "duplicate truth dialogue_id"}
		}
		seen[truth.DialogueID] = true
		out = append(out, truth)
	}
	return out, nil
}

func ValidateTruth(truth Truth) error {
	if truth.DialogueID == "" {
		return &DomainError{Code: CodeSchema, Field: "truths[].dialogue_id", Message: "dialogue_id must not be empty"}
	}
	if truth.FPS <= 0 || truth.FrameCount <= 0 {
		return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Field: "truth.frame_count", Message: "frame_count and fps must be positive"}
	}
	if len(truth.Frames) != truth.FrameCount {
		return &DomainError{
			Code:       CodeSchema,
			DialogueID: truth.DialogueID,
			Field:      "truth.frames",
			Message:    fmt.Sprintf("truth has %d frames but frame_count=%d", len(truth.Frames), truth.FrameCount),
		}
	}
	if truth.ContentHash == "" || truth.QuantizationVersion == "" || truth.AlgorithmVersion == "" {
		return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Field: "truth.versions", Message: "truth hash and versions must not be empty"}
	}
	for i, frame := range truth.Frames {
		if frame.Frame != i {
			return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Field: fmt.Sprintf("truth.frames[%d].frame", i), Message: "truth frames must be sequential"}
		}
		if frame.CharStart < 0 || frame.CharEnd <= frame.CharStart {
			return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Frame: intPtr(i), Field: fmt.Sprintf("truth.frames[%d].char_interval", i), Message: "invalid truth character interval"}
		}
		if !frame.BaseLip.IsValid() || !frame.Lip.IsValid() {
			return &DomainError{Code: CodeLip, DialogueID: truth.DialogueID, Frame: intPtr(i), Field: fmt.Sprintf("truth.frames[%d].lip", i), Left: string(frame.BaseLip), Right: string(frame.Lip), Message: "invalid truth lip category"}
		}
		if frame.Transition && frame.Lip != LipClosed {
			return &DomainError{Code: CodeLip, DialogueID: truth.DialogueID, Frame: intPtr(i), Field: fmt.Sprintf("truth.frames[%d].lip", i), Left: string(frame.BaseLip), Right: string(frame.Lip), Message: "transition truth frame must use the bridge viseme"}
		}
	}
	for i, transition := range truth.Transitions {
		if transition.Frame < 0 || transition.Frame >= truth.FrameCount {
			return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Frame: intPtr(transition.Frame), Field: fmt.Sprintf("truth.transitions[%d].frame", i), Message: "transition frame outside truth range"}
		}
		if len(transition.Inserted) == 0 || len(transition.Inserted) != len(transition.InsertedFrames) {
			return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Frame: intPtr(transition.Frame), Field: fmt.Sprintf("truth.transitions[%d].inserted", i), Message: "transition insertion lists are inconsistent"}
		}
		for _, inserted := range transition.Inserted {
			if !inserted.IsValid() {
				return &DomainError{Code: CodeLip, DialogueID: truth.DialogueID, Frame: intPtr(transition.Frame), Field: fmt.Sprintf("truth.transitions[%d].inserted", i), Left: string(inserted), Message: "invalid inserted viseme"}
			}
		}
		for _, inserted := range transition.InsertedFrames {
			if inserted != transition.Frame {
				return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Frame: intPtr(inserted), Field: fmt.Sprintf("truth.transitions[%d].inserted_frames", i), Message: "transition insertion frame does not match switch frame"}
			}
		}
		if !truth.Frames[transition.Frame].Transition {
			return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Frame: intPtr(transition.Frame), Field: fmt.Sprintf("truth.transitions[%d]", i), Message: "transition record does not match frame truth"}
		}
	}
	for i, switchRecord := range truth.LipSwitches {
		if switchRecord.Frame <= 0 || switchRecord.Frame >= truth.FrameCount {
			return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Frame: intPtr(switchRecord.Frame), Field: fmt.Sprintf("truth.lip_switches[%d].frame", i), Message: "lip switch frame outside truth range"}
		}
		if !switchRecord.From.IsValid() || !switchRecord.To.IsValid() {
			return &DomainError{Code: CodeLip, DialogueID: truth.DialogueID, Frame: intPtr(switchRecord.Frame), Field: fmt.Sprintf("truth.lip_switches[%d]", i), Left: string(switchRecord.From), Right: string(switchRecord.To), Message: "invalid lip switch category"}
		}
		if truth.Frames[switchRecord.Frame-1].BaseLip != switchRecord.From || truth.Frames[switchRecord.Frame].BaseLip != switchRecord.To {
			return &DomainError{Code: CodeSchema, DialogueID: truth.DialogueID, Frame: intPtr(switchRecord.Frame), Field: fmt.Sprintf("truth.lip_switches[%d]", i), Message: "lip switch record does not match frame truth"}
		}
	}
	return nil
}

// VerifyTruth constructs the runtime timeline and checks both frames and
// transition insertion positions against the supplied generator truth.
func VerifyTruth(project DialogueProject, truth Truth) ([]Comparison, error) {
	contentHash, err := ContentHash(project)
	if err != nil {
		return nil, err
	}
	if truth.DialogueID != project.DialogueID || truth.FPS != project.FPS ||
		truth.ContentHash != contentHash ||
		truth.QuantizationVersion != project.Quantization.Version ||
		truth.AlgorithmVersion != project.AlgorithmVersion {
		return nil, &DomainError{
			Code:       CodeSchema,
			DialogueID: project.DialogueID,
			Field:      "truth.identity",
			Expected:   contentHash,
			Current:    truth.ContentHash,
			Message:    "truth does not belong to the project/version",
		}
	}
	timeline, err := BuildTimeline(project)
	if err != nil {
		return nil, err
	}
	return []Comparison{
		CompareFrames(project.DialogueID, "timeline_vs_truth", timeline.Frames, truth.Frames),
		CompareTransitions(project.DialogueID, "transitions_vs_truth", timeline.Transitions, truth.Transitions),
	}, nil
}
