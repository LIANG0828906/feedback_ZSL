package schema

import (
	"strconv"

	"presentkernel/internal/domain"
	"presentkernel/internal/quant"
)

func loadV1(data []byte) (*LoadResult, error) {
	var doc V1Document
	if err := decodeStrict(data, &doc); err != nil {
		return nil, domain.SchemaError("$", "v1 decode failed: %v", err)
	}
	tr := &domain.ConversionTrace{SourceVersion: domain.SchemaV1, Direction: "load-v1-to-internal"}
	if doc.ProjectID == "" {
		return nil, domain.SchemaError("project_id", "project_id is required")
	}
	if doc.AudioFormat == nil || doc.AudioFormat.SampleRateHz <= 0 {
		return nil, domain.SchemaError("audio_format.sample_rate_hz", "positive sample_rate_hz is required")
	}
	if doc.RenderProfile == nil || doc.RenderProfile.FramesPerSecond <= 0 {
		return nil, domain.SchemaError("render_profile.frames_per_second", "positive frames_per_second is required")
	}
	if len(doc.DialogueCues) == 0 {
		return nil, domain.SchemaError("dialogue_cues", "at least one dialogue cue is required")
	}
	if len(doc.VisemeCodebook) == 0 {
		return nil, domain.SchemaError("viseme_codebook", "v1 flat viseme_codebook is required")
	}

	mode := quant.NormalizeMode(doc.RenderProfile.RoundingDirection)
	if doc.RenderProfile.RoundingDirection == "" {
		mode = quant.RoundHalfUp
		addFill(tr, "render_profile.rounding_direction", string(mode), "declared-loader-default")
	}
	minReadable := domainDefaultMinReadable
	if doc.RenderProfile.MinimumReadableFrames == nil {
		addFill(tr, "render_profile.minimum_readable_frames", minReadable, "declared-loader-default")
	} else {
		minReadable = *doc.RenderProfile.MinimumReadableFrames
		if minReadable <= 0 {
			return nil, domain.SchemaError("render_profile.minimum_readable_frames", "must be positive")
		}
	}
	volume := 7200
	if doc.DefaultVolumePermille == nil {
		addFill(tr, "default_volume_permille", 720, "declared-loader-default")
	} else {
		v, _, err := quant.ScaleFixedPoint("default_volume_permille", *doc.DefaultVolumePermille, quant.ScalePermille, quant.ScalePermyriad, quant.RoundFloor)
		if err != nil {
			return nil, err
		}
		volume = v
		addField(tr, "default_volume_permille", "default_volume_permille", "default_volume", "permille_to_permyriad", *doc.DefaultVolumePermille, volume, false, 0)
	}
	addFill(tr, "render_profile.transition_tolerance_frames", domainDefaultTolerance, "v1-unsupported-field-declared-default")

	idToCode := map[int]string{}
	categories := make([]domain.Category, 0, len(doc.VisemeCodebook))
	for i, v := range doc.VisemeCodebook {
		if v.StandardCode == "" {
			return nil, domain.SchemaError("viseme_codebook["+itoa(i)+"].standard_code", "standard_code is required")
		}
		idToCode[v.VisemeID] = v.StandardCode
		categories = append(categories, domain.Category{Code: v.StandardCode, Family: v.Family, Label: v.Label})
		addField(tr, "viseme_codebook["+itoa(i)+"]", "viseme_id/family", "category_table", "flat_numeric_to_standard", v.VisemeID, v.StandardCode, false, 0)
	}
	categories = categoriesOrStandard(categories)

	p := &domain.Project{
		SchemaVersion: domain.SchemaV1, ProjectID: doc.ProjectID, Seed: doc.FixedSeed,
		SampleRateHz: doc.AudioFormat.SampleRateHz, TargetFPS: doc.RenderProfile.FramesPerSecond,
		SourceFPS:         doc.RenderProfile.FramesPerSecond,
		MinReadableFrames: minReadable, TransitionToleranceFrames: domainDefaultTolerance,
		RoundingMode: string(mode), WeightScale: quant.ScalePermyriad, VolumeScale: quant.ScalePermyriad,
		DefaultVolume: volume, CategoryTable: categories,
	}

	for ci, cue := range doc.DialogueCues {
		cuePath := "dialogue_cues[" + itoa(ci) + "]"
		if cue.CueID == "" {
			return nil, domain.SchemaError(cuePath+".cue_id", "cue_id is required")
		}
		if len(cue.Lines) == 0 {
			return nil, domain.SchemaError(cuePath+".lines", "at least one line is required")
		}
		d := domain.Dialogue{ID: cue.CueID, Speaker: cue.SpeakerName}
		for li, line := range cue.Lines {
			path := cuePath + ".lines[" + itoa(li) + "]"
			if line.LineID == "" {
				return nil, domain.SchemaError(path+".line_id", "line_id is required")
			}
			if err := validateTextWeights(path, line.TextContent, len(line.CharacterWeights)); err != nil {
				return nil, err
			}
			if line.DurationSeconds == "" {
				return nil, domain.SchemaError(path+".duration_seconds", "duration_seconds is required")
			}
			if line.PhonemeTrack == nil || len(line.PhonemeTrack.Phonemes) == 0 {
				return nil, domain.SchemaError(path+".phoneme_track.phonemes", "at least one phoneme is required")
			}
			ns, _, err := quant.DecimalToNanoseconds(path+".duration_seconds", line.DurationSeconds, mode)
			if err != nil {
				return nil, err
			}
			samples, rem, err := quant.NanosecondsToSamples(path+".duration_seconds", ns, p.SampleRateHz, mode)
			if err != nil {
				return nil, err
			}
			if rem.Remainder != 0 {
				tr.Remainders = append(tr.Remainders, rem)
			}
			frames, err := framesFromSamples(path+".duration_seconds", samples, p.SampleRateHz, p.TargetFPS, mode, tr)
			if err != nil {
				return nil, err
			}
			seg := domain.Segment{Index: li, ID: line.LineID, Text: line.TextContent, DurationSamples: samples, DurationFrames: frames}
			for wi, w := range line.CharacterWeights {
				if w.Character == "" {
					return nil, domain.SchemaError(path+".character_weights["+itoa(wi)+"].character", "character is required")
				}
				next, _, err := quant.ScaleFixedPoint(path+".character_weights["+itoa(wi)+"].weight_permille", w.WeightPermille, quant.ScalePermille, quant.ScalePermyriad, quant.RoundFloor)
				if err != nil {
					return nil, err
				}
				seg.CharWeights = append(seg.CharWeights, next)
				addField(tr, path+".character_weights["+itoa(wi)+"]", "weight_permille", "char_weights", "permille_to_permyriad_x10", w.WeightPermille, next, false, 0)
			}
			for pi, ph := range line.PhonemeTrack.Phonemes {
				pp := path + ".phoneme_track.phonemes[" + itoa(pi) + "]"
				code, ok := idToCode[ph.VisemeID]
				if !ok {
					return nil, domain.SchemaError(pp+".viseme_id", "v1 viseme_id %d is absent from codebook", ph.VisemeID)
				}
				startNS, _, err := quant.DecimalToNanoseconds(pp+".start_seconds", ph.StartSeconds, mode)
				if err != nil {
					return nil, err
				}
				endNS, _, err := quant.DecimalToNanoseconds(pp+".end_seconds", ph.EndSeconds, mode)
				if err != nil {
					return nil, err
				}
				startSample, srem, err := quant.NanosecondsToSamples(pp+".start_seconds", startNS, p.SampleRateHz, mode)
				if err != nil {
					return nil, err
				}
				endSample, erem, err := quant.NanosecondsToSamples(pp+".end_seconds", endNS, p.SampleRateHz, mode)
				if err != nil {
					return nil, err
				}
				if srem.Remainder != 0 {
					tr.Remainders = append(tr.Remainders, srem)
				}
				if erem.Remainder != 0 {
					tr.Remainders = append(tr.Remainders, erem)
				}
				startFrame, err := framesFromSamples(pp+".start_seconds", startSample, p.SampleRateHz, p.TargetFPS, mode, tr)
				if err != nil {
					return nil, err
				}
				endFrame, err := framesFromSamples(pp+".end_seconds", endSample, p.SampleRateHz, p.TargetFPS, mode, tr)
				if err != nil {
					return nil, err
				}
				seg.Phonemes = append(seg.Phonemes, domain.Phoneme{
					Index: ph.OrderIndex, Symbol: ph.Symbol, CategoryCode: code,
					StartSample: startSample, EndSample: endSample, StartFrame: startFrame, EndFrame: endFrame,
				})
				addField(tr, pp, "start/end_seconds,viseme_id", "phoneme", "seconds_to_samples_to_frames", ph.StartSeconds+".."+ph.EndSeconds, []int{startFrame, endFrame}, false, 0)
			}
			d.Segments = append(d.Segments, seg)
		}
		p.Dialogues = append(p.Dialogues, d)
	}
	p.LoadTrace = tr
	return &LoadResult{Project: p, Trace: tr}, nil
}

const (
	domainDefaultMinReadable = 2
	domainDefaultTolerance   = 1
)

func itoa(v int) string { return strconv.Itoa(v) }
