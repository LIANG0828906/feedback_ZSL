package ocrflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

func LoadRecipeDocument(data []byte) (*Recipe, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, schemaError("unknown", "$", "invalid JSON: %v", err)
	}
	schema, _ := stringValue(root["schema"])
	if schema == "" {
		schema, _ = stringValue(root["schema_version"])
	}
	if schema == "" {
		return nil, schemaError("unknown", "schema", "schema or schema_version is required")
	}

	var (
		r   *Recipe
		err error
	)
	switch schema {
	case SchemaV1:
		r, err = parseV1(root)
	case SchemaV2:
		r, err = parseV2(root)
	case SchemaV3:
		r, err = parseV3(root)
	default:
		return nil, schemaError(schema, "schema", "unsupported recipe schema %q", schema)
	}
	if err != nil {
		return nil, err
	}
	if r.SourceVersion == "" {
		r.SourceVersion = schemaVersionName(schema)
	}
	if r.BlockWidth == 0 {
		r.BlockWidth = DefaultBlockWidth
	}
	if r.BlockHeight == 0 {
		r.BlockHeight = DefaultBlockHeight
	}
	r.ParamVersion = ComputeParamVersion(r)
	if err := r.ValidateSchema(); err != nil {
		return nil, err
	}
	return r, nil
}

func schemaVersionName(schema string) string {
	switch schema {
	case SchemaV1:
		return "v1"
	case SchemaV2:
		return "v2"
	case SchemaV3:
		return "v3"
	default:
		return schema
	}
}

func EncodeRecipeDocument(r *Recipe, schema string) ([]byte, error) {
	doc, err := RecipeToMap(r, schema)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func RecipeToMap(r *Recipe, schema string) (map[string]any, error) {
	switch schema {
	case SchemaV1:
		return recipeToV1(r)
	case SchemaV2:
		return recipeToV2(r)
	case SchemaV3:
		return recipeToV3(r)
	default:
		return nil, schemaError(r.SourceVersion, "schema", "unsupported target schema %q", schema)
	}
}

func stringValue(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func requiredMap(root map[string]any, version, field string) (map[string]any, error) {
	v, ok := root[field]
	if !ok || v == nil {
		return nil, schemaError(version, field, "field is missing")
	}
	m, ok := asMap(v)
	if !ok {
		return nil, schemaError(version, field, "expected object")
	}
	return m, nil
}

func requiredString(root map[string]any, version, field string, aliases ...string) (string, error) {
	keys := append([]string{field}, aliases...)
	for _, key := range keys {
		v, ok := root[key]
		if !ok || v == nil {
			continue
		}
		s, ok := stringValue(v)
		if !ok {
			return "", schemaError(version, key, "expected string")
		}
		if s == "" {
			return "", schemaError(version, key, "must not be empty")
		}
		return s, nil
	}
	return "", schemaError(version, field, "field is missing")
}

func optionalString(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := stringValue(root[key]); ok {
			return s
		}
	}
	return ""
}

func intFromValue(v any, version, field string) (int, error) {
	if v == nil {
		return 0, schemaError(version, field, "field is missing")
	}
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil || !floatFinite(f) {
			return 0, schemaError(version, field, "must be a finite number")
		}
		if math.Trunc(f) != f {
			return 0, schemaError(version, field, "must be an integer in this schema")
		}
		if f > float64(math.MaxInt32) || f < float64(math.MinInt32) {
			return 0, overflowError(field, n.String(), "int32")
		}
		return int(f), nil
	case float64:
		if !floatFinite(n) {
			return 0, schemaError(version, field, "must be finite")
		}
		return int(n), nil
	case int:
		return n, nil
	default:
		return 0, schemaError(version, field, "expected integer")
	}
}

func requiredInt(root map[string]any, version, field string, aliases ...string) (int, error) {
	keys := append([]string{field}, aliases...)
	for _, key := range keys {
		v, ok := root[key]
		if !ok || v == nil {
			continue
		}
		return intFromValue(v, version, key)
	}
	return 0, schemaError(version, field, "field is missing")
}

