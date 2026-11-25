package dialogue

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	SchemaV1ID = "dialogue-v1"
	SchemaV2ID = "dialogue.v2.flat"
	SchemaV3ID = "dialogue.v3.scenes"
)

type envelope struct {
	Schema        string `json:"schema"`
	SchemaVersion *int   `json:"schema_version"`
}

type rawClipV1 struct {
	ID                *string `json:"id"`
	Scene             string  `json:"scene"`
	StartMS           *int64  `json:"start_ms"`
	DurationMS        *int64  `json:"duration_ms"`
	GainPM            *int64  `json:"gain_pm"`
	ToneHz            int     `json:"tone_hz"`
	PCM               []int16 `json:"pcm"`
	DeclaredSamples   int     `json:"declared_samples"`
	ForceStageFailure bool    `json:"force_stage_failure"`
}

type rawV1 struct {
	Schema        string      `json:"schema"`
	DialogueID    *string     `json:"dialogue_id"`
	Seed          int64       `json:"seed"`
	SampleRate    *int        `json:"sample_rate"`
	FrameRate     *int        `json:"frame_rate"`
	SessionGainPM *int64      `json:"session_gain_pm"`
	TargetRMSPM   *int64      `json:"target_rms_pm"`
	MaxAdjustPM   *int64      `json:"max_adjust_pm"`
	TotalBudgetPM *int64      `json:"total_budget_pm"`
	Clips         []rawClipV1 `json:"clips"`
}

type rawClipV2 struct {
	ClipID              *string `json:"clipId"`
	ScenePath           string  `json:"scenePath"`
	StartSample         *int64  `json:"startSample"`
	SampleCount         *int    `json:"sampleCount"`
	GainBP              *int64  `json:"gainBasisPoints"`
	ToneHz              int     `json:"toneHz"`
	PCM                 []int16 `json:"pcmS16LE"`
	DeclaredSampleCount *int    `json:"declaredSampleCount"`
	ForceStageFailure   bool    `json:"forceStageFailure"`
}

type rawV2 struct {
	SchemaVersion *int        `json:"schema_version"`
	DialogueID    *string     `json:"dialogueId"`
	Seed          int64       `json:"seed"`
	SampleRate    *int        `json:"sampleRate"`
	FPS           *int        `json:"fps"`
	SessionGainBP *int64      `json:"sessionGainBasisPoints"`
	TargetLoudCDB *int64      `json:"targetLoudnessCentibel"`
	MaxAdjustBP   *int64      `json:"maxGainAdjustBasisPoints"`
	TotalBudgetBP *int64      `json:"totalBudgetBasisPoints"`
	Clips         []rawClipV2 `json:"clips"`
}

type rawClipV3 struct {
	ID                  *string `json:"id"`
	StartSample         *int64  `json:"start_sample"`
	SampleCount         *int    `json:"sample_count"`
	GainLinearMicro     *int64  `json:"gain_linear_micro"`
	GainBP              *int64  `json:"gain_bp,omitempty"`
	ToneHz              int     `json:"tone_hz"`
	PCM                 []int16 `json:"pcm_s16"`
	DeclaredSampleCount *int    `json:"declared_sample_count"`
	ForceStageFailure   bool    `json:"force_stage_failure"`
}

type rawSceneV3 struct {
	Name     string       `json:"name"`
	Clips    []rawClipV3  `json:"clips"`
	Children []rawSceneV3 `json:"children"`
}

type rawAudioV3 struct {
	SampleRate *int `json:"sample_rate"`
	FrameRate  *int `json:"frame_rate"`
}

type rawCalibrationV3 struct {
	SessionGainBP  *int64 `json:"session_gain_bp"`
	TargetLoudMDB  *int64 `json:"target_loud_mdb"`
	TargetAmpMicro *int64 `json:"target_amp_micro"`
	MaxAdjustBP    *int64 `json:"max_adjust_bp"`
	TotalBudgetBP  *int64 `json:"total_budget_bp"`
}

