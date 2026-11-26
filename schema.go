package loudness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

type wireV1 struct {
	Schema              *string       `json:"schema"`
	ProjectID           *string       `json:"project_id"`
	Seed                *int64        `json:"seed"`
	SampleRate          *int64        `json:"sample_rate"`
	FrameLength         *int64        `json:"frame_length"`
	FrameShift          *int64        `json:"frame_shift"`
	SessionGainQ12      *int64        `json:"session_gain_q12"`
	TargetLoudnessDBQ8  *int64        `json:"target_loudness_db_q8"`
	MaxGainQ12          *int64        `json:"max_gain_q12"`
	LoudnessToleranceQ8 *int64        `json:"loudness_tolerance_q8"`
	Clips               *[]wireV1Clip `json:"clips"`
}

type wireV1Clip struct {
	ClipID           *int     `json:"clip_id"`
	StartSample      *int64   `json:"start_sample"`
	EndSample        *int64   `json:"end_sample"`
	Category         *string  `json:"category"`
	F0TrackHz        *[]int64 `json:"f0_track_hz"`
	LoudnessWeightQ8 *int64   `json:"loudness_weight_q8"`
	GainQ12          *int64   `json:"gain_q12"`
	BaseAmplitudeQ12 *int64   `json:"base_amplitude_q12"`
}

type wireV2 struct {
	Schema               *string        `json:"schema"`
	ProjectID            *string        `json:"project_id"`
	Seed                 *int64         `json:"seed"`
	SampleRate           *int64         `json:"sample_rate"`
	FrameLengthMS        *int64         `json:"frame_length_ms"`
	FrameShiftMS         *int64         `json:"frame_shift_ms"`
	SessionGainBP        *int64         `json:"session_gain_bp"`
	TargetLoudnessCDB    *int64         `json:"target_loudness_cdb"`
	MaxGainBP            *int64         `json:"max_gain_bp"`
	LoudnessToleranceCDB *int64         `json:"loudness_tolerance_cdb"`
	Scenes               *[]wireV2Scene `json:"scenes"`
}

type wireV2Scene struct {
	Name  *string       `json:"scene_name"`
	Clips *[]wireV2Clip `json:"clips"`
}

type wireV2Clip struct {
	ID                *int     `json:"id"`
	StartMS           *int64   `json:"start_ms"`
	EndMS             *int64   `json:"end_ms"`
	Category          *string  `json:"category"`
	F0TrackHz         *[]int64 `json:"f0_track_hz"`
	LoudnessWeightBP  *int64   `json:"loudness_weight_bp"`
	GainBP            *int64   `json:"gain_bp"`
	BaseAmplitudeQ12  *int64   `json:"base_amplitude_q12"`
	CalibrationMarker *string  `json:"calibration_marker,omitempty"`
}

type wireV3 struct {
	Schema               *string          `json:"schema"`
	ProjectID            *string          `json:"projectId"`
	Seed                 *int64           `json:"seed"`
	SampleRate           *int64           `json:"sampleRate"`
	FrameLengthMS        *int64           `json:"frameLengthMs"`
	FrameShiftMS         *int64           `json:"frameShiftMs"`
	SessionGainDBQ8      *int64           `json:"sessionGainDbQ8"`
	TargetLoudnessMDB    *int64           `json:"targetLoudnessMdb"`
	MaxGainDBQ8          *int64           `json:"maxGainDbQ8"`
	LoudnessToleranceMDB *int64           `json:"loudnessToleranceMdb"`
	Segments             *[]wireV3Segment `json:"segments"`
}

type wireV3Segment struct {
	SegmentID        *int               `json:"segmentId"`
	Scene            *string            `json:"scene,omitempty"`
	StartMS          *int64             `json:"startMs"`
	EndMS            *int64             `json:"endMs"`
	EmitterClass     *string            `json:"emitterClass"`
	PitchTrackHz     *[]int64           `json:"pitchTrackHz"`
	LoudnessWeightQ8 *int64             `json:"loudnessWeightQ8"`
	GainDBQ8         *int64             `json:"gainDbQ8"`
	AmplitudeQ12     *int64             `json:"amplitudeQ12"`
	Calibration      *wireV3Calibration `json:"calibration,omitempty"`
}

type wireV3Calibration struct {
	Marker *string `json:"marker"`
}

