package audiofp

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Project struct {
	Dir          string
	Manifest     Manifest
	Initial      map[string]PCMAsset
	Replacements map[int]PCMAsset
	Queries      map[string]PCMAsset
	FinalAssets  []PCMAsset
	FinalRecipe  Recipe
	EditEvents   []EditEvent
}

func object(raw json.RawMessage, field string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, schemaError(field, "must be an object")
	}
	return m, nil
}

func requireKeys(m map[string]json.RawMessage, field string, keys ...string) error {
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			if field == "" {
				return schemaError(key, "required field is missing")
			}
			return schemaError(field+"."+key, "required field is missing")
		}
	}
	return nil
}

func decodeStruct(raw json.RawMessage, field string, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return schemaError(field, err.Error())
	}
	return nil
}

func decodeObject(raw json.RawMessage, field string, out any, keys ...string) error {
	m, err := object(raw, field)
	if err != nil {
		return err
	}
	if err := requireKeys(m, field, keys...); err != nil {
		return err
	}
	return decodeStruct(raw, field, out)
}

// LoadProject loads a project manifest and all referenced raw PCM files.
// Structural issues are E_SCHEMA, numeric boundaries are E_PARA, and
// missing/short/corrupt PCM is E_DECODE.
func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, decodeError("", "file", err.Error())
	}
	root, err := object(data, "")
	if err != nil {
		return nil, err
	}
	if err := requireKeys(root, "", "schema_version", "seed", "batch_id", "assets", "recipe", "edits", "truth"); err != nil {
		return nil, err
	}

	var manifest Manifest
	if err := decodeStruct(data, "manifest", &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 1 {
		return nil, parameterError("schema_version", "only schema_version 1 is supported")
	}
	if strings.TrimSpace(manifest.BatchID) == "" {
		return nil, schemaError("batch_id", "must not be empty")
	}
	recipeRaw, err := object(root["recipe"], "recipe")
	if err != nil {
		return nil, err
	}
	if err := requireKeys(recipeRaw, "recipe", "frame_len", "frame_shift", "bands", "quant_bits", "similarity_threshold", "candidate_limit"); err != nil {
		return nil, err
	}
	if err := ValidateRecipe(manifest.Recipe); err != nil {
		return nil, err
	}

	base := filepath.Dir(path)
	p := &Project{
		Dir:          base,
		Manifest:     manifest,
		Initial:      make(map[string]PCMAsset),
		Replacements: make(map[int]PCMAsset),
		Queries:      make(map[string]PCMAsset),
	}

	var assetRaws []json.RawMessage
	if err := json.Unmarshal(root["assets"], &assetRaws); err != nil || len(assetRaws) == 0 {
		return nil, schemaError("assets", "must be a non-empty array")
	}
	for i, raw := range assetRaws {
		field := fmt.Sprintf("assets[%d]", i)
		var spec AssetSpec
		if err := decodeObject(raw, field, &spec,
			"asset_id", "file", "sample_rate", "total_samples", "channels", "sample_format", "sha256"); err != nil {
			return nil, err
		}
		if err := validateAssetSpec(spec); err != nil {
			return nil, err
		}
		if _, exists := p.Initial[spec.ID]; exists {
			return nil, schemaError(field+".asset_id", "duplicate asset_id")
		}
		asset, err := readPCM(base, spec.File, spec.ID, spec.TotalSamples, spec.SHA256)
		if err != nil {
			return nil, err
		}
		asset.SampleRate = spec.SampleRate
		p.Initial[spec.ID] = asset
	}

	var editRaws []json.RawMessage
	if err := json.Unmarshal(root["edits"], &editRaws); err != nil {
		return nil, schemaError("edits", "must be an array")
	}
	if len(manifest.Edits) != len(editRaws) {
		return nil, schemaError("edits", "could not decode edit array")
	}
	var lastTime int64
	hasTime := false
	for i, raw := range editRaws {
		field := fmt.Sprintf("edits[%d]", i)
		op, err := decodeEditRaw(raw, field)
		if err != nil {
			return nil, err
		}
		if hasTime && op.LogicalTime < lastTime {
			return nil, &Error{
				Code: CodeOrder, Field: field + ".logical_time", AssetID: op.AssetID,
				Message: "logical time must be monotonically non-decreasing", Frame: -1,
			}
		}
		lastTime = op.LogicalTime
		hasTime = true
		if op.Kind == "replace" || op.Kind == "trim" {
			asset, ok := p.Initial[op.AssetID]
			if !ok {
				return nil, schemaError(field+".asset_id", "target asset does not exist")
			}
			if op.StartSample < 0 || op.EndSample <= op.StartSample || op.EndSample > len(asset.Samples) {
				return nil, parameterError(field+".start_sample", "edit range is outside the target asset")
			}
		}
		if op.Kind == "replace" {
			var replRaw map[string]json.RawMessage
			if err := json.Unmarshal(raw, &replRaw); err != nil {
				return nil, schemaError(field, err.Error())
			}
			if err := requireKeys(replRaw, field, "replacement_file", "replacement_sha256", "replacement_total_samples"); err != nil {
				return nil, err
			}
			if op.ReplacementTotalSamples != op.EndSample-op.StartSample {
				return nil, parameterError(field+".replacement_total_samples", "must equal end_sample-start_sample")
			}
			repl, err := readPCM(base, op.ReplacementFile, op.AssetID, op.ReplacementTotalSamples, op.ReplacementSHA256)
			if err != nil {
				return nil, err
			}
			p.Replacements[op.Seq] = repl
		}
		if op.Kind == "set_param" {
			if op.Value == nil {
				return nil, schemaError(field+".value", "required field is missing")
			}
			probe := manifest.Recipe
			if err := ApplyRecipeParam(&probe, op.Param, *op.Value); err != nil {
				if typed, ok := AsError(err); ok {
					typed.Field = field + "." + typed.Field
				}
				return nil, err
			}
		}
	}

	if err := validateTruthRaw(root["truth"]); err != nil {
		return nil, err
	}
	if err := validateTruthModels(&manifest.Truth); err != nil {
		return nil, err
	}
	for i := range manifest.Truth.Queries {
		q := &manifest.Truth.Queries[i]
		query, err := readPCM(base, q.File, q.QueryID, q.TotalSamples, q.SHA256)
		if err != nil {
			return nil, err
		}
		query.SampleRate = q.SampleRate
		p.Queries[q.QueryID] = query
	}
	p.Manifest = manifest
	return p, nil
}

