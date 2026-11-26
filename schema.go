package dialoguecal

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

type sourceField struct {
	value any
	path  string
}

func decodeJSONObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, &Error{Code: CodeDecode, Path: "$", Message: "invalid JSON: " + err.Error()}
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, &Error{Code: CodeDecode, Path: "$", Message: "trailing JSON content"}
	}
	return m, nil
}

func childObject(m map[string]any, key, path string) (map[string]any, error) {
	v, ok := m[key]
	if !ok {
		return nil, &Error{Code: CodeSchema, Path: path + "." + key, Message: "required field is missing"}
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, &Error{Code: CodeSchema, Path: path + "." + key, Message: "expected JSON object"}
	}
	return obj, nil
}

func childArray(m map[string]any, key, path string) ([]any, error) {
	v, ok := m[key]
	if !ok {
		return nil, &Error{Code: CodeSchema, Path: path + "." + key, Message: "required field is missing"}
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, &Error{Code: CodeSchema, Path: path + "." + key, Message: "expected JSON array"}
	}
	return arr, nil
}

func reqString(m map[string]any, key, path string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", &Error{Code: CodeSchema, Path: path + "." + key, Message: "required field is missing"}
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", &Error{Code: CodeSchema, Path: path + "." + key, Message: "expected non-empty string"}
	}
	return s, nil
}

func optString(m map[string]any, key string, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func reqInt64(m map[string]any, key, path string) (int64, error) {
	v, ok := m[key]
	if !ok {
		return 0, &Error{Code: CodeSchema, Path: path + "." + key, Message: "required field is missing"}
	}
	switch n := v.(type) {
	case json.Number:
		x, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return 0, &Error{Code: CodeSchema, Path: path + "." + key, Message: "expected integer"}
		}
		return x, nil
	case float64:
		if n != float64(int64(n)) {
			return 0, &Error{Code: CodeSchema, Path: path + "." + key, Message: "expected integer"}
		}
		return int64(n), nil
	default:
		return 0, &Error{Code: CodeSchema, Path: path + "." + key, Message: "expected integer"}
	}
}

func optInt64(m map[string]any, key string, fallback int64) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return fallback, false
	}
	switch n := v.(type) {
	case json.Number:
		x, err := strconv.ParseInt(n.String(), 10, 64)
		if err == nil {
			return x, true
		}
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return fallback, false
}

func addDefault(p *Project, path, oldValue, def, source string) {
	p.Defaults = append(p.Defaults, DefaultTrace{
		FieldPath: path, OldValue: oldValue, DefaultValue: def, Source: source,
	})
}

func appendTrace(p *Project, tr ConversionTrace, source SchemaVersion) {
	tr.SourceVersion = source
	tr.TargetVersion = VersionV3
	p.Conversions = append(p.Conversions, tr)
}

func arrayPath(base string, i int) string {
	return fmt.Sprintf("%s[%d]", base, i)
}

func decodePCMBase64(encoded string, declared int64, path string) ([]int16, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, &Error{Code: CodeDecode, Path: path, Message: "invalid base64 PCM: " + err.Error()}
	}
	if len(raw)%2 != 0 {
		return nil, &Error{Code: CodeDecode, Path: path, Message: "pcm_s16le payload has odd byte length"}
	}
	samples := make([]int16, len(raw)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	if int64(len(samples)) != declared {
		return nil, &Error{
			Code: CodeDecode, Path: path, OperandA: declared, OperandB: int64(len(samples)),
			Message: "declared sample count does not match PCM payload",
		}
	}
	return samples, nil
}

