package project

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/dsp"
)

// Load detects the interchange version and maps it to the canonical model.
func Load(path string) (*Program, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeVersion, "cannot read program", err).WithPath(path)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, apperr.Wrap(apperr.CodeVersion, "invalid program JSON", err).WithPath(path)
	}
	projectDir := filepath.Dir(path)
	switch {
	case object["schemaVersion"] != nil:
		version, err := stringValue(object["schemaVersion"], "schemaVersion")
		if err != nil {
			return nil, err
		}
		if version != "2" && version != "2.0" {
			return nil, apperr.New(apperr.CodeVersion, "unknown program version").
				WithField("schemaVersion").WithActual(version)
		}
		return loadV2(object, projectDir)
	case object["format"] != nil:
		version, err := stringValue(object["format"], "format")
		if err != nil {
			return nil, err
		}
		if version != "3" && version != "loudness-workbench/3" {
			return nil, apperr.New(apperr.CodeVersion, "unknown program version").
				WithField("format").WithActual(version)
		}
		return loadV3(object, projectDir)
	case object["version"] != nil:
		version, err := intValue(object["version"], "version")
		if err != nil {
			return nil, err
		}
		if version != 1 {
			return nil, apperr.New(apperr.CodeVersion, "unknown program version").
				WithField("version").WithActual(version)
		}
		return loadV1(object, projectDir)
	default:
		return nil, apperr.New(apperr.CodeVersion, "program version marker is missing").WithField("version")
	}
}

func LoadWithManifest(projectPath, manifestPath string) (*Program, error) {
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	program, err := Load(projectPath)
	if err != nil {
		return nil, err
	}
	if err := ValidateAgainstManifest(program, manifest); err != nil {
		return nil, err
	}
	return program, nil
}

func loadV1(root map[string]json.RawMessage, projectDir string) (*Program, error) {
	programID, err := stringField(root, "program_id", "program_id")
	if err != nil {
		return nil, err
	}
	rate, err := intField(root, "sample_rate_hz", "sample_rate_hz")
	if err != nil {
		return nil, err
	}
	target, err := numberField(root, "target_lufs", "target_lufs")
	if err != nil {
		return nil, err
	}
	frameMs, err := numberField(root, "frame_ms", "frame_ms")
	if err != nil {
		return nil, err
	}
	hopMs, err := numberField(root, "hop_ms", "hop_ms")
	if err != nil {
		return nil, err
	}
	gateObj, err := objectField(root, "gate", "gate")
	if err != nil {
		return nil, err
	}
	absolute, err := numberField(gateObj, "absolute_lufs", "gate.absolute_lufs")
	if err != nil {
		return nil, err
	}
	relative, err := numberField(gateObj, "relative_lu", "gate.relative_lu")
	if err != nil {
		return nil, err
	}
	stages, err := stringArrayField(root, "compression_chain", "compression_chain")
	if err != nil {
		return nil, err
	}
	normalizedStages, err := normalizeStagesV1(stages)
	if err != nil {
		return nil, err
	}
	compressorObj, err := objectField(root, "compressor", "compressor")
	if err != nil {
		return nil, err
	}
	compressor, err := compressorV1(compressorObj)
	if err != nil {
		return nil, err
	}
	limiterObj, err := objectField(root, "limiter", "limiter")
	if err != nil {
		return nil, err
	}
	limiter, err := limiterV1(limiterObj)
	if err != nil {
		return nil, err
	}
	segmentArray, err := arrayField(root, "segments", "segments")
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0, len(segmentArray))
	for i, raw := range segmentArray {
		base := fmt.Sprintf("segments[%d]", i)
		segmentObj, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		id, err := stringField(segmentObj, "segment_id", base+".segment_id")
		if err != nil {
			return nil, err
		}
		channels, err := intField(segmentObj, "channels", base+".channels")
		if err != nil {
			return nil, err
		}
		sourceObj, err := objectField(segmentObj, "source", base+".source")
		if err != nil {
			return nil, err
		}
		kind, err := stringField(sourceObj, "kind", base+".source.kind")
		if err != nil {
			return nil, err
		}
		if kind != "wav-slice" {
			return nil, apperr.New(apperr.CodeVersion, "unknown v1 audio reference kind").
				WithField(base + ".source.kind").WithActual(kind)
		}
		refPath, err := stringField(sourceObj, "path", base+".source.path")
		if err != nil {
			return nil, err
		}
		offset, err := intField(sourceObj, "offset_samples", base+".source.offset_samples")
		if err != nil {
			return nil, err
		}
		count, err := intField(sourceObj, "sample_count", base+".source.sample_count")
		if err != nil {
			return nil, err
		}
		hash, err := stringField(sourceObj, "sha256", base+".source.sha256")
		if err != nil {
			return nil, err
		}
		segment, err := loadAudioReference(projectDir, i, id, channels, refPath, offset, count, hash, rate)
		if err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}
	program := &Program{
		ProjectID: programID, SourceVersion: SourceV1, SampleRateHz: rate,
		TargetLUFS: target, FrameMs: frameMs, HopMs: hopMs,
		Gate: gateConfig(absolute, relative), Compressor: compressor, Limiter: limiter,
		StageOrder: normalizedStages, Segments: segments,
	}
	if err := program.Validate(); err != nil {
		return nil, err
	}
	return program, nil
}

