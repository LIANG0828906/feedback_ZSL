package dialogue

import (
	"bytes"
	"encoding/json"
	"fmt"
)

const (
	Version1 = "1.0"
	Version2 = "2.0"
	Version3 = "3.0"
)

type rawPCM struct {
	Channels *int64  `json:"channels"`
	Format   *string `json:"format"`
	Samples  []int64 `json:"samples"`
}

type rawV1Root struct {
	SchemaVersion          *string     `json:"schemaVersion"`
	DialogueID             *string     `json:"dialogue_id"`
	SampleRateHz           *int64      `json:"sample_rate_hz"`
	DeclaredTotalSamples   *int64      `json:"declared_total_samples"`
	DurationMS             *int64      `json:"duration_ms"`
	TargetLoudnessPermille *int64      `json:"target_loudness_permille"`
	GainLimitPermille      *int64      `json:"gain_limit_permille"`
	GainMinPermille        *int64      `json:"session_gain_min_permille"`
	GainMaxPermille        *int64      `json:"session_gain_max_permille"`
	FrameLengthSamples     *int64      `json:"frame_length_samples"`
	HopSamples             *int64      `json:"hop_samples"`
	PCM                    *rawPCM     `json:"pcm"`
	Phrases                []rawV1Seg  `json:"phrases"`
	Truth                  *rawV1Truth `json:"truth"`
}

type rawV1Seg struct {
	ID                  *string        `json:"id"`
	StartSample         *int64         `json:"start_sample"`
	EndSample           *int64         `json:"end_sample"`
	SessionGainPermille *int64         `json:"session_gain_permille"`
	Phonemes            []rawV1Phoneme `json:"phonemes"`
}

type rawV1Phoneme struct {
	ID             *string `json:"id"`
	StartSample    *int64  `json:"start_sample"`
	EndSample      *int64  `json:"end_sample"`
	Kind           *string `json:"kind"`
	WeightPermille *int64  `json:"loudness_weight_permille"`
	BaseF0MilliHz  *int64  `json:"base_f0_milli_hz"`
}

type rawV1Truth struct {
	Frames   []rawV1FrameTruth `json:"frames"`
	Segments []rawV1SegTruth   `json:"segments"`
}

type rawV1FrameTruth struct {
	SegmentID        *string `json:"phrase_id"`
	FrameIndex       *int    `json:"frame_index"`
	StartSample      *int64  `json:"start_sample"`
	EndSample        *int64  `json:"end_sample"`
	Energy           *int64  `json:"fixed_energy"`
	PeakPosition     *int    `json:"peak_position"`
	ExpectedEnvelope *int64  `json:"expected_envelope_permille"`
	Calibrated       *int64  `json:"calibrated_envelope_permille"`
}

type rawV1SegTruth struct {
	SegmentID      *string `json:"phrase_id"`
	GainAdjustment *int64  `json:"gain_adjustment_permille"`
	Calibrated     []int64 `json:"calibrated_frames_permille"`
	Reached        *bool   `json:"reached"`
}

type rawV2Root struct {
	SchemaVersion        *string     `json:"schemaVersion"`
	DialogueID           *string     `json:"dialogueId"`
	SampleRateHz         *int64      `json:"sampleRateHz"`
	DeclaredTotalSamples *int64      `json:"declaredTotalSamples"`
	DurationMS           *int64      `json:"durationMs"`
	TargetLoudnessBP     *int64      `json:"targetLoudnessBasisPoints"`
	GainLimitBP          *int64      `json:"gainLimitBasisPoints"`
	GainMinBP            *int64      `json:"sessionGainMinBasisPoints"`
	GainMaxBP            *int64      `json:"sessionGainMaxBasisPoints"`
	FrameLengthMS        *int64      `json:"frameLengthMs"`
	HopMS                *int64      `json:"hopMs"`
	PCM                  *rawPCM     `json:"pcm"`
	Segments             []rawV2Seg  `json:"segments"`
	Truth                *rawV2Truth `json:"truth"`
}

type rawV2Seg struct {
	ID            *string        `json:"id"`
	SceneID       *string        `json:"sceneId"`
	StartMS       *int64         `json:"startMs"`
	EndMS         *int64         `json:"endMs"`
	SessionGainBP *int64         `json:"sessionGainBasisPoints"`
	Phonemes      []rawV2Phoneme `json:"phonemes"`
}

type rawV2Phoneme struct {
	ID            *string   `json:"id"`
	StartMS       *int64    `json:"startMs"`
	EndMS         *int64    `json:"endMs"`
	Category      *string   `json:"category"`
	WeightBP      *int64    `json:"loudnessWeightBasisPoints"`
	BaseF0MilliHz *int64    `json:"baseF0MilliHz"`
	F0Breakpoints []rawV2F0 `json:"f0Breakpoints"`
}

type rawV2F0 struct {
	AtMS       *int64 `json:"atMs"`
	HertzMilli *int64 `json:"hzMilli"`
}

type rawV2Truth struct {
	Frames   []rawV2FrameTruth `json:"frames"`
	Segments []rawV2SegTruth   `json:"segments"`
}

type rawV2FrameTruth struct {
	SegmentID        *string `json:"segmentId"`
	FrameIndex       *int    `json:"frameIndex"`
	StartMS          *int64  `json:"startMs"`
	EndMS            *int64  `json:"endMs"`
	Energy           *int64  `json:"fixedEnergy"`
	PeakPosition     *int    `json:"peakPosition"`
	ExpectedEnvelope *int64  `json:"expectedEnvelopeBasisPoints"`
	Calibrated       *int64  `json:"calibratedEnvelopeBasisPoints"`
}

type rawV2SegTruth struct {
	SegmentID      *string `json:"segmentId"`
	GainAdjustment *int64  `json:"gainAdjustmentBasisPoints"`
	Calibrated     []int64 `json:"calibratedFramesBasisPoints"`
	Reached        *bool   `json:"reached"`
}

