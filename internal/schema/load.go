package schema

import (
	"encoding/json"
	"fmt"

	"prosodyalign/internal/errs"
	"prosodyalign/internal/model"
)

type schemaHead struct {
	Schema string `json:"schema"`
}

func Load(data []byte) (*model.Project, *Report, error) {
	var head schemaHead
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, nil, errs.Wrap(errs.ESchema, "invalid JSON document", err)
	}
	switch head.Schema {
	case V1Name:
		return loadV1(data)
	case V2Name:
		return loadV2(data)
	case V3Name:
		return loadV3(data)
	case "":
		return nil, nil, errs.Schema("missing", "$.schema", "schema is required")
	default:
		return nil, nil, errs.Schema(head.Schema, "$.schema", "unknown schema version")
	}
}

func requiredPtr[T any](v *T, source, path string) (T, error) {
	var zero T
	if v == nil {
		return zero, errs.Schema(source, path, "required field is missing")
	}
	return *v, nil
}

func validateCommon(source string, p *model.Project) error {
	if p.ID == "" {
		return errs.Schema(source, "$.dialog_id", "dialog identifier is required")
	}
	if p.SampleRateHz <= 0 {
		return errs.Schema(source, "$.sample_rate_hz", "sample rate must be positive")
	}
	if len(p.PCM) == 0 {
		return errs.Schema(source, "$.pcm", "mono PCM description must not be empty")
	}
	if p.SampleCount == 0 {
		p.SampleCount = len(p.PCM)
	}
	if p.SampleCount != len(p.PCM) {
		return errs.Schema(source, "$.sample_count", "sample count %d does not match pcm length %d", p.SampleCount, len(p.PCM))
	}
	if len(p.Phonemes) == 0 {
		return errs.Schema(source, "$.phonemes", "at least one phoneme is required")
	}
	var lastEnd int64
	for i := range p.Phonemes {
		ph := &p.Phonemes[i]
		ph.Index = i
		path := fmt.Sprintf("$.phonemes[%d]", i)
		if ph.ID == "" {
			return errs.Schema(source, path+".phoneme_id", "phoneme id is required")
		}
		if ph.StartSample < 0 {
			return errs.Schema(source, path+".start_sample", "start must be non-negative")
		}
		if ph.EndSample <= ph.StartSample {
			return errs.Schema(source, path+".end_sample", "end must be greater than start")
		}
		if ph.StartSample < lastEnd {
			return errs.Schema(source, path+".start_sample", "phoneme intervals must be ordered and non-overlapping")
		}
		lastEnd = ph.EndSample
		switch ph.Kind {
		case model.Vowel, model.Consonant, model.Silence:
		default:
			return errs.Schema(source, path+".kind", "unknown phoneme class %q", ph.Kind)
		}
		if ph.WeightPer10k < 0 || ph.WeightPer10k > 10000 {
			return errs.Schema(source, path+".weight", "weight %d is outside [0,10000]", ph.WeightPer10k)
		}
	}
	if lastEnd > int64(p.SampleCount) {
		return errs.Schema(source, "$.phonemes", "last phoneme end %d exceeds pcm length %d", lastEnd, p.SampleCount)
	}
	if p.DeclaredTargetDuration <= 0 {
		return errs.Schema(source, "$.target_duration", "target duration must be positive")
	}
	if p.MinSilenceHoldSamples < 0 {
		return errs.Schema(source, "$.min_silence_hold_samples", "minimum silence hold must be non-negative")
	}
	if p.AccentToleranceSamples < 0 {
		return errs.Schema(source, "$.accent_tolerance_samples", "accent tolerance must be non-negative")
	}
	if p.EnvelopeParams.FixedPointScale <= 0 {
		p.EnvelopeParams.FixedPointScale = 1_000_000
	}
	p.ContentHash = model.CanonicalContentHash(p)
	return nil
}

func kindFromV1(s string) (model.PhonemeKind, error) {
	switch s {
	case "vowel", "V", "v":
		return model.Vowel, nil
	case "consonant", "C", "c":
		return model.Consonant, nil
	case "silence", "SIL", "sil":
		return model.Silence, nil
	default:
		return "", fmt.Errorf("unknown class %q", s)
	}
}