func loadV2(root map[string]json.RawMessage, projectDir string) (*Program, error) {
	programID, err := stringField(root, "programId", "programId")
	if err != nil {
		return nil, err
	}
	rate, err := intField(root, "sampleRateHz", "sampleRateHz")
	if err != nil {
		return nil, err
	}
	target, err := numberField(root, "targetLufs", "targetLufs")
	if err != nil {
		return nil, err
	}
	frameMs, err := numberField(root, "frameMs", "frameMs")
	if err != nil {
		return nil, err
	}
	hopMs, err := numberField(root, "hopMs", "hopMs")
	if err != nil {
		return nil, err
	}
	gating, err := objectField(root, "gating", "gating")
	if err != nil {
		return nil, err
	}
	absolute, err := numberField(gating, "absoluteLufs", "gating.absoluteLufs")
	if err != nil {
		return nil, err
	}
	relative, err := numberField(gating, "relativeDropLu", "gating.relativeDropLu")
	if err != nil {
		return nil, err
	}
	stages, err := stringArrayField(root, "stageOrder", "stageOrder")
	if err != nil {
		return nil, err
	}
	normalizedStages, err := normalizeStagesV2(stages)
	if err != nil {
		return nil, err
	}
	compressorObj, err := objectField(root, "compressor", "compressor")
	if err != nil {
		return nil, err
	}
	compressor, err := compressorV2(compressorObj)
	if err != nil {
		return nil, err
	}
	limiterObj, err := objectField(root, "limiter", "limiter")
	if err != nil {
		return nil, err
	}
	limiter, err := limiterV2(limiterObj)
	if err != nil {
		return nil, err
	}
	assetArray, err := arrayField(root, "assets", "assets")
	if err != nil {
		return nil, err
	}
	type asset struct {
		id   string
		path string
		hash string
		rate int
	}
	assets := map[string]asset{}
	for i, raw := range assetArray {
		base := fmt.Sprintf("assets[%d]", i)
		obj, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		id, err := stringField(obj, "assetId", base+".assetId")
		if err != nil {
			return nil, err
		}
		path, err := stringField(obj, "wavPath", base+".wavPath")
		if err != nil {
			return nil, err
		}
		hash, err := stringField(obj, "contentSha256", base+".contentSha256")
		if err != nil {
			return nil, err
		}
		assetRate, err := intField(obj, "sampleRateHz", base+".sampleRateHz")
		if err != nil {
			return nil, err
		}
		if _, exists := assets[id]; exists {
			return nil, apperr.New(apperr.CodeVersion, "duplicate asset id").WithField(base + ".assetId").WithActual(id)
		}
		assets[id] = asset{id: id, path: path, hash: hash, rate: assetRate}
	}
	segmentArray, err := arrayField(root, "segments", "segments")
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0, len(segmentArray))
	for i, raw := range segmentArray {
		base := fmt.Sprintf("segments[%d]", i)
		obj, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		id, err := stringField(obj, "id", base+".id")
		if err != nil {
			return nil, err
		}
		assetRef, err := stringField(obj, "assetRef", base+".assetRef")
		if err != nil {
			return nil, err
		}
		asset, ok := assets[assetRef]
		if !ok {
			return nil, apperr.New(apperr.CodeVersion, "segment references an unknown asset").
				WithField(base + ".assetRef").WithActual(assetRef)
		}
		channels, err := intField(obj, "channels", base+".channels")
		if err != nil {
			return nil, err
		}
		offset, err := intField(obj, "offsetSamples", base+".offsetSamples")
		if err != nil {
			return nil, err
		}
		count, err := intField(obj, "frameCount", base+".frameCount")
		if err != nil {
			return nil, err
		}
		if asset.rate != rate {
			return nil, apperr.New(apperr.CodeSchema, "asset sample rate differs from program").
				WithField(base + ".sampleRateHz").WithExpected(rate).WithActual(asset.rate)
		}
		segment, err := loadAudioReference(projectDir, i, id, channels, asset.path, offset, count, asset.hash, rate)
		if err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}
	program := &Program{
		ProjectID: programID, SourceVersion: SourceV2, SampleRateHz: rate,
		TargetLUFS: target, FrameMs: frameMs, HopMs: hopMs,
		Gate: gateConfig(absolute, relative), Compressor: compressor, Limiter: limiter,
		StageOrder: normalizedStages, Segments: segments,
	}
	if err := program.Validate(); err != nil {
		return nil, err
	}
	return program, nil
}