func optionalInt(root map[string]any, def int, keys ...string) (int, error) {
	for _, key := range keys {
		v, ok := root[key]
		if !ok || v == nil {
			continue
		}
		return intFromValue(v, key, key)
	}
	return def, nil
}

func boolFromValue(v any, def bool) bool {
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

func checkThresholdScale(value, maxScale int, version, field string) error {
	if value < 0 || value > maxScale {
		return schemaError(version, field, "threshold scale outside declared range [0,%d]", maxScale)
	}
	return nil
}

func parseQuadRaw(v any, version, field string, _ ...string) (Quad, error) {
	if v == nil {
		return Quad{}, schemaError(version, field, "field is missing")
	}
	return parseQuadValue(v, version, field)
}

func parseV1(root map[string]any) (*Recipe, error) {
	v := SchemaV1
	rv, err := requiredString(root, v, "recipe_version", "recipeVersion")
	if err != nil {
		return nil, err
	}
	name, err := requiredString(root, v, "name", "recipe_name")
	if err != nil {
		return nil, err
	}
	blockW, blockH := DefaultBlockWidth, DefaultBlockHeight
	if block, err := requiredMap(root, v, "coordinate_block"); err == nil {
		blockW, err = requiredInt(block, v, "width")
		if err != nil {
			return nil, err
		}
		blockH, err = requiredInt(block, v, "height")
		if err != nil {
			return nil, err
		}
	}
	pre, err := requiredMap(root, v, "preprocess")
	if err != nil {
		return nil, err
	}
	orderRaw, ok := pre["order"].([]any)
	if !ok || len(orderRaw) == 0 {
		return nil, schemaError(v, "preprocess.order", "field is missing or empty")
	}
	order := make([]string, 0, len(orderRaw))
	for i, stage := range orderRaw {
		s, ok := stage.(string)
		if !ok {
			return nil, schemaError(v, fmt.Sprintf("preprocess.order[%d]", i), "expected string")
		}
		order = append(order, s)
	}
	det, err := requiredMap(root, v, "detect")
	if err != nil {
		return nil, err
	}
	scorePM, err := requiredInt(det, v, "candidate_score_permille", "min_score_permille")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(scorePM, PermilleScale, v, "detect.candidate_score_permille"); err != nil {
		return nil, err
	}
	scoreBPS, err := PermilleToBPS(scorePM, "detect.candidate_score_permille")
	if err != nil {
		return nil, withSource(err, v)
	}
	overlapPM, err := requiredInt(det, v, "merge_overlap_permille", "overlap_permille")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(overlapPM, PermilleScale, v, "detect.merge_overlap_permille"); err != nil {
		return nil, err
	}
	overlapBPS, err := PermilleToBPS(overlapPM, "detect.merge_overlap_permille")
	if err != nil {
		return nil, withSource(err, v)
	}
	swPM, err := requiredInt(det, v, "stroke_consistency_permille", "sw_consistency_permille")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(swPM, PermilleScale, v, "detect.stroke_consistency_permille"); err != nil {
		return nil, err
	}
	swBPS, err := PermilleToBPS(swPM, "detect.stroke_consistency_permille")
	if err != nil {
		return nil, withSource(err, v)
	}
	gradient, err := optionalInt(det, 55, "gradient_min", "gradient_min_abs")
	if err != nil {
		return nil, err
	}
	minSW, err := optionalInt(det, 1, "min_stroke_width", "min_sw")
	if err != nil {
		return nil, err
	}
	maxSW, err := optionalInt(det, 9, "max_stroke_width", "max_sw")
	if err != nil {
		return nil, err
	}
	rec, err := requiredMap(root, v, "recognize")
	if err != nil {
		return nil, err
	}
	tpl, err := requiredString(rec, v, "template_pack", "template_library")
	if err != nil {
		return nil, err
	}
	tplVer, err := requiredString(rec, v, "template_version", "tpl_version")
	if err != nil {
		return nil, err
	}
	confPM, err := requiredInt(rec, v, "min_confidence_permille")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(confPM, PermilleScale, v, "recognize.min_confidence_permille"); err != nil {
		return nil, err
	}
	confBPS, err := PermilleToBPS(confPM, "recognize.min_confidence_permille")
	if err != nil {
		return nil, withSource(err, v)
	}
	items, err := parseV1Items(root, v, blockW, blockH)
	if err != nil {
		return nil, err
	}
	return &Recipe{
		Schema: v, SourceVersion: "v1", RecipeVersion: rv, Name: name,
		BlockWidth: blockW, BlockHeight: blockH,
		Preprocess:  PreprocessConfig{Order: order, NormalizeLowBPS: 200, NormalizeHighBPS: 9800, BinarizationMethod: "fixed", BinarizationThreshBPS: 4200},
		Detection:   DetectConfig{CandidateScoreBPS: scoreBPS, MergeOverlapBPS: overlapBPS, StrokeConsistencyBPS: swBPS, GradientMinAbs: gradient, MinStrokeWidthPx: minSW, MaxStrokeWidthPx: maxSW},
		Recognition: RecogConfig{TemplateLibrary: tpl, TemplateVersion: tplVer, MinConfidenceBPS: confBPS, SequenceSplit: true},
		Items:       items,
	}, nil
}

func parseV1Items(root map[string]any, version string, blockW, blockH int) ([]RecipeItem, error) {
	raw, ok := root["items"].([]any)
	if !ok {
		raw, _ = root["rois"].([]any)
	}
	items := make([]RecipeItem, 0, len(raw))
	for i, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, schemaError(version, fmt.Sprintf("items[%d]", i), "expected object")
		}
		field := fmt.Sprintf("items[%d]", i)
		id, err := requiredString(m, version, "id", "roi_id", "name")
		if err != nil {
			return nil, err
		}
		text, err := requiredString(m, version, "text")
		if err != nil {
			return nil, err
		}
		thresholdPM, err := requiredInt(m, version, "threshold_permille")
		if err != nil {
			return nil, err
		}
		if err := checkThresholdScale(thresholdPM, PermilleScale, version, field+".threshold_permille"); err != nil {
			return nil, err
		}
		thresholdBPS, err := PermilleToBPS(thresholdPM, field+".threshold_permille")
		if err != nil {
			return nil, withSource(err, version)
		}
		charCount, err := requiredInt(m, version, "char_count", "chars")
		if err != nil {
			return nil, err
		}
		blockQuad, err := parseQuadRaw(m["block_quad"], version, field+".block_quad")
		if err != nil {
			return nil, err
		}
		pixelQuad, err := blockQuadToPixel(blockQuad, blockW, blockH, field+".block_quad")
		if err != nil {
			return nil, withSource(err, version)
		}
		items = append(items, RecipeItem{ID: id, Text: text, ThresholdBPS: thresholdBPS, Box: pixelQuad, CharCount: charCount, CoordinateExpression: "block_derived"})
	}
	return items, nil
}

