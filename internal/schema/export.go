package schema

import (
	"encoding/json"

	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

func Export(version string, p *domain.Project) (*ExportResult, error) {
	switch version {
	case domain.SchemaV1:
		return exportV1(p)
	case domain.SchemaV2:
		return exportV2(p)
	case domain.SchemaV3:
		return exportV3(p)
	default:
		return nil, domain.SchemaError("schemaVersion", "cannot export unknown version %q", version)
	}
}

func exportV1(p *domain.Project) (*ExportResult, error) {
	tr := &domain.ConversionTrace{SourceVersion: p.SchemaVersion, TargetVersion: domain.SchemaV1, Direction: "export-internal-to-v1"}
	doc := V1Document{
		SchemaName: "a11y.accessibility-plan", SchemaVersion: domain.SchemaV1,
		ProjectID: p.ProjectID, FixedSeed: p.Seed,
		AudioFormat:    &V1Audio{SampleRateHz: p.SampleRateHz},
		RenderProfile:  &V1Render{FramesPerSecond: p.TargetFPS, RoundingDirection: p.RoundingMode},
		VisemeCodebook: v1CodebookFromCategories(p.CategoryTable),
	}
	min := p.MinReadableFrames
	doc.RenderProfile.MinimumReadableFrames = &min
	vol := p.DefaultVolume / 10
	doc.DefaultVolumePermille = &vol

	tr.Lossy = append(tr.Lossy, domain.LossyIssue{
		Code: domain.CodeLossy, Path: "render_profile.transition_tolerance_frames", Field: "transition_tolerance_frames",
		Value: p.TransitionToleranceFrames, Reason: "schema v1 has no transition tolerance field",
		Disposition: "omit_and_fill_declared_default_1_on_load; automatic bridge remains deterministic",
	})
	tr.Lossy = append(tr.Lossy, domain.LossyIssue{
		Code: domain.CodeLossy, Path: "viseme_codebook", Field: "lip_category_mapping",
		Reason:      "grouped/phoneme mapping organization is flattened to numeric viseme_id table",
		Disposition: "encode standard_code in codebook so loader restores the same category family",
	})

	for di, d := range p.Dialogues {
		cue := V1Cue{CueID: d.ID, SpeakerName: d.Speaker}
		for si, s := range d.Segments {
			dur, err := secondsFromSamples("duration_samples", s.DurationSamples, p.SampleRateHz)
			if err != nil {
				return nil, err
			}
			line := V1Line{LineID: s.ID, TextContent: s.Text, DurationSeconds: dur}
			runes := []rune(s.Text)
			for wi, w := range s.CharWeights {
				v, rem, err := quant.ScaleFixedPoint("weight", w, quant.ScalePermyriad, quant.ScalePermille, quant.RoundFloor)
				if err != nil {
					return nil, err
				}
				if rem.Remainder != 0 {
					addField(tr, "dialogues["+itoa(di)+"].segments["+itoa(si)+"].weights", "weightPermyriad", "weight_permille", "floor_scale", w, v, true, rem.Remainder)
				}
				char := ""
				if wi < len(runes) {
					char = string(runes[wi])
				}
				line.CharacterWeights = append(line.CharacterWeights, V1WeightPermille{Character: char, WeightPermille: v})
			}
			track := &V1PhonemeTrack{Encoding: "numeric_viseme", Phonemes: []V1Phoneme{}}
			for _, ph := range s.Phonemes {
				start, err := secondsFromSamples("start_sample", ph.StartSample, p.SampleRateHz)
				if err != nil {
					return nil, err
				}
				end, err := secondsFromSamples("end_sample", ph.EndSample, p.SampleRateHz)
				if err != nil {
					return nil, err
				}
				id, ok := v1CodeToID[ph.CategoryCode]
				if !ok {
					id = 200 + stableCodeIndex(ph.CategoryCode)
				}
				track.Phonemes = append(track.Phonemes, V1Phoneme{OrderIndex: ph.Index, Symbol: ph.Symbol, VisemeID: id, StartSeconds: start, EndSeconds: end})
			}
			line.PhonemeTrack = track
			if len(s.TransitionOverrides) > 0 {
				tr.Lossy = append(tr.Lossy, domain.LossyIssue{
					Code: domain.CodeLossy, Path: "dialogue_cues[].lines[].transitions", Field: "transition_overrides",
					Value: s.TransitionOverrides, Reason: "schema v1 cannot express explicit transition overrides",
					Disposition: "omit; deterministic boundary rule regenerates the same bridge frame for standard overrides",
				})
			}
			cue.Lines = append(cue.Lines, line)
		}
		doc.DialogueCues = append(doc.DialogueCues, cue)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: domain.SchemaV1, Data: b, Trace: tr, Lossy: tr.Lossy}, nil
}

func exportV2(p *domain.Project) (*ExportResult, error) {
	tr := &domain.ConversionTrace{SourceVersion: p.SchemaVersion, TargetVersion: domain.SchemaV2, Direction: "export-internal-to-v2"}
	tol := p.TransitionToleranceFrames
	min := p.MinReadableFrames
	vol := p.DefaultVolume
	doc := V2Document{
		SchemaVersion: domain.SchemaV2, ProjectID: p.ProjectID, Seed: p.Seed,
		Media:           &V2Media{SampleRateHz: p.SampleRateHz},
		Timing:          &V2Timing{FrameRate: p.TargetFPS, MinimumReadableFrames: &min, Rounding: p.RoundingMode, TransitionToleranceFrames: &tol},
		DefaultVolumeBp: &vol,
		MouthTable:      &V2MouthTable{Encoding: "grouped-family/v2", Groups: groupsFromCategories(p.CategoryTable)},
	}
	tr.Lossy = append(tr.Lossy, domain.LossyIssue{
		Code: domain.CodeLossy, Path: "mouthTable", Field: "lip_category_mapping",
		Reason:      "flat/v3 phonemeToCategory mapping is regrouped into family shape IDs",
		Disposition: "reversible via standardCode in every group shape",
	})
	for _, d := range p.Dialogues {
		dtoD := V2Dialogue{ID: d.ID, SpeakerName: d.Speaker}
		for _, s := range d.Segments {
			ms, err := msFromSamples("duration_samples", s.DurationSamples, p.SampleRateHz)
			if err != nil {
				return nil, err
			}
			frag := V2Fragment{FragmentID: s.ID, Text: s.Text, DurationMs: ms}
			runes := []rune(s.Text)
			for i, w := range s.CharWeights {
				g := ""
				if i < len(runes) {
					g = string(runes[i])
				}
				frag.CharacterWeightsBp = append(frag.CharacterWeightsBp, V2WeightBp{Grapheme: g, WeightBp: w})
			}
			for _, ph := range s.Phonemes {
				sms, err := msFromSamples("start_sample", ph.StartSample, p.SampleRateHz)
				if err != nil {
					return nil, err
				}
				ems, err := msFromSamples("end_sample", ph.EndSample, p.SampleRateHz)
				if err != nil {
					return nil, err
				}
				frag.Phonemes = append(frag.Phonemes, V2Phoneme{Order: ph.Index, Symbol: ph.Symbol, MouthShapeID: "SHAPE_" + ph.CategoryCode, StartMs: sms, EndMs: ems})
			}
			if len(s.TransitionOverrides) > 0 {
				tr.Lossy = append(tr.Lossy, domain.LossyIssue{
					Code: domain.CodeLossy, Path: "dialogues[].fragments[].transitions", Field: "transition_overrides",
					Value: s.TransitionOverrides, Reason: "schema v2 only carries tolerance, not explicit transition records",
					Disposition: "omit explicit records; deterministic rule reconstructs identical standard bridges",
				})
			}
			dtoD.Fragments = append(dtoD.Fragments, frag)
		}
		doc.Dialogues = append(doc.Dialogues, dtoD)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: domain.SchemaV2, Data: b, Trace: tr, Lossy: tr.Lossy}, nil
}

func exportV3(p *domain.Project) (*ExportResult, error) {
	tr := &domain.ConversionTrace{SourceVersion: p.SchemaVersion, TargetVersion: domain.SchemaV3, Direction: "export-internal-to-v3"}
	doc := V3Document{SchemaVersion: domain.SchemaV3}
	root := V3Project{
		ID: p.ProjectID, DeterministicSeed: p.Seed,
		TimingProfile: V3TimingProfile{
			SampleRateHz: p.SampleRateHz, FramesPerSecond: p.TargetFPS,
			MinimumReadableFrames: p.MinReadableFrames, Quantization: p.RoundingMode,
			TransitionToleranceFrames: p.TransitionToleranceFrames,
		},
		OutputScales: V3Scales{WeightUnit: "permyriad", VolumeUnit: "permyriad", DefaultVolume: p.DefaultVolume},
		LipMapping:   V3LipMapping{Encoding: "phoneme-to-category/v3", PhonemeToCategory: map[string]string{}},
	}
	for _, c := range p.CategoryTable {
		root.LipMapping.Categories = append(root.LipMapping.Categories, V3Category{CategoryID: c.Code, Family: c.Family, Label: c.Label})
	}
	for _, d := range p.Dialogues {
		dd := V3Dialogue{DialogueID: d.ID, VoiceName: d.Speaker}
		for _, s := range d.Segments {
			dms, err := msFromSamples("duration_samples", s.DurationSamples, p.SampleRateHz)
			if err != nil {
				return nil, err
			}
			part := V3Partition{PartitionID: s.ID, SpokenText: s.Text, DurationMs: dms, DurationSamples: s.DurationSamples}
			runes := []rune(s.Text)
			for i, w := range s.CharWeights {
				g := ""
				if i < len(runes) {
					g = string(runes[i])
				}
				part.Weights = append(part.Weights, V3Weight{Grapheme: g, WeightPermyriad: w})
			}
			for _, ph := range s.Phonemes {
				sms, err := msFromSamples("start_sample", ph.StartSample, p.SampleRateHz)
				if err != nil {
					return nil, err
				}
				ems, err := msFromSamples("end_sample", ph.EndSample, p.SampleRateHz)
				if err != nil {
					return nil, err
				}
				part.Phonemes = append(part.Phonemes, V3Phoneme{
					Sequence: ph.Index, Phoneme: ph.Symbol, CategoryID: ph.CategoryCode,
					StartSample: ph.StartSample, EndSample: ph.EndSample, StartMs: sms, EndMs: ems,
				})
				root.LipMapping.PhonemeToCategory[ph.Symbol] = ph.CategoryCode
			}
			for _, ov := range s.TransitionOverrides {
				part.Transitions = append(part.Transitions, V3Transition{
					AtFrame: ov.AtFrame, FromCategoryID: ov.FromCategory, ToCategoryID: ov.ToCategory,
					BridgeCategoryID: ov.BridgeCategory, OffsetFrames: ov.OffsetFrames, Reason: ov.Reason,
				})
			}
			dd.Partitions = append(dd.Partitions, part)
		}
		root.Dialogues = append(root.Dialogues, dd)
	}
	doc.PresentationProject = root
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: domain.SchemaV3, Data: b, Trace: tr}, nil
}

func v1CodebookFromCategories(categories []domain.Category) []V1Viseme {
	out := []V1Viseme{}
	for _, c := range categories {
		id, ok := v1CodeToID[c.Code]
		if !ok {
			id = 200 + stableCodeIndex(c.Code)
		}
		out = append(out, V1Viseme{VisemeID: id, StandardCode: c.Code, Family: c.Family, Label: c.Label})
	}
	return out
}

func groupsFromCategories(categories []domain.Category) []V2MouthGroup {
	order := []string{}
	byFamily := map[string][]V2Shape{}
	for _, c := range categories {
		if _, ok := byFamily[c.Family]; !ok {
			order = append(order, c.Family)
		}
		byFamily[c.Family] = append(byFamily[c.Family], V2Shape{ShapeID: "SHAPE_" + c.Code, StandardCode: c.Code, Label: c.Label})
	}
	out := []V2MouthGroup{}
	for _, family := range order {
		out = append(out, V2MouthGroup{FamilyName: family, Shapes: byFamily[family]})
	}
	return out
}

func stableCodeIndex(code string) int {
	sum := 0
	for _, r := range code {
		sum = (sum*31 + int(r)) % 90
	}
	return sum
}
