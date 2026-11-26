package dialogue

import (
	"encoding/json"
	"fmt"
)

type ExportResult struct {
	Version     string            `json:"version"`
	Data        []byte            `json:"-"`
	Lossy       []ConversionTrace `json:"lossy,omitempty"`
	Truncations []Truncation      `json:"truncations,omitempty"`
}

type outPCM struct {
	Channels int64   `json:"channels"`
	Format   string  `json:"format"`
	Samples  []int64 `json:"samples"`
}

type outV1Root struct {
	SchemaVersion          string     `json:"schemaVersion"`
	DialogueID             string     `json:"dialogue_id"`
	SampleRateHz           int64      `json:"sample_rate_hz"`
	DeclaredTotalSamples   int64      `json:"declared_total_samples"`
	DurationMS             int64      `json:"duration_ms"`
	TargetLoudnessPermille int64      `json:"target_loudness_permille"`
	GainLimitPermille      int64      `json:"gain_limit_permille"`
	GainMinPermille        int64      `json:"session_gain_min_permille"`
	GainMaxPermille        int64      `json:"session_gain_max_permille"`
	FrameLengthSamples     int64      `json:"frame_length_samples"`
	HopSamples             int64      `json:"hop_samples"`
	PCM                    outPCM     `json:"pcm"`
	Phrases                []outV1Seg `json:"phrases"`
	Truth                  outV1Truth `json:"truth"`
}

type outV1Seg struct {
	ID                  string         `json:"id"`
	StartSample         int64          `json:"start_sample"`
	EndSample           int64          `json:"end_sample"`
	SessionGainPermille int64          `json:"session_gain_permille"`
	Phonemes            []outV1Phoneme `json:"phonemes"`
}

type outV1Phoneme struct {
	ID             string `json:"id"`
	StartSample    int64  `json:"start_sample"`
	EndSample      int64  `json:"end_sample"`
	Kind           string `json:"kind"`
	WeightPermille int64  `json:"loudness_weight_permille"`
	BaseF0MilliHz  int64  `json:"base_f0_milli_hz"`
}

type outV1Truth struct {
	Frames   []outV1FrameTruth `json:"frames"`
	Segments []outV1SegTruth   `json:"segments"`
}

type outV1FrameTruth struct {
	SegmentID        string `json:"phrase_id"`
	FrameIndex       int    `json:"frame_index"`
	StartSample      int64  `json:"start_sample"`
	EndSample        int64  `json:"end_sample"`
	Energy           int64  `json:"fixed_energy"`
	PeakPosition     int    `json:"peak_position"`
	ExpectedEnvelope int64  `json:"expected_envelope_permille"`
	Calibrated       int64  `json:"calibrated_envelope_permille"`
}

type outV1SegTruth struct {
	SegmentID      string  `json:"phrase_id"`
	GainAdjustment int64   `json:"gain_adjustment_permille"`
	Calibrated     []int64 `json:"calibrated_frames_permille"`
	Reached        bool    `json:"reached"`
}

type outV2Root struct {
	SchemaVersion        string     `json:"schemaVersion"`
	DialogueID           string     `json:"dialogueId"`
	SampleRateHz         int64      `json:"sampleRateHz"`
	DeclaredTotalSamples int64      `json:"declaredTotalSamples"`
	DurationMS           int64      `json:"durationMs"`
	TargetLoudnessBP     int64      `json:"targetLoudnessBasisPoints"`
	GainLimitBP          int64      `json:"gainLimitBasisPoints"`
	GainMinBP            int64      `json:"sessionGainMinBasisPoints"`
	GainMaxBP            int64      `json:"sessionGainMaxBasisPoints"`
	FrameLengthMS        int64      `json:"frameLengthMs"`
	HopMS                int64      `json:"hopMs"`
	PCM                  outPCM     `json:"pcm"`
	Segments             []outV2Seg `json:"segments"`
	Truth                outV2Truth `json:"truth"`
}

type outV2Seg struct {
	ID            string         `json:"id"`
	SceneID       string         `json:"sceneId"`
	StartMS       int64          `json:"startMs"`
	EndMS         int64          `json:"endMs"`
	SessionGainBP int64          `json:"sessionGainBasisPoints"`
	Phonemes      []outV2Phoneme `json:"phonemes"`
}