func blockQuadToPixel(q Quad, w, h int, field string) (Quad, error) {
	var out Quad
	for i, p := range q {
		pp, err := BlockPointToPixel(p, w, h, fmt.Sprintf("%s[%d]", field, i))
		if err != nil {
			return Quad{}, err
		}
		out[i] = pp
	}
	return out, nil
}

func parseV2(root map[string]any) (*Recipe, error) {
	v := SchemaV2
	rv, err := requiredString(root, v, "recipeVersion", "recipe_version")
	if err != nil {
		return nil, err
	}
	name, err := requiredString(root, v, "recipeName", "name")
	if err != nil {
		return nil, err
	}
	pre, err := requiredMap(root, v, "preprocessing")
	if err != nil {
		return nil, err
	}
	code, err := requiredInt(pre, v, "order_code", "pipeline_order")
	if err != nil {
		return nil, err
	}
	order, err := orderFromCode(code, v, "preprocessing.order_code")
	if err != nil {
		return nil, err
	}
	det, err := requiredMap(root, v, "detection")
	if err != nil {
		return nil, err
	}
	score, err := requiredInt(det, v, "score_bps", "candidate_score_bps")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(score, InternalScale, v, "detection.score_bps"); err != nil {
		return nil, err
	}
	overlap, err := requiredInt(det, v, "overlap_bps", "merge_overlap_bps")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(overlap, InternalScale, v, "detection.overlap_bps"); err != nil {
		return nil, err
	}
	sw, err := requiredInt(det, v, "sw_consistency_bps", "stroke_consistency_bps")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(sw, InternalScale, v, "detection.sw_consistency_bps"); err != nil {
		return nil, err
	}
	gradient, err := optionalInt(det, 55, "gradient_threshold", "gradient_min_abs")
	if err != nil {
		return nil, err
	}
	minSW, err := optionalInt(det, 1, "min_sw", "min_stroke_width_px")
	if err != nil {
		return nil, err
	}
	maxSW, err := optionalInt(det, 9, "max_sw", "max_stroke_width_px")
	if err != nil {
		return nil, err
	}
	rec, err := requiredMap(root, v, "recognition")
	if err != nil {
		return nil, err
	}
	library, err := requiredString(rec, v, "pack", "template_library")
	if err != nil {
		return nil, err
	}
	tplVer, err := requiredString(rec, v, "tpl_version", "template_version")
	if err != nil {
		return nil, err
	}
	conf, err := requiredInt(rec, v, "min_confidence_bps")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(conf, InternalScale, v, "recognition.min_confidence_bps"); err != nil {
		return nil, err
	}
	items, err := parseV2Items(root, v)
	if err != nil {
		return nil, err
	}
	return &Recipe{
		Schema: v, SourceVersion: "v2", RecipeVersion: rv, Name: name,
		BlockWidth: DefaultBlockWidth, BlockHeight: DefaultBlockHeight,
		Preprocess:  PreprocessConfig{Order: order, NormalizeLowBPS: 200, NormalizeHighBPS: 9800, BinarizationMethod: "fixed", BinarizationThreshBPS: 4200},
		Detection:   DetectConfig{CandidateScoreBPS: score, MergeOverlapBPS: overlap, StrokeConsistencyBPS: sw, GradientMinAbs: gradient, MinStrokeWidthPx: minSW, MaxStrokeWidthPx: maxSW},
		Recognition: RecogConfig{TemplateLibrary: library, TemplateVersion: tplVer, MinConfidenceBPS: conf, SequenceSplit: boolFromValue(rec["sequence_split"], true)},
		Items:       items,
	}, nil
}