// LoadProject detects the schema revision, maps it to the internal sample and
// per-myriad scales, records conversion/default traces, then validates all
// source invariants before returning.
func LoadProject(data []byte) (*Project, error) {
	root, err := decodeJSONObject(data)
	if err != nil {
		return nil, err
	}
	version, err := detectVersion(root)
	if err != nil {
		return nil, err
	}
	var p *Project
	switch version {
	case VersionV1:
		p, err = loadV1(root)
	case VersionV2:
		p, err = loadV2(root)
	case VersionV3:
		p, err = loadV3(root)
	default:
		err = &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: "unknown schema version"}
	}
	if err != nil {
		return nil, err
	}
	p.SourceVersion = version
	appendSchemaMappings(p, version)
	if p.QuantizationVersion == "" {
		p.QuantizationVersion = DefaultQuantizationVersion
		addDefault(p, "$.quantizationVersion", "<missing>", p.QuantizationVersion, "schema default")
	}
	if p.ParallelVersion == "" {
		p.ParallelVersion = DefaultParallelVersion
		addDefault(p, "$.parallelVersion", "<missing>", p.ParallelVersion, "schema default")
	}
	if p.ConversionVersion == "" {
		p.ConversionVersion = DefaultConversionVersion
		addDefault(p, "$.conversionVersion", "<missing>", p.ConversionVersion, "schema default")
	}
	if p.AlgorithmVersion == "" {
		p.AlgorithmVersion = DefaultAlgorithmVersion
		addDefault(p, "$.algorithmVersion", "<missing>", p.AlgorithmVersion, "schema default")
	}
	if version < VersionV3 && len(p.CalibrationMarkers) == 0 {
		addDefault(p, "$.calibrationMarkers", "<missing>", "[]", "schema v1/v2 has no marker table")
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	p.Sort()
	return p, nil
}

func appendSchemaMappings(p *Project, version SchemaVersion) {
	add := func(source, internal, form string) {
		p.Mappings = append(p.Mappings, FieldMapping{
			SourcePath: source, InternalPath: internal, Form: form, SourceVersion: version,
		})
	}
	switch version {
	case VersionV1:
		add("$.project_id", "$.id", "snake_case")
		add("$.frame_rate_hz", "$.frameRateHz", "integer identity")
		add("$.sample_rate", "$.sampleRate", "integer identity")
		add("$.target_loudness_permille", "$.targetLoudness", "permille -> permyriad")
		add("$.session_gain_permille", "$.sessionGain", "permille -> permyriad")
		add("$.gain_upper_bound_permille", "$.gainUpperBound", "permille -> permyriad")
		add("$.tolerance_permille", "$.tolerance", "permille -> permyriad")
		add("$.max_parallelism", "$.maxParallelism", "integer identity")
		add("$.worker_buffer_samples", "$.workerBufferSamples", "integer identity")
		add("$.queue_budget", "$.queueBudget", "integer identity")
		add("$.frame_energy_limit", "$.frameEnergyLimit", "integer identity")
		add("$.audio_tracks[]", "$.dialogues[]", "flat audio table -> dialogue")
		add("$.segments[].dialogue_id", "$.dialogues[].id", "denormalized relation -> nested")
		add("$.segments[].start_ms", "$.segments[].startSample", "ms -> sample")
		add("$.segments[].end_ms", "$.segments[].endSample", "ms -> sample")
		add("$.segments[].gain_permille", "$.segments[].gain", "permille -> permyriad")
		add("$.segments[].phonemes[].start_ms", "$.phonemes[].startSample", "ms -> sample")
		add("$.segments[].phonemes[].end_ms", "$.phonemes[].endSample", "ms -> sample")
		add("$.segments[].phonemes[].weight_permille", "$.phonemes[].weight", "permille -> permyriad")
	case VersionV2:
		add("$.projectId", "$.id", "camelCase")
		add("$.frameRateHz", "$.frameRateHz", "integer identity")
		add("$.sampleRate", "$.sampleRate", "integer identity")
		add("$.targetLoudnessBasisPoints", "$.targetLoudness", "basis-point identity")
		add("$.sessionGainBasisPoints", "$.sessionGain", "basis-point identity")
		add("$.gainUpperBoundBasisPoints", "$.gainUpperBound", "basis-point identity")
		add("$.toleranceBasisPoints", "$.tolerance", "basis-point identity")
		add("$.parallel.maxParallelism", "$.maxParallelism", "nested -> scalar")
		add("$.parallel.workerBufferSamples", "$.workerBufferSamples", "nested -> scalar")
		add("$.parallel.queueBudget", "$.queueBudget", "nested -> scalar")
		add("$.quantization.version", "$.quantizationVersion", "nested -> scalar")
		add("$.conversion.version", "$.conversionVersion", "nested -> scalar")
		add("$.dialogues[]", "$.dialogues[]", "dialogue-nested")
		add("$.dialogues[].segments[].gainBasisPoints", "$.segments[].gain", "basis-point identity")
		add("$.dialogues[].segments[].phonemes[].weightBasisPoints", "$.phonemes[].weight", "basis-point identity")
	case VersionV3:
		add("$.project.id", "$.id", "project envelope")
		add("$.project.sampleRate", "$.sampleRate", "integer identity")
		add("$.project.frameRateHz", "$.frameRateHz", "integer identity")
		add("$.project.calibration.targetLoudnessPermyriad", "$.targetLoudness", "permyriad identity")
		add("$.project.calibration.sessionGainPermyriad", "$.sessionGain", "permyriad identity")
		add("$.project.calibration.gainUpperBoundPermyriad", "$.gainUpperBound", "permyriad identity")
		add("$.project.calibration.tolerancePermyriad", "$.tolerance", "permyriad identity")
		add("$.project.limitedParallel.maxParallelism", "$.maxParallelism", "nested -> scalar")
		add("$.project.limitedParallel.workerBufferSamples", "$.workerBufferSamples", "nested -> scalar")
		add("$.project.limitedParallel.queueBudget", "$.queueBudget", "nested -> scalar")
		add("$.project.versions.quantization", "$.quantizationVersion", "nested -> scalar")
		add("$.project.versions.parallelBudget", "$.parallelVersion", "nested -> scalar")
		add("$.project.versions.conversion", "$.conversionVersion", "nested -> scalar")
		add("$.project.versions.algorithm", "$.algorithmVersion", "nested -> scalar")
		add("$.sceneGroups[].dialogues[]", "$.dialogues[]", "scene nesting -> flat dialogue list")
		add("$.sceneGroups[].dialogues[].segments[]", "$.segments[]", "nested segment table")
		add("$.project.calibration.markers[]", "$.calibrationMarkers", "global marker table")
		add("$.sceneGroups[].dialogues[].segments[].calibrationMarkers[]", "$.segments[].markers", "segment marker table")
	}
}