type outV2Phoneme struct {
	ID            string    `json:"id"`
	StartMS       int64     `json:"startMs"`
	EndMS         int64     `json:"endMs"`
	Category      string    `json:"category"`
	WeightBP      int64     `json:"loudnessWeightBasisPoints"`
	BaseF0MilliHz int64     `json:"baseF0MilliHz"`
	F0Breakpoints []outV2F0 `json:"f0Breakpoints"`
}

type outV2F0 struct {
	AtMS       int64 `json:"atMs"`
	HertzMilli int64 `json:"hzMilli"`
}

type outV2Truth struct {
	Frames   []outV2FrameTruth `json:"frames"`
	Segments []outV2SegTruth   `json:"segments"`
}

type outV2FrameTruth struct {
	SegmentID        string `json:"segmentId"`
	FrameIndex       int    `json:"frameIndex"`
	StartMS          int64  `json:"startMs"`
	EndMS            int64  `json:"endMs"`
	Energy           int64  `json:"fixedEnergy"`
	PeakPosition     int    `json:"peakPosition"`
	ExpectedEnvelope int64  `json:"expectedEnvelopeBasisPoints"`
	Calibrated       int64  `json:"calibratedEnvelopeBasisPoints"`
}

type outV2SegTruth struct {
	SegmentID      string  `json:"segmentId"`
	GainAdjustment int64   `json:"gainAdjustmentBasisPoints"`
	Calibrated     []int64 `json:"calibratedFramesBasisPoints"`
	Reached        bool    `json:"reached"`
}

type outV3Root struct {
	SchemaVersion string       `json:"schemaVersion"`
	Dialogue      outV3Project `json:"dialogue"`
	PCM           outPCM       `json:"pcm"`
	Scenes        []outV3Scene `json:"scenes"`
	Truth         outV3Truth   `json:"truth"`
}

type outV3Project struct {
	ID                   string `json:"id"`
	SampleRate           int64  `json:"sampleRate"`
	DeclaredTotalSamples int64  `json:"declaredTotalSamples"`
	DurationSamples      int64  `json:"durationSamples"`
	TargetLoudnessBP     int64  `json:"targetLoudnessBasisPoints"`
	GainLimitPermille    int64  `json:"gainLimitPermille"`
	GainMinPermille      int64  `json:"sessionGainMinPermille"`
	GainMaxPermille      int64  `json:"sessionGainMaxPermille"`
	FrameLengthSamples   int64  `json:"frameLengthSamples"`
	HopSamples           int64  `json:"hopSamples"`
}

type outV3Scene struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Segments []outV3Seg `json:"segments"`
}

type outV3Seg struct {
	ID              string         `json:"id"`
	StartSample     int64          `json:"startSample"`
	EndSample       int64          `json:"endSample"`
	SessionGainPerm int64          `json:"sessionGainPermille"`
	Phonemes        []outV3Phoneme `json:"phonemes"`
}

type outV3Phoneme struct {
	ID            string    `json:"id"`
	StartSample   int64     `json:"startSample"`
	EndSample     int64     `json:"endSample"`
	Category      string    `json:"category"`
	WeightBP      int64     `json:"loudnessWeightBasisPoints"`
	BaseF0MilliHz int64     `json:"baseF0MilliHz"`
	F0            []outV3F0 `json:"f0Breakpoints"`
}

type outV3F0 struct {
	Sample     int64 `json:"sample"`
	HertzMilli int64 `json:"hertzMilli"`
}

type outV3Truth struct {
	Frames   []outV3FrameTruth `json:"frameTruth"`
	Segments []outV3SegTruth   `json:"segmentTruth"`
}

type outV3FrameTruth struct {
	SegmentID        string `json:"segmentId"`
	FrameIndex       int    `json:"frameIndex"`
	StartSample      int64  `json:"startSample"`
	EndSample        int64  `json:"endSample"`
	Energy           int64  `json:"fixedEnergy"`
	PeakPosition     int    `json:"peakPosition"`
	ExpectedEnvelope int64  `json:"expectedEnvelopeBasisPoints"`
	Calibrated       int64  `json:"calibratedEnvelopeBasisPoints"`
}

type outV3SegTruth struct {
	SegmentID      string  `json:"segmentId"`
	GainAdjustment int64   `json:"gainAdjustmentBasisPoints"`
	Calibrated     []int64 `json:"calibratedFramesBasisPoints"`
	Reached        bool    `json:"reached"`
}

