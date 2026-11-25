package schema

import (
	"encoding/json"
	"fmt"

	"prosodyalign/internal/model"
)

func Export(p *model.Project, target model.SchemaName) ([]byte, *Report, error) {
	switch target {
	case model.SchemaV1:
		return exportV1(p)
	case model.SchemaV2:
		return exportV2(p)
	case model.SchemaV3:
		return exportV3(p)
	default:
		return nil, &Report{Target: string(target)}, fmt.Errorf("unknown target schema %q", target)
	}
}

func exportV3(p *model.Project) ([]byte, *Report, error) {
	rep := &Report{Operation: "export", Source: p.SourceVersion, Target: V3Name}
	phs := make([]v3Phoneme, 0, len(p.Phonemes))
	for _, ph := range p.Phonemes {
		f0 := append([]model.F0Point(nil), ph.F0...)
		phs = append(phs, v3Phoneme{
			Index: ph.Index, ID: ph.ID, Range: model.Range{StartSample: ph.StartSample, EndSample: ph.EndSample},
			Kind: ph.Kind, Stretchable: ph.Stretchable, WeightPer10k: ph.WeightPer10k,
			F0: f0, MaxStretchSamples: ph.MaxStretchSamples,
		})
	}
	doc := v3File{
		Schema: V3Name, ID: p.ID, Seed: p.Seed, SampleRateHz: p.SampleRateHz,
		SampleCount: p.SampleCount, PCM: append([]int16(nil), p.PCM...),
		Timing: v3Timing{
			Basis: "samples", TargetDurationSamples: p.DeclaredTargetDuration,
			DefaultMaxStretchSamples: p.DeclaredMaxStretch,
			MinSilenceHoldSamples:    p.MinSilenceHoldSamples,
			AccentToleranceSamples:   p.AccentToleranceSamples,
		},
		Envelope: p.EnvelopeParams, Phonemes: phs,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	return data, rep, err
}

func exportV2(p *model.Project) ([]byte, *Report, error) {
	rep := &Report{Operation: "export_downgrade", Source: p.SourceVersion, Target: V2Name}
	targetMs, tr, err := SamplesToMillis(p.DeclaredTargetDuration, p.SampleRateHz, "$.targetDurationMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "$.targetDurationMs")
	maxMs, tr, err := SamplesToMillis(p.DeclaredMaxStretch, p.SampleRateHz, "$.maxStretchMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "$.maxStretchMs")
	silMs, tr, err := SamplesToMillis(p.MinSilenceHoldSamples, p.SampleRateHz, "$.minSilenceHoldMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "$.minSilenceHoldMs")
	tolMs, tr, err := SamplesToMillis(p.AccentToleranceSamples, p.SampleRateHz, "$.accentToleranceMs")
	if err != nil {
		return nil, rep, err
	}
	rep.addTruncation(tr, "$.accentToleranceMs")

	phs := make([]v2Phoneme, 0, len(p.Phonemes))
	for i, ph := range p.Phonemes {
		path := fmt.Sprintf("$.phonemes[%d]", i)
		startMs, tr, err := SamplesToMillis(ph.StartSample, p.SampleRateHz, path+".startMs")
		if err != nil {
			return nil, rep, err
		}
		rep.addTruncation(tr, path+".startMs")
		endMs, tr, err := SamplesToMillis(ph.EndSample, p.SampleRateHz, path+".endMs")
		if err != nil {
			return nil, rep, err
		}
		rep.addTruncation(tr, path+".endMs")
		f0 := make([]v2F0Point, 0, len(ph.F0))
		for j, q := range ph.F0 {
			offMs, tr, err := SamplesToMillis(q.OffsetSample, p.SampleRateHz, fmt.Sprintf("%s.f0Trajectory[%d].offsetMs", path, j))
			if err != nil {
				return nil, rep, err
			}
			rep.addTruncation(tr, fmt.Sprintf("%s.f0Trajectory[%d].offsetMs", path, j))
			f0 = append(f0, v2F0Point{OffsetMs: offMs, MilliHz: q.MilliHz})
		}
		var capMs *int64
		if ph.MaxStretchSamples != nil {
			v, tr, err := SamplesToMillis(*ph.MaxStretchSamples, p.SampleRateHz, path+".maxStretchMs")
			if err != nil {
				return nil, rep, err
			}
			rep.addTruncation(tr, path+".maxStretchMs")
			capMs = &v
		}
		weight := ph.WeightPer10k
		phs = append(phs, v2Phoneme{
			PhonemeID: ph.ID, StartMs: &startMs, EndMs: &endMs,
			Kind: stringPtr(string(ph.Kind)), Stretchable: ph.Stretchable,
			WeightPer10k: &weight, F0: f0, MaxStretchMs: capMs,
		})
	}
	doc := v2File{
		Schema: V2Name, DialogID: p.ID, Seed: p.Seed, SampleRateHz: p.SampleRateHz,
		SampleCount: p.SampleCount, PCM: append([]int16(nil), p.PCM...),
		TargetDurationMs: &targetMs, DefaultMaxStretchMs: &maxMs,
		MinSilenceHoldMs: &silMs, AccentToleranceMs: &tolMs,
		Envelope: &v2Envelope{
			FrameLengthSamples: p.EnvelopeParams.FrameLengthSamples,
			HopSamples:         p.EnvelopeParams.HopSamples,
			SmoothWindow:       p.EnvelopeParams.SmoothWindow,
			FixedPointScale:    p.EnvelopeParams.FixedPointScale,
		},
		Phonemes: phs,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	return data, rep, err
}

func exportV1(p *model.Project) ([]byte, *Report, error) {
	rep := &Report{Operation: "export_downgrade", Source: p.SourceVersion, Target: V1Name}
	rep.addNote(Note{
		Code: "E_LOSSY", Severity: "warning", FieldPath: "$.default_max_stretch_samples",
		SourceVersion: V1Name, OldValue: p.DeclaredMaxStretch,
		Disposition: "omit_native_field_and_preserve_in_x_default_max_stretch_samples",
		Reason:      "prosody.v1 has no native stretch upper-bound field",
	})
	exactDefaultCap := p.DeclaredMaxStretch

	phs := make([]v1Phoneme, 0, len(p.Phonemes))
	for i, ph := range p.Phonemes {
		path := fmt.Sprintf("$.phonemes[%d]", i)
		weightPermille, tr := Per10kToPermilleFloor(ph.WeightPer10k, path+".loudness_weight_permille")
		rep.addTruncation(tr, path+".loudness_weight_permille")
		if tr.Remainder != 0 {
			rep.addNote(Note{Code: "E_LOSSY", Severity: "warning", FieldPath: path + ".loudness_weight_permille", OldValue: ph.WeightPer10k, NewValue: weightPermille, Disposition: "floor_remainder_before_v1_scale_conversion"})
		}
		f0 := make([]v1F0Point, 0, len(ph.F0))
		for j, q := range ph.F0 {
			centi, tr := MilliHzToCentiHzFloor(q.MilliHz, fmt.Sprintf("%s.f0_centi_hz_trajectory[%d].f0_centi_hz", path, j))
			rep.addTruncation(tr, fmt.Sprintf("%s.f0[%d].f0_milli_hz", path, j))
			point := v1F0Point{OffsetSample: q.OffsetSample, CentiHz: &centi}
			if tr.Remainder != 0 {
				exact := q.MilliHz
				point.XMilliHz = &exact
				rep.addNote(Note{Code: "E_LOSSY", Severity: "warning", FieldPath: fmt.Sprintf("%s.f0_centi_hz_trajectory[%d].f0_centi_hz", path, j), OldValue: q.MilliHz, NewValue: centi, Disposition: "floor_to_centi_hz_and_preserve_exact_milli_hz_in_x_f0_milli_hz", Reason: "v1 uses centi-Hz fixed point"})
			}
			f0 = append(f0, point)
		}
		var xCap *int64
		if ph.MaxStretchSamples != nil {
			v := *ph.MaxStretchSamples
			xCap = &v
			rep.addNote(Note{Code: "E_LOSSY", Severity: "warning", FieldPath: path + ".max_stretch_samples", OldValue: v, Disposition: "preserve_in_x_max_stretch_samples", Reason: "prosody.v1 has no native stretch upper-bound field"})
		}
		start, end, class, weight := ph.StartSample, ph.EndSample, string(ph.Kind), weightPermille
		phs = append(phs, v1Phoneme{
			PhonemeID: ph.ID, StartSample: &start, EndSample: &end, Class: &class,
			Stretchable: ph.Stretchable, LoudnessWeightPermille: &weight, F0: f0,
			XMaxStretchSamples: xCap,
		})
	}

	doc := v1File{
		Schema: V1Name, DialogID: p.ID, Seed: p.Seed, SampleRateHz: p.SampleRateHz,
		SampleCount: p.SampleCount, PCM: append([]int16(nil), p.PCM...),
		TargetDurationSamples:     &p.DeclaredTargetDuration,
		XDefaultMaxStretchSamples: &exactDefaultCap,
		MinSilenceHoldSamples:     &p.MinSilenceHoldSamples,
		AccentToleranceSamples:    &p.AccentToleranceSamples,
		Envelope: &v1Envelope{
			FrameLengthSamples: p.EnvelopeParams.FrameLengthSamples,
			HopSamples:         p.EnvelopeParams.HopSamples,
			SmoothWindow:       p.EnvelopeParams.SmoothWindow,
			FixedPointScale:    p.EnvelopeParams.FixedPointScale,
		},
		Phonemes: phs,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	return data, rep, err
}

func stringPtr(s string) *string { return &s }