func loadV1(data []byte) (*model.Project, *Report, error) {
	source := V1Name
	var in v1File
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, nil, errs.Wrap(errs.ESchema, "invalid prosody.v1 JSON", err)
	}
	rep := &Report{Operation: "load_upgrade", Source: source, Target: V3Name}
	target, err := requiredPtr(in.TargetDurationSamples, source, "$.target_duration_samples")
	if err != nil {
		return nil, rep, err
	}
	minSil, err := requiredPtr(in.MinSilenceHoldSamples, source, "$.min_silence_hold_samples")
	if err != nil {
		return nil, rep, err
	}
	tol, err := requiredPtr(in.AccentToleranceSamples, source, "$.accent_tolerance_samples")
	if err != nil {
		return nil, rep, err
	}
	if in.DialogID == "" {
		return nil, rep, errs.Schema(source, "$.dialog_id", "required field is missing")
	}
	if in.SampleRateHz <= 0 {
		return nil, rep, errs.Schema(source, "$.sample_rate_hz", "sample_rate_hz is required and must be positive")
	}
	if in.PCM == nil {
		return nil, rep, errs.Schema(source, "$.pcm", "required field is missing")
	}

	env := model.DefaultEnvelopeParams(in.SampleRateHz)
	if in.Envelope == nil || in.Envelope.FrameLengthSamples == 0 {
		rep.addNote(Note{Code: "DEFAULT_FILLED", Severity: "info", FieldPath: "$.envelope.frame_length_samples", SourceVersion: source, OldValue: nil, NewValue: env.FrameLengthSamples, DefaultValue: env.FrameLengthSamples, Reason: "v1 optional envelope field absent"})
	} else {
		env.FrameLengthSamples = in.Envelope.FrameLengthSamples
	}
	if in.Envelope == nil || in.Envelope.HopSamples == 0 {
		rep.addNote(Note{Code: "DEFAULT_FILLED", Severity: "info", FieldPath: "$.envelope.hop_samples", SourceVersion: source, OldValue: nil, NewValue: env.HopSamples, DefaultValue: env.HopSamples})
	} else {
		env.HopSamples = in.Envelope.HopSamples
	}
	if in.Envelope == nil || in.Envelope.SmoothWindow == 0 {
		rep.addNote(Note{Code: "DEFAULT_FILLED", Severity: "info", FieldPath: "$.envelope.smooth_window", SourceVersion: source, OldValue: nil, NewValue: env.SmoothWindow, DefaultValue: env.SmoothWindow})
	} else {
		env.SmoothWindow = in.Envelope.SmoothWindow
	}
	if in.Envelope != nil && in.Envelope.FixedPointScale != 0 {
		env.FixedPointScale = in.Envelope.FixedPointScale
	}

	defaultCap := int64(in.SampleRateHz / 10)
	if in.DefaultMaxStretchSamples != nil {
		defaultCap = *in.DefaultMaxStretchSamples
	} else if in.XDefaultMaxStretchSamples != nil {
		defaultCap = *in.XDefaultMaxStretchSamples
		rep.addNote(Note{Code: "EXTENSION_RESTORED", Severity: "warning", FieldPath: "$.x_default_max_stretch_samples", SourceVersion: source, NewValue: defaultCap, Disposition: "restore loss-preserving extension"})
	} else {
		rep.addNote(Note{Code: "DEFAULT_FILLED", Severity: "warning", FieldPath: "$.default_max_stretch_samples", SourceVersion: source, OldValue: nil, NewValue: defaultCap, DefaultValue: defaultCap, Reason: "v1 has no native stretch upper-bound field"})
	}

	phs := make([]model.Phoneme, 0, len(in.Phonemes))
	for i, p := range in.Phonemes {
		path := fmt.Sprintf("$.phonemes[%d]", i)
		start, err := requiredPtr(p.StartSample, source, path+".start_sample")
		if err != nil {
			return nil, rep, err
		}
		end, err := requiredPtr(p.EndSample, source, path+".end_sample")
		if err != nil {
			return nil, rep, err
		}
		class, err := requiredPtr(p.Class, source, path+".class")
		if err != nil {
			return nil, rep, err
		}
		kind, err := kindFromV1(class)
		if err != nil {
			return nil, rep, errs.Schema(source, path+".class", err.Error())
		}
		weightPermille, err := requiredPtr(p.LoudnessWeightPermille, source, path+".loudness_weight_permille")
		if err != nil {
			return nil, rep, err
		}
		if weightPermille < 0 || weightPermille > 1000 {
			return nil, rep, errs.Schema(source, path+".loudness_weight_permille", "weight must be in [0,1000]")
		}
		f0 := make([]model.F0Point, 0, len(p.F0))
		for j, q := range p.F0 {
			var mhz int64
			if q.XMilliHz != nil {
				mhz = *q.XMilliHz
			} else {
				chz, err := requiredPtr(q.CentiHz, source, fmt.Sprintf("%s.f0_centi_hz_trajectory[%d].f0_centi_hz", path, j))
				if err != nil {
					return nil, rep, err
				}
				mhz, err = CentiHzToMilliHz(chz, fmt.Sprintf("%s.f0[%d].f0_centi_hz", path, j))
				if err != nil {
					return nil, rep, err
				}
			}
			f0 = append(f0, model.F0Point{OffsetSample: q.OffsetSample, MilliHz: mhz})
		}
		var cap *int64
		if p.XMaxStretchSamples != nil {
			v := *p.XMaxStretchSamples
			cap = &v
		}
		id := p.PhonemeID
		if id == "" {
			id = fmt.Sprintf("p%d", i)
		}
		phs = append(phs, model.Phoneme{
			Index: i, ID: id, StartSample: start, EndSample: end, Kind: kind,
			Stretchable: p.Stretchable, WeightPer10k: PermilleToPer10k(weightPermille),
			F0: f0, MaxStretchSamples: cap,
		})
	}

	project := &model.Project{
		SchemaVersion: model.SchemaV1, SourceVersion: source, LoadedFrom: "json:" + source,
		ID: in.DialogID, Seed: in.Seed, SampleRateHz: in.SampleRateHz, SampleCount: in.SampleCount, PCM: in.PCM,
		DeclaredTargetDuration: target, DeclaredMaxStretch: defaultCap,
		MinSilenceHoldSamples: minSil, AccentToleranceSamples: tol,
		EnvelopeParams: env, Phonemes: phs,
	}
	if err := validateCommon(source, project); err != nil {
		return nil, rep, err
	}
	return project, rep, nil
}