func loadV3(root map[string]json.RawMessage, projectDir string) (*Program, error) {
	programObj, err := objectField(root, "program", "program")
	if err != nil {
		return nil, err
	}
	programID, err := stringField(programObj, "id", "program.id")
	if err != nil {
		return nil, err
	}
	rate, err := intField(programObj, "sampleRateHz", "program.sampleRateHz")
	if err != nil {
		return nil, err
	}
	target, err := numberField(programObj, "targetLufs", "program.targetLufs")
	if err != nil {
		return nil, err
	}
	analysis, err := objectField(root, "analysis", "analysis")
	if err != nil {
		return nil, err
	}
	frameMs, err := numberField(analysis, "frameMs", "analysis.frameMs")
	if err != nil {
		return nil, err
	}
	hopMs, err := numberField(analysis, "hopMs", "analysis.hopMs")
	if err != nil {
		return nil, err
	}
	gateArray, err := arrayField(analysis, "gates", "analysis.gates")
	if err != nil {
		return nil, err
	}
	absolute, relative, err := parseV3Gates(gateArray)
	if err != nil {
		return nil, err
	}
	stageArray, err := arrayField(root, "processingStages", "processingStages")
	if err != nil {
		return nil, err
	}
	stageOrder := make([]string, 0, len(stageArray))
	var compressor dspCompressorConfig
	var limiter dspLimiterConfig
	for i, raw := range stageArray {
		base := fmt.Sprintf("processingStages[%d]", i)
		obj, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		stageType, err := stringField(obj, "type", base+".type")
		if err != nil {
			return nil, err
		}
		switch stageType {
		case "compressor":
			compressor, err = compressorV3(obj, base)
			if err != nil {
				return nil, err
			}
			stageOrder = append(stageOrder, "compressor")
		case "limiter":
			limiter, err = limiterV3(obj, base)
			if err != nil {
				return nil, err
			}
			stageOrder = append(stageOrder, "limiter")
		default:
			return nil, apperr.New(apperr.CodeVersion, "unknown processing stage").
				WithField(base + ".type").WithActual(stageType)
		}
	}
	if compressor.set == false || limiter.set == false {
		return nil, apperr.New(apperr.CodeVersion, "processing stage list must contain compressor and limiter").
			WithField("processingStages")
	}
	mediaObj, err := objectField(root, "media", "media")
	if err != nil {
		return nil, err
	}
	baseDir, err := stringField(mediaObj, "baseDir", "media.baseDir")
	if err != nil {
		return nil, err
	}
	clipsObj, err := objectField(mediaObj, "clips", "media.clips")
	if err != nil {
		return nil, err
	}
	type clip struct {
		file string
		hash string
		rate int
	}
	clips := map[string]clip{}
	for key, raw := range clipsObj {
		base := "media.clips." + key
		obj, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		file, err := stringField(obj, "file", base+".file")
		if err != nil {
			return nil, err
		}
		hash, err := stringField(obj, "contentSha256", base+".contentSha256")
		if err != nil {
			return nil, err
		}
		clipRate, err := intField(obj, "sampleRateHz", base+".sampleRateHz")
		if err != nil {
			return nil, err
		}
		clips[key] = clip{file: filepath.Join(baseDir, file), hash: hash, rate: clipRate}
	}
	segmentArray, err := arrayField(root, "segments", "segments")
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0, len(segmentArray))
	for i, raw := range segmentArray {
		base := fmt.Sprintf("segments[%d]", i)
		obj, err := asObject(raw, base)
		if err != nil {
			return nil, err
		}
		id, err := stringField(obj, "key", base+".key")
		if err != nil {
			return nil, err
		}
		clipKey, err := stringField(obj, "clip", base+".clip")
		if err != nil {
			return nil, err
		}
		clip, ok := clips[clipKey]
		if !ok {
			return nil, apperr.New(apperr.CodeVersion, "segment references an unknown clip").
				WithField(base + ".clip").WithActual(clipKey)
		}
		channels, err := intField(obj, "channels", base+".channels")
		if err != nil {
			return nil, err
		}
		offset, err := intField(obj, "offsetSamples", base+".offsetSamples")
		if err != nil {
			return nil, err
		}
		count, err := intField(obj, "lengthSamples", base+".lengthSamples")
		if err != nil {
			return nil, err
		}
		if clip.rate != rate {
			return nil, apperr.New(apperr.CodeSchema, "clip sample rate differs from program").
				WithField(base + ".sampleRateHz").WithExpected(rate).WithActual(clip.rate)
		}
		segment, err := loadAudioReference(projectDir, i, id, channels, clip.file, offset, count, clip.hash, rate)
		if err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}
	program := &Program{
		ProjectID: programID, SourceVersion: SourceV3, SampleRateHz: rate,
		TargetLUFS: target, FrameMs: frameMs, HopMs: hopMs,
		Gate: gateConfig(absolute, relative), Compressor: compressor.value, Limiter: limiter.value,
		StageOrder: stageOrder, Segments: segments,
	}
	if err := program.Validate(); err != nil {
		return nil, err
	}
	return program, nil
}