func orderFromCode(code int, version, field string) ([]string, error) {
	if code <= 0 {
		return nil, schemaError(version, field, "order_code must be positive")
	}
	digits := fmt.Sprintf("%d", code)
	order := make([]string, 0, len(digits))
	seen := map[rune]bool{}
	for _, d := range digits {
		if seen[d] {
			return nil, schemaError(version, field, "stage code %c repeated", d)
		}
		seen[d] = true
		switch d {
		case '1':
			order = append(order, "grayscale")
		case '2':
			order = append(order, "normalize")
		case '3':
			order = append(order, "binarize")
		default:
			return nil, schemaError(version, field, "digits must use stage codes 1,2,3")
		}
	}
	return order, nil
}

func parseV2Items(root map[string]any, version string) ([]RecipeItem, error) {
	raw, ok := root["rois"].([]any)
	if !ok {
		raw, _ = root["items"].([]any)
	}
	items := make([]RecipeItem, 0, len(raw))
	for i, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, schemaError(version, fmt.Sprintf("rois[%d]", i), "expected object")
		}
		field := fmt.Sprintf("rois[%d]", i)
		id, err := requiredString(m, version, "name", "id")
		if err != nil {
			return nil, err
		}
		text, err := requiredString(m, version, "text")
		if err != nil {
			return nil, err
		}
		threshold, err := requiredInt(m, version, "threshold_bps")
		if err != nil {
			return nil, err
		}
		if err := checkThresholdScale(threshold, InternalScale, version, field+".threshold_bps"); err != nil {
			return nil, err
		}
		charCount, err := requiredInt(m, version, "character_count", "char_count")
		if err != nil {
			return nil, err
		}
		q, err := parseQuadRaw(m["pixel_quad"], version, field+".pixel_quad")
		if err != nil {
			return nil, err
		}
		items = append(items, RecipeItem{ID: id, Text: text, ThresholdBPS: threshold, Box: q, CharCount: charCount, CoordinateExpression: "absolute_pixels"})
	}
	return items, nil
}