type rawV3 struct {
	SchemaVersion *int              `json:"schema_version"`
	DialogueID    *string           `json:"dialogue_id"`
	Seed          int64             `json:"seed"`
	Audio         *rawAudioV3       `json:"audio"`
	Calibration   *rawCalibrationV3 `json:"calibration"`
	Scenes        []rawSceneV3      `json:"scenes"`
	Markers       []Marker          `json:"calibration_markers"`
	// FlatClips is accepted for tolerance; canonical export uses scenes.
	Clips []rawClipV3 `json:"clips"`
}

// LoadDocument detects a schema version and maps it to the unified model.
func LoadDocument(data []byte, sourceID string) (*Project, error) {
	var head envelope
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, schemaError(sourceID, "$", "invalid JSON: %v", err)
	}
	version := 0
	if head.SchemaVersion != nil {
		version = *head.SchemaVersion
	}
	if strings.EqualFold(head.Schema, SchemaV1ID) || head.Schema == "dialogue-v1-flat" {
		version = 1
	}
	switch version {
	case 1:
		var raw rawV1
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, schemaError(sourceID, "$", "invalid v1 document: %v", err)
		}
		return loadV1(&raw, sourceID)
	case 2:
		var raw rawV2
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, schemaError(sourceID, "$", "invalid v2 document: %v", err)
		}
		return loadV2(&raw, sourceID)
	case 3:
		var raw rawV3
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, schemaError(sourceID, "$", "invalid v3 document: %v", err)
		}
		return loadV3(&raw, sourceID)
	default:
		path := "$.schema_version"
		if head.Schema != "" && head.SchemaVersion == nil {
			path = "$.schema"
		}
		return nil, schemaError(sourceID, path, "unsupported schema version %q", schemaDescriptor(head))
	}
}

func schemaDescriptor(h envelope) string {
	if h.SchemaVersion != nil {
		return fmt.Sprintf("%d", *h.SchemaVersion)
	}
	return h.Schema
}

func requireString(v *string, sourceID, path string) (string, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return "", schemaError(sourceID, path, "required field is missing or empty")
	}
	return *v, nil
}

func requireInt64(v *int64, sourceID, path string) (int64, error) {
	if v == nil {
		return 0, schemaError(sourceID, path, "required field is missing")
	}
	return *v, nil
}

func requireInt(v *int, sourceID, path string) (int, error) {
	if v == nil {
		return 0, schemaError(sourceID, path, "required field is missing")
	}
	return *v, nil
}

func addTrace(p *Project, path, sourceField, rule string, old, new, defaultValue any, tr *Truncation) {
	p.Traces = append(p.Traces, FieldTrace{
		Path:          path,
		SourceVersion: p.SchemaVersion,
		SourceField:   sourceField,
		Rule:          rule,
		OldValue:      old,
		NewValue:      new,
		DefaultValue:  defaultValue,
		Truncation:    tr,
	})
}

func defaultValueInt(v *int, def int, p *Project, path, sourceField string) int {
	if v != nil {
		return *v
	}
	addTrace(p, path, sourceField, "fill_declared_default", nil, def, def, nil)
	return def
}

func defaultValueInt64(v *int64, def int64, p *Project, path, sourceField string) int64 {
	if v != nil {
		return *v
	}
	addTrace(p, path, sourceField, "fill_declared_default", nil, def, def, nil)
	return def
}

func newLoadedProject(version int, sourceID string) *Project {
	return &Project{
		SchemaVersion:    version,
		SourceFile:       sourceID,
		AlgorithmVersion: AlgorithmVersion,
		QuantVersion:     QuantVersion,
		Traces:           make([]FieldTrace, 0),
	}
}