// Small wrappers let v3 stage parsing track presence without importing config
// types under different names.
type dspCompressorConfig struct {
	value dsp.CompressorConfig
	set   bool
}

type dspLimiterConfig struct {
	value dsp.LimiterConfig
	set   bool
}

func loadAudioReference(projectDir string, index int, id string, channels int, refPath string, offset, count int, hash string, programRate int) (Segment, error) {
	base := fmt.Sprintf("segments[%d]", index)
	if count <= 0 {
		return Segment{}, apperr.New(apperr.CodeSchema, "segment length must be positive").
			WithField(base + ".sample_count").WithActual(count)
	}
	if offset < 0 {
		return Segment{}, apperr.New(apperr.CodeSchema, "segment offset must not be negative").
			WithField(base + ".offset_samples").WithActual(offset)
	}
	path := refPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(projectDir, path)
	}
	signal, err := audio.ReadWAV(path)
	if err != nil {
		if e, ok := apperr.As(err); ok {
			e.WithSegment(index, id)
		}
		return Segment{}, err
	}
	if signal.SampleRateHz != programRate {
		return Segment{}, apperr.New(apperr.CodeSchema, "audio sample rate differs from program").
			WithField(base + ".sample_rate_hz").WithExpected(programRate).WithActual(signal.SampleRateHz)
	}
	if signal.Channels != channels {
		return Segment{}, apperr.New(apperr.CodeSchema, "audio channel count differs from segment declaration").
			WithField(base + ".channels").WithExpected(channels).WithActual(signal.Channels)
	}
	if offset+count > signal.Frames() {
		return Segment{}, apperr.New(apperr.CodeSchema, "audio slice exceeds source frame count").
			WithField(base + ".sample_count").WithExpected(signal.Frames() - offset).WithActual(count)
	}
	slice := audio.New(signal.SampleRateHz, signal.Channels, count)
	copy(slice.Data, signal.Data[offset*channels:(offset+count)*channels])
	actualHash := audio.ContentHash(slice)
	if hash != actualHash {
		return Segment{}, apperr.New(apperr.CodeSchema, "audio content hash mismatch").
			WithPath(base + ".content_sha256").WithExpected(hash).WithActual(actualHash)
	}
	absolute, _ := filepath.Abs(path)
	return Segment{
		Index: index, ID: id, Channels: channels, SourcePath: absolute,
		ContentSHA256: actualHash, Source: slice,
	}, nil
}