func parseV3(root map[string]any) (*Recipe, error) {
	v := SchemaV3
	rv, err := requiredString(root, v, "recipe_version", "recipeVersion")
	if err != nil {
		return nil, err
	}
	name, err := requiredString(root, v, "name")
	if err != nil {
		if meta, mErr := requiredMap(root, v, "metadata"); mErr == nil {
			name, err = requiredString(meta, v, "name")
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	pre, err := requiredMap(root, v, "preprocess")
	if err != nil {
		return nil, err
	}
	stagesRaw, ok := pre["stages"].([]any)
	if !ok || len(stagesRaw) == 0 {
		return nil, schemaError(v, "preprocess.stages", "field is missing or empty")
	}
	order := make([]string, 0, len(stagesRaw))
	low, high := 200, 9800
	for i, sr := range stagesRaw {
		switch val := sr.(type) {
		case string:
			order = append(order, val)
		case map[string]any:
			op, ok := val["op"].(string)
			if !ok {
				return nil, schemaError(v, fmt.Sprintf("preprocess.stages[%d].op", i), "expected string")
			}
			order = append(order, op)
			if op == "normalize" {
				if x, pErr := optionalInt(val, low, "low_bps"); pErr == nil {
					low = x
				}
				if x, pErr := optionalInt(val, high, "high_bps"); pErr == nil {
					high = x
				}
			}
		default:
			return nil, schemaError(v, fmt.Sprintf("preprocess.stages[%d]", i), "expected string or object")
		}
	}
	binEnabled, binThresh := false, 4200
	binMethod := "fixed"
	if bin, ok := pre["binarization"].(map[string]any); ok {
		binEnabled = boolFromValue(bin["enabled"], false)
		binMethod = optionalString(bin, "method")
		if binMethod == "" {
			binMethod = "fixed"
		}
		if x, err := optionalInt(bin, binThresh, "threshold_bps"); err == nil {
			binThresh = x
		} else {
			return nil, err
		}
	}
	if bin, ok := root["binarization"].(map[string]any); ok {
		binEnabled = boolFromValue(bin["enabled"], binEnabled)
		binMethod = optionalString(bin, "method")
		if binMethod == "" {
			binMethod = "fixed"
		}
		x, err := optionalInt(bin, binThresh, "threshold_bps")
		if err != nil {
			return nil, err
		}
		binThresh = x
	}
	detRoot, err := requiredMap(root, v, "detection")
	if err != nil {
		return nil, err
	}
	candidate, err := requiredMap(detRoot, v, "candidate")
	if err != nil {
		return nil, prefixField(err, "detection")
	}
	score, err := requiredInt(candidate, v, "min_confidence_bps")
	if err != nil {
		return nil, prefixField(err, "detection.candidate")
	}
	if err := checkThresholdScale(score, InternalScale, v, "detection.candidate.min_confidence_bps"); err != nil {
		return nil, err
	}
	merge, err := requiredMap(detRoot, v, "merge")
	if err != nil {
		return nil, prefixField(err, "detection")
	}
	overlap, err := requiredInt(merge, v, "min_overlap_bps")
	if err != nil {
		return nil, prefixField(err, "detection.merge")
	}
	if err := checkThresholdScale(overlap, InternalScale, v, "detection.merge.min_overlap_bps"); err != nil {
		return nil, err
	}
	stroke, err := requiredMap(detRoot, v, "stroke")
	if err != nil {
		return nil, prefixField(err, "detection")
	}
	sw, err := requiredInt(stroke, v, "consistency_bps")
	if err != nil {
		return nil, prefixField(err, "detection.stroke")
	}
	if err := checkThresholdScale(sw, InternalScale, v, "detection.stroke.consistency_bps"); err != nil {
		return nil, err
	}
	minSW, err := requiredInt(stroke, v, "min_width_px")
	if err != nil {
		return nil, prefixField(err, "detection.stroke")
	}
	maxSW, err := requiredInt(stroke, v, "max_width_px")
	if err != nil {
		return nil, prefixField(err, "detection.stroke")
	}
	gradient, err := optionalInt(detRoot, 55, "gradient_min_abs")
	if err != nil {
		return nil, err
	}
	rec, err := requiredMap(root, v, "recognition")
	if err != nil {
		return nil, err
	}
	library, err := requiredString(rec, v, "template_library")
	if err != nil {
		return nil, prefixField(err, "recognition")
	}
	tplVer, err := requiredString(rec, v, "template_version")
	if err != nil {
		return nil, prefixField(err, "recognition")
	}
	conf, err := requiredInt(rec, v, "min_confidence_bps")
	if err != nil {
		return nil, err
	}
	if err := checkThresholdScale(conf, InternalScale, v, "recognition.min_confidence_bps"); err != nil {
		return nil, err
	}
	items, err := parseV3Items(root, v)
	if err != nil {
		return nil, err
	}
	return &Recipe{
		Schema: v, SourceVersion: "v3", RecipeVersion: rv, Name: name,
		BlockWidth: DefaultBlockWidth, BlockHeight: DefaultBlockHeight,
		Preprocess:  PreprocessConfig{Order: order, NormalizeLowBPS: low, NormalizeHighBPS: high, BinarizationEnabled: binEnabled, BinarizationMethod: binMethod, BinarizationThreshBPS: binThresh},
		Detection:   DetectConfig{CandidateScoreBPS: score, MergeOverlapBPS: overlap, StrokeConsistencyBPS: sw, GradientMinAbs: gradient, MinStrokeWidthPx: minSW, MaxStrokeWidthPx: maxSW},
		Recognition: RecogConfig{TemplateLibrary: library, TemplateVersion: tplVer, MinConfidenceBPS: conf, SequenceSplit: boolFromValue(rec["sequence_split"], true)},
		Items:       items,
	}, nil
}

func parseV3Items(root map[string]any, version string) ([]RecipeItem, error) {
	raw, ok := root["entries"].([]any)
	if !ok {
		raw, _ = root["items"].([]any)
	}
	items := make([]RecipeItem, 0, len(raw))
	for i, entry := range raw {
		m, ok := entry.(map[string]any)
		if !ok {
			return nil, schemaError(version, fmt.Sprintf("entries[%d]", i), "expected object")
		}
		field := fmt.Sprintf("entries[%d]", i)
		id, err := requiredString(m, version, "entry_id", "id")
		if err != nil {
			return nil, err
		}
		text, err := requiredString(m, version, "text")
		if err != nil {
			return nil, err
		}
		threshold, err := requiredInt(m, version, "threshold_bps")
		if err != nil {
			return nil, err
		}
		if err := checkThresholdScale(threshold, InternalScale, version, field+".threshold_bps"); err != nil {
			return nil, err
		}
		charCount, err := requiredInt(m, version, "char_count")
		if err != nil {
			return nil, err
		}
		q, err := parseQuadRaw(m["pixel_quad"], version, field+".pixel_quad", "quad")
		if err != nil {
			return nil, err
		}
		items = append(items, RecipeItem{ID: id, Text: text, ThresholdBPS: threshold, Box: q, CharCount: charCount, CoordinateExpression: "absolute_pixels"})
	}
	return items, nil
}

func parseQuadValue(v any, version, field string) (Quad, error) {
	arr, ok := v.([]any)
	if !ok || len(arr) != 4 {
		return Quad{}, schemaError(version, field, "expected four coordinate points")
	}
	var q Quad
	for i, pv := range arr {
		path := fmt.Sprintf("%s[%d]", field, i)
		pair, ok := pv.([]any)
		if !ok || len(pair) != 2 {
			return Quad{}, schemaError(version, path, "expected [x,y] pair")
		}
		x, err := intFromValue(pair[0], version, path+".x")
		if err != nil {
			return Quad{}, err
		}
		y, err := intFromValue(pair[1], version, path+".y")
		if err != nil {
			return Quad{}, err
		}
		q[i] = Point{X: x, Y: y}
	}
	if !ValidQuad(q) {
		return Quad{}, schemaError(version, field, "quad must be finite and non-degenerate")
	}
	return q, nil
}

func withSource(err error, version string) error {
	if e, ok := asError(err); ok && e.SourceVersion == "" {
		e.SourceVersion = version
		return e
	}
	return err
}

func prefixField(err error, prefix string) error {
	if e, ok := asError(err); ok && e.Field != "" {
		if e.Field != prefix && !strings.HasPrefix(e.Field, prefix+".") {
			e.Field = prefix + "." + e.Field
		}
	}
	return err
}

func recipeToV1(r *Recipe) (map[string]any, error) {
	items := make([]any, 0, len(r.Items))
	for _, item := range r.Items {
		tPM, err := BPSToPermille(item.ThresholdBPS, "items.threshold_bps")
		if err != nil {
			return nil, err
		}
		bq, err := pixelQuadToBlockEnclosing(item.Box, r.BlockWidth, r.BlockHeight)
		if err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"id":                    item.ID,
			"text":                  item.Text,
			"threshold_permille":    tPM,
			"block_quad":            quadToArrays(bq),
			"char_count":            item.CharCount,
			"coordinate_expression": "block_derived",
		})
	}
	score, err := BPSToPermille(r.Detection.CandidateScoreBPS, "detection.candidate_score_bps")
	if err != nil {
		return nil, err
	}
	overlap, err := BPSToPermille(r.Detection.MergeOverlapBPS, "detection.merge_overlap_bps")
	if err != nil {
		return nil, err
	}
	sw, err := BPSToPermille(r.Detection.StrokeConsistencyBPS, "detection.stroke_consistency_bps")
	if err != nil {
		return nil, err
	}
	conf, err := BPSToPermille(r.Recognition.MinConfidenceBPS, "recognition.min_confidence_bps")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"schema":           SchemaV1,
		"source_version":   "v1",
		"recipe_version":   r.RecipeVersion,
		"name":             r.Name,
		"coordinate_block": map[string]any{"width": r.BlockWidth, "height": r.BlockHeight},
		"preprocess":       map[string]any{"order": r.Preprocess.Order},
		"detect": map[string]any{
			"candidate_score_permille":    score,
			"merge_overlap_permille":      overlap,
			"stroke_consistency_permille": sw,
			"gradient_min":                r.Detection.GradientMinAbs,
			"min_stroke_width":            r.Detection.MinStrokeWidthPx,
			"max_stroke_width":            r.Detection.MaxStrokeWidthPx,
		},
		"recognize": map[string]any{
			"template_pack":           r.Recognition.TemplateLibrary,
			"template_version":        r.Recognition.TemplateVersion,
			"min_confidence_permille": conf,
		},
		"items": items,
	}, nil
}