// Load detects the schema version and normalizes it to the internal model.
func Load(data []byte) (*Project, error) {
	var head struct {
		Schema *string `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, &DomainError{Code: ESchema, Path: "$", Detail: "invalid JSON", Cause: err}
	}
	if head.Schema == nil {
		return nil, &DomainError{Code: ESchema, Path: "$.schema", Detail: "required field is missing"}
	}
	version := Version(*head.Schema)
	switch version {
	case SchemaV1:
		return loadV1(data)
	case SchemaV2:
		return loadV2(data)
	case SchemaV3:
		return loadV3(data)
	default:
		return nil, &DomainError{Code: ESchema, Path: "$.schema", SourceVersion: version, Detail: "unknown schema version"}
	}
}

func required[T any](value *T, path string, version Version) (T, error) {
	var zero T
	if value == nil {
		return zero, &DomainError{Code: ESchema, Path: path, SourceVersion: version, Detail: "required field is missing"}
	}
	return *value, nil
}

func requiredString(value *string, path string, version Version) (string, error) {
	result, err := required(value, path, version)
	if err != nil {
		return "", err
	}
	if result == "" {
		return "", &DomainError{Code: ESchema, Path: path, SourceVersion: version, Detail: "required field is empty"}
	}
	return result, nil
}

func requiredSlice[T any](value *[]T, path string, version Version) ([]T, error) {
	result, err := required(value, path, version)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, &DomainError{Code: ESchema, Path: path, SourceVersion: version, Detail: "required list is empty"}
	}
	return result, nil
}

func attachErrorSource(err error, version Version, path string, field string) error {
	if err == nil {
		return nil
	}
	if domainErr, ok := AsDomainError(err); ok {
		if domainErr.SourceVersion == "" {
			domainErr.SourceVersion = version
		}
		if domainErr.Path == "" {
			domainErr.Path = path
		}
		if domainErr.Field == "" {
			domainErr.Field = field
		}
	}
	return err
}

func appendMapped(traces *[]FieldTrace, version Version, sourcePath, internalPath, value string) {
	*traces = append(*traces, FieldTrace{
		SourceVersion: version,
		SourcePath:    sourcePath,
		InternalPath:  internalPath,
		Kind:          TraceMap,
		OldValue:      value,
		NewValue:      value,
	})
}

func appendConverted(traces *[]FieldTrace, version Version, sourcePath, internalPath, oldValue, newValue string, trace Truncation) {
	traceCopy := trace
	*traces = append(*traces, FieldTrace{
		SourceVersion: version,
		SourcePath:    sourcePath,
		InternalPath:  internalPath,
		Kind:          TraceConvert,
		OldValue:      oldValue,
		NewValue:      newValue,
		Truncation:    &traceCopy,
	})
}

func loadV1(data []byte) (*Project, error) {
	var wire wireV1
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, &DomainError{Code: ESchema, Path: "$", SourceVersion: SchemaV1, Detail: "invalid v1 JSON", Cause: err}
	}
	projectID, err := requiredString(wire.ProjectID, "$.project_id", SchemaV1)
	if err != nil {
		return nil, err
	}
	seed, err := required(wire.Seed, "$.seed", SchemaV1)
	if err != nil {
		return nil, err
	}
	sampleRate, err := required(wire.SampleRate, "$.sample_rate", SchemaV1)
	if err != nil {
		return nil, err
	}
	frameLength, err := required(wire.FrameLength, "$.frame_length", SchemaV1)
	if err != nil {
		return nil, err
	}
	frameShift, err := required(wire.FrameShift, "$.frame_shift", SchemaV1)
	if err != nil {
		return nil, err
	}
	sessionGain, err := required(wire.SessionGainQ12, "$.session_gain_q12", SchemaV1)
	if err != nil {
		return nil, err
	}
	target, err := required(wire.TargetLoudnessDBQ8, "$.target_loudness_db_q8", SchemaV1)
	if err != nil {
		return nil, err
	}
	maxGain, err := required(wire.MaxGainQ12, "$.max_gain_q12", SchemaV1)
	if err != nil {
		return nil, err
	}
	tolerance, err := required(wire.LoudnessToleranceQ8, "$.loudness_tolerance_q8", SchemaV1)
	if err != nil {
		return nil, err
	}
	clipsWire, err := requiredSlice(wire.Clips, "$.clips", SchemaV1)
	if err != nil {
		return nil, err
	}
	traces := make([]FieldTrace, 0, 16+len(clipsWire)*7)
	appendMapped(&traces, SchemaV1, "$.project_id", "$.id", projectID)
	appendMapped(&traces, SchemaV1, "$.seed", "$.seed", strconv.FormatInt(seed, 10))
	appendMapped(&traces, SchemaV1, "$.sample_rate", "$.sampleRate", strconv.FormatInt(sampleRate, 10))
	appendMapped(&traces, SchemaV1, "$.frame_length", "$.frameLength", strconv.FormatInt(frameLength, 10))
	appendMapped(&traces, SchemaV1, "$.frame_shift", "$.frameShift", strconv.FormatInt(frameShift, 10))
	appendMapped(&traces, SchemaV1, "$.session_gain_q12", "$.sessionGainQ12", strconv.FormatInt(sessionGain, 10))
	appendMapped(&traces, SchemaV1, "$.target_loudness_db_q8", "$.targetLoudnessDBQ8", strconv.FormatInt(target, 10))
	appendMapped(&traces, SchemaV1, "$.max_gain_q12", "$.maxGainQ12", strconv.FormatInt(maxGain, 10))
	appendMapped(&traces, SchemaV1, "$.loudness_tolerance_q8", "$.loudnessToleranceQ8", strconv.FormatInt(tolerance, 10))

	clips := make([]Clip, 0, len(clipsWire))
	for i := range clipsWire {
		sourcePath := fmt.Sprintf("$.clips[%d]", i)
		internalPath := fmt.Sprintf("$.clips[%d]", i)
		id, err := required(clipsWire[i].ClipID, sourcePath+".clip_id", SchemaV1)
		if err != nil {
			return nil, err
		}
		start, err := required(clipsWire[i].StartSample, sourcePath+".start_sample", SchemaV1)
		if err != nil {
			return nil, err
		}
		end, err := required(clipsWire[i].EndSample, sourcePath+".end_sample", SchemaV1)
		if err != nil {
			return nil, err
		}
		categoryRaw, err := requiredString(clipsWire[i].Category, sourcePath+".category", SchemaV1)
		if err != nil {
			return nil, err
		}
		f0, err := requiredSlice(clipsWire[i].F0TrackHz, sourcePath+".f0_track_hz", SchemaV1)
		if err != nil {
			return nil, err
		}
		weight, err := required(clipsWire[i].LoudnessWeightQ8, sourcePath+".loudness_weight_q8", SchemaV1)
		if err != nil {
			return nil, err
		}
		gain, err := required(clipsWire[i].GainQ12, sourcePath+".gain_q12", SchemaV1)
		if err != nil {
			return nil, err
		}
		amplitude, err := required(clipsWire[i].BaseAmplitudeQ12, sourcePath+".base_amplitude_q12", SchemaV1)
		if err != nil {
			return nil, err
		}
		clip := Clip{
			ID: id, StartSample: start, EndSample: end, Category: Category(categoryRaw),
			F0TrackHz: append([]int64(nil), f0...), AmplitudeQ12: amplitude,
			LoudnessWeightQ8: weight, GainQ12: gain,
		}
		clips = append(clips, clip)
		appendMapped(&traces, SchemaV1, sourcePath+".clip_id", internalPath+".id", strconv.Itoa(id))
		appendMapped(&traces, SchemaV1, sourcePath+".start_sample", internalPath+".startSample", strconv.FormatInt(start, 10))
		appendMapped(&traces, SchemaV1, sourcePath+".end_sample", internalPath+".endSample", strconv.FormatInt(end, 10))
		appendMapped(&traces, SchemaV1, sourcePath+".category", internalPath+".category", categoryRaw)
		appendMapped(&traces, SchemaV1, sourcePath+".f0_track_hz", internalPath+".f0TrackHz", fmt.Sprint(f0))
		appendMapped(&traces, SchemaV1, sourcePath+".loudness_weight_q8", internalPath+".loudnessWeightQ8", strconv.FormatInt(weight, 10))
		appendMapped(&traces, SchemaV1, sourcePath+".gain_q12", internalPath+".gainQ12", strconv.FormatInt(gain, 10))
		appendMapped(&traces, SchemaV1, sourcePath+".base_amplitude_q12", internalPath+".amplitudeQ12", strconv.FormatInt(amplitude, 10))
	}
	project := &Project{
		FormatVersion: SchemaV1, ID: projectID, Seed: seed, SampleRate: sampleRate,
		FrameLength: frameLength, FrameShift: frameShift, SessionGainQ12: sessionGain,
		TargetLoudnessDBQ8: target, MaxGainQ12: maxGain, LoudnessToleranceQ8: tolerance,
		Clips: clips, Quantization: DefaultQuantization(),
		Provenance: Provenance{OriginalVersion: SchemaV1, CurrentVersion: SchemaV1, Traces: traces},
	}
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	return project, nil
}

func loadV2(data []byte) (*Project, error) {
	var wire wireV2
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, &DomainError{Code: ESchema, Path: "$", SourceVersion: SchemaV2, Detail: "invalid v2 JSON", Cause: err}
	}
	projectID, err := requiredString(wire.ProjectID, "$.project_id", SchemaV2)
	if err != nil {
		return nil, err
	}
	seed, err := required(wire.Seed, "$.seed", SchemaV2)
	if err != nil {
		return nil, err
	}
	sampleRate, err := required(wire.SampleRate, "$.sample_rate", SchemaV2)
	if err != nil {
		return nil, err
	}
	frameLengthMS, err := required(wire.FrameLengthMS, "$.frame_length_ms", SchemaV2)
	if err != nil {
		return nil, err
	}
	frameShiftMS, err := required(wire.FrameShiftMS, "$.frame_shift_ms", SchemaV2)
	if err != nil {
		return nil, err
	}
	sessionBP, err := required(wire.SessionGainBP, "$.session_gain_bp", SchemaV2)
	if err != nil {
		return nil, err
	}
	targetCDB, err := required(wire.TargetLoudnessCDB, "$.target_loudness_cdb", SchemaV2)
	if err != nil {
		return nil, err
	}
	maxGainBP, err := required(wire.MaxGainBP, "$.max_gain_bp", SchemaV2)
	if err != nil {
		return nil, err
	}
	toleranceCDB, err := required(wire.LoudnessToleranceCDB, "$.loudness_tolerance_cdb", SchemaV2)
	if err != nil {
		return nil, err
	}
	scenesWire, err := requiredSlice(wire.Scenes, "$.scenes", SchemaV2)
	if err != nil {
		return nil, err
	}

	traces := make([]FieldTrace, 0, 20)
	sessionGain, tr, err := ConvertScale(sessionBP, 10000, InternalGainScale, "$.session_gain_bp", RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV2, "$.session_gain_bp", "session_gain_bp")
	}
	appendConverted(&traces, SchemaV2, "$.session_gain_bp", "$.sessionGainQ12", strconv.FormatInt(sessionBP, 10), strconv.FormatInt(sessionGain, 10), tr)
	target, tr, err := ConvertScale(targetCDB, 100, InternalLoudnessScale, "$.target_loudness_cdb", RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV2, "$.target_loudness_cdb", "target_loudness_cdb")
	}
	appendConverted(&traces, SchemaV2, "$.target_loudness_cdb", "$.targetLoudnessDBQ8", strconv.FormatInt(targetCDB, 10), strconv.FormatInt(target, 10), tr)
	maxGain, tr, err := ConvertScale(maxGainBP, 10000, InternalGainScale, "$.max_gain_bp", RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV2, "$.max_gain_bp", "max_gain_bp")
	}
	appendConverted(&traces, SchemaV2, "$.max_gain_bp", "$.maxGainQ12", strconv.FormatInt(maxGainBP, 10), strconv.FormatInt(maxGain, 10), tr)
	tolerance, tr, err := ConvertScale(toleranceCDB, 100, InternalLoudnessScale, "$.loudness_tolerance_cdb", RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV2, "$.loudness_tolerance_cdb", "loudness_tolerance_cdb")
	}
	appendConverted(&traces, SchemaV2, "$.loudness_tolerance_cdb", "$.loudnessToleranceQ8", strconv.FormatInt(toleranceCDB, 10), strconv.FormatInt(tolerance, 10), tr)
	frameLength, tr, err := MillisToSamples(frameLengthMS, sampleRate, RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV2, "$.frame_length_ms", "frame_length_ms")
	}
	appendConverted(&traces, SchemaV2, "$.frame_length_ms", "$.frameLength", strconv.FormatInt(frameLengthMS, 10), strconv.FormatInt(frameLength, 10), tr)
	frameShift, tr, err := MillisToSamples(frameShiftMS, sampleRate, RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV2, "$.frame_shift_ms", "frame_shift_ms")
	}
	appendConverted(&traces, SchemaV2, "$.frame_shift_ms", "$.frameShift", strconv.FormatInt(frameShiftMS, 10), strconv.FormatInt(frameShift, 10), tr)
	appendMapped(&traces, SchemaV2, "$.project_id", "$.id", projectID)
	appendMapped(&traces, SchemaV2, "$.seed", "$.seed", strconv.FormatInt(seed, 10))
	appendMapped(&traces, SchemaV2, "$.sample_rate", "$.sampleRate", strconv.FormatInt(sampleRate, 10))

	clips := make([]Clip, 0)
	scenes := make([]Scene, 0, len(scenesWire))
	for sceneIndex := range scenesWire {
		scenePath := fmt.Sprintf("$.scenes[%d]", sceneIndex)
		sceneName, err := requiredString(scenesWire[sceneIndex].Name, scenePath+".scene_name", SchemaV2)
		if err != nil {
			return nil, err
		}
		sceneClips, err := requiredSlice(scenesWire[sceneIndex].Clips, scenePath+".clips", SchemaV2)
		if err != nil {
			return nil, err
		}
		scene := Scene{Name: sceneName}
		for clipIndex := range sceneClips {
			sourcePath := fmt.Sprintf("%s.clips[%d]", scenePath, clipIndex)
			internalIndex := len(clips)
			internalPath := fmt.Sprintf("$.clips[%d]", internalIndex)
			id, err := required(sceneClips[clipIndex].ID, sourcePath+".id", SchemaV2)
			if err != nil {
				return nil, err
			}
			startMS, err := required(sceneClips[clipIndex].StartMS, sourcePath+".start_ms", SchemaV2)
			if err != nil {
				return nil, err
			}
			endMS, err := required(sceneClips[clipIndex].EndMS, sourcePath+".end_ms", SchemaV2)
			if err != nil {
				return nil, err
			}
			categoryRaw, err := requiredString(sceneClips[clipIndex].Category, sourcePath+".category", SchemaV2)
			if err != nil {
				return nil, err
			}
			f0, err := requiredSlice(sceneClips[clipIndex].F0TrackHz, sourcePath+".f0_track_hz", SchemaV2)
			if err != nil {
				return nil, err
			}
			weightBP, err := required(sceneClips[clipIndex].LoudnessWeightBP, sourcePath+".loudness_weight_bp", SchemaV2)
			if err != nil {
				return nil, err
			}
			gainBP, err := required(sceneClips[clipIndex].GainBP, sourcePath+".gain_bp", SchemaV2)
			if err != nil {
				return nil, err
			}
			amplitude, err := required(sceneClips[clipIndex].BaseAmplitudeQ12, sourcePath+".base_amplitude_q12", SchemaV2)
			if err != nil {
				return nil, err
			}
			start, startTrace, err := MillisToSamples(startMS, sampleRate, RoundHalfUp)
			if err != nil {
				return nil, attachErrorSource(err, SchemaV2, sourcePath+".start_ms", "start_ms")
			}
			end, endTrace, err := MillisToSamples(endMS, sampleRate, RoundHalfUp)
			if err != nil {
				return nil, attachErrorSource(err, SchemaV2, sourcePath+".end_ms", "end_ms")
			}
			weight, weightTrace, err := ConvertScale(weightBP, 10000, InternalWeightScale, sourcePath+".loudness_weight_bp", RoundHalfUp)
			if err != nil {
				return nil, attachErrorSource(err, SchemaV2, sourcePath+".loudness_weight_bp", "loudness_weight_bp")
			}
			gain, gainTrace, err := ConvertScale(gainBP, 10000, InternalGainScale, sourcePath+".gain_bp", RoundHalfUp)
			if err != nil {
				return nil, attachErrorSource(err, SchemaV2, sourcePath+".gain_bp", "gain_bp")
			}
			var marker *string
			if sceneClips[clipIndex].CalibrationMarker != nil {
				value := *sceneClips[clipIndex].CalibrationMarker
				marker = &value
			}
			clips = append(clips, Clip{
				ID: id, Scene: sceneName, StartSample: start, EndSample: end,
				Category: Category(categoryRaw), F0TrackHz: append([]int64(nil), f0...),
				AmplitudeQ12: amplitude, LoudnessWeightQ8: weight, GainQ12: gain,
				CalibrationMarker: marker,
			})
			scene.ClipIDs = append(scene.ClipIDs, id)
			appendMapped(&traces, SchemaV2, scenePath+".scene_name", internalPath+".scene", sceneName)
			appendConverted(&traces, SchemaV2, sourcePath+".start_ms", internalPath+".startSample", strconv.FormatInt(startMS, 10), strconv.FormatInt(start, 10), startTrace)
			appendConverted(&traces, SchemaV2, sourcePath+".end_ms", internalPath+".endSample", strconv.FormatInt(endMS, 10), strconv.FormatInt(end, 10), endTrace)
			appendConverted(&traces, SchemaV2, sourcePath+".loudness_weight_bp", internalPath+".loudnessWeightQ8", strconv.FormatInt(weightBP, 10), strconv.FormatInt(weight, 10), weightTrace)
			appendConverted(&traces, SchemaV2, sourcePath+".gain_bp", internalPath+".gainQ12", strconv.FormatInt(gainBP, 10), strconv.FormatInt(gain, 10), gainTrace)
			if marker == nil {
				appendMapped(&traces, SchemaV2, sourcePath+".calibration_marker", internalPath+".calibrationMarker", "<absent>")
			} else {
				appendMapped(&traces, SchemaV2, sourcePath+".calibration_marker", internalPath+".calibrationMarker", *marker)
			}
		}
		scenes = append(scenes, scene)
	}
	project := &Project{
		FormatVersion: SchemaV2, ID: projectID, Seed: seed, SampleRate: sampleRate,
		FrameLength: frameLength, FrameShift: frameShift, SessionGainQ12: sessionGain,
		TargetLoudnessDBQ8: target, MaxGainQ12: maxGain, LoudnessToleranceQ8: tolerance,
		Scenes: scenes, Clips: clips, Quantization: DefaultQuantization(),
		Provenance: Provenance{OriginalVersion: SchemaV2, CurrentVersion: SchemaV2, Traces: traces},
	}
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	return project, nil
}

func loadV3(data []byte) (*Project, error) {
	var wire wireV3
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, &DomainError{Code: ESchema, Path: "$", SourceVersion: SchemaV3, Detail: "invalid v3 JSON", Cause: err}
	}
	projectID, err := requiredString(wire.ProjectID, "$.projectId", SchemaV3)
	if err != nil {
		return nil, err
	}
	seed, err := required(wire.Seed, "$.seed", SchemaV3)
	if err != nil {
		return nil, err
	}
	sampleRate, err := required(wire.SampleRate, "$.sampleRate", SchemaV3)
	if err != nil {
		return nil, err
	}
	frameLengthMS, err := required(wire.FrameLengthMS, "$.frameLengthMs", SchemaV3)
	if err != nil {
		return nil, err
	}
	frameShiftMS, err := required(wire.FrameShiftMS, "$.frameShiftMs", SchemaV3)
	if err != nil {
		return nil, err
	}
	sessionGainDB, err := required(wire.SessionGainDBQ8, "$.sessionGainDbQ8", SchemaV3)
	if err != nil {
		return nil, err
	}
	targetMDB, err := required(wire.TargetLoudnessMDB, "$.targetLoudnessMdb", SchemaV3)
	if err != nil {
		return nil, err
	}
	maxGainDB, err := required(wire.MaxGainDBQ8, "$.maxGainDbQ8", SchemaV3)
	if err != nil {
		return nil, err
	}
	toleranceMDB, err := required(wire.LoudnessToleranceMDB, "$.loudnessToleranceMdb", SchemaV3)
	if err != nil {
		return nil, err
	}
	segments, err := requiredSlice(wire.Segments, "$.segments", SchemaV3)
	if err != nil {
		return nil, err
	}
	traces := make([]FieldTrace, 0, 20+len(segments)*8)
	sessionGain, tr, err := DBQ8ToGainQ12(sessionGainDB, "$.sessionGainDbQ8")
	if err != nil {
		return nil, attachErrorSource(err, SchemaV3, "$.sessionGainDbQ8", "sessionGainDbQ8")
	}
	appendConverted(&traces, SchemaV3, "$.sessionGainDbQ8", "$.sessionGainQ12", strconv.FormatInt(sessionGainDB, 10), strconv.FormatInt(sessionGain, 10), tr)
	target, tr, err := ConvertScale(targetMDB, 1000, InternalLoudnessScale, "$.targetLoudnessMdb", RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV3, "$.targetLoudnessMdb", "targetLoudnessMdb")
	}
	appendConverted(&traces, SchemaV3, "$.targetLoudnessMdb", "$.targetLoudnessDBQ8", strconv.FormatInt(targetMDB, 10), strconv.FormatInt(target, 10), tr)
	maxGain, tr, err := DBQ8ToGainQ12(maxGainDB, "$.maxGainDbQ8")
	if err != nil {
		return nil, attachErrorSource(err, SchemaV3, "$.maxGainDbQ8", "maxGainDbQ8")
	}
	appendConverted(&traces, SchemaV3, "$.maxGainDbQ8", "$.maxGainQ12", strconv.FormatInt(maxGainDB, 10), strconv.FormatInt(maxGain, 10), tr)
	tolerance, tr, err := ConvertScale(toleranceMDB, 1000, InternalLoudnessScale, "$.loudnessToleranceMdb", RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV3, "$.loudnessToleranceMdb", "loudnessToleranceMdb")
	}
	appendConverted(&traces, SchemaV3, "$.loudnessToleranceMdb", "$.loudnessToleranceQ8", strconv.FormatInt(toleranceMDB, 10), strconv.FormatInt(tolerance, 10), tr)
	frameLength, tr, err := MillisToSamples(frameLengthMS, sampleRate, RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV3, "$.frameLengthMs", "frameLengthMs")
	}
	appendConverted(&traces, SchemaV3, "$.frameLengthMs", "$.frameLength", strconv.FormatInt(frameLengthMS, 10), strconv.FormatInt(frameLength, 10), tr)
	frameShift, tr, err := MillisToSamples(frameShiftMS, sampleRate, RoundHalfUp)
	if err != nil {
		return nil, attachErrorSource(err, SchemaV3, "$.frameShiftMs", "frameShiftMs")
	}
	appendConverted(&traces, SchemaV3, "$.frameShiftMs", "$.frameShift", strconv.FormatInt(frameShiftMS, 10), strconv.FormatInt(frameShift, 10), tr)
	appendMapped(&traces, SchemaV3, "$.projectId", "$.id", projectID)
	appendMapped(&traces, SchemaV3, "$.seed", "$.seed", strconv.FormatInt(seed, 10))
	appendMapped(&traces, SchemaV3, "$.sampleRate", "$.sampleRate", strconv.FormatInt(sampleRate, 10))

	clips := make([]Clip, 0, len(segments))
	sceneOrder := make([]string, 0)
	sceneClips := make(map[string][]int)
	for i := range segments {
		sourcePath := fmt.Sprintf("$.segments[%d]", i)
		internalPath := fmt.Sprintf("$.clips[%d]", i)
		id, err := required(segments[i].SegmentID, sourcePath+".segmentId", SchemaV3)
		if err != nil {
			return nil, err
		}
		startMS, err := required(segments[i].StartMS, sourcePath+".startMs", SchemaV3)
		if err != nil {
			return nil, err
		}
		endMS, err := required(segments[i].EndMS, sourcePath+".endMs", SchemaV3)
		if err != nil {
			return nil, err
		}
		categoryRaw, err := requiredString(segments[i].EmitterClass, sourcePath+".emitterClass", SchemaV3)
		if err != nil {
			return nil, err
		}
		f0, err := requiredSlice(segments[i].PitchTrackHz, sourcePath+".pitchTrackHz", SchemaV3)
		if err != nil {
			return nil, err
		}
		weight, err := required(segments[i].LoudnessWeightQ8, sourcePath+".loudnessWeightQ8", SchemaV3)
		if err != nil {
			return nil, err
		}
		gainDB, err := required(segments[i].GainDBQ8, sourcePath+".gainDbQ8", SchemaV3)
		if err != nil {
			return nil, err
		}
		amplitude, err := required(segments[i].AmplitudeQ12, sourcePath+".amplitudeQ12", SchemaV3)
		if err != nil {
			return nil, err
		}
		start, startTrace, err := MillisToSamples(startMS, sampleRate, RoundHalfUp)
		if err != nil {
			return nil, attachErrorSource(err, SchemaV3, sourcePath+".startMs", "startMs")
		}
		end, endTrace, err := MillisToSamples(endMS, sampleRate, RoundHalfUp)
		if err != nil {
			return nil, attachErrorSource(err, SchemaV3, sourcePath+".endMs", "endMs")
		}
		gain, gainTrace, err := DBQ8ToGainQ12(gainDB, sourcePath+".gainDbQ8")
		if err != nil {
			return nil, attachErrorSource(err, SchemaV3, sourcePath+".gainDbQ8", "gainDbQ8")
		}
		var sceneName string
		if segments[i].Scene != nil {
			sceneName = *segments[i].Scene
		}
		var marker *string
		if segments[i].Calibration != nil {
			value, err := requiredString(segments[i].Calibration.Marker, sourcePath+".calibration.marker", SchemaV3)
			if err != nil {
				return nil, err
			}
			marker = &value
		}
		clips = append(clips, Clip{
			ID: id, Scene: sceneName, StartSample: start, EndSample: end,
			Category: Category(categoryRaw), F0TrackHz: append([]int64(nil), f0...),
			AmplitudeQ12: amplitude, LoudnessWeightQ8: weight, GainQ12: gain,
			CalibrationMarker: marker,
		})
		if sceneName != "" {
			if _, exists := sceneClips[sceneName]; !exists {
				sceneOrder = append(sceneOrder, sceneName)
			}
			sceneClips[sceneName] = append(sceneClips[sceneName], id)
		}
		appendConverted(&traces, SchemaV3, sourcePath+".startMs", internalPath+".startSample", strconv.FormatInt(startMS, 10), strconv.FormatInt(start, 10), startTrace)
		appendConverted(&traces, SchemaV3, sourcePath+".endMs", internalPath+".endSample", strconv.FormatInt(endMS, 10), strconv.FormatInt(end, 10), endTrace)
		appendConverted(&traces, SchemaV3, sourcePath+".gainDbQ8", internalPath+".gainQ12", strconv.FormatInt(gainDB, 10), strconv.FormatInt(gain, 10), gainTrace)
		appendMapped(&traces, SchemaV3, sourcePath+".emitterClass", internalPath+".category", categoryRaw)
		appendMapped(&traces, SchemaV3, sourcePath+".pitchTrackHz", internalPath+".f0TrackHz", fmt.Sprint(f0))
		appendMapped(&traces, SchemaV3, sourcePath+".loudnessWeightQ8", internalPath+".loudnessWeightQ8", strconv.FormatInt(weight, 10))
		appendMapped(&traces, SchemaV3, sourcePath+".amplitudeQ12", internalPath+".amplitudeQ12", strconv.FormatInt(amplitude, 10))
		if sceneName == "" {
			appendMapped(&traces, SchemaV3, sourcePath+".scene", internalPath+".scene", "<absent>")
		} else {
			appendMapped(&traces, SchemaV3, sourcePath+".scene", internalPath+".scene", sceneName)
		}
		if marker == nil {
			appendMapped(&traces, SchemaV3, sourcePath+".calibration.marker", internalPath+".calibrationMarker", "<absent>")
		} else {
			appendMapped(&traces, SchemaV3, sourcePath+".calibration.marker", internalPath+".calibrationMarker", *marker)
		}
	}
	scenes := make([]Scene, 0, len(sceneOrder))
	for _, name := range sceneOrder {
		scenes = append(scenes, Scene{Name: name, ClipIDs: append([]int(nil), sceneClips[name]...)})
	}
	project := &Project{
		FormatVersion: SchemaV3, ID: projectID, Seed: seed, SampleRate: sampleRate,
		FrameLength: frameLength, FrameShift: frameShift, SessionGainQ12: sessionGain,
		TargetLoudnessDBQ8: target, MaxGainQ12: maxGain, LoudnessToleranceQ8: tolerance,
		Scenes: scenes, Clips: clips, Quantization: DefaultQuantization(),
		Provenance: Provenance{OriginalVersion: SchemaV3, CurrentVersion: SchemaV3, Traces: traces},
	}
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	return project, nil
}

// ExportResult always carries bytes when encoding succeeded, even when the
// returned error is E_LOSSY.
type ExportResult struct {
	Version Version      `json:"version"`
	Data    []byte       `json:"-"`
	Losses  []Loss       `json:"losses,omitempty"`
	Traces  []FieldTrace `json:"traces,omitempty"`
}

// Export serializes an internal project into a concrete schema. Downgrades
// that drop scene nesting or calibration markers return both E_LOSSY and a
// valid payload that the target loader can read back.
func Export(project *Project, target Version, mode Rounding) (ExportResult, error) {
	result := ExportResult{Version: target}
	if project == nil {
		return result, &DomainError{Code: ESchema, Path: "$", Detail: "project is nil"}
	}
	if !target.Valid() {
		return result, &DomainError{Code: ESchema, Path: "$.schema", SourceVersion: target, Detail: "unknown target schema version"}
	}
	if !mode.Valid() {
		return result, &DomainError{Code: EPara, Path: "$.rounding", Field: "rounding", Detail: "unknown rounding mode"}
	}
	if err := validateProjectBasics(project); err != nil {
		return result, err
	}
	switch target {
	case SchemaV1:
		data, losses, traces, err := exportV1(project, mode)
		result.Data, result.Losses, result.Traces = data, losses, traces
		if err != nil {
			return result, err
		}
	case SchemaV2:
		data, losses, traces, err := exportV2(project, mode)
		result.Data, result.Losses, result.Traces = data, losses, traces
		if err != nil {
			return result, err
		}
	case SchemaV3:
		data, losses, traces, err := exportV3(project, mode)
		result.Data, result.Losses, result.Traces = data, losses, traces
		if err != nil {
			return result, err
		}
	}
	if len(result.Losses) > 0 {
		return result, &DomainError{
			Code: ELossy, SourceVersion: target, Losses: result.Losses,
			Detail: fmt.Sprintf("%d value(s) cannot be represented by %s", len(result.Losses), target),
		}
	}
	return result, nil
}

func collectLossesForV1(project *Project) []Loss {
	losses := make([]Loss, 0)
	seenScene := make(map[string]struct{})
	for i, clip := range project.Clips {
		if clip.Scene != "" {
			if _, seen := seenScene[clip.Scene]; !seen {
				seenScene[clip.Scene] = struct{}{}
			}
			losses = append(losses, Loss{
				Path: fmt.Sprintf("$.clips[%d].scene", i), Value: clip.Scene,
				Reason: "v1 has no scene grouping field", Disposition: "scene name flattened and dropped",
			})
		}
		if clip.CalibrationMarker != nil {
			losses = append(losses, Loss{
				Path: fmt.Sprintf("$.clips[%d].calibrationMarker", i), Value: *clip.CalibrationMarker,
				Reason: "v1 has no calibration marker field", Disposition: "marker dropped",
			})
		}
	}
	if len(project.Scenes) > 0 {
		losses = append(losses, Loss{
			Path: "$.scenes", Value: fmt.Sprintf("%d scene(s)", len(project.Scenes)),
			Reason: "v1 cannot represent a scene tree", Disposition: "clips exported as a flat table",
		})
	}
	return losses
}

func exportV1(project *Project, mode Rounding) ([]byte, []Loss, []FieldTrace, error) {
	losses := collectLossesForV1(project)
	traces := make([]FieldTrace, 0, 12+len(project.Clips)*8)
	clips := make([]wireV1Clip, 0, len(project.Clips))
	for i, clip := range project.Clips {
		sourcePath := fmt.Sprintf("$.clips[%d]", i)
		targetPath := fmt.Sprintf("$.clips[%d]", i)
		id, start, end, category, f0, weight, gain, amplitude := clip.ID, clip.StartSample, clip.EndSample, string(clip.Category), append([]int64(nil), clip.F0TrackHz...), clip.LoudnessWeightQ8, clip.GainQ12, clip.AmplitudeQ12
		clips = append(clips, wireV1Clip{
			ClipID: &id, StartSample: &start, EndSample: &end, Category: &category,
			F0TrackHz: &f0, LoudnessWeightQ8: &weight, GainQ12: &gain, BaseAmplitudeQ12: &amplitude,
		})
		appendMapped(&traces, project.FormatVersion, sourcePath+".id", targetPath+".clip_id", strconv.Itoa(id))
		appendMapped(&traces, project.FormatVersion, sourcePath+".startSample", targetPath+".start_sample", strconv.FormatInt(start, 10))
		appendMapped(&traces, project.FormatVersion, sourcePath+".endSample", targetPath+".end_sample", strconv.FormatInt(end, 10))
		appendMapped(&traces, project.FormatVersion, sourcePath+".gainQ12", targetPath+".gain_q12", strconv.FormatInt(gain, 10))
	}
	projectID := project.ID
	seed := project.Seed
	sampleRate := project.SampleRate
	frameLength := project.FrameLength
	frameShift := project.FrameShift
	sessionGain := project.SessionGainQ12
	target := project.TargetLoudnessDBQ8
	maxGain := project.MaxGainQ12
	tolerance := project.LoudnessToleranceQ8
	wire := wireV1{
		Schema: stringPtr(string(SchemaV1)), ProjectID: &projectID, Seed: &seed,
		SampleRate: &sampleRate, FrameLength: &frameLength, FrameShift: &frameShift,
		SessionGainQ12: &sessionGain, TargetLoudnessDBQ8: &target, MaxGainQ12: &maxGain,
		LoudnessToleranceQ8: &tolerance, Clips: &clips,
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, nil, nil, &DomainError{Code: ESchema, Path: "$", SourceVersion: SchemaV1, Detail: "encode failed", Cause: err}
	}
	return data, losses, traces, nil
}

func exportV2(project *Project, mode Rounding) ([]byte, []Loss, []FieldTrace, error) {
	traces := make([]FieldTrace, 0, 16+len(project.Clips)*6)
	sampleRate := project.SampleRate
	sessionBP, tr, err := ConvertScale(project.SessionGainQ12, InternalGainScale, 10000, "$.sessionGainQ12", mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.sessionGainQ12", "sessionGainQ12")
	}
	appendConverted(&traces, project.FormatVersion, "$.sessionGainQ12", "$.session_gain_bp", strconv.FormatInt(project.SessionGainQ12, 10), strconv.FormatInt(sessionBP, 10), tr)
	targetCDB, tr, err := ConvertScale(project.TargetLoudnessDBQ8, InternalLoudnessScale, 100, "$.targetLoudnessDBQ8", mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.targetLoudnessDBQ8", "targetLoudnessDBQ8")
	}
	appendConverted(&traces, project.FormatVersion, "$.targetLoudnessDBQ8", "$.target_loudness_cdb", strconv.FormatInt(project.TargetLoudnessDBQ8, 10), strconv.FormatInt(targetCDB, 10), tr)
	maxGainBP, tr, err := ConvertScale(project.MaxGainQ12, InternalGainScale, 10000, "$.maxGainQ12", mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.maxGainQ12", "maxGainQ12")
	}
	appendConverted(&traces, project.FormatVersion, "$.maxGainQ12", "$.max_gain_bp", strconv.FormatInt(project.MaxGainQ12, 10), strconv.FormatInt(maxGainBP, 10), tr)
	toleranceCDB, tr, err := ConvertScale(project.LoudnessToleranceQ8, InternalLoudnessScale, 100, "$.loudnessToleranceQ8", mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.loudnessToleranceQ8", "loudnessToleranceQ8")
	}
	appendConverted(&traces, project.FormatVersion, "$.loudnessToleranceQ8", "$.loudness_tolerance_cdb", strconv.FormatInt(project.LoudnessToleranceQ8, 10), strconv.FormatInt(toleranceCDB, 10), tr)
	frameLengthMS, tr, err := SamplesToMillis(project.FrameLength, sampleRate, mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.frameLength", "frameLength")
	}
	appendConverted(&traces, project.FormatVersion, "$.frameLength", "$.frame_length_ms", strconv.FormatInt(project.FrameLength, 10), strconv.FormatInt(frameLengthMS, 10), tr)
	frameShiftMS, tr, err := SamplesToMillis(project.FrameShift, sampleRate, mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.frameShift", "frameShift")
	}
	appendConverted(&traces, project.FormatVersion, "$.frameShift", "$.frame_shift_ms", strconv.FormatInt(project.FrameShift, 10), strconv.FormatInt(frameShiftMS, 10), tr)

	sceneMap := make(map[string][]Clip)
	sceneOrder := make([]string, 0)
	for _, clip := range project.Clips {
		name := clip.Scene
		if name == "" {
			name = "default"
		}
		if _, exists := sceneMap[name]; !exists {
			sceneOrder = append(sceneOrder, name)
		}
		sceneMap[name] = append(sceneMap[name], clip)
	}
	for _, scene := range project.Scenes {
		if _, exists := sceneMap[scene.Name]; !exists {
			sceneOrder = append(sceneOrder, scene.Name)
			sceneMap[scene.Name] = nil
		}
	}
	scenes := make([]wireV2Scene, 0, len(sceneOrder))
	for _, sceneName := range sceneOrder {
		sceneClips := sceneMap[sceneName]
		clips := make([]wireV2Clip, 0, len(sceneClips))
		for _, clip := range sceneClips {
			sourcePath := fmt.Sprintf("$.clips[%d]", indexOfClip(project, clip.ID))
			targetPath := fmt.Sprintf("$.scenes[%s].clips[%d]", sceneName, len(clips))
			startMS, startTrace, err := SamplesToMillis(clip.StartSample, sampleRate, mode)
			if err != nil {
				return nil, nil, nil, attachErrorSource(err, project.FormatVersion, sourcePath+".startSample", "startSample")
			}
			endMS, endTrace, err := SamplesToMillis(clip.EndSample, sampleRate, mode)
			if err != nil {
				return nil, nil, nil, attachErrorSource(err, project.FormatVersion, sourcePath+".endSample", "endSample")
			}
			weightBP, weightTrace, err := ConvertScale(clip.LoudnessWeightQ8, InternalWeightScale, 10000, sourcePath+".loudnessWeightQ8", mode)
			if err != nil {
				return nil, nil, nil, attachErrorSource(err, project.FormatVersion, sourcePath+".loudnessWeightQ8", "loudnessWeightQ8")
			}
			gainBP, gainTrace, err := ConvertScale(clip.GainQ12, InternalGainScale, 10000, sourcePath+".gainQ12", mode)
			if err != nil {
				return nil, nil, nil, attachErrorSource(err, project.FormatVersion, sourcePath+".gainQ12", "gainQ12")
			}
			id := clip.ID
			category := string(clip.Category)
			f0 := append([]int64(nil), clip.F0TrackHz...)
			amplitude := clip.AmplitudeQ12
			clips = append(clips, wireV2Clip{
				ID: &id, StartMS: &startMS, EndMS: &endMS, Category: &category,
				F0TrackHz: &f0, LoudnessWeightBP: &weightBP, GainBP: &gainBP,
				BaseAmplitudeQ12: &amplitude, CalibrationMarker: cloneStringPtr(clip.CalibrationMarker),
			})
			appendConverted(&traces, project.FormatVersion, sourcePath+".startSample", targetPath+".start_ms", strconv.FormatInt(clip.StartSample, 10), strconv.FormatInt(startMS, 10), startTrace)
			appendConverted(&traces, project.FormatVersion, sourcePath+".endSample", targetPath+".end_ms", strconv.FormatInt(clip.EndSample, 10), strconv.FormatInt(endMS, 10), endTrace)
			appendConverted(&traces, project.FormatVersion, sourcePath+".loudnessWeightQ8", targetPath+".loudness_weight_bp", strconv.FormatInt(clip.LoudnessWeightQ8, 10), strconv.FormatInt(weightBP, 10), weightTrace)
			appendConverted(&traces, project.FormatVersion, sourcePath+".gainQ12", targetPath+".gain_bp", strconv.FormatInt(clip.GainQ12, 10), strconv.FormatInt(gainBP, 10), gainTrace)
		}
		name := sceneName
		scenes = append(scenes, wireV2Scene{Name: &name, Clips: &clips})
	}
	projectID, seed := project.ID, project.Seed
	wire := wireV2{
		Schema: stringPtr(string(SchemaV2)), ProjectID: &projectID, Seed: &seed,
		SampleRate: &sampleRate, FrameLengthMS: &frameLengthMS, FrameShiftMS: &frameShiftMS,
		SessionGainBP: &sessionBP, TargetLoudnessCDB: &targetCDB, MaxGainBP: &maxGainBP,
		LoudnessToleranceCDB: &toleranceCDB, Scenes: &scenes,
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, nil, nil, &DomainError{Code: ESchema, Path: "$", SourceVersion: SchemaV2, Detail: "encode failed", Cause: err}
	}
	return data, nil, traces, nil
}

func exportV3(project *Project, mode Rounding) ([]byte, []Loss, []FieldTrace, error) {
	traces := make([]FieldTrace, 0, 16+len(project.Clips)*6)
	sampleRate := project.SampleRate
	sessionDB, tr, err := GainQ12ToDBQ8(project.SessionGainQ12, "$.sessionGainQ12")
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.sessionGainQ12", "sessionGainQ12")
	}
	appendConverted(&traces, project.FormatVersion, "$.sessionGainQ12", "$.sessionGainDbQ8", strconv.FormatInt(project.SessionGainQ12, 10), strconv.FormatInt(sessionDB, 10), tr)
	targetMDB, tr, err := ConvertScale(project.TargetLoudnessDBQ8, InternalLoudnessScale, 1000, "$.targetLoudnessDBQ8", mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.targetLoudnessDBQ8", "targetLoudnessDBQ8")
	}
	appendConverted(&traces, project.FormatVersion, "$.targetLoudnessDBQ8", "$.targetLoudnessMdb", strconv.FormatInt(project.TargetLoudnessDBQ8, 10), strconv.FormatInt(targetMDB, 10), tr)
	maxGainDB, tr, err := GainQ12ToDBQ8(project.MaxGainQ12, "$.maxGainQ12")
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.maxGainQ12", "maxGainQ12")
	}
	appendConverted(&traces, project.FormatVersion, "$.maxGainQ12", "$.maxGainDbQ8", strconv.FormatInt(project.MaxGainQ12, 10), strconv.FormatInt(maxGainDB, 10), tr)
	toleranceMDB, tr, err := ConvertScale(project.LoudnessToleranceQ8, InternalLoudnessScale, 1000, "$.loudnessToleranceQ8", mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.loudnessToleranceQ8", "loudnessToleranceQ8")
	}
	appendConverted(&traces, project.FormatVersion, "$.loudnessToleranceQ8", "$.loudnessToleranceMdb", strconv.FormatInt(project.LoudnessToleranceQ8, 10), strconv.FormatInt(toleranceMDB, 10), tr)
	frameLengthMS, tr, err := SamplesToMillis(project.FrameLength, sampleRate, mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.frameLength", "frameLength")
	}
	appendConverted(&traces, project.FormatVersion, "$.frameLength", "$.frameLengthMs", strconv.FormatInt(project.FrameLength, 10), strconv.FormatInt(frameLengthMS, 10), tr)
	frameShiftMS, tr, err := SamplesToMillis(project.FrameShift, sampleRate, mode)
	if err != nil {
		return nil, nil, nil, attachErrorSource(err, project.FormatVersion, "$.frameShift", "frameShift")
	}
	appendConverted(&traces, project.FormatVersion, "$.frameShift", "$.frameShiftMs", strconv.FormatInt(project.FrameShift, 10), strconv.FormatInt(frameShiftMS, 10), tr)
	segments := make([]wireV3Segment, 0, len(project.Clips))
	for i, clip := range project.Clips {
		sourcePath := fmt.Sprintf("$.clips[%d]", i)
		targetPath := fmt.Sprintf("$.segments[%d]", i)
		startMS, startTrace, err := SamplesToMillis(clip.StartSample, sampleRate, mode)
		if err != nil {
			return nil, nil, nil, attachErrorSource(err, project.FormatVersion, sourcePath+".startSample", "startSample")
		}
		endMS, endTrace, err := SamplesToMillis(clip.EndSample, sampleRate, mode)
		if err != nil {
			return nil, nil, nil, attachErrorSource(err, project.FormatVersion, sourcePath+".endSample", "endSample")
		}
		gainDB, gainTrace, err := GainQ12ToDBQ8(clip.GainQ12, sourcePath+".gainQ12")
		if err != nil {
			return nil, nil, nil, attachErrorSource(err, project.FormatVersion, sourcePath+".gainQ12", "gainQ12")
		}
		id := clip.ID
		category := string(clip.Category)
		f0 := append([]int64(nil), clip.F0TrackHz...)
		weight := clip.LoudnessWeightQ8
		amplitude := clip.AmplitudeQ12
		var calibration *wireV3Calibration
		if clip.CalibrationMarker != nil {
			calibration = &wireV3Calibration{Marker: cloneStringPtr(clip.CalibrationMarker)}
		}
		var scene *string
		if clip.Scene != "" {
			scene = stringPtr(clip.Scene)
		}
		segments = append(segments, wireV3Segment{
			SegmentID: &id, Scene: scene, StartMS: &startMS, EndMS: &endMS,
			EmitterClass: &category, PitchTrackHz: &f0, LoudnessWeightQ8: &weight,
			GainDBQ8: &gainDB, AmplitudeQ12: &amplitude, Calibration: calibration,
		})
		appendConverted(&traces, project.FormatVersion, sourcePath+".startSample", targetPath+".startMs", strconv.FormatInt(clip.StartSample, 10), strconv.FormatInt(startMS, 10), startTrace)
		appendConverted(&traces, project.FormatVersion, sourcePath+".endSample", targetPath+".endMs", strconv.FormatInt(clip.EndSample, 10), strconv.FormatInt(endMS, 10), endTrace)
		appendConverted(&traces, project.FormatVersion, sourcePath+".gainQ12", targetPath+".gainDbQ8", strconv.FormatInt(clip.GainQ12, 10), strconv.FormatInt(gainDB, 10), gainTrace)
	}
	projectID, seed := project.ID, project.Seed
	wire := wireV3{
		Schema: stringPtr(string(SchemaV3)), ProjectID: &projectID, Seed: &seed,
		SampleRate: &sampleRate, FrameLengthMS: &frameLengthMS, FrameShiftMS: &frameShiftMS,
		SessionGainDBQ8: &sessionDB, TargetLoudnessMDB: &targetMDB, MaxGainDBQ8: &maxGainDB,
		LoudnessToleranceMDB: &toleranceMDB, Segments: &segments,
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, nil, nil, &DomainError{Code: ESchema, Path: "$", SourceVersion: SchemaV3, Detail: "encode failed", Cause: err}
	}
	return data, nil, traces, nil
}

func stringPtr(value string) *string { return &value }

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func indexOfClip(project *Project, id int) int {
	for i := range project.Clips {
		if project.Clips[i].ID == id {
			return i
		}
	}
	return -1
}

// UpgradeReport lists defaults inserted while moving to a newer schema.
type UpgradeReport struct {
	From     Version      `json:"from"`
	To       Version      `json:"to"`
	Defaults []FieldTrace `json:"defaults"`
}

func versionRank(version Version) int {
	switch version {
	case SchemaV1:
		return 1
	case SchemaV2:
		return 2
	case SchemaV3:
		return 3
	default:
		return 0
	}
}

// Upgrade moves an internal model to a newer schema by applying declared
// defaults. Downgrades must use Export so losses are reported.
func Upgrade(project *Project, target Version) (*Project, UpgradeReport, error) {
	report := UpgradeReport{}
	if project == nil {
		return nil, report, &DomainError{Code: ESchema, Path: "$", Detail: "project is nil"}
	}
	if !target.Valid() {
		return nil, report, &DomainError{Code: ESchema, Path: "$.schema", SourceVersion: target, Detail: "unknown target schema version"}
	}
	from := project.FormatVersion
	report.From, report.To = from, target
	if versionRank(target) < versionRank(from) {
		return nil, report, &DomainError{Code: ESchema, Path: "$.schema", SourceVersion: from, Detail: "Upgrade target must not be older than source"}
	}
	clone := CloneProject(project)
	if versionRank(from) < 2 && versionRank(target) >= 2 {
		sceneNames := make(map[string]struct{})
		for i := range clone.Clips {
			if clone.Clips[i].Scene == "" {
				clone.Clips[i].Scene = "default"
				trace := FieldTrace{
					SourceVersion: from,
					SourcePath:    fmt.Sprintf("$.clips[%d].scene", i),
					InternalPath:  fmt.Sprintf("$.clips[%d].scene", i),
					Kind:          TraceDefault,
					OldValue:      "<absent>",
					NewValue:      "default",
					DefaultSource: "game-audio/v2:default-scene",
				}
				clone.Provenance.Traces = append(clone.Provenance.Traces, trace)
				report.Defaults = append(report.Defaults, trace)
			}
			sceneNames[clone.Clips[i].Scene] = struct{}{}
		}
		for name := range sceneNames {
			found := false
			for _, scene := range clone.Scenes {
				if scene.Name == name {
					found = true
					break
				}
			}
			if !found {
				ids := make([]int, 0)
				for _, clip := range clone.Clips {
					if clip.Scene == name {
						ids = append(ids, clip.ID)
					}
				}
				clone.Scenes = append(clone.Scenes, Scene{Name: name, ClipIDs: ids})
			}
		}
	}
	if versionRank(from) < 2 && versionRank(target) >= 2 {
		for i := range clone.Clips {
			if clone.Clips[i].CalibrationMarker == nil {
				value := "unreviewed"
				clone.Clips[i].CalibrationMarker = &value
				trace := FieldTrace{
					SourceVersion: from,
					SourcePath:    fmt.Sprintf("$.clips[%d].calibrationMarker", i),
					InternalPath:  fmt.Sprintf("$.clips[%d].calibrationMarker", i),
					Kind:          TraceDefault,
					OldValue:      "<absent>",
					NewValue:      value,
					DefaultSource: "game-audio/v2:default-calibration-marker=unreviewed",
				}
				clone.Provenance.Traces = append(clone.Provenance.Traces, trace)
				report.Defaults = append(report.Defaults, trace)
			}
		}
	}
	clone.FormatVersion = target
	clone.Provenance.CurrentVersion = target
	sort.SliceStable(report.Defaults, func(i, j int) bool {
		return report.Defaults[i].InternalPath < report.Defaults[j].InternalPath
	})
	return clone, report, nil
}