func normalizeStagesV1(stages []string) ([]string, error) {
	if len(stages) != 2 {
		return nil, apperr.New(apperr.CodeVersion, "v1 compression_chain must contain two stages").
			WithField("compression_chain").WithActual(stages)
	}
	mapping := map[string]string{"compress": "compressor", "limit": "limiter"}
	out := make([]string, len(stages))
	for i, stage := range stages {
		normalized, ok := mapping[stage]
		if !ok {
			return nil, apperr.New(apperr.CodeVersion, "unknown v1 compression stage").
				WithField(fmt.Sprintf("compression_chain[%d]", i)).WithActual(stage)
		}
		out[i] = normalized
	}
	return validateCanonicalStageOrder(out)
}

func normalizeStagesV2(stages []string) ([]string, error) {
	if len(stages) != 2 {
		return nil, apperr.New(apperr.CodeVersion, "v2 stageOrder must contain two stages").
			WithField("stageOrder").WithActual(stages)
	}
	return validateCanonicalStageOrder(append([]string(nil), stages...))
}

func validateCanonicalStageOrder(stages []string) ([]string, error) {
	for i, expected := range CanonicalStageOrder {
		if i >= len(stages) || stages[i] != expected {
			return nil, apperr.New(apperr.CodeVersion, "unsupported or reordered processing stage list").
				WithField(fmt.Sprintf("processing_stages[%d]", i)).WithExpected(expected).WithActual(stages)
		}
	}
	return stages, nil
}

func compressorV1(obj map[string]json.RawMessage) (dsp.CompressorConfig, error) {
	threshold, err := numberField(obj, "threshold_dbfs", "compressor.threshold_dbfs")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	ratio, err := numberField(obj, "ratio", "compressor.ratio")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	attack, err := numberField(obj, "attack_ms", "compressor.attack_ms")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	release, err := numberField(obj, "release_ms", "compressor.release_ms")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	return dsp.CompressorConfig{ThresholdDBFS: threshold, Ratio: ratio, AttackMs: attack, ReleaseMs: release}, nil
}

