package lipsync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// LoadProjectsFile reads a projects JSON document. Both a top-level array and
// {"projects": [...]} are accepted.
func LoadProjectsFile(path string) ([]DialogueProject, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, &DomainError{
			Code:    CodeSchema,
			Field:   "projects_file",
			Message: "cannot read project file",
			Cause:   err.Error(),
		}
	}
	return LoadProjects(bytes.NewReader(b))
}

func LoadProjects(r io.Reader) ([]DialogueProject, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "cannot read input", Cause: err.Error()}
	}
	var root json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "invalid JSON", Cause: err.Error()}
	}
	var raws []json.RawMessage
	switch firstNonSpace(root) {
	case '[':
		if err := json.Unmarshal(root, &raws); err != nil {
			return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "invalid project array", Cause: err.Error()}
		}
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(root, &obj); err != nil {
			return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "invalid project object", Cause: err.Error()}
		}
		raw, ok := obj["projects"]
		if !ok {
			return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "missing field projects"}
		}
		if err := json.Unmarshal(raw, &raws); err != nil {
			return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "projects must be an array", Cause: err.Error()}
		}
	default:
		return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "expected an object or array"}
	}
	if len(raws) == 0 {
		return nil, &DomainError{Code: CodeSchema, Field: "projects", Message: "at least one dialogue project is required"}
	}
	projects := make([]DialogueProject, 0, len(raws))
	seen := map[string]bool{}
	for i, raw := range raws {
		p, err := decodeProject(raw, fmt.Sprintf("projects[%d]", i))
		if err != nil {
			return nil, err
		}
		if seen[p.DialogueID] {
			return nil, &DomainError{
				Code:       CodeSchema,
				DialogueID: p.DialogueID,
				Field:      fmt.Sprintf("projects[%d].dialogue_id", i),
				Message:    "duplicate dialogue_id",
			}
		}
		seen[p.DialogueID] = true
		projects = append(projects, p)
	}
	return projects, nil
}

func firstNonSpace(b []byte) byte {
	for _, c := range b {
		if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
			return c
		}
	}
	return 0
}

func requireKeys(raw map[string]json.RawMessage, path, dialogueID string, keys ...string) error {
	for _, key := range keys {
		v, ok := raw[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return &DomainError{
				Code:       CodeSchema,
				DialogueID: dialogueID,
				Field:      path + "." + key,
				Path:       path,
				Message:    "missing required field " + key,
			}
		}
	}
	return nil
}

func object(raw json.RawMessage, fieldPath, dialogueID string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, &DomainError{
			Code:       CodeSchema,
			DialogueID: dialogueID,
			Field:      fieldPath,
			Message:    "expected object",
			Cause:      err.Error(),
		}
	}
	return obj, nil
}

func array(raw json.RawMessage, fieldPath, dialogueID string) ([]json.RawMessage, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, &DomainError{
			Code:       CodeSchema,
			DialogueID: dialogueID,
			Field:      fieldPath,
			Message:    "expected array",
			Cause:      err.Error(),
		}
	}
	return arr, nil
}

