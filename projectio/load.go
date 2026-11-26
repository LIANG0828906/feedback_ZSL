package projectio

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"

	"dialoguefit/model"
)

func LoadProjectFile(path string) (*model.Project, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, model.DecodeError("project", "project_file", "project manifest missing: %s", path)
		}
		return nil, model.DecodeError("project", "project_file", "read project manifest: %v", err)
	}
	var project model.Project
	if err := json.Unmarshal(raw, &project); err != nil {
		return nil, model.DecodeError("project", "json", "decode project manifest: %v", err)
	}
	if err := ValidateProject(&project); err != nil {
		return nil, err
	}
	return &project, nil
}

func LoadTruthFile(path string) (*model.TruthFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, model.DecodeError("*", "truth_file", "truth manifest missing: %s", path)
		}
		return nil, model.DecodeError("*", "truth_file", "read truth manifest: %v", err)
	}
	var truth model.TruthFile
	if err := json.Unmarshal(raw, &truth); err != nil {
		return nil, model.DecodeError("*", "json", "decode truth manifest: %v", err)
	}
	if err := ValidateTruthShape(&truth); err != nil {
		return nil, err
	}
	return &truth, nil
}

func LoadBundle(projectPath, truthPath string) (*model.Project, *model.TruthFile, error) {
	project, err := LoadProjectFile(projectPath)
	if err != nil {
		return nil, nil, err
	}
	truth, err := LoadTruthFile(truthPath)
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateBundle(project, truth); err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

func ValidateProject(project *model.Project) error {
	if project == nil {
		return model.DecodeError("project", "$", "nil project")
	}
	if project.SchemaVersion == "" {
		return model.DecodeError("project", "schema_version", "missing schema_version")
	}
	if project.AlgorithmVersion == "" {
		return model.DecodeError("project", "algorithm_version", "missing algorithm_version")
	}
	if project.Envelope.FrameLength <= 0 {
		return model.DecodeError("project", "envelope.frame_length", "must be positive")
	}
	if project.Envelope.FrameShift <= 0 {
		return model.DecodeError("project", "envelope.frame_shift", "must be positive")
	}
	if project.Envelope.SmoothWindow <= 0 {
		return model.DecodeError("project", "envelope.smooth_window", "must be positive")
	}
	if len(project.Dialogues) == 0 {
		return model.DecodeError("project", "dialogues", "at least one dialogue is required")
	}
	seen := map[string]struct{}{}
	for di := range project.Dialogues {
		d := &project.Dialogues[di]
		if err := validateDialogue(d); err != nil {
			return err
		}
		if _, ok := seen[d.ID]; ok {
			return model.DecodeError(d.ID, "id", "duplicate dialogue id")
		}
		seen[d.ID] = struct{}{}
	}
	return nil
}

func validateDialogue(d *model.Dialogue) error {
	if d.ID == "" {
		return model.DecodeError("", "id", "missing dialogue id")
	}
	if d.PCM.Encoding == "" {
		return model.DecodeError(d.ID, "pcm.encoding", "missing PCM encoding")
	}
	if d.PCM.Encoding != "pcm_s16le" {
		return model.DecodeError(d.ID, "pcm.encoding", "unsupported encoding %q", d.PCM.Encoding)
	}
	if d.PCM.SampleRate <= 0 {
		return model.DecodeError(d.ID, "pcm.sample_rate", "sample_rate must be positive")
	}
	if d.PCM.SampleCount <= 0 {
		return model.DecodeError(d.ID, "pcm.sample_count", "sample_count must be positive")
	}
	if err := decodePCM(d); err != nil {
		return err
	}
	if len(d.PCM.Samples) != d.PCM.SampleCount {
		return model.DecodeError(d.ID, "pcm.data", "decoded %d samples but sample_count=%d", len(d.PCM.Samples), d.PCM.SampleCount)
	}
	if len(d.Phonemes) == 0 {
		return model.DecodeError(d.ID, "phonemes", "at least one phoneme is required")
	}
	if d.Params.TargetSamples <= 0 {
		return model.DecodeError(d.ID, "params.target_samples", "target duration must be positive")
	}
	if d.Params.MaxStretch <= 0 {
		return model.DecodeError(d.ID, "params.max_stretch", "stretch upper bound must be positive")
	}
	if d.Params.MinSilenceSamples < 0 {
		return model.DecodeError(d.ID, "params.min_silence_samples", "minimum silence hold must be non-negative")
	}
	if d.Params.StressToleranceSamples < 0 {
		return model.DecodeError(d.ID, "params.stress_tolerance_samples", "stress tolerance must be non-negative")
	}

	prevEnd := -1
	seenPhoneme := map[string]struct{}{}
	for i := range d.Phonemes {
		p := &d.Phonemes[i]
		field := "phonemes[" + itoa(i) + "]"
		if p.ID == "" {
			return model.DecodeError(d.ID, field+".id", "missing phoneme id")
		}
		if _, ok := seenPhoneme[p.ID]; ok {
			return model.DecodeError(d.ID, field+".id", "duplicate phoneme id %s", p.ID)
		}
		seenPhoneme[p.ID] = struct{}{}
		if p.Index != i {
			return model.DecodeError(d.ID, field+".index", "phoneme index=%d but slice position=%d", p.Index, i)
		}
		if !p.Category.Valid() {
			return model.DecodeError(d.ID, field+".category", "invalid category %q", p.Category)
		}
		if p.StartSample < 0 {
			return model.DecodeError(d.ID, field+".start_sample", "start must be non-negative")
		}
		if p.EndSample <= p.StartSample {
			return model.DecodeError(d.ID, field+".end_sample", "end must be greater than start")
		}
		if p.StartSample < prevEnd {
			return model.DecodeError(d.ID, field+".start_sample", "phoneme intervals overlap or are not strictly ordered: start=%d previous_end=%d", p.StartSample, prevEnd)
		}
		prevEnd = p.EndSample
		if p.EndSample > d.PCM.SampleCount {
			return model.DecodeError(d.ID, field+".end_sample", "end=%d exceeds sample_count=%d", p.EndSample, d.PCM.SampleCount)
		}
		if p.F0StepSamples <= 0 {
			return model.DecodeError(d.ID, field+".f0_step_samples", "f0 step must be positive")
		}
		expectedF0 := int(math.Ceil(float64(p.Duration()) / float64(p.F0StepSamples)))
		if len(p.F0) != expectedF0 {
			return model.DecodeError(d.ID, field+".f0", "f0 trajectory points=%d incompatible with interval=%d and step=%d; expected %d", len(p.F0), p.Duration(), p.F0StepSamples, expectedF0)
		}
		if math.IsNaN(p.LoudnessWeight) || math.IsInf(p.LoudnessWeight, 0) || p.LoudnessWeight < 0 {
			return model.DecodeError(d.ID, field+".loudness_weight", "loudness weight must be finite and non-negative")
		}
		for j, f := range p.F0 {
			if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
				return model.DecodeError(d.ID, field+".f0["+itoa(j)+"]", "f0 must be finite and non-negative")
			}
		}
	}
	return nil
}