func limiterV1(obj map[string]json.RawMessage) (dsp.LimiterConfig, error) {
	ceiling, err := numberField(obj, "ceiling_dbtp", "limiter.ceiling_dbtp")
	if err != nil {
		return dsp.LimiterConfig{}, err
	}
	lookahead, err := numberField(obj, "lookahead_ms", "limiter.lookahead_ms")
	if err != nil {
		return dsp.LimiterConfig{}, err
	}
	return dsp.LimiterConfig{CeilingDBTP: ceiling, LookaheadMs: lookahead}, nil
}

func compressorV2(obj map[string]json.RawMessage) (dsp.CompressorConfig, error) {
	threshold, err := numberField(obj, "thresholdDbfs", "compressor.thresholdDbfs")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	ratio, err := numberField(obj, "ratio", "compressor.ratio")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	attack, err := numberField(obj, "attackMs", "compressor.attackMs")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	release, err := numberField(obj, "releaseMs", "compressor.releaseMs")
	if err != nil {
		return dsp.CompressorConfig{}, err
	}
	return dsp.CompressorConfig{ThresholdDBFS: threshold, Ratio: ratio, AttackMs: attack, ReleaseMs: release}, nil
}

func limiterV2(obj map[string]json.RawMessage) (dsp.LimiterConfig, error) {
	ceiling, err := numberField(obj, "ceilingDbtp", "limiter.ceilingDbtp")
	if err != nil {
		return dsp.LimiterConfig{}, err
	}
	lookahead, err := numberField(obj, "lookaheadMs", "limiter.lookaheadMs")
	if err != nil {
		return dsp.LimiterConfig{}, err
	}
	return dsp.LimiterConfig{CeilingDBTP: ceiling, LookaheadMs: lookahead}, nil
}

func compressorV3(obj map[string]json.RawMessage, base string) (dspCompressorConfig, error) {
	threshold, err := numberField(obj, "thresholdDbfs", base+".thresholdDbfs")
	if err != nil {
		return dspCompressorConfig{}, err
	}
	ratio, err := numberField(obj, "ratio", base+".ratio")
	if err != nil {
		return dspCompressorConfig{}, err
	}
	attack, err := numberField(obj, "attackMs", base+".attackMs")
	if err != nil {
		return dspCompressorConfig{}, err
	}
	release, err := numberField(obj, "releaseMs", base+".releaseMs")
	if err != nil {
		return dspCompressorConfig{}, err
	}
	return dspCompressorConfig{
		value: dsp.CompressorConfig{ThresholdDBFS: threshold, Ratio: ratio, AttackMs: attack, ReleaseMs: release},
		set:   true,
	}, nil
}

func limiterV3(obj map[string]json.RawMessage, base string) (dspLimiterConfig, error) {
	ceiling, err := numberField(obj, "ceilingDbtp", base+".ceilingDbtp")
	if err != nil {
		return dspLimiterConfig{}, err
	}
	lookahead, err := numberField(obj, "lookaheadMs", base+".lookaheadMs")
	if err != nil {
		return dspLimiterConfig{}, err
	}
	return dspLimiterConfig{
		value: dsp.LimiterConfig{CeilingDBTP: ceiling, LookaheadMs: lookahead},
		set:   true,
	}, nil
}