func detectVersion(root map[string]any) (SchemaVersion, error) {
	var version SchemaVersion
	var have bool
	if s, ok := root["schema"].(string); ok {
		switch s {
		case "dialogue.v1":
			version, have = VersionV1, true
		case "dialogue.v3":
			version, have = VersionV3, true
		default:
			if n, ok := root["schemaVersion"]; ok {
				_ = n
			} else {
				return 0, &Error{Code: CodeSchema, Path: "$.schema", Message: "unknown schema identifier " + s}
			}
		}
	}
	if n, ok := root["schemaVersion"]; ok {
		var got int64
		switch v := n.(type) {
		case json.Number:
			got, _ = strconv.ParseInt(v.String(), 10, 64)
		case float64:
			got = int64(v)
		default:
			return 0, &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: "schemaVersion must be an integer"}
		}
		sv, ok := ParseSchemaVersion(int(got))
		if !ok {
			return 0, &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: fmt.Sprintf("unknown schema version %d", got)}
		}
		if have && sv != version {
			return 0, &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: "schema identifier and version disagree"}
		}
		version, have = sv, true
	}
	if !have {
		if _, ok := root["segments"]; ok {
			return 0, &Error{Code: CodeSchema, Path: "$.schema", Message: "legacy document is missing schema identifier"}
		}
		return 0, &Error{Code: CodeSchema, Path: "$.schemaVersion", Message: "required field is missing"}
	}
	return version, nil
}