func validateAssetSpec(spec AssetSpec) error {
	if strings.TrimSpace(spec.ID) == "" {
		return schemaError("asset_id", "must not be empty")
	}
	if strings.TrimSpace(spec.File) == "" {
		return schemaError("file", "must not be empty")
	}
	if spec.SampleRate < 1000 || spec.SampleRate > 384000 {
		return parameterError("sample_rate", "must be in [1000,384000]")
	}
	if spec.TotalSamples <= 0 {
		return parameterError("total_samples", "must be positive")
	}
	if spec.Channels != 1 {
		return parameterError("channels", "only mono PCM is supported")
	}
	if spec.SampleFormat != "s16le" {
		return parameterError("sample_format", "only s16le is supported")
	}
	if len(spec.SHA256) != 64 {
		return schemaError("sha256", "must be a 64-character SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(spec.SHA256); err != nil {
		return schemaError("sha256", "must be hexadecimal")
	}
	return nil
}

func decodeEditRaw(raw json.RawMessage, field string) (EditOp, error) {
	var op EditOp
	m, err := object(raw, field)
	if err != nil {
		return op, err
	}
	if err := requireKeys(m, field, "seq", "logical_time", "kind"); err != nil {
		return op, err
	}
	if err := decodeStruct(raw, field, &op); err != nil {
		return op, err
	}
	switch op.Kind {
	case "replace", "trim":
		if err := requireKeys(m, field, "asset_id", "start_sample", "end_sample"); err != nil {
			return op, err
		}
	case "set_param":
		if err := requireKeys(m, field, "param", "value"); err != nil {
			return op, err
		}
	default:
		return op, schemaError(field+".kind", "must be replace, trim, or set_param")
	}
	return op, nil
}

func resolveProjectPath(base, rel, field string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", schemaError(field, "must not be empty")
	}
	if filepath.IsAbs(rel) {
		return "", schemaError(field, "must be a relative path")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", schemaError(field, "must stay inside the project directory")
	}
	return filepath.Join(base, clean), nil
}