func parseV3Gates(raw []json.RawMessage) (float64, float64, error) {
	var absolute, relative float64
	var haveAbsolute, haveRelative bool
	for i, item := range raw {
		base := fmt.Sprintf("analysis.gates[%d]", i)
		obj, err := asObject(item, base)
		if err != nil {
			return 0, 0, err
		}
		kind, err := stringField(obj, "kind", base+".kind")
		if err != nil {
			return 0, 0, err
		}
		value, err := numberField(obj, "value", base+".value")
		if err != nil {
			return 0, 0, err
		}
		unit, err := stringField(obj, "unit", base+".unit")
		if err != nil {
			return 0, 0, err
		}
		switch kind {
		case "absolute":
			if unit != "LUFS" {
				return 0, 0, apperr.New(apperr.CodeVersion, "v3 absolute gate unit must be LUFS").
					WithField(base + ".unit").WithActual(unit)
			}
			absolute = value
			haveAbsolute = true
		case "relative":
			switch unit {
			case "energy_ratio":
				if value <= 0 || value > 1 {
					return 0, 0, apperr.New(apperr.CodeVersion, "v3 relative energy ratio must be in (0, 1]").
						WithField(base + ".value").WithActual(value)
				}
				relative = roundCanonical(-10 * math.Log10(value))
			case "LU", "lu":
				relative = value
			default:
				return 0, 0, apperr.New(apperr.CodeVersion, "unknown v3 relative gate unit").
					WithField(base + ".unit").WithActual(unit)
			}
			haveRelative = true
		default:
			return 0, 0, apperr.New(apperr.CodeVersion, "unknown v3 gate kind").
				WithField(base + ".kind").WithActual(kind)
		}
	}
	if !haveAbsolute || !haveRelative {
		return 0, 0, apperr.New(apperr.CodeVersion, "v3 gates must contain absolute and relative entries").
			WithField("analysis.gates")
	}
	return absolute, relative, nil
}

func roundCanonical(value float64) float64 {
	if math.Abs(value) < 5e-13 {
		return 0
	}
	return math.Round(value*1e12) / 1e12
}

func gateConfig(absolute, relative float64) dsp.GateConfig {
	return dsp.GateConfig{AbsoluteLUFS: absolute, RelativeLU: relative}
}

func stringValue(raw json.RawMessage, path string) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", apperr.New(apperr.CodeVersion, "field must be a string").WithField(path)
	}
	return value, nil
}

func intValue(raw json.RawMessage, path string) (int, error) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, apperr.New(apperr.CodeVersion, "field must be an integer").WithField(path)
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, apperr.New(apperr.CodeVersion, "field must be an integer").WithField(path).WithActual(number.String())
	}
	return int(value), nil
}

func objectField(object map[string]json.RawMessage, key, path string) (map[string]json.RawMessage, error) {
	raw, ok := object[key]
	if !ok {
		return nil, apperr.New(apperr.CodeVersion, "required field is missing").WithField(path).WithPath(path)
	}
	return asObject(raw, path)
}

func asObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, apperr.New(apperr.CodeVersion, "field must be an object").WithField(path)
	}
	return object, nil
}

func arrayField(object map[string]json.RawMessage, key, path string) ([]json.RawMessage, error) {
	raw, ok := object[key]
	if !ok {
		return nil, apperr.New(apperr.CodeVersion, "required field is missing").WithField(path).WithPath(path)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, apperr.New(apperr.CodeVersion, "field must be an array").WithField(path)
	}
	return values, nil
}

func stringField(object map[string]json.RawMessage, key, path string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", apperr.New(apperr.CodeVersion, "required field is missing").WithField(path).WithPath(path)
	}
	return stringValue(raw, path)
}

func numberField(object map[string]json.RawMessage, key, path string) (float64, error) {
	raw, ok := object[key]
	if !ok {
		return 0, apperr.New(apperr.CodeVersion, "required field is missing").WithField(path).WithPath(path)
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, apperr.New(apperr.CodeVersion, "field must be a finite number").WithField(path)
	}
	return value, nil
}

func intField(object map[string]json.RawMessage, key, path string) (int, error) {
	raw, ok := object[key]
	if !ok {
		return 0, apperr.New(apperr.CodeVersion, "required field is missing").WithField(path).WithPath(path)
	}
	return intValue(raw, path)
}

func stringArrayField(object map[string]json.RawMessage, key, path string) ([]string, error) {
	raw, ok := object[key]
	if !ok {
		return nil, apperr.New(apperr.CodeVersion, "required field is missing").WithField(path).WithPath(path)
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, apperr.New(apperr.CodeVersion, "field must be a string array").WithField(path)
	}
	return values, nil
}
