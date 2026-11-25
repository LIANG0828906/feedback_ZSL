package schema

import (
	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

func loadV2(data []byte) (*LoadResult, error) {
	var doc V2Document
	if err := decodeStrict(data, &doc); err != nil {
		return nil, domain.SchemaError("$", "v2 decode failed: %v", err)
	}
	tr := &domain.ConversionTrace{SourceVersion: domain.SchemaV2, Direction: "load-v2-to-internal"}
	if doc.ProjectID == "" {
		return nil, domain.SchemaError("projectId", "projectId is required")
	}
	if doc.Media == nil || doc.Media.SampleRateHz <= 0 {
		return nil, domain.SchemaError("media.sampleRateHz", "positive sampleRateHz is required")
	}
	if doc.Timing == nil || doc.Timing.FrameRate <= 0 {
		return nil, domain.SchemaError("timing.frameRate", "positive frameRate is required")
	}
	if doc.MouthTable == nil || len(doc.MouthTable.Groups) == 0 {
		return nil, domain.SchemaError("mouthTable.groups", "v2 grouped mouth table is required")
	}
	if len(doc.Dialogues) == 0 {
		return nil, domain.SchemaError("dialogues", "at least one dialogue is required")
	}

	mode := quant.NormalizeMode(doc.Timing.Rounding)
	if doc.Timing.Rounding == "" {
		mode = quant.RoundHalfUp
		addFill(tr, "timing.rounding", string(mode), "declared-loader-default")
	}
	minReadable := domainDefaultMinReadable
	if doc.Timing.MinimumReadableFrames == nil {
		addFill(tr, "timing.minimumReadableFrames", minReadable, "declared-loader-default")
	} else {
		minReadable = *doc.Timing.MinimumReadableFrames
		if minReadable <= 0 {
			return nil, domain.SchemaError("timing.minimumReadableFrames", "must be positive")
		}
	}
	tolerance := domainDefaultTolerance
	if doc.Timing.TransitionToleranceFrames == nil {
		addFill(tr, "timing.transitionToleranceFrames", tolerance, "v2-optional-field-absent-declared-default")
	} else {
		tolerance = *doc.Timing.TransitionToleranceFrames
	}
	volume := 7200
	if doc.DefaultVolumeBp == nil {
		addFill(tr, "defaultVolumeBp", volume, "declared-loader-default")
	} else {
		volume = *doc.DefaultVolumeBp
		addField(tr, "defaultVolumeBp", "defaultVolumeBp", "defaultVolume", "permyriad_direct", volume, volume, false, 0)
	}

	shapeToCode := map[string]string{}
	categories := []domain.Category{}
	for gi, group := range doc.MouthTable.Groups {
		for si, shape := range group.Shapes {
			if shape.StandardCode == "" || shape.ShapeID == "" {
				return nil, domain.SchemaError("mouthTable.groups["+itoa(gi)+"].shapes["+itoa(si)+"]", "shapeId and standardCode are required")
			}
			shapeToCode[shape.ShapeID] = shape.StandardCode
			categories = append(categories, domain.Category{Code: shape.StandardCode, Family: group.FamilyName, Label: shape.Label})
		}
		addField(tr, "mouthTable.groups["+itoa(gi)+"]", "groups.familyName.shapes", "category_table", "grouped_family_to_flat_internal", group.FamilyName, len(group.Shapes), false, 0)
	}
	categories = categoriesOrStandard(categories)

	p := &domain.Project{
		SchemaVersion: domain.SchemaV2, ProjectID: doc.ProjectID, Seed: doc.Seed,
		SampleRateHz: doc.Media.SampleRateHz, TargetFPS: doc.Timing.FrameRate,
		SourceFPS:         doc.Timing.FrameRate,
		MinReadableFrames: minReadable, TransitionToleranceFrames: tolerance,
		RoundingMode: string(mode), WeightScale: quant.ScalePermyriad, VolumeScale: quant.ScalePermyriad,
		DefaultVolume: volume, CategoryTable: categories,
	}

	for di, dialogue := range doc.Dialogues {
		dpath := "dialogues[" + itoa(di) + "]"
		if dialogue.ID == "" {
			return nil, domain.SchemaError(dpath+".id", "dialogue id is required")
		}
		if len(dialogue.Fragments) == 0 {
			return nil, domain.SchemaError(dpath+".fragments", "at least one fragment is required")
		}
		d := domain.Dialogue{ID: dialogue.ID, Speaker: dialogue.SpeakerName}
		for fi, frag := range dialogue.Fragments {
			path := dpath + ".fragments[" + itoa(fi) + "]"
			if frag.FragmentID == "" {
				return nil, domain.SchemaError(path+".fragmentId", "fragmentId is required")
			}
			if err := validateTextWeights(path, frag.Text, len(frag.CharacterWeightsBp)); err != nil {
				return nil, err
			}
			if frag.DurationMs <= 0 {
				return nil, domain.SchemaError(path+".durationMs", "positive durationMs is required")
			}
			if len(frag.Phonemes) == 0 {
				return nil, domain.SchemaError(path+".phonemes", "at least one phoneme is required")
			}
			samples, rem, err := quant.MsToSamples(path+".durationMs", frag.DurationMs, p.SampleRateHz, mode)
			if err != nil {
				return nil, err
			}
			if rem.Remainder != 0 {
				tr.Remainders = append(tr.Remainders, rem)
			}
			frames, err := framesFromSamples(path+".durationMs", samples, p.SampleRateHz, p.TargetFPS, mode, tr)
			if err != nil {
				return nil, err
			}
			seg := domain.Segment{Index: fi, ID: frag.FragmentID, Text: frag.Text, DurationSamples: samples, DurationFrames: frames}
			for wi, w := range frag.CharacterWeightsBp {
				if w.Grapheme == "" {
					return nil, domain.SchemaError(path+".characterWeightsBp["+itoa(wi)+"].grapheme", "grapheme is required")
				}
				seg.CharWeights = append(seg.CharWeights, w.WeightBp)
				addField(tr, path+".characterWeightsBp["+itoa(wi)+"]", "weightBp", "char_weights", "permyriad_direct", w.WeightBp, w.WeightBp, false, 0)
			}
			fragTolerance := tolerance
			if frag.TransitionToleranceFrames != nil {
				fragTolerance = *frag.TransitionToleranceFrames
			}
			_ = fragTolerance
			for pi, ph := range frag.Phonemes {
				pp := path + ".phonemes[" + itoa(pi) + "]"
				if ph.MouthShapeID == "" {
					return nil, domain.SchemaError(pp+".mouthShapeId", "mouthShapeId is required")
				}
				code, ok := shapeToCode[ph.MouthShapeID]
				if !ok {
					code = ph.MouthShapeID
				}
				startSample, srem, err := quant.MsToSamples(pp+".startMs", ph.StartMs, p.SampleRateHz, mode)
				if err != nil {
					return nil, err
				}
				endSample, erem, err := quant.MsToSamples(pp+".endMs", ph.EndMs, p.SampleRateHz, mode)
				if err != nil {
					return nil, err
				}
				if srem.Remainder != 0 {
					tr.Remainders = append(tr.Remainders, srem)
				}
				if erem.Remainder != 0 {
					tr.Remainders = append(tr.Remainders, erem)
				}
				startFrame, err := framesFromSamples(pp+".startMs", startSample, p.SampleRateHz, p.TargetFPS, mode, tr)
				if err != nil {
					return nil, err
				}
				endFrame, err := framesFromSamples(pp+".endMs", endSample, p.SampleRateHz, p.TargetFPS, mode, tr)
				if err != nil {
					return nil, err
				}
				seg.Phonemes = append(seg.Phonemes, domain.Phoneme{
					Index: ph.Order, Symbol: ph.Symbol, CategoryCode: code,
					StartSample: startSample, EndSample: endSample, StartFrame: startFrame, EndFrame: endFrame,
				})
			}
			d.Segments = append(d.Segments, seg)
		}
		p.Dialogues = append(p.Dialogues, d)
	}
	p.LoadTrace = tr
	return &LoadResult{Project: p, Trace: tr}, nil
}