func loadV1(root map[string]any) (*Project, error) {
	const base = "$"
	projectID, err := reqString(root, "project_id", base)
	if err != nil {
		return nil, err
	}
	frameRate, err := reqInt64(root, "frame_rate_hz", base)
	if err != nil {
		return nil, err
	}
	sampleRate, err := reqInt64(root, "sample_rate", base)
	if err != nil {
		return nil, err
	}
	maxParallel, err := reqInt64(root, "max_parallelism", base)
	if err != nil {
		return nil, err
	}
	bufferSamples, err := reqInt64(root, "worker_buffer_samples", base)
	if err != nil {
		return nil, err
	}
	queueBudget, err := reqInt64(root, "queue_budget", base)
	if err != nil {
		return nil, err
	}
	energyLimit, err := reqInt64(root, "frame_energy_limit", base)
	if err != nil {
		return nil, err
	}
	targetPermille, err := reqInt64(root, "target_loudness_permille", base)
	if err != nil {
		return nil, err
	}
	upperPermille, err := reqInt64(root, "gain_upper_bound_permille", base)
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID: projectID, SourceVersion: VersionV1, FrameRateHz: int(frameRate),
		SampleRate: int(sampleRate), FrameEnergyLimit: energyLimit,
		MaxParallelism: int(maxParallel), WorkerBufferSamples: int(bufferSamples),
		QueueBudget: int(queueBudget), Organization: "flat-segment-table",
	}
	target, tr, err := ScaleToPerMyriad(targetPermille, ScalePermille, "$.target_loudness_permille", RoundHalfAway)
	if err != nil {
		return nil, err
	}
	p.TargetLoudness = target
	appendTrace(p, tr, VersionV1)
	upper, tr, err := ScaleToPerMyriad(upperPermille, ScalePermille, "$.gain_upper_bound_permille", RoundHalfAway)
	if err != nil {
		return nil, err
	}
	p.GainUpperBound = upper
	appendTrace(p, tr, VersionV1)
	if session, ok := optInt64(root, "session_gain_permille", 0); ok {
		v, tr, err := ScaleToPerMyriad(session, ScalePermille, "$.session_gain_permille", RoundHalfAway)
		if err != nil {
			return nil, err
		}
		p.SessionGain = v
		appendTrace(p, tr, VersionV1)
	} else {
		addDefault(p, "$.session_gain_permille", "<missing>", "0", "v1 optional default")
	}
	if tol, ok := optInt64(root, "tolerance_permille", 10); ok {
		v, tr, err := ScaleToPerMyriad(tol, ScalePermille, "$.tolerance_permille", RoundHalfAway)
		if err != nil {
			return nil, err
		}
		p.Tolerance = v
		appendTrace(p, tr, VersionV1)
	} else {
		addDefault(p, "$.tolerance_permille", "<missing>", "10", "v1 optional default")
	}
	tracks, err := childArray(root, "audio_tracks", base)
	if err != nil {
		return nil, err
	}
	dialogues := make(map[string]*Dialogue, len(tracks))
	order := make([]string, 0, len(tracks))
	for i, v := range tracks {
		obj, ok := v.(map[string]any)
		path := arrayPath("$.audio_tracks", i)
		if !ok {
			return nil, &Error{Code: CodeSchema, Path: path, Message: "expected JSON object"}
		}
		id, err := reqString(obj, "dialogue_id", path)
		if err != nil {
			return nil, err
		}
		encoding, err := reqString(obj, "encoding", path)
		if err != nil {
			return nil, err
		}
		channels, err := reqInt64(obj, "channels", path)
		if err != nil {
			return nil, err
		}
		trackRate, err := reqInt64(obj, "sample_rate", path)
		if err != nil {
			return nil, err
		}
		count, err := reqInt64(obj, "sample_count", path)
		if err != nil {
			return nil, err
		}
		payload, err := reqString(obj, "pcm_base64", path)
		if err != nil {
			return nil, err
		}
		samples, err := decodePCMBase64(payload, count, path+".pcm_base64")
		if err != nil {
			return nil, err
		}
		if _, exists := dialogues[id]; exists {
			return nil, &Error{Code: CodeSchema, Path: path + ".dialogue_id", Message: "duplicate dialogue ID"}
		}
		dialogues[id] = &Dialogue{ID: id, PCM: PCM{
			Encoding: encoding, SampleRate: int(trackRate), Channels: int(channels),
			Samples: samples, DeclaredSampleCount: count,
		}}
		order = append(order, id)
	}
	segments, err := childArray(root, "segments", base)
	if err != nil {
		return nil, err
	}
	for i, v := range segments {
		obj, ok := v.(map[string]any)
		path := arrayPath("$.segments", i)
		if !ok {
			return nil, &Error{Code: CodeSchema, Path: path, Message: "expected JSON object"}
		}
		dialogueID, err := reqString(obj, "dialogue_id", path)
		if err != nil {
			return nil, err
		}
		d := dialogues[dialogueID]
		if d == nil {
			return nil, &Error{Code: CodeSchema, Path: path + ".dialogue_id", Message: "segment references unknown audio track"}
		}
		segID, err := reqString(obj, "segment_id", path)
		if err != nil {
			return nil, err
		}
		startMS, err := reqInt64(obj, "start_ms", path)
		if err != nil {
			return nil, err
		}
		endMS, err := reqInt64(obj, "end_ms", path)
		if err != nil {
			return nil, err
		}
		start, tr, err := MillisToSamples(startMS, int64(d.PCM.SampleRate), path+".start_ms", RoundHalfAway)
		if err != nil {
			return nil, err
		}
		appendTrace(p, tr, VersionV1)
		end, tr, err := MillisToSamples(endMS, int64(d.PCM.SampleRate), path+".end_ms", RoundHalfAway)
		if err != nil {
			return nil, err
		}
		appendTrace(p, tr, VersionV1)
		gainPermille, err := reqInt64(obj, "gain_permille", path)
		if err != nil {
			return nil, err
		}
		gain, tr, err := ScaleToPerMyriad(gainPermille, ScalePermille, path+".gain_permille", RoundHalfAway)
		if err != nil {
			return nil, err
		}
		appendTrace(p, tr, VersionV1)
		seg := Segment{ID: segID, DialogueID: dialogueID, StartSample: start, EndSample: end, Gain: gain}
		phArr, err := childArray(obj, "phonemes", path)
		if err != nil {
			return nil, err
		}
		for j, pv := range phArr {
			pobj, ok := pv.(map[string]any)
			ppath := arrayPath(path+".phonemes", j)
			if !ok {
				return nil, &Error{Code: CodeSchema, Path: ppath, Message: "expected JSON object"}
			}
			phID, err := reqString(pobj, "phoneme_id", ppath)
			if err != nil {
				return nil, err
			}
			phStartMS, err := reqInt64(pobj, "start_ms", ppath)
			if err != nil {
				return nil, err
			}
			phEndMS, err := reqInt64(pobj, "end_ms", ppath)
			if err != nil {
				return nil, err
			}
			ps, tr, err := MillisToSamples(phStartMS, int64(d.PCM.SampleRate), ppath+".start_ms", RoundHalfAway)
			if err != nil {
				return nil, err
			}
			appendTrace(p, tr, VersionV1)
			pe, tr, err := MillisToSamples(phEndMS, int64(d.PCM.SampleRate), ppath+".end_ms", RoundHalfAway)
			if err != nil {
				return nil, err
			}
			appendTrace(p, tr, VersionV1)
			class, err := reqString(pobj, "class", ppath)
			if err != nil {
				return nil, err
			}
			weightPermille, err := reqInt64(pobj, "weight_permille", ppath)
			if err != nil {
				return nil, err
			}
			weight, tr, err := ScaleToPerMyriad(weightPermille, ScalePermille, ppath+".weight_permille", RoundHalfAway)
			if err != nil {
				return nil, err
			}
			appendTrace(p, tr, VersionV1)
			seg.Phonemes = append(seg.Phonemes, Phoneme{
				ID: phID, StartSample: ps, EndSample: pe, Class: class, Weight: int(weight),
			})
		}
		d.Segments = append(d.Segments, seg)
	}
	for _, id := range order {
		p.Dialogues = append(p.Dialogues, *dialogues[id])
	}
	return p, nil
}