type rawV3Root struct {
	SchemaVersion *string       `json:"schemaVersion"`
	Dialogue      *rawV3Project `json:"dialogue"`
	PCM           *rawPCM       `json:"pcm"`
	Scenes        []rawV3Scene  `json:"scenes"`
	Truth         *rawV3Truth   `json:"truth"`
}

type rawV3Project struct {
	ID                   *string `json:"id"`
	SampleRate           *int64  `json:"sampleRate"`
	DeclaredTotalSamples *int64  `json:"declaredTotalSamples"`
	DurationSamples      *int64  `json:"durationSamples"`
	TargetLoudnessBP     *int64  `json:"targetLoudnessBasisPoints"`
	GainLimitPermille    *int64  `json:"gainLimitPermille"`
	GainMinPermille      *int64  `json:"sessionGainMinPermille"`
	GainMaxPermille      *int64  `json:"sessionGainMaxPermille"`
	FrameLengthSamples   *int64  `json:"frameLengthSamples"`
	HopSamples           *int64  `json:"hopSamples"`
}

type rawV3Scene struct {
	ID       *string    `json:"id"`
	Name     *string    `json:"name"`
	Segments []rawV3Seg `json:"segments"`
}

type rawV3Seg struct {
	ID              *string        `json:"id"`
	StartSample     *int64         `json:"startSample"`
	EndSample       *int64         `json:"endSample"`
	SessionGainPerm *int64         `json:"sessionGainPermille"`
	Phonemes        []rawV3Phoneme `json:"phonemes"`
}

type rawV3Phoneme struct {
	ID            *string   `json:"id"`
	StartSample   *int64    `json:"startSample"`
	EndSample     *int64    `json:"endSample"`
	Category      *string   `json:"category"`
	WeightBP      *int64    `json:"loudnessWeightBasisPoints"`
	BaseF0MilliHz *int64    `json:"baseF0MilliHz"`
	F0            []rawV3F0 `json:"f0Breakpoints"`
}

type rawV3F0 struct {
	Sample     *int64 `json:"sample"`
	HertzMilli *int64 `json:"hertzMilli"`
}

type rawV3Truth struct {
	Frames   []rawV3FrameTruth `json:"frameTruth"`
	Segments []rawV3SegTruth   `json:"segmentTruth"`
}

type rawV3FrameTruth struct {
	SegmentID        *string `json:"segmentId"`
	FrameIndex       *int    `json:"frameIndex"`
	StartSample      *int64  `json:"startSample"`
	EndSample        *int64  `json:"endSample"`
	Energy           *int64  `json:"fixedEnergy"`
	PeakPosition     *int    `json:"peakPosition"`
	ExpectedEnvelope *int64  `json:"expectedEnvelopeBasisPoints"`
	Calibrated       *int64  `json:"calibratedEnvelopeBasisPoints"`
}

type rawV3SegTruth struct {
	SegmentID      *string `json:"segmentId"`
	GainAdjustment *int64  `json:"gainAdjustmentBasisPoints"`
	Calibrated     []int64 `json:"calibratedFramesBasisPoints"`
	Reached        *bool   `json:"reached"`
}

// ConversionTrace records an upgrade default or a downgrade treatment.
type ConversionTrace struct {
	Code         Code   `json:"code"`
	DialogueID   string `json:"dialogue_id"`
	Path         string `json:"path"`
	OldValue     string `json:"old_value"`
	DefaultValue string `json:"default_value,omitempty"`
	Source       string `json:"source"`
	Treatment    string `json:"treatment,omitempty"`
}

// LoadResult is the canonical project plus all conversion provenance.
type LoadResult struct {
	Project       *Project          `json:"project"`
	SourceVersion string            `json:"source_version"`
	Truncations   []Truncation      `json:"truncations,omitempty"`
	Conversions   []ConversionTrace `json:"conversion_traces,omitempty"`
}

// LoadProject detects and loads any supported schema version.
func LoadProject(data []byte) (*LoadResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var versionProbe struct {
		SchemaVersion string `json:"schemaVersion"`
		DialogueID    string `json:"dialogue_id"`
		Dialogue      struct {
			ID string `json:"id"`
		} `json:"dialogue"`
	}
	if err := decoder.Decode(&versionProbe); err != nil {
		return nil, newError(CodeSchema, "", "$", "invalid JSON: "+err.Error())
	}
	dialogueID := versionProbe.DialogueID
	if dialogueID == "" {
		dialogueID = versionProbe.Dialogue.ID
	}
	if versionProbe.SchemaVersion == "" {
		return nil, newError(CodeSchema, dialogueID, "schemaVersion", "missing required field")
	}
	switch versionProbe.SchemaVersion {
	case Version1:
		return loadV1(data)
	case Version2:
		return loadV2(data)
	case Version3:
		return loadV3(data)
	default:
		return nil, newError(CodeSchema, dialogueID, "schemaVersion", "unsupported schema version "+versionProbe.SchemaVersion)
	}
}

func decodeRaw(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return newError(CodeSchema, "", "$", "invalid JSON shape: "+err.Error())
	}
	return nil
}