func loadV1(raw *rawV1, sourceID string) (*Project, error) {
	p := newLoadedProject(1, sourceID)
	id, err := requireString(raw.DialogueID, sourceID, "$.dialogue_id")
	if err != nil {
		return nil, err
	}
	p.DialogueID = id
	p.Seed = raw.Seed
	p.SampleRate = defaultValueInt(raw.SampleRate, DefaultSampleRate, p, "$.sample_rate", "sample_rate")
	p.FrameRate = defaultValueInt(raw.FrameRate, DefaultFrameRate, p, "$.frame_rate", "frame_rate")
	sessionPM := defaultValueInt64(raw.SessionGainPM, 1000, p, "$.session_gain_pm", "session_gain_pm")
	sessionBP, convErr := PermilleToBasis(sessionPM, "$.session_gain_pm")
	if convErr != nil {
		return nil, convErr
	}
	p.SessionGainBP = sessionBP
	addTrace(p, "$.session_gain_pm", "session_gain_pm", "permille_to_basis_exact",
		sessionPM, sessionBP, nil, nil)

	targetPM, err := requireInt64(raw.TargetRMSPM, sourceID, "$.target_rms_pm")
	if err != nil {
		return nil, err
	}
	addTrace(p, "$.target_rms_pm", "target_rms_pm",
		"permille_amplitude_to_linear_micro_then_db", targetPM,
		map[string]int64{"target_amp_micro": p.TargetAmpMicro, "target_loud_mdb": p.TargetLoudMDB},
		nil, nil)
	p.TargetAmpMicro = targetPM * (LinearScale / PermilleScale)
	p.TargetLoudMDB, _, err = LinearToDBMilli(p.TargetAmpMicro, "$.target_rms_pm")
	if err != nil {
		return nil, err
	}
	maxPM := defaultValueInt64(raw.MaxAdjustPM, 250, p, "$.max_adjust_pm", "max_adjust_pm")
	maxBP, _ := PermilleToBasis(maxPM, "$.max_adjust_pm")
	p.MaxAdjustBP = maxBP
	budgetPM := defaultValueInt64(raw.TotalBudgetPM, 1200, p, "$.total_budget_pm", "total_budget_pm")
	budgetBP, _ := PermilleToBasis(budgetPM, "$.total_budget_pm")
	p.TotalBudgetBP = budgetBP

	if raw.Clips == nil {
		return nil, schemaError(sourceID, "$.clips", "required field is missing")
	}
	p.Clips = make([]*Clip, 0, len(raw.Clips))
	for i := range raw.Clips {
		rc := &raw.Clips[i]
		idPath := fmt.Sprintf("$.clips[%d].id", i)
		clipID, err := requireString(rc.ID, sourceID, idPath)
		if err != nil {
			return nil, err
		}
		startMS, err := requireInt64(rc.StartMS, sourceID, fmt.Sprintf("$.clips[%d].start_ms", i))
		if err != nil {
			return nil, err
		}
		durationMS, err := requireInt64(rc.DurationMS, sourceID, fmt.Sprintf("$.clips[%d].duration_ms", i))
		if err != nil {
			return nil, err
		}
		startSample, tr, convErr := MillisToSamples(startMS, int64(p.SampleRate), RoundNearest,
			fmt.Sprintf("$.clips[%d].start_ms", i))
		if convErr != nil {
			return nil, convErr
		}
		durationSamples, dtr, convErr := MillisToSamples(durationMS, int64(p.SampleRate), RoundNearest,
			fmt.Sprintf("$.clips[%d].duration_ms", i))
		if convErr != nil {
			return nil, convErr
		}
		gainPM := defaultValueInt64(rc.GainPM, 1000, p,
			fmt.Sprintf("$.clips[%d].gain_pm", i), "clips[].gain_pm")
		gainBP, convErr := PermilleToBasis(gainPM, fmt.Sprintf("$.clips[%d].gain_pm", i))
		if convErr != nil {
			return nil, convErr
		}
		addTrace(p, fmt.Sprintf("$.clips[%d].gain_pm", i), "gain_pm",
			"permille_to_basis_exact", gainPM, gainBP, nil, nil)
		scenePath := []string{}
		if rc.Scene != "" {
			scenePath = strings.Split(rc.Scene, "/")
		}
		declared := rc.DeclaredSamples
		if declared == 0 {
			declared = len(rc.PCM)
		}
		clip := &Clip{
			ID:                  clipID,
			ScenePath:           scenePath,
			StartSample:         startSample,
			Samples:             append([]int16(nil), rc.PCM...),
			DeclaredSampleCount: declared,
			GainBP:              gainBP,
			ToneHz:              rc.ToneHz,
			AudioHash:           AudioHash(p.SampleRate, startSample, rc.PCM),
			ForceStageFailure:   rc.ForceStageFailure,
		}
		addTrace(p, fmt.Sprintf("$.clips[%d].start_ms", i), "start_ms", "ms_to_samples_round_nearest",
			startMS, startSample, nil, &tr)
		addTrace(p, fmt.Sprintf("$.clips[%d].duration_ms", i), "duration_ms", "ms_to_samples_round_nearest",
			durationMS, durationSamples, nil, &dtr)
		p.Clips = append(p.Clips, clip)
	}
	p.SceneTree = BuildSceneTree(p.Clips)
	return p, nil
}