func decodeProject(raw json.RawMessage, path string) (DialogueProject, error) {
	obj, err := object(raw, path, "")
	if err != nil {
		return DialogueProject{}, err
	}
	id := ""
	if idRaw, ok := obj["dialogue_id"]; ok {
		_ = json.Unmarshal(idRaw, &id)
	}
	if err := requireKeys(obj, path, id,
		"dialogue_id", "segments", "subtitle", "fps", "sample_rate",
		"quantization", "min_readable_frames", "lip_transition_tolerance",
		"algorithm_version"); err != nil {
		return DialogueProject{}, err
	}
	segments, err := array(obj["segments"], path+".segments", id)
	if err != nil {
		return DialogueProject{}, err
	}
	if len(segments) == 0 {
		return DialogueProject{}, &DomainError{Code: CodeSchema, DialogueID: id, Field: path + ".segments", Message: "segments must not be empty"}
	}
	for si, segRaw := range segments {
		segPath := fmt.Sprintf("%s.segments[%d]", path, si)
		seg, err := object(segRaw, segPath, id)
		if err != nil {
			return DialogueProject{}, err
		}
		if err := requireKeys(seg, segPath, id, "id", "voice_ref", "start_sample", "end_sample", "phonemes"); err != nil {
			return DialogueProject{}, err
		}
		phs, err := array(seg["phonemes"], segPath+".phonemes", id)
		if err != nil {
			return DialogueProject{}, err
		}
		if len(phs) == 0 {
			return DialogueProject{}, &DomainError{Code: CodeSchema, DialogueID: id, Field: segPath + ".phonemes", Message: "phonemes must not be empty"}
		}
		for pi, phRaw := range phs {
			phPath := fmt.Sprintf("%s.phonemes[%d]", segPath, pi)
			ph, err := object(phRaw, phPath, id)
			if err != nil {
				return DialogueProject{}, err
			}
			if err := requireKeys(ph, phPath, id, "id", "start_sample", "end_sample", "lip"); err != nil {
				return DialogueProject{}, err
			}
		}
	}
	subtitle, err := object(obj["subtitle"], path+".subtitle", id)
	if err != nil {
		return DialogueProject{}, err
	}
	if err := requireKeys(subtitle, path+".subtitle", id, "text", "weights"); err != nil {
		return DialogueProject{}, err
	}
	quant, err := object(obj["quantization"], path+".quantization", id)
	if err != nil {
		return DialogueProject{}, err
	}
	if err := requireKeys(quant, path+".quantization", id, "direction", "version"); err != nil {
		return DialogueProject{}, err
	}

	var project DialogueProject
	if err := json.Unmarshal(raw, &project); err != nil {
		field := path
		if te, ok := err.(*json.UnmarshalTypeError); ok && te.Field != "" {
			field = path + "." + te.Field
		}
		return DialogueProject{}, &DomainError{
			Code:       CodeSchema,
			DialogueID: id,
			Field:      field,
			Message:    "field has the wrong type",
			Cause:      err.Error(),
		}
	}
	if err := ValidateProject(project); err != nil {
		return DialogueProject{}, err
	}
	return project, nil
}