func loadV2(root map[string]any) (*Project, error) {
	const base = "$"
	projectID, err := reqString(root, "projectId", base)
	if err != nil {
		return nil, err
	}
	frameRate, err := reqInt64(root, "frameRateHz", base)
	if err != nil {
		return nil, err
	}
	sampleRate, err := reqInt64(root, "sampleRate", base)
	if err != nil {
		return nil, err
	}
	energyLimit, err := reqInt64(root, "frameEnergyLimit", base)
	if err != nil {
		return nil, err
	}
	parallel, err := childObject(root, "parallel", base)
	if err != nil {
		return nil, err
	}
	maxParallel, err := reqInt64(parallel, "maxParallelism", "$.parallel")
	if err != nil {
		return nil, err
	}
	bufferSamples, err := reqInt64(parallel, "workerBufferSamples", "$.parallel")
	if err != nil {
		return nil, err
	}
	queueBudget, err := reqInt64(parallel, "queueBudget", "$.parallel")
	if err != nil {
		return nil, err
	}
	target, err := reqInt64(root, "targetLoudnessBasisPoints", base)
	if err != nil {
		return nil, err
	}
	upper, err := reqInt64(root, "gainUpperBoundBasisPoints", base)
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID: projectID, SourceVersion: VersionV2, FrameRateHz: int(frameRate),
		SampleRate: int(sampleRate), FrameEnergyLimit: energyLimit,
		MaxParallelism: int(maxParallel), WorkerBufferSamples: int(bufferSamples),
		QueueBudget: int(queueBudget), Organization: "dialogue-nested",
	}
	if p.TargetLoudness, err = addScaleTrace(p, target, ScalePerMyriad, "$.targetLoudnessBasisPoints", VersionV2); err != nil {
		return nil, err
	}
	if p.GainUpperBound, err = addScaleTrace(p, upper, ScalePerMyriad, "$.gainUpperBoundBasisPoints", VersionV2); err != nil {
		return nil, err
	}
	if v, ok := optInt64(root, "sessionGainBasisPoints", 0); ok {
		if p.SessionGain, err = addScaleTrace(p, v, ScalePerMyriad, "$.sessionGainBasisPoints", VersionV2); err != nil {
			return nil, err
		}
	} else {
		addDefault(p, "$.sessionGainBasisPoints", "<missing>", "0", "v2 optional default")
	}
	if v, ok := optInt64(root, "toleranceBasisPoints", 100); ok {
		if p.Tolerance, err = addScaleTrace(p, v, ScalePerMyriad, "$.toleranceBasisPoints", VersionV2); err != nil {
			return nil, err
		}
	} else {
		addDefault(p, "$.toleranceBasisPoints", "<missing>", "100", "v2 optional default")
	}
	if q, ok := root["quantization"].(map[string]any); ok {
		p.QuantizationVersion = optString(q, "version", DefaultQuantizationVersion)
		if p.QuantizationVersion == "" {
			p.QuantizationVersion = DefaultQuantizationVersion
		}
	} else {
		p.QuantizationVersion = DefaultQuantizationVersion
		addDefault(p, "$.quantization.version", "<missing>", DefaultQuantizationVersion, "v2 optional default")
	}
	if conv, ok := root["conversion"].(map[string]any); ok {
		p.ConversionVersion = optString(conv, "version", DefaultConversionVersion)
	}
	p.ParallelVersion = DefaultParallelVersion
	if alg, ok := root["algorithmVersion"].(string); ok {
		p.AlgorithmVersion = alg
	}
	dialogues, err := childArray(root, "dialogues", base)
	if err != nil {
		return nil, err
	}
	for i, v := range dialogues {
		obj, ok := v.(map[string]any)
		path := arrayPath("$.dialogues", i)
		if !ok {
			return nil, &Error{Code: CodeSchema, Path: path, Message: "expected JSON object"}
		}
		d, err := loadNestedDialogue(obj, path, p, VersionV2, false)
		if err != nil {
			return nil, err
		}
		p.Dialogues = append(p.Dialogues, *d)
	}
	return p, nil
}