func loadV2(raw *rawV2, sourceID string) (*Project, error) {
	if raw.SchemaVersion == nil || *raw.SchemaVersion != 2 {
		return nil, schemaError(sourceID, "$.schema_version", "schema_version must equal 2")
	}
	p := newLoadedProject(2, sourceID)
	id, err := requireString(raw.DialogueID, sourceID, "$.dialogueId")
	if err != nil {
		return nil, err
	}
	p.DialogueID = id
	p.Seed = raw.Seed
	p.SampleRate = defaultValueInt(raw.SampleRate, DefaultSampleRate, p, "$.sampleRate", "sampleRate")
	p.FrameRate = defaultValueInt(raw.FPS, DefaultFrameRate, p, "$.fps", "fps")
	p.SessionGainBP = defaultValueInt64(raw.SessionGainBP, 10_000, p,
		"$.sessionGainBasisPoints", "sessionGainBasisPoints")
	if raw.SessionGainBP != nil {
		addTrace(p, "$.sessionGainBasisPoints", "sessionGainBasisPoints",
			"basis_points_direct", *raw.SessionGainBP, p.SessionGainBP, nil, nil)
	}
	targetCDB, err := requireInt64(raw.TargetLoudCDB, sourceID, "$.targetLoudnessCentibel")
	if err != nil {
		return nil, err
	}
	p.TargetLoudMDB = targetCDB * 10
	p.TargetAmpMicro, _, err = DBMilliToLinear(p.TargetLoudMDB, "$.targetLoudnessCentibel")
	if err != nil {
		return nil, err
	}
	addTrace(p, "$.targetLoudnessCentibel", "targetLoudnessCentibel",
		"centibel_to_millibel_then_integer_db_to_linear", targetCDB,
		map[string]int64{"target_loud_mdb": p.TargetLoudMDB, "target_amp_micro": p.TargetAmpMicro}, nil, nil)
	addTrace(p, "$.fps", "fps", "map_frame_rate", raw.FPS, p.FrameRate, nil, nil)
	p.MaxAdjustBP = defaultValueInt64(raw.MaxAdjustBP, 2_500, p,
		"$.maxGainAdjustBasisPoints", "maxGainAdjustBasisPoints")
	p.TotalBudgetBP = defaultValueInt64(raw.TotalBudgetBP, 12_000, p,
		"$.totalBudgetBasisPoints", "totalBudgetBasisPoints")
	if raw.Clips == nil {
		return nil, schemaError(sourceID, "$.clips", "required field is missing")
	}
	p.Clips = make([]*Clip, 0, len(raw.Clips))
	for i := range raw.Clips {
		rc := &raw.Clips[i]
		clipID, err := requireString(rc.ClipID, sourceID, fmt.Sprintf("$.clips[%d].clipId", i))
		if err != nil {
			return nil, err
		}
		start, err := requireInt64(rc.StartSample, sourceID, fmt.Sprintf("$.clips[%d].startSample", i))
		if err != nil {
			return nil, err
		}
		gain := defaultValueInt64(rc.GainBP, 10_000, p,
			fmt.Sprintf("$.clips[%d].gainBasisPoints", i), "clips[].gainBasisPoints")
		if rc.GainBP != nil {
			addTrace(p, fmt.Sprintf("$.clips[%d].gainBasisPoints", i), "gainBasisPoints",
				"basis_points_direct", *rc.GainBP, gain, nil, nil)
		}
		declared := len(rc.PCM)
		if rc.DeclaredSampleCount != nil {
			declared = *rc.DeclaredSampleCount
		} else if rc.SampleCount != nil {
			declared = *rc.SampleCount
		}
		scenePath := []string{}
		if rc.ScenePath != "" {
			scenePath = strings.Split(rc.ScenePath, "/")
		}
		clip := &Clip{
			ID:                  clipID,
			ScenePath:           scenePath,
			StartSample:         start,
			Samples:             append([]int16(nil), rc.PCM...),
			DeclaredSampleCount: declared,
			GainBP:              gain,
			ToneHz:              rc.ToneHz,
			AudioHash:           AudioHash(p.SampleRate, start, rc.PCM),
			ForceStageFailure:   rc.ForceStageFailure,
		}
		p.Clips = append(p.Clips, clip)
	}
	p.SceneTree = BuildSceneTree(p.Clips)
	return p, nil
}