func ValidateProject(p DialogueProject) error {
	if p.DialogueID == "" {
		return newError(CodeSchema, "", "dialogue_id", "dialogue_id must not be empty")
	}
	if p.AlgorithmVersion == "" {
		return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: "algorithm_version", Message: "algorithm_version must not be empty"}
	}
	if p.FPS < SupportedMinFPS || p.FPS > SupportedMaxFPS || p.FPS > p.SampleRate {
		return &DomainError{
			Code:       CodeRate,
			DialogueID: p.DialogueID,
			Field:      "fps",
			FPS:        intPtr(p.FPS),
			SampleRate: intPtr(p.SampleRate),
			Reason:     "sample_rate/fps contradiction",
			Message:    fmt.Sprintf("fps %d is outside supported range %d..%d or exceeds sample_rate", p.FPS, SupportedMinFPS, SupportedMaxFPS),
		}
	}
	if p.SampleRate < SupportedMinSampleRate || p.SampleRate > SupportedMaxSampleRate {
		return &DomainError{
			Code:       CodeRate,
			DialogueID: p.DialogueID,
			Field:      "sample_rate",
			FPS:        intPtr(p.FPS),
			SampleRate: intPtr(p.SampleRate),
			Reason:     "unsupported sample rate",
			Message:    fmt.Sprintf("sample_rate %d is outside supported range %d..%d", p.SampleRate, SupportedMinSampleRate, SupportedMaxSampleRate),
		}
	}
	switch p.Quantization.Direction {
	case "ceil", "floor", "round":
	default:
		return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: "quantization.direction", Message: "direction must be ceil, floor or round"}
	}
	if p.Quantization.Version == "" {
		return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: "quantization.version", Message: "quantization.version must not be empty"}
	}
	if p.MinReadableFrames < 1 {
		return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: "min_readable_frames", Message: "min_readable_frames must be at least 1"}
	}
	if p.LipTransitionTolerance < 0 {
		return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: "lip_transition_tolerance", Message: "lip_transition_tolerance must be non-negative"}
	}
	if p.RuneCount() == 0 {
		return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: "subtitle.text", Message: "subtitle.text must not be empty"}
	}
	if len(p.Subtitle.Weights) != p.RuneCount() {
		return &DomainError{
			Code:       CodeSchema,
			DialogueID: p.DialogueID,
			Field:      "subtitle.weights",
			Message:    fmt.Sprintf("subtitle has %d characters but %d weights", p.RuneCount(), len(p.Subtitle.Weights)),
		}
	}
	for i, w := range p.Subtitle.Weights {
		if w <= 0 {
			return &DomainError{
				Code:       CodeSchema,
				DialogueID: p.DialogueID,
				Field:      fmt.Sprintf("subtitle.weights[%d]", i),
				Message:    "subtitle weight must be positive",
			}
		}
	}
	if len(p.Segments) == 0 {
		return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: "segments", Message: "segments must not be empty"}
	}

	seenSegments := map[string]bool{}
	var previousSegmentEnd int64 = -1
	phonemeOrder := make([]struct {
		id    string
		start int64
		end   int64
		lip   LipCategory
		path  string
	}, 0)
	for si, seg := range p.Segments {
		segPath := fmt.Sprintf("segments[%d]", si)
		if seg.ID == "" {
			return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: segPath + ".id", Message: "segment id must not be empty"}
		}
		if seenSegments[seg.ID] {
			return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: segPath + ".id", Message: "duplicate segment id"}
		}
		seenSegments[seg.ID] = true
		if len(seg.Phonemes) == 0 {
			return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: segPath + ".phonemes", Message: "phonemes must not be empty"}
		}
		if seg.VoiceRef == "" {
			return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: segPath + ".voice_ref", Message: "voice_ref must not be empty"}
		}
		if seg.StartSample < 0 || seg.EndSample <= seg.StartSample {
			return &DomainError{
				Code:       CodeOrder,
				DialogueID: p.DialogueID,
				Field:      segPath + ".start_sample",
				Reason:     "segment interval must be strictly increasing",
				Message:    "segment start_sample/end_sample interval is invalid",
			}
		}
		if seg.StartSample < previousSegmentEnd {
			return &DomainError{
				Code:       CodeOrder,
				DialogueID: p.DialogueID,
				Field:      segPath + ".start_sample",
				Reason:     "segments overlap or are out of order",
				Message:    "segment intervals must not overlap and must be strictly increasing",
			}
		}
		previousSegmentEnd = seg.EndSample
		var previousPhonemeStart int64 = -1
		var previousPhonemeEnd int64 = -1
		for pi, ph := range seg.Phonemes {
			phPath := fmt.Sprintf("%s.phonemes[%d]", segPath, pi)
			if ph.ID == "" {
				return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: phPath + ".id", Message: "phoneme id must not be empty"}
			}
			if ph.StartSample < 0 || ph.EndSample <= ph.StartSample {
				return &DomainError{
					Code:       CodeOrder,
					DialogueID: p.DialogueID,
					Field:      phPath + ".start_sample",
					Reason:     "phoneme interval must be strictly increasing",
					Message:    "phoneme start_sample/end_sample interval is invalid",
				}
			}
			if ph.StartSample < previousPhonemeStart || (previousPhonemeEnd >= 0 && ph.StartSample < previousPhonemeEnd) {
				return &DomainError{
					Code:       CodeOrder,
					DialogueID: p.DialogueID,
					Field:      phPath + ".start_sample",
					Reason:     "phoneme intervals overlap or are out of order",
					Message:    "phoneme intervals must not overlap and must be strictly increasing",
				}
			}
			if ph.StartSample < seg.StartSample || ph.EndSample > seg.EndSample {
				return &DomainError{
					Code:       CodeSchema,
					DialogueID: p.DialogueID,
					Field:      phPath,
					Message:    "phoneme interval must be contained by its segment",
				}
			}
			if !ph.Lip.IsValid() {
				frame := -1
				if q, err := QuantizeFrame(ph.StartSample, p.SampleRate, p.FPS, p.Quantization.Direction); err == nil {
					frame = int(q)
				}
				return &DomainError{
					Code:       CodeLip,
					DialogueID: p.DialogueID,
					Field:      phPath + ".lip",
					Frame:      intPtr(frame),
					Left:       string(ph.Lip),
					Reason:     "unknown lip category",
					Message:    "unknown lip category",
				}
			}
			previousPhonemeStart = ph.StartSample
			previousPhonemeEnd = ph.EndSample
			phonemeOrder = append(phonemeOrder, struct {
				id    string
				start int64
				end   int64
				lip   LipCategory
				path  string
			}{ph.ID, ph.StartSample, ph.EndSample, ph.Lip, phPath})
		}
	}
	if !sort.SliceIsSorted(phonemeOrder, func(i, j int) bool { return phonemeOrder[i].start < phonemeOrder[j].start }) {
		return &DomainError{Code: CodeOrder, DialogueID: p.DialogueID, Field: "segments.phonemes", Message: "phonemes are not globally ordered"}
	}
	seenPhonemes := map[string]bool{}
	for _, ph := range phonemeOrder {
		if seenPhonemes[ph.id] {
			return &DomainError{Code: CodeSchema, DialogueID: p.DialogueID, Field: ph.path + ".id", Message: "duplicate phoneme id"}
		}
		seenPhonemes[ph.id] = true
	}
	return nil
}