func loadV1(data []byte) (*LoadResult, error) {
	var raw rawV1Root
	if err := decodeRaw(data, &raw); err != nil {
		return nil, err
	}
	id := valueOr(raw.DialogueID, "")
	missing := func(path string) error { return newError(CodeSchema, id, path, "missing required field") }
	if raw.DialogueID == nil || *raw.DialogueID == "" {
		return nil, missing("dialogue_id")
	}
	if raw.SampleRateHz == nil || *raw.SampleRateHz <= 0 {
		return nil, missing("sample_rate_hz")
	}
	if raw.DeclaredTotalSamples == nil {
		return nil, missing("declared_total_samples")
	}
	if raw.DurationMS == nil {
		return nil, missing("duration_ms")
	}
	if raw.TargetLoudnessPermille == nil {
		return nil, missing("target_loudness_permille")
	}
	if raw.GainLimitPermille == nil {
		return nil, missing("gain_limit_permille")
	}
	if raw.GainMinPermille == nil {
		return nil, missing("session_gain_min_permille")
	}
	if raw.GainMaxPermille == nil {
		return nil, missing("session_gain_max_permille")
	}
	if raw.FrameLengthSamples == nil {
		return nil, missing("frame_length_samples")
	}
	if raw.HopSamples == nil {
		return nil, missing("hop_samples")
	}
	if raw.PCM == nil {
		return nil, missing("pcm")
	}
	if raw.Phrases == nil {
		return nil, missing("phrases")
	}
	if raw.Truth == nil {
		return nil, missing("truth")
	}
	result := &LoadResult{SourceVersion: Version1}
	source := SourceInfo{SchemaVersion: Version1, NativeTimeUnit: "sample", NativeGainScale: 1000, NativeLoudnessScale: 1000}
	p := &Project{
		ID:                   id,
		SampleRate:           *raw.SampleRateHz,
		DeclaredTotalSamples: *raw.DeclaredTotalSamples,
		TargetLoudness:       0,
		GainLimit:            0,
		GainMin:              0,
		GainMax:              0,
		FrameLength:          *raw.FrameLengthSamples,
		Hop:                  *raw.HopSamples,
		Source:               source,
	}
	var err error
	if p.TargetLoudness, err = scaleAppend(result, *raw.TargetLoudnessPermille, 1000, InternalScale, "target_loudness_permille", "load.v1.target"); err != nil {
		return nil, err
	}
	if p.GainLimit, err = scaleAppend(result, *raw.GainLimitPermille, 1000, InternalScale, "gain_limit_permille", "load.v1.gain_limit"); err != nil {
		return nil, err
	}
	if p.GainMin, err = scaleAppend(result, *raw.GainMinPermille, 1000, InternalScale, "session_gain_min_permille", "load.v1.gain_min"); err != nil {
		return nil, err
	}
	if p.GainMax, err = scaleAppend(result, *raw.GainMaxPermille, 1000, InternalScale, "session_gain_max_permille", "load.v1.gain_max"); err != nil {
		return nil, err
	}
	if err := loadPCM(raw.PCM, id, "pcm", p); err != nil {
		return nil, err
	}
	scene := Scene{ID: DefaultSceneID, Name: DefaultSceneName}
	for i, rawSeg := range raw.Phrases {
		path := fmt.Sprintf("phrases[%d]", i)
		segment, segmentConversions, err := loadV1Segment(rawSeg, path, id, result)
		if err != nil {
			return nil, err
		}
		segment.SceneID = scene.ID
		scene.Segments = append(scene.Segments, segment)
		result.Conversions = append(result.Conversions, segmentConversions...)
		result.Conversions = append(result.Conversions, ConversionTrace{
			Code: CodeDefault, DialogueID: id, Path: path + ".scene_group", OldValue: "<absent>",
			DefaultValue: DefaultSceneID, Source: DefaultSceneSource,
			Treatment: "created one default scene because schema v1 has a flat phrase table",
		})
	}
	p.Scenes = []Scene{scene}
	result.Conversions = append(result.Conversions, ConversionTrace{
		Code: CodeDefault, DialogueID: id, Path: "scenes." + scene.ID + ".name", OldValue: "<absent>",
		DefaultValue: scene.Name, Source: "schema.v1.defaultSceneName",
		Treatment: "default scene display name",
	})
	expectedDurationMS, traces, err := SamplesToMilliseconds(p.DeclaredTotalSamples, p.SampleRate, "duration_ms", "load.v1.duration")
	if err != nil {
		return nil, err
	}
	result.Truncations = append(result.Truncations, traces...)
	if expectedDurationMS != *raw.DurationMS {
		return nil, newError(CodeSchema, id, "duration_ms", fmt.Sprintf("declared duration %d ms does not match %d samples (%d ms)", *raw.DurationMS, p.DeclaredTotalSamples, expectedDurationMS))
	}
	truth, err := loadV1Truth(raw.Truth, id, result)
	if err != nil {
		return nil, err
	}
	p.Truth = truth
	if err := validateProject(p); err != nil {
		return nil, err
	}
	result.Project = p
	return result, nil
}

func loadV1Segment(raw rawV1Seg, path, dialogueID string, result *LoadResult) (Segment, []ConversionTrace, error) {
	missing := func(field string) error {
		return newError(CodeSchema, dialogueID, path+"."+field, "missing required field")
	}
	if raw.ID == nil || *raw.ID == "" {
		return Segment{}, nil, missing("id")
	}
	if raw.StartSample == nil {
		return Segment{}, nil, missing("start_sample")
	}
	if raw.EndSample == nil {
		return Segment{}, nil, missing("end_sample")
	}
	if raw.SessionGainPermille == nil {
		return Segment{}, nil, missing("session_gain_permille")
	}
	if raw.Phonemes == nil {
		return Segment{}, nil, missing("phonemes")
	}
	gain, traces, err := ConvertScale(*raw.SessionGainPermille, 1000, InternalScale, path+".session_gain_permille", "load.v1.segment_gain")
	if err != nil {
		return Segment{}, nil, err
	}
	result.Truncations = append(result.Truncations, traces...)
	segment := Segment{ID: *raw.ID, StartSample: *raw.StartSample, EndSample: *raw.EndSample, SessionGain: gain}
	var conversions []ConversionTrace
	for i, rawPhoneme := range raw.Phonemes {
		phonemePath := fmt.Sprintf("%s.phonemes[%d]", path, i)
		phoneme, more, err := loadV1Phoneme(rawPhoneme, phonemePath, dialogueID, result)
		if err != nil {
			return Segment{}, nil, err
		}
		segment.Phonemes = append(segment.Phonemes, phoneme)
		conversions = append(conversions, more...)
	}
	return segment, conversions, nil
}