func decodePCM(d *model.Dialogue) error {
	if len(d.PCM.Samples) == d.PCM.SampleCount && d.PCM.SampleCount > 0 {
		return nil
	}
	if d.PCM.Data == "" {
		return model.DecodeError(d.ID, "pcm.data", "missing base64 PCM data")
	}
	raw, err := base64.StdEncoding.DecodeString(d.PCM.Data)
	if err != nil {
		return model.DecodeError(d.ID, "pcm.data", "truncated or invalid base64 PCM: %v", err)
	}
	if len(raw)%2 != 0 {
		return model.DecodeError(d.ID, "pcm.data", "truncated s16le sample: %d bytes is odd", len(raw))
	}
	samples := make([]int16, len(raw)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2 : i*2+2]))
	}
	d.PCM.Samples = samples
	return nil
}

func ValidateTruthShape(t *model.TruthFile) error {
	if t == nil {
		return model.DecodeError("*", "truth", "nil truth file")
	}
	if t.SchemaVersion == "" {
		return model.DecodeError("*", "schema_version", "missing truth schema_version")
	}
	if len(t.Dialogues) == 0 {
		return model.DecodeError("*", "dialogues", "truth manifest contains no dialogues")
	}
	seen := map[string]struct{}{}
	for i := range t.Dialogues {
		d := &t.Dialogues[i]
		if d.DialogueID == "" {
			return model.DecodeError("*", "dialogues["+itoa(i)+"].dialogue_id", "missing dialogue_id")
		}
		if _, ok := seen[d.DialogueID]; ok {
			return model.DecodeError(d.DialogueID, "dialogue_id", "duplicate truth dialogue")
		}
		seen[d.DialogueID] = struct{}{}
		if d.SampleRate <= 0 {
			return model.DecodeError(d.DialogueID, "sample_rate", "must be positive")
		}
		if d.SampleCount <= 0 {
			return model.DecodeError(d.DialogueID, "sample_count", "must be positive")
		}
		if d.ContentHash == "" {
			return model.DecodeError(d.DialogueID, "content_hash", "missing content hash")
		}
		if d.Fingerprint == "" {
			return model.DecodeError(d.DialogueID, "fingerprint", "missing truth fingerprint")
		}
		if d.EnvelopeParams.FrameLength <= 0 || d.EnvelopeParams.FrameShift <= 0 || d.EnvelopeParams.SmoothWindow <= 0 {
			return model.DecodeError(d.DialogueID, "envelope_params", "frame length, shift and smooth window must be positive")
		}
		if d.FitParams.TargetSamples <= 0 || d.FitParams.MaxStretch <= 0 {
			return model.DecodeError(d.DialogueID, "fit_params", "target_samples and max_stretch must be positive")
		}
		expectedFrames := FrameCount(d.SampleCount, d.EnvelopeParams.FrameLength, d.EnvelopeParams.FrameShift)
		if len(d.FrameValues) != expectedFrames {
			return model.DecodeError(d.DialogueID, "frame_values", "frame values=%d expected=%d", len(d.FrameValues), expectedFrames)
		}
		for fi, v := range d.FrameValues {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1.000000000001 {
				return model.DecodeError(d.DialogueID, "frame_values["+itoa(fi)+"]", "frame value %v outside normalized domain", v)
			}
		}
	}
	return nil
}