func pixelQuadToBlockEnclosing(q Quad, w, h int) (Quad, error) {
	rounds := []RoundingDirection{RoundFloor, RoundFloor, RoundCeil, RoundCeil}
	xRounds := []RoundingDirection{RoundFloor, RoundCeil, RoundCeil, RoundFloor}
	var out Quad
	for i, p := range q {
		px, err := PixelPointToBlock(p, w, h, xRounds[i])
		if err != nil {
			return Quad{}, err
		}
		py, err := PixelPointToBlock(p, w, h, rounds[i])
		if err != nil {
			return Quad{}, err
		}
		out[i] = Point{X: px.X, Y: py.Y}
	}
	return out, nil
}

func quadToArrays(q Quad) [4][2]int {
	return [4][2]int{{q[0].X, q[0].Y}, {q[1].X, q[1].Y}, {q[2].X, q[2].Y}, {q[3].X, q[3].Y}}
}

func recipeToV2(r *Recipe) (map[string]any, error) {
	rois := make([]any, 0, len(r.Items))
	for _, item := range r.Items {
		rois = append(rois, map[string]any{
			"name":            item.ID,
			"text":            item.Text,
			"threshold_bps":   item.ThresholdBPS,
			"pixel_quad":      quadToArrays(item.Box),
			"character_count": item.CharCount,
		})
	}
	return map[string]any{
		"schema_version": SchemaV2,
		"source":         "v2",
		"recipeVersion":  r.RecipeVersion,
		"recipeName":     r.Name,
		"preprocessing":  map[string]any{"order_code": orderToCode(r.Preprocess.Order)},
		"detection": map[string]any{
			"score_bps":          r.Detection.CandidateScoreBPS,
			"overlap_bps":        r.Detection.MergeOverlapBPS,
			"sw_consistency_bps": r.Detection.StrokeConsistencyBPS,
			"gradient_threshold": r.Detection.GradientMinAbs,
			"min_sw":             r.Detection.MinStrokeWidthPx,
			"max_sw":             r.Detection.MaxStrokeWidthPx,
		},
		"recognition": map[string]any{
			"pack":               r.Recognition.TemplateLibrary,
			"tpl_version":        r.Recognition.TemplateVersion,
			"min_confidence_bps": r.Recognition.MinConfidenceBPS,
			"sequence_split":     r.Recognition.SequenceSplit,
		},
		"rois": rois,
	}, nil
}