func loadV1Phoneme(raw rawV1Phoneme, path, dialogueID string, result *LoadResult) (Phoneme, []ConversionTrace, error) {
	missing := func(field string) error {
		return newError(CodeSchema, dialogueID, path+"."+field, "missing required field")
	}
	if raw.ID == nil || *raw.ID == "" {
		return Phoneme{}, nil, missing("id")
	}
	if raw.StartSample == nil || raw.EndSample == nil || raw.Kind == nil || raw.WeightPermille == nil || raw.BaseF0MilliHz == nil {
		for name, present := range map[string]bool{
			"start_sample": raw.StartSample != nil, "end_sample": raw.EndSample != nil,
			"kind": raw.Kind != nil, "loudness_weight_permille": raw.WeightPermille != nil,
			"base_f0_milli_hz": raw.BaseF0MilliHz != nil,
		} {
			if !present {
				return Phoneme{}, nil, missing(name)
			}
		}
	}
	weight, traces, err := ConvertScale(*raw.WeightPermille, 1000, InternalScale, path+".loudness_weight_permille", "load.v1.weight")
	if err != nil {
		return Phoneme{}, nil, err
	}
	result.Truncations = append(result.Truncations, traces...)
	phoneme := Phoneme{
		ID: *raw.ID, StartSample: *raw.StartSample, EndSample: *raw.EndSample,
		Kind: PhonemeKind(normalizeCategory(*raw.Kind)), Weight: weight, BaseF0MilliHz: *raw.BaseF0MilliHz,
	}
	phoneme.F0Points = []F0Point{{AtSample: phoneme.StartSample, HertzMilli: phoneme.BaseF0MilliHz}}
	trace := ConversionTrace{
		Code: CodeDefault, DialogueID: dialogueID, Path: path + ".f0_breakpoints",
		OldValue: "<absent>", DefaultValue: fmt.Sprintf("[{sample:%d,hertzMilli:%d}]", phoneme.StartSample, phoneme.BaseF0MilliHz),
		Source: DefaultF0Source, Treatment: "constant F0 breakpoint derived from base_f0_milli_hz",
	}
	return phoneme, []ConversionTrace{trace}, nil
}

func loadV2(data []byte) (*LoadResult, error) {
	var raw rawV2Root
	if err := decodeRaw(data, &raw); err != nil {
		return nil, err
	}
	id := valueOr(raw.DialogueID, "")
	missing := func(path string) error { return newError(CodeSchema, id, path, "missing required field") }
	if raw.DialogueID == nil || *raw.DialogueID == "" {
		return nil, missing("dialogueId")
	}
	required := map[string]bool{
		"sampleRateHz": raw.SampleRateHz != nil, "declaredTotalSamples": raw.DeclaredTotalSamples != nil,
		"durationMs":                raw.DurationMS != nil,
		"targetLoudnessBasisPoints": raw.TargetLoudnessBP != nil, "gainLimitBasisPoints": raw.GainLimitBP != nil,
		"sessionGainMinBasisPoints": raw.GainMinBP != nil, "sessionGainMaxBasisPoints": raw.GainMaxBP != nil,
		"frameLengthMs": raw.FrameLengthMS != nil, "hopMs": raw.HopMS != nil,
		"pcm": raw.PCM != nil, "segments": raw.Segments != nil, "truth": raw.Truth != nil,
	}
	for _, field := range []string{"sampleRateHz", "declaredTotalSamples", "durationMs", "targetLoudnessBasisPoints", "gainLimitBasisPoints", "sessionGainMinBasisPoints", "sessionGainMaxBasisPoints", "frameLengthMs", "hopMs", "pcm", "segments", "truth"} {
		if !required[field] {
			return nil, missing(field)
		}
	}
	result := &LoadResult{SourceVersion: Version2}
	p := &Project{
		ID: id, SampleRate: *raw.SampleRateHz, DeclaredTotalSamples: *raw.DeclaredTotalSamples,
		Source: SourceInfo{SchemaVersion: Version2, NativeTimeUnit: "ms", NativeGainScale: 10000, NativeLoudnessScale: 10000},
	}
	p.TargetLoudness = *raw.TargetLoudnessBP
	p.GainLimit = *raw.GainLimitBP
	p.GainMin = *raw.GainMinBP
	p.GainMax = *raw.GainMaxBP
	var err error
	p.FrameLength, result.Truncations, err = MillisecondsToSamples(*raw.FrameLengthMS, p.SampleRate, "frameLengthMs", "load.v2.frame_length")
	if err != nil {
		return nil, err
	}
	p.Hop, result.Truncations, err = millisecondsToSamplesAppend(*raw.HopMS, p.SampleRate, "hopMs", "load.v2.hop", result.Truncations)
	if err != nil {
		return nil, err
	}
	if err := loadPCM(raw.PCM, id, "pcm", p); err != nil {
		return nil, err
	}
	sceneIndex := map[string]int{}
	var scenes []Scene
	for i, rawSeg := range raw.Segments {
		path := fmt.Sprintf("segments[%d]", i)
		segment, conversions, err := loadV2Segment(rawSeg, path, id, p.SampleRate, result)
		if err != nil {
			return nil, err
		}
		sceneID := DefaultSceneID
		if rawSeg.SceneID != nil && *rawSeg.SceneID != "" {
			sceneID = *rawSeg.SceneID
		} else {
			result.Conversions = append(result.Conversions, ConversionTrace{
				Code: CodeDefault, DialogueID: id, Path: path + ".sceneId", OldValue: "<absent>",
				DefaultValue: sceneID, Source: DefaultSceneSource, Treatment: "created default flat scene group",
			})
		}
		segment.SceneID = sceneID
		result.Conversions = append(result.Conversions, ConversionTrace{
			Code: CodeDefault, DialogueID: id, Path: path + ".scene_group", OldValue: "flat:" + sceneID,
			DefaultValue: sceneID, Source: "schema.v2.flatSceneGrouping",
			Treatment: "grouped flat segments by sceneId while preserving segment order",
		})
		idx, ok := sceneIndex[sceneID]
		if !ok {
			idx = len(scenes)
			sceneIndex[sceneID] = idx
			scenes = append(scenes, Scene{ID: sceneID, Name: sceneID})
			result.Conversions = append(result.Conversions, ConversionTrace{
				Code: CodeDefault, DialogueID: id, Path: "scenes." + sceneID + ".name", OldValue: "<absent>",
				DefaultValue: sceneID, Source: "schema.v2.defaultSceneName",
				Treatment: "scene display name defaults to sceneId",
			})
		}
		scenes[idx].Segments = append(scenes[idx].Segments, segment)
		result.Conversions = append(result.Conversions, conversions...)
	}
	p.Scenes = scenes
	expectedDurationMS, traces, err := SamplesToMilliseconds(p.DeclaredTotalSamples, p.SampleRate, "durationMs", "load.v2.duration")
	if err != nil {
		return nil, err
	}
	result.Truncations = append(result.Truncations, traces...)
	if expectedDurationMS != *raw.DurationMS {
		return nil, newError(CodeSchema, id, "durationMs", fmt.Sprintf("declared duration %d ms does not match %d samples (%d ms)", *raw.DurationMS, p.DeclaredTotalSamples, expectedDurationMS))
	}
	truth, err := loadV2Truth(raw.Truth, id, p, result)
	if err != nil {
		return nil, err
	}
	p.Truth = truth
	if err := validateProject(p); err != nil {
		return nil, err
	}
	result.Project = p
	return result, nil
}