func loadV3(raw *rawV3, sourceID string) (*Project, error) {
	if raw.SchemaVersion == nil || *raw.SchemaVersion != 3 {
		return nil, schemaError(sourceID, "$.schema_version", "schema_version must equal 3")
	}
	p := newLoadedProject(3, sourceID)
	id, err := requireString(raw.DialogueID, sourceID, "$.dialogue_id")
	if err != nil {
		return nil, err
	}
	p.DialogueID = id
	p.Seed = raw.Seed
	if raw.Audio != nil {
		p.SampleRate = defaultValueInt(raw.Audio.SampleRate, DefaultSampleRate, p,
			"$.audio.sample_rate", "audio.sample_rate")
		p.FrameRate = defaultValueInt(raw.Audio.FrameRate, DefaultFrameRate, p,
			"$.audio.frame_rate", "audio.frame_rate")
	} else {
		p.SampleRate = DefaultSampleRate
		p.FrameRate = DefaultFrameRate
		addTrace(p, "$.audio", "audio", "fill_declared_default", nil,
			map[string]int{"sample_rate": p.SampleRate, "frame_rate": p.FrameRate},
			map[string]int{"sample_rate": p.SampleRate, "frame_rate": p.FrameRate}, nil)
	}
	cal := raw.Calibration
	if cal == nil {
		cal = &rawCalibrationV3{}
		addTrace(p, "$.calibration", "calibration", "fill_declared_default", nil,
			"default calibration", "session gain 10000 BP", nil)
	}
	p.SessionGainBP = defaultValueInt64(cal.SessionGainBP, 10_000, p,
		"$.calibration.session_gain_bp", "calibration.session_gain_bp")
	p.MaxAdjustBP = defaultValueInt64(cal.MaxAdjustBP, 2_500, p,
		"$.calibration.max_adjust_bp", "calibration.max_adjust_bp")
	p.TotalBudgetBP = defaultValueInt64(cal.TotalBudgetBP, 12_000, p,
		"$.calibration.total_budget_bp", "calibration.total_budget_bp")
	switch {
	case cal.TargetAmpMicro != nil:
		p.TargetAmpMicro = *cal.TargetAmpMicro
		p.TargetLoudMDB, _, err = LinearToDBMilli(p.TargetAmpMicro, "$.calibration.target_amp_micro")
		if err != nil {
			return nil, err
		}
		addTrace(p, "$.calibration.target_amp_micro", "target_amp_micro",
			"linear_micro_to_milli_db", *cal.TargetAmpMicro, p.TargetLoudMDB, nil, nil)
		if cal.TargetLoudMDB != nil {
			delta := abs64(*cal.TargetLoudMDB - p.TargetLoudMDB)
			if delta > LoudnessToleranceMDB {
				return nil, parameterError("", "$.calibration.target_loud_mdb",
					"target dB and linear amplitude disagree beyond tolerance")
			}
			p.TargetLoudMDB = *cal.TargetLoudMDB
		}
	case cal.TargetLoudMDB != nil:
		p.TargetLoudMDB = *cal.TargetLoudMDB
		p.TargetAmpMicro, _, err = DBMilliToLinear(p.TargetLoudMDB, "$.calibration.target_loud_mdb")
		if err != nil {
			return nil, err
		}
		addTrace(p, "$.calibration.target_loud_mdb", "target_loud_mdb",
			"milli_db_to_linear_micro", *cal.TargetLoudMDB, p.TargetAmpMicro, nil, nil)
	default:
		return nil, schemaError(sourceID, "$.calibration.target_loud_mdb",
			"one of target_loud_mdb or target_amp_micro is required")
	}
	p.Markers = append([]Marker(nil), raw.Markers...)
	p.Clips = make([]*Clip, 0)
	if raw.Scenes != nil {
		if err := flattenScenesV3(raw.Scenes, []string{}, "$.scenes", p, sourceID); err != nil {
			return nil, err
		}
	}
	if raw.Clips != nil {
		if err := appendClipsV3(raw.Clips, []string{}, "$.clips", p, sourceID); err != nil {
			return nil, err
		}
	}
	if len(p.Clips) == 0 {
		return nil, schemaError(sourceID, "$.scenes", "at least one clip is required")
	}
	p.SceneTree = BuildSceneTree(p.Clips)
	return p, nil
}

