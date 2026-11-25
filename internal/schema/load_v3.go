package schema

import (
	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

func loadV3(data []byte) (*LoadResult, error) {
	var doc V3Document
	if err := decodeStrict(data, &doc); err != nil {
		return nil, domain.SchemaError("$", "v3 decode failed: %v", err)
	}
	tr := &domain.ConversionTrace{SourceVersion: domain.SchemaV3, Direction: "load-v3-to-internal"}
	root := doc.PresentationProject
	if root.ID == "" {
		return nil, domain.SchemaError("presentationProject.id", "id is required")
	}
	if root.TimingProfile.SampleRateHz <= 0 {
		return nil, domain.SchemaError("presentationProject.timingProfile.sampleRateHz", "positive sampleRateHz is required")
	}
	if root.TimingProfile.FramesPerSecond <= 0 {
		return nil, domain.SchemaError("presentationProject.timingProfile.framesPerSecond", "positive framesPerSecond is required")
	}
	if root.TimingProfile.MinimumReadableFrames <= 0 {
		return nil, domain.SchemaError("presentationProject.timingProfile.minimumReadableFrames", "positive minimumReadableFrames is required")
	}
	if len(root.Dialogues) == 0 {
		return nil, domain.SchemaError("presentationProject.dialogues", "at least one dialogue is required")
	}
	if len(root.LipMapping.Categories) == 0 {
		return nil, domain.SchemaError("presentationProject.lipMapping.categories", "v3 lipMapping.categories is required")
	}
	mode := quant.NormalizeMode(root.TimingProfile.Quantization)
	if root.TimingProfile.Quantization == "" {
		mode = quant.RoundHalfUp
		addFill(tr, "presentationProject.timingProfile.quantization", string(mode), "declared-loader-default")
	}
	if root.TimingProfile.TransitionToleranceFrames <= 0 {
		root.TimingProfile.TransitionToleranceFrames = domainDefaultTolerance
		addFill(tr, "presentationProject.timingProfile.transitionToleranceFrames", domainDefaultTolerance, "declared-loader-default")
	}
	volume := root.OutputScales.DefaultVolume
	if volume <= 0 {
		volume = 7200
		addFill(tr, "presentationProject.outputScales.defaultVolume", volume, "declared-loader-default")
	}
	if root.OutputScales.WeightUnit != "permyriad" {
		addField(tr, "presentationProject.outputScales.weightUnit", root.OutputScales.WeightUnit, "weight_scale", "declared-unit", root.OutputScales.WeightUnit, quant.ScalePermyriad, false, 0)
	}

	categories := make([]domain.Category, 0, len(root.LipMapping.Categories))
	for i, c := range root.LipMapping.Categories {
		if c.CategoryID == "" {
			return nil, domain.SchemaError("presentationProject.lipMapping.categories["+itoa(i)+"].categoryId", "categoryId is required")
		}
		categories = append(categories, domain.Category{Code: c.CategoryID, Family: c.Family, Label: c.Label})
	}
	categories = categoriesOrStandard(categories)

	p := &domain.Project{
		SchemaVersion: domain.SchemaV3, ProjectID: root.ID, Seed: root.DeterministicSeed,
		SampleRateHz: root.TimingProfile.SampleRateHz, TargetFPS: root.TimingProfile.FramesPerSecond,
		SourceFPS:                 root.TimingProfile.FramesPerSecond,
		MinReadableFrames:         root.TimingProfile.MinimumReadableFrames,
		TransitionToleranceFrames: root.TimingProfile.TransitionToleranceFrames,
		RoundingMode:              string(mode), WeightScale: quant.ScalePermyriad, VolumeScale: quant.ScalePermyriad,
		DefaultVolume: volume, CategoryTable: categories,
	}

	for di, dialogue := range root.Dialogues {
		dpath := "presentationProject.dialogues[" + itoa(di) + "]"
		if dialogue.DialogueID == "" {
			return nil, domain.SchemaError(dpath+".dialogueId", "dialogueId is required")
		}
		if len(dialogue.Partitions) == 0 {
			return nil, domain.SchemaError(dpath+".partitions", "at least one partition is required")
		}
		d := domain.Dialogue{ID: dialogue.DialogueID, Speaker: dialogue.VoiceName}
		for pi, part := range dialogue.Partitions {
			path := dpath + ".partitions[" + itoa(pi) + "]"
			if part.PartitionID == "" {
				return nil, domain.SchemaError(path+".partitionId", "partitionId is required")
			}
			if err := validateTextWeights(path, part.SpokenText, len(part.Weights)); err != nil {
				return nil, err
			}
			if part.DurationSamples <= 0 {
				return nil, domain.SchemaError(path+".durationSamples", "positive durationSamples is required")
			}
			frames, err := framesFromSamples(path+".durationSamples", part.DurationSamples, p.SampleRateHz, p.TargetFPS, mode, tr)
			if err != nil {
				return nil, err
			}
			seg := domain.Segment{Index: pi, ID: part.PartitionID, Text: part.SpokenText, DurationSamples: part.DurationSamples, DurationFrames: frames}
			for wi, w := range part.Weights {
				if w.Grapheme == "" {
					return nil, domain.SchemaError(path+".weights["+itoa(wi)+"].grapheme", "grapheme is required")
				}
				seg.CharWeights = append(seg.CharWeights, w.WeightPermyriad)
			}
			for phi, ph := range part.Phonemes {
				pp := path + ".phonemes[" + itoa(phi) + "]"
				if ph.CategoryID == "" {
					mapped, ok := root.LipMapping.PhonemeToCategory[ph.Phoneme]
					if !ok {
						return nil, domain.SchemaError(pp+".categoryId", "categoryId or lipMapping.phonemeToCategory[%q] is required", ph.Phoneme)
					}
					ph.CategoryID = mapped
				}
				startFrame, err := framesFromSamples(pp+".startSample", ph.StartSample, p.SampleRateHz, p.TargetFPS, mode, tr)
				if err != nil {
					return nil, err
				}
				endFrame, err := framesFromSamples(pp+".endSample", ph.EndSample, p.SampleRateHz, p.TargetFPS, mode, tr)
				if err != nil {
					return nil, err
				}
				seg.Phonemes = append(seg.Phonemes, domain.Phoneme{
					Index: ph.Sequence, Symbol: ph.Phoneme, CategoryCode: ph.CategoryID,
					StartSample: ph.StartSample, EndSample: ph.EndSample, StartFrame: startFrame, EndFrame: endFrame,
				})
			}
			for _, tv := range part.Transitions {
				seg.TransitionOverrides = append(seg.TransitionOverrides, domain.TransitionOverride{
					AtFrame: tv.AtFrame, FromCategory: tv.FromCategoryID, ToCategory: tv.ToCategoryID,
					BridgeCategory: tv.BridgeCategoryID, OffsetFrames: tv.OffsetFrames, Reason: tv.Reason,
				})
			}
			d.Segments = append(d.Segments, seg)
		}
		p.Dialogues = append(p.Dialogues, d)
	}
	p.LoadTrace = tr
	return &LoadResult{Project: p, Trace: tr}, nil
}