func readPCM(base, rel, id string, totalSamples int, wantSHA string) (PCMAsset, error) {
	path, err := resolveProjectPath(base, rel, "file")
	if err != nil {
		return PCMAsset{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return PCMAsset{}, decodeError(id, "file", err.Error())
	}
	wantBytes := totalSamples * 2
	if len(data) != wantBytes {
		return PCMAsset{}, decodeError(id, "total_samples",
			fmt.Sprintf("PCM file has %d bytes, manifest expects %d", len(data), wantBytes))
	}
	sum := sha256.Sum256(data)
	gotSHA := hex.EncodeToString(sum[:])
	if !strings.EqualFold(gotSHA, wantSHA) {
		return PCMAsset{}, decodeError(id, "sha256", "PCM content hash does not match manifest")
	}
	samples := make([]int16, totalSamples)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return PCMAsset{ID: id, Samples: samples}, nil
}

func validateTruthRaw(raw json.RawMessage) error {
	m, err := object(raw, "truth")
	if err != nil {
		return err
	}
	if err := requireKeys(m, "truth", "final_recipe", "assets", "markers", "queries"); err != nil {
		return err
	}
	recipeRaw, err := object(m["final_recipe"], "truth.final_recipe")
	if err != nil {
		return err
	}
	if err := requireKeys(recipeRaw, "truth.final_recipe",
		"frame_len", "frame_shift", "bands", "quant_bits", "similarity_threshold", "candidate_limit"); err != nil {
		return err
	}
	var assets []json.RawMessage
	if err := json.Unmarshal(m["assets"], &assets); err != nil {
		return schemaError("truth.assets", "must be an array")
	}
	for i, rawAsset := range assets {
		af := fmt.Sprintf("truth.assets[%d]", i)
		var t AssetTruth
		if err := decodeObject(rawAsset, af, &t, "asset_id", "content_hash", "total_samples", "frames"); err != nil {
			return err
		}
		am, _ := object(rawAsset, af)
		var frames []json.RawMessage
		if err := json.Unmarshal(am["frames"], &frames); err != nil {
			return schemaError(af+".frames", "must be an array")
		}
		for j, rawFrame := range frames {
			ff := fmt.Sprintf("%s.frames[%d]", af, j)
			if err := decodeObject(rawFrame, ff, &FrameTruth{},
				"frame_no", "start_sample", "bands", "bit_width", "levels", "packed_hex", "zero", "summary"); err != nil {
				return err
			}
		}
	}
	var markers []json.RawMessage
	if err := json.Unmarshal(m["markers"], &markers); err != nil {
		return schemaError("truth.markers", "must be an array")
	}
	for i, rawMarker := range markers {
		mf := fmt.Sprintf("truth.markers[%d]", i)
		if err := decodeObject(rawMarker, mf, &MarkerTruth{}, "marker_id", "asset_id", "sample", "frame_no", "description"); err != nil {
			return err
		}
	}
	var queries []json.RawMessage
	if err := json.Unmarshal(m["queries"], &queries); err != nil {
		return schemaError("truth.queries", "must be an array")
	}
	for i, rawQuery := range queries {
		qf := fmt.Sprintf("truth.queries[%d]", i)
		if err := decodeObject(rawQuery, qf, &QueryTruth{},
			"query_id", "asset_id", "file", "sample_rate", "total_samples", "sha256",
			"start_sample", "frame_count", "similarity_threshold", "candidate_limit", "expected"); err != nil {
			return err
		}
	}
	return nil
}

func validateTruthModels(t *Truth) error {
	if err := ValidateRecipe(t.FinalRecipe); err != nil {
		return err
	}
	for _, a := range t.Assets {
		if len(a.ContentHash) != 64 {
			return schemaError("truth.assets.content_hash", "must be a SHA-256 hex digest")
		}
		if a.TotalSamples <= 0 {
			return parameterError("truth.assets.total_samples", "must be positive")
		}
		wantFrames := frameCount(a.TotalSamples, t.FinalRecipe)
		if len(a.Frames) != wantFrames {
			return parameterError("truth.assets.frames", "frame count does not match final recipe")
		}
		for i, f := range a.Frames {
			if f.FrameNo != i {
				return &Error{Code: CodeOrder, AssetID: a.AssetID, Frame: i, Field: "frame_no",
					Message: "truth frames must be in ascending frame order"}
			}
			if f.StartSample != FrameStartOffset(i, t.FinalRecipe) {
				return parameterError("truth.assets.frames.start_sample", "frame start does not match recipe")
			}
			if f.Bands != t.FinalRecipe.Bands || f.BitWidth != t.FinalRecipe.QuantBits {
				return parameterError("truth.assets.frames.bit_width", "frame dimensions do not match recipe")
			}
			if len(f.Levels) != f.Bands {
				return parameterError("truth.assets.frames.levels", "level count does not match bands")
			}
			for _, level := range f.Levels {
				if int(level) > maxLevel(f.BitWidth) {
					return parameterError("truth.assets.frames.levels", "level exceeds quant_bits range")
				}
			}
			if got := PackLevels(f.Levels, f.BitWidth); got != f.PackedHex {
				return schemaError("truth.assets.frames.packed_hex", "does not match levels")
			}
			if got := FrameSummary(f.StartSample, f.Levels, f.BitWidth, f.Zero); got != f.Summary {
				return schemaError("truth.assets.frames.summary", "does not match frame contents")
			}
		}
	}
	for i := range t.Queries {
		q := &t.Queries[i]
		if err := ValidateRecipe(Recipe{
			FrameLen: t.FinalRecipe.FrameLen, FrameShift: t.FinalRecipe.FrameShift,
			Bands: t.FinalRecipe.Bands, QuantBits: t.FinalRecipe.QuantBits,
			SimilarityThreshold: q.Threshold, CandidateLimit: q.CandidateLimit,
		}); err != nil {
			return err
		}
		if q.FrameCount <= 0 {
			return parameterError("truth.queries.frame_count", "must be positive")
		}
		wantSamples := (q.FrameCount-1)*t.FinalRecipe.FrameShift + t.FinalRecipe.FrameLen
		if q.TotalSamples != wantSamples {
			return parameterError("truth.queries.total_samples", "does not match query frame count")
		}
		if q.StartSample < 0 || q.StartSample%t.FinalRecipe.FrameShift != 0 {
			return parameterError("truth.queries.start_sample", "must be aligned to frame_shift")
		}
		if len(q.Expected) > q.CandidateLimit {
			return parameterError("truth.queries.expected", "exceeds candidate_limit")
		}
		for rank := range q.Expected {
			if q.Expected[rank].Rank != rank+1 {
				return schemaError("truth.queries.expected.rank", "ranks must be sequential from 1")
			}
			if q.Expected[rank].EndSample <= q.Expected[rank].StartSample {
				return parameterError("truth.queries.expected.end_sample", "must follow start_sample")
			}
		}
	}
	return nil
}

// ApplyEdits clones initial assets and applies the manifest edit sequence.
func (p *Project) ApplyEdits() ([]PCMAsset, Recipe, []EditEvent, error) {
	assets := make(map[string]PCMAsset, len(p.Initial))
	ids := make([]string, 0, len(p.Initial))
	for id, asset := range p.Initial {
		assets[id] = asset.Clone()
		ids = append(ids, id)
	}
	sort.Strings(ids)
	recipe := p.Manifest.Recipe
	events := make([]EditEvent, 0, len(p.Manifest.Edits))
	for _, op := range p.Manifest.Edits {
		event, err := ApplyEdit(assets, p.Replacements, &recipe, op)
		if err != nil {
			return nil, Recipe{}, nil, err
		}
		events = append(events, event)
	}
	out := make([]PCMAsset, 0, len(ids))
	for _, id := range ids {
		out = append(out, assets[id])
	}
	return out, recipe, events, nil
}

// ApplyEdit applies one validated edit to an in-memory asset map.
func ApplyEdit(assets map[string]PCMAsset, replacements map[int]PCMAsset, recipe *Recipe, op EditOp) (EditEvent, error) {
	event := EditEvent{
		Seq: op.Seq, LogicalTime: op.LogicalTime, Kind: op.Kind, AssetID: op.AssetID,
	}
	switch op.Kind {
	case "set_param":
		if op.Value == nil {
			return event, schemaError("value", "required field is missing")
		}
		old := *recipe
		if err := ApplyRecipeParam(recipe, op.Param, *op.Value); err != nil {
			return event, err
		}
		event.Reason = fmt.Sprintf("recipe parameter %s changed", op.Param)
		event.FramesInvalidated = recipeIsFrameAffecting(old, *recipe)
		return event, nil
	case "replace", "trim":
		asset, ok := assets[op.AssetID]
		if !ok {
			return event, schemaError("asset_id", "target asset does not exist")
		}
		if op.StartSample < 0 || op.EndSample <= op.StartSample || op.EndSample > len(asset.Samples) {
			return event, parameterError("start_sample", "edit range is outside the target asset")
		}
		if op.Kind == "replace" {
			repl, ok := replacements[op.Seq]
			if !ok {
				return event, decodeError(op.AssetID, "replacement_file", "replacement PCM was not loaded")
			}
			if len(repl.Samples) != op.EndSample-op.StartSample {
				return event, parameterError("replacement_total_samples", "replacement length does not match edit range")
			}
			copy(asset.Samples[op.StartSample:op.EndSample], repl.Samples)
			event.Reason = fmt.Sprintf("asset segment [%d,%d) replaced", op.StartSample, op.EndSample)
		} else {
			asset.Samples = append(asset.Samples[:op.StartSample], asset.Samples[op.EndSample:]...)
			event.Reason = fmt.Sprintf("asset segment [%d,%d) trimmed", op.StartSample, op.EndSample)
		}
		asset.Samples = append([]int16(nil), asset.Samples...)
		assets[op.AssetID] = asset
		event.FramesInvalidated = true
		return event, nil
	default:
		return event, schemaError("kind", "unsupported edit kind")
	}
}

// ValidateTruthAgainstFinal checks the generated truth against the edited
// project without invoking spectral extraction.
func (p *Project) ValidateTruthAgainstFinal(final []PCMAsset, recipe Recipe) error {
	if recipe != p.Manifest.Truth.FinalRecipe {
		return schemaError("truth.final_recipe", "does not match the recipe produced by edits")
	}
	byID := make(map[string]AssetTruth, len(p.Manifest.Truth.Assets))
	for _, t := range p.Manifest.Truth.Assets {
		byID[t.AssetID] = t
	}
	if len(byID) != len(final) {
		return schemaError("truth.assets", "must contain one entry per final asset")
	}
	for _, asset := range final {
		t, ok := byID[asset.ID]
		if !ok {
			return schemaError("truth.assets.asset_id", "missing final asset "+asset.ID)
		}
		if t.TotalSamples != len(asset.Samples) {
			return parameterError("truth.assets.total_samples", "does not match edited asset")
		}
		if len(asset.Samples) < recipe.FrameLen {
			return parameterError("total_samples", "edited asset is shorter than frame_len")
		}
		if !strings.EqualFold(t.ContentHash, asset.ContentHash()) {
			return decodeError(asset.ID, "truth.assets.content_hash", "does not match edited asset content")
		}
	}
	assetMap := make(map[string]PCMAsset, len(final))
	for _, a := range final {
		assetMap[a.ID] = a
	}
	for _, marker := range p.Manifest.Truth.Markers {
		asset, ok := assetMap[marker.AssetID]
		if !ok {
			return schemaError("truth.markers.asset_id", "unknown asset")
		}
		if marker.Sample < 0 || marker.Sample >= len(asset.Samples) {
			return parameterError("truth.markers.sample", "outside the final asset")
		}
		if marker.FrameNo != marker.Sample/recipe.FrameShift {
			return parameterError("truth.markers.frame_no", "does not match sample position")
		}
	}
	for _, q := range p.Manifest.Truth.Queries {
		if _, ok := assetMap[q.AssetID]; !ok {
			return schemaError("truth.queries.asset_id", "unknown asset")
		}
		query, ok := p.Queries[q.QueryID]
		if !ok {
			return schemaError("truth.queries.query_id", "query PCM was not loaded")
		}
		if len(query.Samples) != q.TotalSamples {
			return parameterError("truth.queries.total_samples", "query file length mismatch")
		}
		if q.SampleRate != assetMap[q.AssetID].SampleRate {
			return parameterError("truth.queries.sample_rate", "does not match query asset")
		}
	}
	return nil
}

// EncodePCM returns little-endian s16le bytes.
func EncodePCM(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(s))
	}
	return out
}

// DecodePCM parses little-endian s16le bytes.
func DecodePCM(data []byte) ([]int16, error) {
	if len(data)%2 != 0 {
		return nil, decodeError("", "file", "PCM byte count must be even")
	}
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return out, nil
}

// PCMBytesEqual is useful for deterministic fixture checks.
func PCMBytesEqual(a, b []int16) bool {
	if len(a) != len(b) {
		return false
	}
	ab := EncodePCM(a)
	bb := EncodePCM(b)
	return bytes.Equal(ab, bb)
}