func addScaleTrace(p *Project, value, scale int64, path string, version SchemaVersion) (int64, error) {
	v, tr, err := ScaleToPerMyriad(value, scale, path, RoundHalfAway)
	if err != nil {
		return 0, err
	}
	p.Conversions = append(p.Conversions, tr)
	p.Conversions[len(p.Conversions)-1].SourceVersion = version
	p.Conversions[len(p.Conversions)-1].TargetVersion = VersionV3
	return v, nil
}

func loadNestedDialogue(obj map[string]any, path string, p *Project, version SchemaVersion, withMarkers bool) (*Dialogue, error) {
	id, err := reqString(obj, "id", path)
	if err != nil {
		return nil, err
	}
	pcmObj, err := childObject(obj, "pcm", path)
	if err != nil {
		return nil, err
	}
	encoding, err := reqString(pcmObj, "encoding", path+".pcm")
	if err != nil {
		return nil, err
	}
	trackRate, err := reqInt64(pcmObj, "sampleRate", path+".pcm")
	if err != nil {
		return nil, err
	}
	channels, err := reqInt64(pcmObj, "channels", path+".pcm")
	if err != nil {
		return nil, err
	}
	count, err := reqInt64(pcmObj, "sampleCount", path+".pcm")
	if err != nil {
		return nil, err
	}
	dataKey := "dataBase64"
	if version == VersionV3 {
		dataKey = "samplesBase64"
	}
	payload, err := reqString(pcmObj, dataKey, path+".pcm")
	if err != nil {
		return nil, err
	}
	samples, err := decodePCMBase64(payload, count, path+".pcm."+dataKey)
	if err != nil {
		return nil, err
	}
	d := &Dialogue{ID: id, PCM: PCM{
		Encoding: encoding, SampleRate: int(trackRate), Channels: int(channels),
		Samples: samples, DeclaredSampleCount: count,
	}}
	segs, err := childArray(obj, "segments", path)
	if err != nil {
		return nil, err
	}
	for i, v := range segs {
		sobj, ok := v.(map[string]any)
		spath := arrayPath(path+".segments", i)
		if !ok {
			return nil, &Error{Code: CodeSchema, Path: spath, Message: "expected JSON object"}
		}
		segID, err := reqString(sobj, "id", spath)
		if err != nil {
			return nil, err
		}
		start, err := reqInt64(sobj, "startSample", spath)
		if err != nil {
			return nil, err
		}
		end, err := reqInt64(sobj, "endSample", spath)
		if err != nil {
			return nil, err
		}
		gainKey := "gainBasisPoints"
		weightKey := "weightBasisPoints"
		phStartKey, phEndKey := "startSample", "endSample"
		if version == VersionV3 {
			gainKey = "gainPermyriad"
			weightKey = "weightPermyriad"
		}
		gainRaw, err := reqInt64(sobj, gainKey, spath)
		if err != nil {
			return nil, err
		}
		gain, tr, err := ScaleToPerMyriad(gainRaw, ScalePerMyriad, spath+"."+gainKey, RoundHalfAway)
		if err != nil {
			return nil, err
		}
		appendTrace(p, tr, version)
		seg := Segment{ID: segID, DialogueID: id, StartSample: start, EndSample: end, Gain: gain}
		phs, err := childArray(sobj, "phonemes", spath)
		if err != nil {
			return nil, err
		}
		for j, pv := range phs {
			pobj, ok := pv.(map[string]any)
			ppath := arrayPath(spath+".phonemes", j)
			if !ok {
				return nil, &Error{Code: CodeSchema, Path: ppath, Message: "expected JSON object"}
			}
			phID, err := reqString(pobj, "id", ppath)
			if err != nil {
				return nil, err
			}
			ps, err := reqInt64(pobj, phStartKey, ppath)
			if err != nil {
				return nil, err
			}
			pe, err := reqInt64(pobj, phEndKey, ppath)
			if err != nil {
				return nil, err
			}
			class, err := reqString(pobj, "class", ppath)
			if err != nil {
				return nil, err
			}
			weightRaw, err := reqInt64(pobj, weightKey, ppath)
			if err != nil {
				return nil, err
			}
			weight, tr, err := ScaleToPerMyriad(weightRaw, ScalePerMyriad, ppath+"."+weightKey, RoundHalfAway)
			if err != nil {
				return nil, err
			}
			appendTrace(p, tr, version)
			seg.Phonemes = append(seg.Phonemes, Phoneme{
				ID: phID, StartSample: ps, EndSample: pe, Class: class, Weight: int(weight),
			})
		}
		if withMarkers {
			if rawMarkers, ok := sobj["calibrationMarkers"].([]any); ok {
				for j, mv := range rawMarkers {
					markerObj, ok := mv.(map[string]any)
					mpath := arrayPath(spath+".calibrationMarkers", j)
					if !ok {
						return nil, &Error{Code: CodeSchema, Path: mpath, Message: "expected JSON object"}
					}
					kind, err := reqString(markerObj, "kind", mpath)
					if err != nil {
						return nil, err
					}
					value, err := reqString(markerObj, "value", mpath)
					if err != nil {
						return nil, err
					}
					seg.Markers = append(seg.Markers, CalibrationMarker{
						Path: mpath, SegmentID: segID, Kind: kind, Value: value,
					})
				}
			}
		}
		d.Segments = append(d.Segments, seg)
	}
	return d, nil
}

