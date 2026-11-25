package schema

import (
	"encoding/json"
	"fmt"

	"prosodyalign/internal/errs"
	"prosodyalign/internal/model"
)

func loadV2(data []byte) (*model.Project, *Report, error) {
	source := V2Name
	var in v2File
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, nil, errs.Wrap(errs.ESchema, "invalid prosody.v2 JSON", err)
	}
	rep := &Report{Operation: "load_convert", Source: source, Target: V3Name}
	targetMs, err := requiredPtr(in.TargetDurationMs, source, "$.targetDurationMs")
	if err != nil {
		return nil, rep, err
	}
	maxMs, err := requiredPtr(in.DefaultMaxStretchMs, source, "$.maxStretchMs")
	if err != nil {
		return nil, rep, err
	}
	silMs, err := requiredPtr(in.MinSilenceHoldMs, source, "$.minSilenceHoldMs")
	if err != nil {
		return nil, rep, err
	}
	tolMs, err := requiredPtr(in.AccentToleranceMs, source, "$.accentToleranceMs")
	if err != nil {
		return nil, rep, err
	}
	if in.DialogID == "" {
		return nil, rep, errs.Schema(source, "$.dialogId", "required field is missing")
	}
	if in.SampleRateHz <= 0 {
		return nil, rep, errs.Schema(source, "$.sampleRateHz", "sampleRateHz is required and must be positive")
	}
	if in.PCM == nil {
		return nil, rep, errs.Schema(source, "$.pcm", "required field is missing")
	}

	target, tr, err := MillisToSamples(targetMs, in.SampleRateHz, "$.targetDurationMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "")
	maxSamples, tr, err := MillisToSamples(maxMs, in.SampleRateHz, "$.maxStretchMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "")
	sil, tr, err := MillisToSamples(silMs, in.SampleRateHz, "$.minSilenceHoldMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "")
	tol, tr, err := MillisToSamples(tolMs, in.SampleRateHz, "$.accentToleranceMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "")

	env := model.DefaultEnvelopeParams(in.SampleRateHz)
	if in.Envelope != nil {
		if in.Envelope.FrameLengthSamples != 0 {
			env.FrameLengthSamples = in.Envelope.FrameLengthSamples
		}
		if in.Envelope.HopSamples != 0 {
			env.HopSamples = in.Envelope.HopSamples
		}
		if in.Envelope.SmoothWindow != 0 {
			env.SmoothWindow = in.Envelope.SmoothWindow
		}
		if in.Envelope.FixedPointScale != 0 {
			env.FixedPointScale = in.Envelope.FixedPointScale
		}
	}

	phs := make([]model.Phoneme, 0, len(in.Phonemes))
	for i, p := range in.Phonemes {
		path := fmt.Sprintf("$.phonemes[%d]", i)
		startMs, err := requiredPtr(p.StartMs, source, path+".startMs")
		if err != nil {
			return nil, rep, err
		}
		endMs, err := requiredPtr(p.EndMs, source, path+".endMs")
		if err != nil {
			return nil, rep, err
		}
		kindText, err := requiredPtr(p.Kind, source, path+".kind")
		if err != nil {
			return nil, rep, err
		}
		weight, err := requiredPtr(p.WeightPer10k, source, path+".weightPer10k")
		if err != nil {
			return nil, rep, err
		}
		start, tr, err := MillisToSamples(startMs, in.SampleRateHz, path+".startMs")
		if err != nil {
			return nil, rep, err
		}
		rep.addTruncation(tr, "")
		end, tr, err := MillisToSamples(endMs, in.SampleRateHz, path+".endMs")
		if err != nil {
			return nil, rep, err
		}
		rep.addTruncation(tr, "")
		f0 := make([]model.F0Point, 0, len(p.F0))
		for j, q := range p.F0 {
			off, tr, err := MillisToSamples(q.OffsetMs, in.SampleRateHz, fmt.Sprintf("%s.f0Trajectory[%d].offsetMs", path, j))
			if err != nil {
				return nil, rep, err
			}
			rep.addTruncation(tr, "")
			mhz := q.MilliHz
			if q.XMilliHz != nil {
				mhz = *q.XMilliHz
			}
			f0 = append(f0, model.F0Point{OffsetSample: off, MilliHz: mhz})
		}
		var cap *int64
		if p.MaxStretchMs != nil {
			v, tr, err := MillisToSamples(*p.MaxStretchMs, in.SampleRateHz, path+".maxStretchMs")
			if err != nil {
				return nil, rep, err
			}
			rep.addTruncation(tr, path+".maxStretchMs")
			cap = &v
		}
		id := p.PhonemeID
		if id == "" {
			id = fmt.Sprintf("p%d", i)
		}
		phs = append(phs, model.Phoneme{
			Index: i, ID: id, StartSample: start, EndSample: end,
			Kind: model.PhonemeKind(kindText), Stretchable: p.Stretchable,
			WeightPer10k: weight, F0: f0, MaxStretchSamples: cap,
		})
	}

	p := &model.Project{
		SchemaVersion: model.SchemaV2, SourceVersion: source, LoadedFrom: "json:" + source,
		ID: in.DialogID, Seed: in.Seed, SampleRateHz: in.SampleRateHz, SampleCount: in.SampleCount, PCM: in.PCM,
		DeclaredTargetDuration: target, DeclaredMaxStretch: maxSamples,
		MinSilenceHoldSamples: sil, AccentToleranceSamples: tol,
		EnvelopeParams: env, Phonemes: phs,
	}
	if err := validateCommon(source, p); err != nil {
		return nil, rep, err
	}
	return p, rep, nil
}

func loadV3(data []byte) (*model.Project, *Report, error) {
	source := V3Name
	var in v3File
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, nil, errs.Wrap(errs.ESchema, "invalid prosody.v3 JSON", err)
	}
	rep := &Report{Operation: "load", Source: source, Target: V3Name}
	if in.Schema == "" {
		return nil, rep, errs.Schema(source, "$.schema", "required field is missing")
	}
	if in.ID == "" {
		return nil, rep, errs.Schema(source, "$.id", "required field is missing")
	}
	if in.SampleRateHz <= 0 {
		return nil, rep, errs.Schema(source, "$.sample_rate_hz", "sample_rate_hz is required and must be positive")
	}
	if in.PCM == nil {
		return nil, rep, errs.Schema(source, "$.pcm", "required field is missing")
	}
	if in.Timing.Basis != "samples" {
		return nil, rep, errs.Schema(source, "$.timing.basis", "v3 timing basis must be samples")
	}
	if in.Timing.TargetDurationSamples <= 0 {
		return nil, rep, errs.Schema(source, "$.timing.target_duration_samples", "required field is missing or non-positive")
	}

	phs := make([]model.Phoneme, 0, len(in.Phonemes))
	for i, p := range in.Phonemes {
		if p.ID == "" {
			return nil, rep, errs.Schema(source, fmt.Sprintf("$.phonemes[%d].id", i), "required field is missing")
		}
		phs = append(phs, model.Phoneme{
			Index: i, ID: p.ID, StartSample: p.Range.StartSample, EndSample: p.Range.EndSample,
			Kind: p.Kind, Stretchable: p.Stretchable, WeightPer10k: p.WeightPer10k,
			F0: p.F0, MaxStretchSamples: p.MaxStretchSamples,
		})
	}
	env := in.Envelope
	if env.FrameLengthSamples == 0 {
		env = model.DefaultEnvelopeParams(in.SampleRateHz)
		rep.addNote(Note{Code: "DEFAULT_FILLED", Severity: "info", FieldPath: "$.envelope", SourceVersion: source, OldValue: nil, DefaultValue: env, NewValue: env})
	}
	p := &model.Project{
		SchemaVersion: model.SchemaV3, SourceVersion: source, LoadedFrom: "json:" + source,
		ID: in.ID, Seed: in.Seed, SampleRateHz: in.SampleRateHz, SampleCount: in.SampleCount, PCM: in.PCM,
		DeclaredTargetDuration: in.Timing.TargetDurationSamples,
		DeclaredMaxStretch:     in.Timing.DefaultMaxStretchSamples,
		MinSilenceHoldSamples:  in.Timing.MinSilenceHoldSamples,
		AccentToleranceSamples: in.Timing.AccentToleranceSamples,
		EnvelopeParams:         env, Phonemes: phs,
	}
	if err := validateCommon(source, p); err != nil {
		return nil, rep, err
	}
	return p, rep, nil
}