func orderToCode(order []string) int {
	code := 0
	for _, stage := range order {
		d := 0
		switch stage {
		case "grayscale":
			d = 1
		case "normalize":
			d = 2
		case "binarize":
			d = 3
		}
		code = code*10 + d
	}
	return code
}

func recipeToV3(r *Recipe) (map[string]any, error) {
	entries := make([]any, 0, len(r.Items))
	for _, item := range r.Items {
		entries = append(entries, map[string]any{
			"entry_id":      item.ID,
			"text":          item.Text,
			"threshold_bps": item.ThresholdBPS,
			"pixel_quad":    quadToArrays(item.Box),
			"char_count":    item.CharCount,
		})
	}
	stages := []any{
		map[string]any{"op": "grayscale"},
		map[string]any{"op": "normalize", "low_bps": r.Preprocess.NormalizeLowBPS, "high_bps": r.Preprocess.NormalizeHighBPS},
	}
	if r.Preprocess.BinarizationEnabled {
		stages = append(stages, map[string]any{"op": "binarize"})
	}
	return map[string]any{
		"schema":         SchemaV3,
		"source_version": "v3",
		"recipe_version": r.RecipeVersion,
		"name":           r.Name,
		"preprocess":     map[string]any{"stages": stages},
		"binarization": map[string]any{
			"enabled":       r.Preprocess.BinarizationEnabled,
			"method":        r.Preprocess.BinarizationMethod,
			"threshold_bps": r.Preprocess.BinarizationThreshBPS,
		},
		"detection": map[string]any{
			"candidate": map[string]any{"min_confidence_bps": r.Detection.CandidateScoreBPS},
			"merge":     map[string]any{"min_overlap_bps": r.Detection.MergeOverlapBPS},
			"stroke": map[string]any{
				"consistency_bps": r.Detection.StrokeConsistencyBPS,
				"min_width_px":    r.Detection.MinStrokeWidthPx,
				"max_width_px":    r.Detection.MaxStrokeWidthPx,
			},
			"gradient_min_abs": r.Detection.GradientMinAbs,
		},
		"recognition": map[string]any{
			"template_library":   r.Recognition.TemplateLibrary,
			"template_version":   r.Recognition.TemplateVersion,
			"min_confidence_bps": r.Recognition.MinConfidenceBPS,
			"sequence_split":     r.Recognition.SequenceSplit,
		},
		"entries": entries,
	}, nil
}