func loadV2Segment(raw rawV2Seg, path, dialogueID string, sampleRate int64, result *LoadResult) (Segment, []ConversionTrace, error) {
	missing := func(field string) error {
		return newError(CodeSchema, dialogueID, path+"."+field, "missing required field")
	}
	if raw.ID == nil || *raw.ID == "" {
		return Segment{}, nil, missing("id")
	}
	if raw.StartMS == nil || raw.EndMS == nil || raw.SessionGainBP == nil || raw.Phonemes == nil {
		for name, present := range map[string]bool{"startMs": raw.StartMS != nil, "endMs": raw.EndMS != nil, "sessionGainBasisPoints": raw.SessionGainBP != nil, "phonemes": raw.Phonemes != nil} {
			if !present {
				return Segment{}, nil, missing(name)
			}
		}
	}
	start, more, err := MillisecondsToSamples(*raw.StartMS, sampleRate, path+".startMs", "load.v2.segment_start")
	if err != nil {
		return Segment{}, nil, err
	}
	result.Truncations = append(result.Truncations, more...)
	end, more, err := MillisecondsToSamples(*raw.EndMS, sampleRate, path+".endMs", "load.v2.segment_end")
	if err != nil {
		return Segment{}, nil, err
	}
	result.Truncations = append(result.Truncations, more...)
	segment := Segment{ID: *raw.ID, StartSample: start, EndSample: end, SessionGain: *raw.SessionGainBP}
	var conversions []ConversionTrace
	for i, rawPhoneme := range raw.Phonemes {
		phoneme, more, err := loadV2Phoneme(rawPhoneme, fmt.Sprintf("%s.phonemes[%d]", path, i), dialogueID, sampleRate, result)
		if err != nil {
			return Segment{}, nil, err
		}
		segment.Phonemes = append(segment.Phonemes, phoneme)
		conversions = append(conversions, more...)
	}
	return segment, conversions, nil
}

func loadV2Phoneme(raw rawV2Phoneme, path, dialogueID string, sampleRate int64, result *LoadResult) (Phoneme, []ConversionTrace, error) {
	missing := func(field string) error {
		return newError(CodeSchema, dialogueID, path+"."+field, "missing required field")
	}
	if raw.ID == nil || *raw.ID == "" {
		return Phoneme{}, nil, missing("id")
	}
	if raw.StartMS == nil || raw.EndMS == nil || raw.Category == nil || raw.WeightBP == nil || raw.BaseF0MilliHz == nil {
		for name, present := range map[string]bool{"startMs": raw.StartMS != nil, "endMs": raw.EndMS != nil, "category": raw.Category != nil, "loudnessWeightBasisPoints": raw.WeightBP != nil, "baseF0MilliHz": raw.BaseF0MilliHz != nil} {
			if !present {
				return Phoneme{}, nil, missing(name)
			}
		}
	}
	start, more, err := MillisecondsToSamples(*raw.StartMS, sampleRate, path+".startMs", "load.v2.phoneme_start")
	if err != nil {
		return Phoneme{}, nil, err
	}
	result.Truncations = append(result.Truncations, more...)
	end, more, err := MillisecondsToSamples(*raw.EndMS, sampleRate, path+".endMs", "load.v2.phoneme_end")
	if err != nil {
		return Phoneme{}, nil, err
	}
	result.Truncations = append(result.Truncations, more...)
	phoneme := Phoneme{ID: *raw.ID, StartSample: start, EndSample: end, Kind: PhonemeKind(normalizeCategory(*raw.Category)), Weight: *raw.WeightBP, BaseF0MilliHz: *raw.BaseF0MilliHz}
	var conversions []ConversionTrace
	for i, point := range raw.F0Breakpoints {
		if point.AtMS == nil || point.HertzMilli == nil {
			return Phoneme{}, nil, missing(fmt.Sprintf("f0Breakpoints[%d]", i))
		}
		at, more, err := MillisecondsToSamples(*point.AtMS, sampleRate, fmt.Sprintf("%s.f0Breakpoints[%d].atMs", path, i), "load.v2.f0")
		if err != nil {
			return Phoneme{}, nil, err
		}
		result.Truncations = append(result.Truncations, more...)
		phoneme.F0Points = append(phoneme.F0Points, F0Point{AtSample: at, HertzMilli: *point.HertzMilli})
	}
	if len(phoneme.F0Points) == 0 {
		phoneme.F0Points = []F0Point{{AtSample: phoneme.StartSample, HertzMilli: phoneme.BaseF0MilliHz}}
		conversions = append(conversions, ConversionTrace{
			Code: CodeDefault, DialogueID: dialogueID, Path: path + ".f0Breakpoints", OldValue: "<absent>",
			DefaultValue: fmt.Sprintf("[{sample:%d,hertzMilli:%d}]", phoneme.StartSample, phoneme.BaseF0MilliHz),
			Source:       "schema.v2.defaultF0Breakpoints", Treatment: "constant F0 at phoneme start",
		})
	}
	return phoneme, conversions, nil
}