func flattenScenesV3(scenes []rawSceneV3, parents []string, path string, p *Project, sourceID string) error {
	for i := range scenes {
		scene := &scenes[i]
		if scene.Name == "" {
			return schemaError(sourceID, fmt.Sprintf("%s[%d].name", path, i), "scene name is required")
		}
		scenePath := append(append([]string(nil), parents...), scene.Name)
		sceneJSONPath := fmt.Sprintf("%s[%d]", path, i)
		if err := appendClipsV3(scene.Clips, scenePath, sceneJSONPath+".clips", p, sourceID); err != nil {
			return err
		}
		if err := flattenScenesV3(scene.Children, scenePath, sceneJSONPath+".children", p, sourceID); err != nil {
			return err
		}
	}
	return nil
}

func appendClipsV3(clips []rawClipV3, scenePath []string, path string, p *Project, sourceID string) error {
	for i := range clips {
		rc := &clips[i]
		idPath := fmt.Sprintf("%s[%d].id", path, i)
		clipID, err := requireString(rc.ID, sourceID, idPath)
		if err != nil {
			return err
		}
		start, err := requireInt64(rc.StartSample, sourceID, fmt.Sprintf("%s[%d].start_sample", path, i))
		if err != nil {
			return err
		}
		gain, err := resolveV3Gain(rc, path, i, clipID)
		if err != nil {
			return err
		}
		if rc.GainLinearMicro != nil {
			addTrace(p, fmt.Sprintf("%s[%d].gain_linear_micro", path, i), "gain_linear_micro",
				"linear_micro_to_basis_round_nearest", *rc.GainLinearMicro, gain, nil, nil)
		}
		declared := len(rc.PCM)
		if rc.DeclaredSampleCount != nil {
			declared = *rc.DeclaredSampleCount
		} else if rc.SampleCount != nil {
			declared = *rc.SampleCount
		}
		clip := &Clip{
			ID:                  clipID,
			ScenePath:           append([]string(nil), scenePath...),
			StartSample:         start,
			Samples:             append([]int16(nil), rc.PCM...),
			DeclaredSampleCount: declared,
			GainBP:              gain,
			ToneHz:              rc.ToneHz,
			AudioHash:           AudioHash(p.SampleRate, start, rc.PCM),
			ForceStageFailure:   rc.ForceStageFailure,
		}
		p.Clips = append(p.Clips, clip)
	}
	return nil
}

func resolveV3Gain(rc *rawClipV3, path string, i int, clipID string) (int64, error) {
	gainPath := fmt.Sprintf("%s[%d].gain_linear_micro", path, i)
	if rc.GainLinearMicro == nil && rc.GainBP == nil {
		return 10_000, nil
	}
	if rc.GainLinearMicro != nil {
		bp, _, err := LinearMicroToBasis(*rc.GainLinearMicro, RoundNearest, gainPath)
		if err != nil {
			return 0, err
		}
		if rc.GainBP != nil {
			convertedLinear, err := BasisToLinearMicro(*rc.GainBP, gainPath)
			if err != nil {
				return 0, err
			}
			if abs64(convertedLinear-*rc.GainLinearMicro) > LinearScale/BasisScale {
				return 0, &Error{
					Code:     CodePara,
					Message:  "gain basis-point and linear scales contradict each other",
					Path:     gainPath,
					Field:    "gain",
					ClipID:   clipID,
					Operands: []int64{*rc.GainLinearMicro, *rc.GainBP},
					Details: map[string]int64{
						"linear_micro_from_bp": convertedLinear,
						"bp_from_linear":       bp,
					},
				}
			}
		}
		return bp, nil
	}
	return *rc.GainBP, nil
}