func loadV3(root map[string]any) (*Project, error) {
	const base = "$"
	project, err := childObject(root, "project", base)
	if err != nil {
		return nil, err
	}
	id, err := reqString(project, "id", "$.project")
	if err != nil {
		return nil, err
	}
	frameRate, err := reqInt64(project, "frameRateHz", "$.project")
	if err != nil {
		return nil, err
	}
	sampleRate, err := reqInt64(project, "sampleRate", "$.project")
	if err != nil {
		return nil, err
	}
	energyLimit, err := reqInt64(project, "frameEnergyLimit", "$.project")
	if err != nil {
		return nil, err
	}
	cal, err := childObject(project, "calibration", "$.project")
	if err != nil {
		return nil, err
	}
	target, err := reqInt64(cal, "targetLoudnessPermyriad", "$.project.calibration")
	if err != nil {
		return nil, err
	}
	upper, err := reqInt64(cal, "gainUpperBoundPermyriad", "$.project.calibration")
	if err != nil {
		return nil, err
	}
	tolerance, err := reqInt64(cal, "tolerancePermyriad", "$.project.calibration")
	if err != nil {
		return nil, err
	}
	parallel, err := childObject(project, "limitedParallel", "$.project")
	if err != nil {
		return nil, err
	}
	maxParallel, err := reqInt64(parallel, "maxParallelism", "$.project.limitedParallel")
	if err != nil {
		return nil, err
	}
	bufferSamples, err := reqInt64(parallel, "workerBufferSamples", "$.project.limitedParallel")
	if err != nil {
		return nil, err
	}
	queueBudget, err := reqInt64(parallel, "queueBudget", "$.project.limitedParallel")
	if err != nil {
		return nil, err
	}
	versionsObj, err := childObject(project, "versions", "$.project")
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID: id, SourceVersion: VersionV3, FrameRateHz: int(frameRate),
		SampleRate: int(sampleRate), FrameEnergyLimit: energyLimit,
		TargetLoudness: target, GainUpperBound: upper, Tolerance: tolerance,
		MaxParallelism: int(maxParallel), WorkerBufferSamples: int(bufferSamples),
		QueueBudget: int(queueBudget), Organization: "scene-grouped",
	}
	if v, ok := optInt64(cal, "sessionGainPermyriad", 0); ok {
		p.SessionGain = v
	} else {
		addDefault(p, "$.project.calibration.sessionGainPermyriad", "<missing>", "0", "v3 optional default")
	}
	p.QuantizationVersion, err = reqString(versionsObj, "quantization", "$.project.versions")
	if err != nil {
		return nil, err
	}
	p.ParallelVersion, err = reqString(versionsObj, "parallelBudget", "$.project.versions")
	if err != nil {
		return nil, err
	}
	p.ConversionVersion, err = reqString(versionsObj, "conversion", "$.project.versions")
	if err != nil {
		return nil, err
	}
	p.AlgorithmVersion, err = reqString(versionsObj, "algorithm", "$.project.versions")
	if err != nil {
		return nil, err
	}
	if markers, ok := cal["markers"].([]any); ok {
		for i, mv := range markers {
			mobj, ok := mv.(map[string]any)
			path := arrayPath("$.project.calibration.markers", i)
			if !ok {
				return nil, &Error{Code: CodeSchema, Path: path, Message: "expected JSON object"}
			}
			kind, err := reqString(mobj, "kind", path)
			if err != nil {
				return nil, err
			}
			value, err := reqString(mobj, "value", path)
			if err != nil {
				return nil, err
			}
			p.CalibrationMarkers = append(p.CalibrationMarkers, CalibrationMarker{
				Path: path, Kind: kind, Value: value,
			})
		}
	} else {
		addDefault(p, "$.project.calibration.markers", "<missing>", "[]", "v3 optional default")
	}
	groups, err := childArray(root, "sceneGroups", base)
	if err != nil {
		return nil, err
	}
	for gi, gv := range groups {
		gobj, ok := gv.(map[string]any)
		gpath := arrayPath("$.sceneGroups", gi)
		if !ok {
			return nil, &Error{Code: CodeSchema, Path: gpath, Message: "expected JSON object"}
		}
		groupID, err := reqString(gobj, "id", gpath)
		if err != nil {
			return nil, err
		}
		sg := SceneGroup{ID: groupID}
		dialogues, err := childArray(gobj, "dialogues", gpath)
		if err != nil {
			return nil, err
		}
		for di, dv := range dialogues {
			dobj, ok := dv.(map[string]any)
			dpath := arrayPath(gpath+".dialogues", di)
			if !ok {
				return nil, &Error{Code: CodeSchema, Path: dpath, Message: "expected JSON object"}
			}
			d, err := loadNestedDialogue(dobj, dpath, p, VersionV3, true)
			if err != nil {
				return nil, err
			}
			p.Dialogues = append(p.Dialogues, *d)
			sg.DialogueIDs = append(sg.DialogueIDs, d.ID)
		}
		p.SceneGroups = append(p.SceneGroups, sg)
	}
	return p, nil
}