func ValidateBundle(p *model.Project, t *model.TruthFile) error {
	if err := ValidateProject(p); err != nil {
		return err
	}
	if err := ValidateTruthShape(t); err != nil {
		return err
	}
	truthByID := map[string]*model.TruthDialogue{}
	for i := range t.Dialogues {
		truthByID[t.Dialogues[i].DialogueID] = &t.Dialogues[i]
	}
	for di := range p.Dialogues {
		d := &p.Dialogues[di]
		td, ok := truthByID[d.ID]
		if !ok {
			return model.DecodeError(d.ID, "truth.dialogues", "truth manifest missing dialogue")
		}
		if td.SampleRate != d.PCM.SampleRate {
			return model.DecodeError(d.ID, "truth.sample_rate", "truth=%d project=%d", td.SampleRate, d.PCM.SampleRate)
		}
		if td.SampleCount != d.PCM.SampleCount {
			return model.DecodeError(d.ID, "truth.sample_count", "truth=%d project=%d", td.SampleCount, d.PCM.SampleCount)
		}
		if td.AlgorithmVersion != "" && td.AlgorithmVersion != p.AlgorithmVersion {
			return model.DecodeError(d.ID, "truth.algorithm_version", "truth=%s project=%s", td.AlgorithmVersion, p.AlgorithmVersion)
		}
		if td.EnvelopeParams != p.Envelope {
			return model.DecodeError(d.ID, "truth.envelope_params", "truth and project envelope parameters differ")
		}
		if len(td.Stretched) != len(d.Phonemes) {
			return model.DecodeError(d.ID, "truth.stretched", "segments=%d phonemes=%d", len(td.Stretched), len(d.Phonemes))
		}
		for i, ph := range d.Phonemes {
			st := td.Stretched[i]
			if st.Index != i || st.PhonemeID != ph.ID {
				return model.DecodeError(d.ID, "truth.stretched["+itoa(i)+"]", "segment identity mismatch: got %s/%d", st.PhonemeID, st.Index)
			}
			if st.SourceStart != ph.StartSample || st.SourceEnd != ph.EndSample {
				return model.DecodeError(d.ID, "truth.stretched["+itoa(i)+"]", "source interval mismatch")
			}
			if st.StartSample < 0 || st.EndSample <= st.StartSample {
				return model.DecodeError(d.ID, "truth.stretched["+itoa(i)+"]", "invalid stretched interval")
			}
		}
		for _, sm := range td.Stress {
			if sm.Index < 0 || sm.Index >= len(d.Phonemes) || sm.PhonemeID != d.Phonemes[sm.Index].ID {
				return model.DecodeError(d.ID, "truth.stress", "invalid stress identity %s/%d", sm.PhonemeID, sm.Index)
			}
			if sm.Sample < 0 || sm.Sample > d.PCM.SampleCount {
				return model.DecodeError(d.ID, "truth.stress.sample", "sample %d out of bounds", sm.Sample)
			}
		}
	}
	return nil
}

func FrameCount(sampleCount, frameLength, frameShift int) int {
	if sampleCount < frameLength || frameLength <= 0 || frameShift <= 0 {
		return 0
	}
	return (sampleCount-frameLength)/frameShift + 1
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