func loadV3(data []byte) (*LoadResult, error) {
	var raw rawV3Root
	if err := decodeRaw(data, &raw); err != nil {
		return nil, err
	}
	if raw.Dialogue == nil {
		return nil, newError(CodeSchema, "", "dialogue", "missing required field")
	}
	id := valueOr(raw.Dialogue.ID, "")
	missing := func(path string) error { return newError(CodeSchema, id, path, "missing required field") }
	if raw.Dialogue.ID == nil || *raw.Dialogue.ID == "" {
		return nil, missing("dialogue.id")
	}
	if raw.Dialogue.SampleRate == nil {
		return nil, missing("dialogue.sampleRate")
	}
	if raw.Dialogue.DeclaredTotalSamples == nil {
		return nil, missing("dialogue.declaredTotalSamples")
	}
	if raw.Dialogue.DurationSamples == nil {
		return nil, missing("dialogue.durationSamples")
	}
	if raw.Dialogue.TargetLoudnessBP == nil {
		return nil, missing("dialogue.targetLoudnessBasisPoints")
	}
	if raw.Dialogue.GainLimitPermille == nil {
		return nil, missing("dialogue.gainLimitPermille")
	}
	if raw.Dialogue.GainMinPermille == nil {
		return nil, missing("dialogue.sessionGainMinPermille")
	}
	if raw.Dialogue.GainMaxPermille == nil {
		return nil, missing("dialogue.sessionGainMaxPermille")
	}
	if raw.Dialogue.FrameLengthSamples == nil {
		return nil, missing("dialogue.frameLengthSamples")
	}
	if raw.Dialogue.HopSamples == nil {
		return nil, missing("dialogue.hopSamples")
	}
	if raw.PCM == nil {
		return nil, missing("pcm")
	}
	if raw.Scenes == nil {
		return nil, missing("scenes")
	}
	if raw.Truth == nil {
		return nil, missing("truth")
	}
	result := &LoadResult{SourceVersion: Version3}
	var err error
	p := &Project{
		ID: id, SampleRate: *raw.Dialogue.SampleRate, DeclaredTotalSamples: *raw.Dialogue.DeclaredTotalSamples,
		TargetLoudness: *raw.Dialogue.TargetLoudnessBP,
		FrameLength:    *raw.Dialogue.FrameLengthSamples, Hop: *raw.Dialogue.HopSamples,
		Source: SourceInfo{SchemaVersion: Version3, NativeTimeUnit: "sample", NativeGainScale: 1000, NativeLoudnessScale: 10000},
	}
	if *raw.Dialogue.DurationSamples != p.DeclaredTotalSamples {
		return nil, newError(CodeSchema, id, "dialogue.durationSamples", fmt.Sprintf("duration %d does not match declared total samples %d", *raw.Dialogue.DurationSamples, p.DeclaredTotalSamples))
	}
	if p.GainMin, err = scaleAppend(result, *raw.Dialogue.GainMinPermille, 1000, InternalScale, "dialogue.sessionGainMinPermille", "load.v3.gain_min"); err != nil {
		return nil, err
	}
	if p.GainMax, err = scaleAppend(result, *raw.Dialogue.GainMaxPermille, 1000, InternalScale, "dialogue.sessionGainMaxPermille", "load.v3.gain_max"); err != nil {
		return nil, err
	}
	if p.GainLimit, err = scaleAppend(result, *raw.Dialogue.GainLimitPermille, 1000, InternalScale, "dialogue.gainLimitPermille", "load.v3.gain_limit"); err != nil {
		return nil, err
	}
	if err := loadPCM(raw.PCM, id, "pcm", p); err != nil {
		return nil, err
	}
	for i, rawScene := range raw.Scenes {
		scenePath := fmt.Sprintf("scenes[%d]", i)
		if rawScene.ID == nil || *rawScene.ID == "" {
			return nil, newError(CodeSchema, id, scenePath+".id", "missing required field")
		}
		scene := Scene{ID: *rawScene.ID, Name: valueOr(rawScene.Name, *rawScene.ID)}
		for j, rawSeg := range rawScene.Segments {
			segment, err := loadV3Segment(rawSeg, fmt.Sprintf("%s.segments[%d]", scenePath, j), id, result)
			if err != nil {
				return nil, err
			}
			segment.SceneID = scene.ID
			scene.Segments = append(scene.Segments, segment)
		}
		p.Scenes = append(p.Scenes, scene)
	}
	truth, err := loadV3Truth(raw.Truth, id, result)
	if err != nil {
		return nil, err
	}
	p.Truth = truth
	if err := validateProject(p); err != nil {
		return nil, err
	}
	result.Project = p
	return result, nil
}

func loadV3Segment(raw rawV3Seg, path, dialogueID string, result *LoadResult) (Segment, error) {
	missing := func(field string) error {
		return newError(CodeSchema, dialogueID, path+"."+field, "missing required field")
	}
	if raw.ID == nil || *raw.ID == "" {
		return Segment{}, missing("id")
	}
	if raw.StartSample == nil || raw.EndSample == nil || raw.SessionGainPerm == nil || raw.Phonemes == nil {
		for name, present := range map[string]bool{"startSample": raw.StartSample != nil, "endSample": raw.EndSample != nil, "sessionGainPermille": raw.SessionGainPerm != nil, "phonemes": raw.Phonemes != nil} {
			if !present {
				return Segment{}, missing(name)
			}
		}
	}
	gain, traces, err := ConvertScale(*raw.SessionGainPerm, 1000, InternalScale, path+".sessionGainPermille", "load.v3.segment_gain")
	if err != nil {
		return Segment{}, err
	}
	result.Truncations = append(result.Truncations, traces...)
	segment := Segment{ID: *raw.ID, StartSample: *raw.StartSample, EndSample: *raw.EndSample, SessionGain: gain}
	for i, rawPhoneme := range raw.Phonemes {
		phoneme, more, err := loadV3Phoneme(rawPhoneme, fmt.Sprintf("%s.phonemes[%d]", path, i), dialogueID)
		if err != nil {
			return Segment{}, err
		}
		result.Conversions = append(result.Conversions, more...)
		segment.Phonemes = append(segment.Phonemes, phoneme)
	}
	return segment, nil
}