// ExportProject writes a native document. A non-empty Lossy list is not an
// error: the document remains loadable and the treatment is explicit.
func ExportProject(project *Project, version string) (*ExportResult, error) {
	if project == nil {
		return nil, newError(CodeSchema, "", "project", "project is nil")
	}
	if project.Truth == nil {
		return nil, newError(CodeSchema, project.ID, "truth", "truth manifest is required for export")
	}
	result := &ExportResult{Version: version}
	var document any
	var err error
	switch version {
	case Version1:
		document, err = exportV1(project, result)
	case Version2:
		document, err = exportV2(project, result)
	case Version3:
		document, err = exportV3(project, result)
	default:
		return nil, newError(CodeSchema, project.ID, "schemaVersion", "unsupported export version "+version)
	}
	if err != nil {
		return nil, err
	}
	result.Data, err = json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", version, err)
	}
	return result, nil
}

func exportV1(project *Project, result *ExportResult) (outV1Root, error) {
	durationMS, traces, err := SamplesToMilliseconds(project.DeclaredTotalSamples, project.SampleRate, "duration_ms", "export.v1.duration")
	if err != nil {
		return outV1Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	target, traces, err := ConvertScale(project.TargetLoudness, InternalScale, 1000, "target_loudness_permille", "export.v1.target")
	if err != nil {
		return outV1Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	limit, traces, err := ConvertScale(project.GainLimit, InternalScale, 1000, "gain_limit_permille", "export.v1.limit")
	if err != nil {
		return outV1Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	gainMin, traces, err := ConvertScale(project.GainMin, InternalScale, 1000, "session_gain_min_permille", "export.v1.gain_min")
	if err != nil {
		return outV1Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	gainMax, traces, err := ConvertScale(project.GainMax, InternalScale, 1000, "session_gain_max_permille", "export.v1.gain_max")
	if err != nil {
		return outV1Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	root := outV1Root{
		SchemaVersion: Version1, DialogueID: project.ID, SampleRateHz: project.SampleRate,
		DeclaredTotalSamples: project.DeclaredTotalSamples, DurationMS: durationMS,
		TargetLoudnessPermille: target, GainLimitPermille: limit, GainMinPermille: gainMin, GainMaxPermille: gainMax,
		FrameLengthSamples: project.FrameLength, HopSamples: project.Hop, PCM: outPCM{Channels: 1, Format: "pcm_s16le", Samples: pcmValues(project.PCM)},
	}
	if len(project.Scenes) != 1 || project.Scenes[0].ID != DefaultSceneID {
		for _, scene := range project.Scenes {
			result.Lossy = append(result.Lossy, ConversionTrace{
				Code: CodeLossy, DialogueID: project.ID, Path: "scene." + scene.ID, OldValue: scene.ID,
				DefaultValue: DefaultSceneID, Source: "schema.v1.flatPhrases",
				Treatment: "scene tree flattened into phrases; loader will rebuild one default scene",
			})
		}
	}
	for sceneIndex, scene := range project.Scenes {
		for segmentIndex, segment := range scene.Segments {
			gain, traces, err := ConvertScale(segment.SessionGain, InternalScale, 1000, fmt.Sprintf("scenes[%d].segments[%d].session_gain", sceneIndex, segmentIndex), "export.v1.segment_gain")
			if err != nil {
				return outV1Root{}, err
			}
			result.Truncations = append(result.Truncations, traces...)
			outSegment := outV1Seg{ID: segment.ID, StartSample: segment.StartSample, EndSample: segment.EndSample, SessionGainPermille: gain}
			for phonemeIndex, phoneme := range segment.Phonemes {
				weight, traces, err := ConvertScale(phoneme.Weight, InternalScale, 1000, fmt.Sprintf("scenes[%d].segments[%d].phonemes[%d].loudness_weight", sceneIndex, segmentIndex, phonemeIndex), "export.v1.weight")
				if err != nil {
					return outV1Root{}, err
				}
				result.Truncations = append(result.Truncations, traces...)
				outSegment.Phonemes = append(outSegment.Phonemes, outV1Phoneme{
					ID: phoneme.ID, StartSample: phoneme.StartSample, EndSample: phoneme.EndSample,
					Kind: string(phoneme.Kind), WeightPermille: weight, BaseF0MilliHz: phoneme.BaseF0MilliHz,
				})
				if !constantF0(phoneme.F0Points) {
					result.Lossy = append(result.Lossy, ConversionTrace{
						Code: CodeLossy, DialogueID: project.ID,
						Path:     fmt.Sprintf("scenes[%d].segments[%d].phonemes[%d].f0_breakpoints", sceneIndex, segmentIndex, phonemeIndex),
						OldValue: fmt.Sprintf("%v", phoneme.F0Points), DefaultValue: fmt.Sprintf("base_f0_milli_hz=%d", phoneme.BaseF0MilliHz),
						Source:    "schema.v1.constantF0Only",
						Treatment: "F0 curve collapsed to one constant breakpoint at phoneme start",
					})
				}
			}
			root.Phrases = append(root.Phrases, outSegment)
		}
	}
	for _, frame := range project.Truth.Frames {
		expected, traces, err := ConvertScale(frame.ExpectedEnvelope, InternalScale, 1000, "truth.expected_envelope", "export.v1.truth_envelope")
		if err != nil {
			return outV1Root{}, err
		}
		result.Truncations = append(result.Truncations, traces...)
		calibrated, traces, err := ConvertScale(frame.CalibratedEnvelope, InternalScale, 1000, "truth.calibrated_envelope", "export.v1.truth_calibrated")
		if err != nil {
			return outV1Root{}, err
		}
		result.Truncations = append(result.Truncations, traces...)
		root.Truth.Frames = append(root.Truth.Frames, outV1FrameTruth{
			SegmentID: frame.SegmentID, FrameIndex: frame.FrameIndex, StartSample: frame.StartSample, EndSample: frame.EndSample,
			Energy: frame.Energy, PeakPosition: frame.PeakPosition, ExpectedEnvelope: expected, Calibrated: calibrated,
		})
	}
	for _, segment := range project.Truth.Segments {
		adjustment, traces, err := ConvertScale(segment.GainAdjustment, InternalScale, 1000, "truth.gain_adjustment", "export.v1.truth_adjustment")
		if err != nil {
			return outV1Root{}, err
		}
		result.Truncations = append(result.Truncations, traces...)
		calibrated := make([]int64, len(segment.Calibrated))
		for i, value := range segment.Calibrated {
			calibrated[i], traces, err = ConvertScale(value, InternalScale, 1000, fmt.Sprintf("truth.calibrated_frames[%d]", i), "export.v1.truth_frames")
			if err != nil {
				return outV1Root{}, err
			}
			result.Truncations = append(result.Truncations, traces...)
		}
		root.Truth.Segments = append(root.Truth.Segments, outV1SegTruth{
			SegmentID: segment.SegmentID, GainAdjustment: adjustment, Calibrated: calibrated, Reached: segment.Reached,
		})
	}
	return root, nil
}

func exportV2(project *Project, result *ExportResult) (outV2Root, error) {
	durationMS, traces, err := SamplesToMilliseconds(project.DeclaredTotalSamples, project.SampleRate, "durationMs", "export.v2.duration")
	if err != nil {
		return outV2Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	frameMS, traces, err := SamplesToMilliseconds(project.FrameLength, project.SampleRate, "frameLengthMs", "export.v2.frame_length")
	if err != nil {
		return outV2Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	hopMS, traces, err := SamplesToMilliseconds(project.Hop, project.SampleRate, "hopMs", "export.v2.hop")
	if err != nil {
		return outV2Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	root := outV2Root{
		SchemaVersion: Version2, DialogueID: project.ID, SampleRateHz: project.SampleRate,
		DeclaredTotalSamples: project.DeclaredTotalSamples, DurationMS: durationMS,
		TargetLoudnessBP: project.TargetLoudness, GainLimitBP: project.GainLimit,
		GainMinBP: project.GainMin, GainMaxBP: project.GainMax,
		FrameLengthMS: frameMS, HopMS: hopMS,
		PCM: outPCM{Channels: 1, Format: "pcm_s16le", Samples: pcmValues(project.PCM)},
	}
	for _, scene := range project.Scenes {
		if scene.Name != scene.ID {
			result.Lossy = append(result.Lossy, ConversionTrace{
				Code: CodeLossy, DialogueID: project.ID, Path: "scene." + scene.ID + ".name", OldValue: scene.Name,
				DefaultValue: scene.ID, Source: "schema.v2.flatSegments",
				Treatment: "nested scene tree flattened into segment.sceneId; scene display name omitted",
			})
		}
	}
	for _, scene := range project.Scenes {
		for segmentIndex, segment := range scene.Segments {
			startMS, traces, err := SamplesToMilliseconds(segment.StartSample, project.SampleRate, fmt.Sprintf("scene.%s.segment[%d].startMs", scene.ID, segmentIndex), "export.v2.segment_start")
			if err != nil {
				return outV2Root{}, err
			}
			result.Truncations = append(result.Truncations, traces...)
			endMS, traces, err := SamplesToMilliseconds(segment.EndSample, project.SampleRate, fmt.Sprintf("scene.%s.segment[%d].endMs", scene.ID, segmentIndex), "export.v2.segment_end")
			if err != nil {
				return outV2Root{}, err
			}
			result.Truncations = append(result.Truncations, traces...)
			outSegment := outV2Seg{
				ID: segment.ID, SceneID: scene.ID, StartMS: startMS, EndMS: endMS, SessionGainBP: segment.SessionGain,
			}
			for phonemeIndex, phoneme := range segment.Phonemes {
				start, traces, err := SamplesToMilliseconds(phoneme.StartSample, project.SampleRate, fmt.Sprintf("scene.%s.segment.%s.phoneme[%d].startMs", scene.ID, segment.ID, phonemeIndex), "export.v2.phoneme_start")
				if err != nil {
					return outV2Root{}, err
				}
				result.Truncations = append(result.Truncations, traces...)
				end, traces, err := SamplesToMilliseconds(phoneme.EndSample, project.SampleRate, fmt.Sprintf("scene.%s.segment.%s.phoneme[%d].endMs", scene.ID, segment.ID, phonemeIndex), "export.v2.phoneme_end")
				if err != nil {
					return outV2Root{}, err
				}
				result.Truncations = append(result.Truncations, traces...)
				outPhoneme := outV2Phoneme{
					ID: phoneme.ID, StartMS: start, EndMS: end, Category: string(phoneme.Kind),
					WeightBP: phoneme.Weight, BaseF0MilliHz: phoneme.BaseF0MilliHz,
				}
				for pointIndex, point := range phoneme.F0Points {
					at, traces, err := SamplesToMilliseconds(point.AtSample, project.SampleRate, fmt.Sprintf("scene.%s.segment.%s.phoneme[%d].f0[%d].atMs", scene.ID, segment.ID, phonemeIndex, pointIndex), "export.v2.f0")
					if err != nil {
						return outV2Root{}, err
					}
					result.Truncations = append(result.Truncations, traces...)
					if len(traces) > 0 {
						result.Lossy = append(result.Lossy, ConversionTrace{
							Code: CodeLossy, DialogueID: project.ID,
							Path:     fmt.Sprintf("scene.%s.segment.%s.phoneme[%d].f0[%d]", scene.ID, segment.ID, phonemeIndex, pointIndex),
							OldValue: fmt.Sprintf("sample=%d", point.AtSample), DefaultValue: fmt.Sprintf("ms=%d", at),
							Source:    "schema.v2.millisecondTimebase",
							Treatment: "spin/sample rounding recorded; loader re-expands milliseconds to samples",
						})
					}
					outPhoneme.F0Breakpoints = append(outPhoneme.F0Breakpoints, outV2F0{AtMS: at, HertzMilli: point.HertzMilli})
				}
				outSegment.Phonemes = append(outSegment.Phonemes, outPhoneme)
			}
			root.Segments = append(root.Segments, outSegment)
		}
	}
	for _, frame := range project.Truth.Frames {
		start, traces, err := SamplesToMilliseconds(frame.StartSample, project.SampleRate, "truth.startMs", "export.v2.truth_start")
		if err != nil {
			return outV2Root{}, err
		}
		result.Truncations = append(result.Truncations, traces...)
		end, traces, err := SamplesToMilliseconds(frame.EndSample, project.SampleRate, "truth.endMs", "export.v2.truth_end")
		if err != nil {
			return outV2Root{}, err
		}
		result.Truncations = append(result.Truncations, traces...)
		root.Truth.Frames = append(root.Truth.Frames, outV2FrameTruth{
			SegmentID: frame.SegmentID, FrameIndex: frame.FrameIndex, StartMS: start, EndMS: end,
			Energy: frame.Energy, PeakPosition: frame.PeakPosition, ExpectedEnvelope: frame.ExpectedEnvelope, Calibrated: frame.CalibratedEnvelope,
		})
	}
	for _, segment := range project.Truth.Segments {
		root.Truth.Segments = append(root.Truth.Segments, outV2SegTruth{
			SegmentID: segment.SegmentID, GainAdjustment: segment.GainAdjustment,
			Calibrated: append([]int64(nil), segment.Calibrated...), Reached: segment.Reached,
		})
	}
	return root, nil
}

func exportV3(project *Project, result *ExportResult) (outV3Root, error) {
	limit, traces, err := ConvertScale(project.GainLimit, InternalScale, 1000, "dialogue.gainLimitPermille", "export.v3.gain_limit")
	if err != nil {
		return outV3Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	gainMin, traces, err := ConvertScale(project.GainMin, InternalScale, 1000, "dialogue.sessionGainMinPermille", "export.v3.gain_min")
	if err != nil {
		return outV3Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	gainMax, traces, err := ConvertScale(project.GainMax, InternalScale, 1000, "dialogue.sessionGainMaxPermille", "export.v3.gain_max")
	if err != nil {
		return outV3Root{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	root := outV3Root{
		SchemaVersion: Version3,
		Dialogue: outV3Project{
			ID: project.ID, SampleRate: project.SampleRate, DeclaredTotalSamples: project.DeclaredTotalSamples,
			DurationSamples: project.DeclaredTotalSamples, TargetLoudnessBP: project.TargetLoudness,
			GainLimitPermille: limit, GainMinPermille: gainMin, GainMaxPermille: gainMax,
			FrameLengthSamples: project.FrameLength, HopSamples: project.Hop,
		},
		PCM: outPCM{Channels: 1, Format: "pcm_s16le", Samples: pcmValues(project.PCM)},
	}
	for _, scene := range project.Scenes {
		outScene := outV3Scene{ID: scene.ID, Name: scene.Name}
		for segmentIndex, segment := range scene.Segments {
			gain, traces, err := ConvertScale(segment.SessionGain, InternalScale, 1000, fmt.Sprintf("scene.%s.segment[%d].sessionGainPermille", scene.ID, segmentIndex), "export.v3.segment_gain")
			if err != nil {
				return outV3Root{}, err
			}
			result.Truncations = append(result.Truncations, traces...)
			outSegment := outV3Seg{ID: segment.ID, StartSample: segment.StartSample, EndSample: segment.EndSample, SessionGainPerm: gain}
			for _, phoneme := range segment.Phonemes {
				outPhoneme := outV3Phoneme{
					ID: phoneme.ID, StartSample: phoneme.StartSample, EndSample: phoneme.EndSample,
					Category: string(phoneme.Kind), WeightBP: phoneme.Weight, BaseF0MilliHz: phoneme.BaseF0MilliHz,
				}
				for _, point := range phoneme.F0Points {
					outPhoneme.F0 = append(outPhoneme.F0, outV3F0{Sample: point.AtSample, HertzMilli: point.HertzMilli})
				}
				outSegment.Phonemes = append(outSegment.Phonemes, outPhoneme)
			}
			outScene.Segments = append(outScene.Segments, outSegment)
		}
		root.Scenes = append(root.Scenes, outScene)
	}
	for _, frame := range project.Truth.Frames {
		root.Truth.Frames = append(root.Truth.Frames, outV3FrameTruth{
			SegmentID: frame.SegmentID, FrameIndex: frame.FrameIndex, StartSample: frame.StartSample, EndSample: frame.EndSample,
			Energy: frame.Energy, PeakPosition: frame.PeakPosition, ExpectedEnvelope: frame.ExpectedEnvelope, Calibrated: frame.CalibratedEnvelope,
		})
	}
	for _, segment := range project.Truth.Segments {
		root.Truth.Segments = append(root.Truth.Segments, outV3SegTruth{
			SegmentID: segment.SegmentID, GainAdjustment: segment.GainAdjustment,
			Calibrated: append([]int64(nil), segment.Calibrated...), Reached: segment.Reached,
		})
	}
	return root, nil
}

func pcmValues(samples []int16) []int64 {
	values := make([]int64, len(samples))
	for i, sample := range samples {
		values[i] = int64(sample)
	}
	return values
}

func constantF0(points []F0Point) bool {
	if len(points) <= 1 {
		return true
	}
	for _, point := range points[1:] {
		if point.HertzMilli != points[0].HertzMilli {
			return false
		}
	}
	return true
}