func loadV3Phoneme(raw rawV3Phoneme, path, dialogueID string) (Phoneme, []ConversionTrace, error) {
	missing := func(field string) error {
		return newError(CodeSchema, dialogueID, path+"."+field, "missing required field")
	}
	if raw.ID == nil || *raw.ID == "" {
		return Phoneme{}, nil, missing("id")
	}
	if raw.StartSample == nil || raw.EndSample == nil || raw.Category == nil || raw.WeightBP == nil || raw.BaseF0MilliHz == nil {
		for name, present := range map[string]bool{"startSample": raw.StartSample != nil, "endSample": raw.EndSample != nil, "category": raw.Category != nil, "loudnessWeightBasisPoints": raw.WeightBP != nil, "baseF0MilliHz": raw.BaseF0MilliHz != nil} {
			if !present {
				return Phoneme{}, nil, missing(name)
			}
		}
	}
	phoneme := Phoneme{ID: *raw.ID, StartSample: *raw.StartSample, EndSample: *raw.EndSample, Kind: PhonemeKind(normalizeCategory(*raw.Category)), Weight: *raw.WeightBP, BaseF0MilliHz: *raw.BaseF0MilliHz}
	var conversions []ConversionTrace
	for i, point := range raw.F0 {
		if point.Sample == nil || point.HertzMilli == nil {
			return Phoneme{}, nil, missing(fmt.Sprintf("f0Breakpoints[%d]", i))
		}
		phoneme.F0Points = append(phoneme.F0Points, F0Point{AtSample: *point.Sample, HertzMilli: *point.HertzMilli})
	}
	if len(phoneme.F0Points) == 0 {
		phoneme.F0Points = []F0Point{{AtSample: phoneme.StartSample, HertzMilli: phoneme.BaseF0MilliHz}}
		conversions = append(conversions, ConversionTrace{
			Code: CodeDefault, DialogueID: dialogueID, Path: path + ".f0Breakpoints", OldValue: "<absent>",
			DefaultValue: fmt.Sprintf("[{sample:%d,hertzMilli:%d}]", phoneme.StartSample, phoneme.BaseF0MilliHz),
			Source:       "schema.v3.defaultF0Breakpoints", Treatment: "constant F0 at phoneme start",
		})
	}
	return phoneme, conversions, nil
}

func loadV1Truth(raw *rawV1Truth, dialogueID string, result *LoadResult) (*Truth, error) {
	truth := &Truth{}
	if raw.Frames == nil {
		return nil, newError(CodeSchema, dialogueID, "truth.frames", "missing required field")
	}
	if raw.Segments == nil {
		return nil, newError(CodeSchema, dialogueID, "truth.segments", "missing required field")
	}
	for i, frame := range raw.Frames {
		path := fmt.Sprintf("truth.frames[%d]", i)
		if frame.SegmentID == nil || frame.FrameIndex == nil || frame.StartSample == nil || frame.EndSample == nil || frame.Energy == nil || frame.PeakPosition == nil || frame.ExpectedEnvelope == nil || frame.Calibrated == nil {
			return nil, newError(CodeSchema, dialogueID, path, "missing required truth field")
		}
		expected, err := scaleAppend(result, *frame.ExpectedEnvelope, 1000, InternalScale, path+".expected_envelope_permille", "load.v1.truth_envelope")
		if err != nil {
			return nil, err
		}
		calibrated, err := scaleAppend(result, *frame.Calibrated, 1000, InternalScale, path+".calibrated_envelope_permille", "load.v1.truth_calibrated")
		if err != nil {
			return nil, err
		}
		truth.Frames = append(truth.Frames, FrameTruth{SegmentID: *frame.SegmentID, FrameIndex: *frame.FrameIndex, StartSample: *frame.StartSample, EndSample: *frame.EndSample, Energy: *frame.Energy, PeakPosition: *frame.PeakPosition, ExpectedEnvelope: expected, CalibratedEnvelope: calibrated})
	}
	for i, segment := range raw.Segments {
		path := fmt.Sprintf("truth.segments[%d]", i)
		if segment.SegmentID == nil || segment.GainAdjustment == nil || segment.Calibrated == nil || segment.Reached == nil {
			return nil, newError(CodeSchema, dialogueID, path, "missing required truth field")
		}
		adjustment, err := scaleAppend(result, *segment.GainAdjustment, 1000, InternalScale, path+".gain_adjustment_permille", "load.v1.truth_adjustment")
		if err != nil {
			return nil, err
		}
		calibrated := make([]int64, len(segment.Calibrated))
		for j, value := range segment.Calibrated {
			calibrated[j], err = scaleAppend(result, value, 1000, InternalScale, fmt.Sprintf("%s.calibrated_frames_permille[%d]", path, j), "load.v1.truth_calibrated_frames")
			if err != nil {
				return nil, err
			}
		}
		truth.Segments = append(truth.Segments, SegmentTruth{SegmentID: *segment.SegmentID, GainAdjustment: adjustment, Calibrated: calibrated, Reached: *segment.Reached})
	}
	return truth, nil
}

func loadV2Truth(raw *rawV2Truth, dialogueID string, p *Project, result *LoadResult) (*Truth, error) {
	truth := &Truth{}
	if raw.Frames == nil || raw.Segments == nil {
		return nil, newError(CodeSchema, dialogueID, "truth", "missing required truth fields")
	}
	for i, frame := range raw.Frames {
		path := fmt.Sprintf("truth.frames[%d]", i)
		if frame.SegmentID == nil || frame.FrameIndex == nil || frame.StartMS == nil || frame.EndMS == nil || frame.Energy == nil || frame.PeakPosition == nil || frame.ExpectedEnvelope == nil || frame.Calibrated == nil {
			return nil, newError(CodeSchema, dialogueID, path, "missing required truth field")
		}
		start, traces, err := MillisecondsToSamples(*frame.StartMS, p.SampleRate, path+".startMs", "load.v2.truth_start")
		if err != nil {
			return nil, err
		}
		result.Truncations = append(result.Truncations, traces...)
		end, traces, err := MillisecondsToSamples(*frame.EndMS, p.SampleRate, path+".endMs", "load.v2.truth_end")
		if err != nil {
			return nil, err
		}
		result.Truncations = append(result.Truncations, traces...)
		truth.Frames = append(truth.Frames, FrameTruth{SegmentID: *frame.SegmentID, FrameIndex: *frame.FrameIndex, StartSample: start, EndSample: end, Energy: *frame.Energy, PeakPosition: *frame.PeakPosition, ExpectedEnvelope: *frame.ExpectedEnvelope, CalibratedEnvelope: *frame.Calibrated})
	}
	for i, segment := range raw.Segments {
		path := fmt.Sprintf("truth.segments[%d]", i)
		if segment.SegmentID == nil || segment.GainAdjustment == nil || segment.Calibrated == nil || segment.Reached == nil {
			return nil, newError(CodeSchema, dialogueID, path, "missing required truth field")
		}
		truth.Segments = append(truth.Segments, SegmentTruth{SegmentID: *segment.SegmentID, GainAdjustment: *segment.GainAdjustment, Calibrated: append([]int64(nil), segment.Calibrated...), Reached: *segment.Reached})
	}
	return truth, nil
}

func loadV3Truth(raw *rawV3Truth, dialogueID string, result *LoadResult) (*Truth, error) {
	truth := &Truth{}
	if raw.Frames == nil || raw.Segments == nil {
		return nil, newError(CodeSchema, dialogueID, "truth", "missing required truth fields")
	}
	for i, frame := range raw.Frames {
		path := fmt.Sprintf("truth.frameTruth[%d]", i)
		if frame.SegmentID == nil || frame.FrameIndex == nil || frame.StartSample == nil || frame.EndSample == nil || frame.Energy == nil || frame.PeakPosition == nil || frame.ExpectedEnvelope == nil || frame.Calibrated == nil {
			return nil, newError(CodeSchema, dialogueID, path, "missing required truth field")
		}
		truth.Frames = append(truth.Frames, FrameTruth{SegmentID: *frame.SegmentID, FrameIndex: *frame.FrameIndex, StartSample: *frame.StartSample, EndSample: *frame.EndSample, Energy: *frame.Energy, PeakPosition: *frame.PeakPosition, ExpectedEnvelope: *frame.ExpectedEnvelope, CalibratedEnvelope: *frame.Calibrated})
	}
	for i, segment := range raw.Segments {
		path := fmt.Sprintf("truth.segmentTruth[%d]", i)
		if segment.SegmentID == nil || segment.GainAdjustment == nil || segment.Calibrated == nil || segment.Reached == nil {
			return nil, newError(CodeSchema, dialogueID, path, "missing required truth field")
		}
		truth.Segments = append(truth.Segments, SegmentTruth{SegmentID: *segment.SegmentID, GainAdjustment: *segment.GainAdjustment, Calibrated: append([]int64(nil), segment.Calibrated...), Reached: *segment.Reached})
	}
	return truth, nil
}

func loadPCM(raw *rawPCM, dialogueID, path string, p *Project) error {
	if raw.Channels == nil {
		return newError(CodeSchema, dialogueID, path+".channels", "missing required field")
	}
	if raw.Format == nil {
		return newError(CodeSchema, dialogueID, path+".format", "missing required field")
	}
	if raw.Samples == nil {
		return newError(CodeSchema, dialogueID, path+".samples", "missing required field")
	}
	if *raw.Channels != 1 {
		return newError(CodeSchema, dialogueID, path+".channels", fmt.Sprintf("mono PCM required, got %d channels", *raw.Channels))
	}
	if *raw.Format != "pcm_s16le" {
		return newError(CodeSchema, dialogueID, path+".format", "unsupported PCM format "+*raw.Format)
	}
	if int64(len(raw.Samples)) != p.DeclaredTotalSamples {
		return newError(CodeSchema, dialogueID, path+".samples", fmt.Sprintf("sample count %d does not match declared_total_samples %d", len(raw.Samples), p.DeclaredTotalSamples))
	}
	p.PCM = make([]int16, len(raw.Samples))
	for i, sample := range raw.Samples {
		if sample < -32768 || sample > 32767 {
			return newError(CodeSchema, dialogueID, fmt.Sprintf("%s.samples[%d]", path, i), "sample outside int16 range")
		}
		p.PCM[i] = int16(sample)
	}
	return nil
}

func scaleAppend(result *LoadResult, value, from, to int64, field, location string) (int64, error) {
	converted, traces, err := ConvertScale(value, from, to, field, location)
	if err != nil {
		return 0, err
	}
	result.Truncations = append(result.Truncations, traces...)
	return converted, nil
}

func millisecondsToSamplesAppend(ms, sampleRate int64, field, location string, traces []Truncation) (int64, []Truncation, error) {
	value, more, err := MillisecondsToSamples(ms, sampleRate, field, location)
	return value, append(traces, more...), err
}

func normalizeCategory(value string) string {
	switch value {
	case "VOWEL", "Vowel", "vowel":
		return string(PhonemeVowel)
	case "CONSONANT", "Consonant", "consonant":
		return string(PhonemeConsonant)
	case "SILENCE", "Silence", "silence":
		return string(PhonemeSilence)
	default:
		return value
	}
}

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}
