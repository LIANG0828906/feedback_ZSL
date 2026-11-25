package ocrflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	SchemaV1 = "ocr.recipe/v1"
	SchemaV2 = "ocr.recipe/v2"
	SchemaV3 = "ocr.recipe/v3"

	RecipeAlgorithmVersion = "algo-detect-1.2.0+recognize-1.1.0"
	DefaultTemplateName    = "game-block-5x7"
	TemplateVersionV1      = "tpl-1.0.0"
	TemplateVersionV2      = "tpl-1.1.0"
)

const FixedSeedVersion = "deterministic-generator-v1"

type PreprocessConfig struct {
	Order                 []string `json:"order"`
	NormalizeLowBPS       int      `json:"normalize_low_bps"`
	NormalizeHighBPS      int      `json:"normalize_high_bps"`
	BinarizationEnabled   bool     `json:"binarization_enabled"`
	BinarizationMethod    string   `json:"binarization_method,omitempty"`
	BinarizationThreshBPS int      `json:"binarization_threshold_bps,omitempty"`
}

type DetectConfig struct {
	CandidateScoreBPS    int `json:"candidate_score_bps"`
	MergeOverlapBPS      int `json:"merge_overlap_bps"`
	StrokeConsistencyBPS int `json:"stroke_consistency_bps"`
	GradientMinAbs       int `json:"gradient_min_abs"`
	MinStrokeWidthPx     int `json:"min_stroke_width_px"`
	MaxStrokeWidthPx     int `json:"max_stroke_width_px"`
}

type RecogConfig struct {
	TemplateLibrary  string `json:"template_library"`
	TemplateVersion  string `json:"template_version"`
	MinConfidenceBPS int    `json:"min_confidence_bps"`
	SequenceSplit    bool   `json:"sequence_split"`
}

type RecipeItem struct {
	ID                   string `json:"id"`
	Text                 string `json:"text"`
	ThresholdBPS         int    `json:"threshold_bps"`
	Box                  Quad   `json:"box"`
	CharCount            int    `json:"char_count"`
	CoordinateExpression string `json:"coordinate_expression"`
}

type Recipe struct {
	Schema        string           `json:"schema"`
	SourceVersion string           `json:"source_version"`
	RecipeVersion string           `json:"recipe_version"`
	Name          string           `json:"name"`
	ParamVersion  string           `json:"param_version"`
	BlockWidth    int              `json:"block_width"`
	BlockHeight   int              `json:"block_height"`
	Preprocess    PreprocessConfig `json:"preprocess"`
	Detection     DetectConfig     `json:"detection"`
	Recognition   RecogConfig      `json:"recognition"`
	Items         []RecipeItem     `json:"items"`
}

func DefaultRecipe() *Recipe {
	r := &Recipe{
		Schema:        SchemaV3,
		SourceVersion: "v3",
		RecipeVersion: "settlement-leaderboard-2026q4",
		Name:          "settlement-leaderboard",
		BlockWidth:    DefaultBlockWidth,
		BlockHeight:   DefaultBlockHeight,
		Preprocess: PreprocessConfig{
			Order:                 []string{"grayscale", "normalize"},
			NormalizeLowBPS:       200,
			NormalizeHighBPS:      9800,
			BinarizationEnabled:   false,
			BinarizationMethod:    "fixed",
			BinarizationThreshBPS: 4200,
		},
		Detection: DetectConfig{
			CandidateScoreBPS:    6200,
			MergeOverlapBPS:      4500,
			StrokeConsistencyBPS: 7000,
			GradientMinAbs:       55,
			MinStrokeWidthPx:     1,
			MaxStrokeWidthPx:     9,
		},
		Recognition: RecogConfig{
			TemplateLibrary:  DefaultTemplateName,
			TemplateVersion:  TemplateVersionV1,
			MinConfidenceBPS: 5000,
			SequenceSplit:    true,
		},
	}
	r.ParamVersion = ComputeParamVersion(r)
	return r
}

func ComputeParamVersion(r *Recipe) string {
	copy := *r
	copy.ParamVersion = ""
	copy.SourceVersion = ""
	copy.Schema = ""
	data, _ := json.Marshal(copy)
	return "param-" + HashString(string(data))[:16]
}

func (r *Recipe) ValidateSchema() error {
	v := r.SourceVersion
	if v == "" {
		v = r.Schema
	}
	if r.RecipeVersion == "" {
		return schemaError(v, "recipe_version", "field is required")
	}
	if r.Name == "" {
		return schemaError(v, "name", "field is required")
	}
	if r.BlockWidth <= 0 || r.BlockHeight <= 0 {
		return schemaError(v, "coordinate_block", "block dimensions must be positive")
	}
	if len(r.Preprocess.Order) == 0 {
		return schemaError(v, "preprocess.order", "at least one preprocessing stage is required")
	}
	for i, stage := range r.Preprocess.Order {
		switch stage {
		case "grayscale", "normalize", "binarize":
		default:
			return schemaError(v, fmt.Sprintf("preprocess.order[%d]", i), "unsupported stage %q", stage)
		}
	}
	if r.Preprocess.NormalizeLowBPS < 0 || r.Preprocess.NormalizeLowBPS >= r.Preprocess.NormalizeHighBPS || r.Preprocess.NormalizeHighBPS > 10000 {
		return schemaError(v, "preprocess.normalize_range_bps", "expected 0 <= low < high <= 10000")
	}
	if r.Preprocess.BinarizationThreshBPS < 0 || r.Preprocess.BinarizationThreshBPS > 10000 {
		return schemaError(v, "preprocess.binarization_threshold_bps", "threshold must be in [0,10000]")
	}
	d := r.Detection
	if d.CandidateScoreBPS < 100 || d.CandidateScoreBPS > 10000 {
		return schemaError(v, "detection.candidate_score_bps", "threshold must be in declared range [100,10000]")
	}
	if d.MergeOverlapBPS < 100 || d.MergeOverlapBPS > 10000 {
		return schemaError(v, "detection.merge_overlap_bps", "threshold must be in declared range [100,10000]")
	}
	if d.StrokeConsistencyBPS < 1000 || d.StrokeConsistencyBPS > 10000 {
		return schemaError(v, "detection.stroke_consistency_bps", "threshold must be in declared range [1000,10000]")
	}
	if d.GradientMinAbs < 0 || d.GradientMinAbs > 1020 {
		return schemaError(v, "detection.gradient_min_abs", "gradient threshold must be in [0,1020]")
	}
	if d.MinStrokeWidthPx < 1 || d.MaxStrokeWidthPx < d.MinStrokeWidthPx || d.MaxStrokeWidthPx > 64 {
		return schemaError(v, "detection.stroke_width_px", "expected 1 <= min <= max <= 64")
	}
	if r.Recognition.TemplateLibrary == "" || r.Recognition.TemplateVersion == "" {
		return schemaError(v, "recognition.template", "template library and version are required")
	}
	if r.Recognition.MinConfidenceBPS < 0 || r.Recognition.MinConfidenceBPS > 10000 {
		return schemaError(v, "recognition.min_confidence_bps", "confidence must be in [0,10000]")
	}
	for i, item := range r.Items {
		field := fmt.Sprintf("items[%d]", i)
		if item.ID == "" {
			return schemaError(v, field+".id", "field is required")
		}
		if strings.TrimSpace(item.Text) == "" {
			return schemaError(v, field+".text", "text content must not be empty")
		}
		if item.CharCount != len([]rune(item.Text)) {
			return schemaError(v, field+".char_count", "char_count %d does not match text length %d", item.CharCount, len([]rune(item.Text)))
		}
		if item.ThresholdBPS < 0 || item.ThresholdBPS > 10000 {
			return schemaError(v, field+".threshold_bps", "threshold must be in [0,10000]")
		}
		if !ValidQuad(item.Box) {
			return schemaError(v, field+".quad", "four coordinate points must be finite and non-degenerate")
		}
	}
	return nil
}

// ValidateRuntimeParameters enforces the narrower operational allow-set used
// by detector construction.
func (r *Recipe) ValidateRuntimeParameters() error {
	allowedOverlap := []int{2000, 2500, 3000, 3500, 4000, 4500, 5000, 5500, 6000, 6500, 7000, 7500, 8000}
	if !containsInt(allowedOverlap, r.Detection.MergeOverlapBPS) {
		return paramError("detection.merge_overlap_bps", map[string]any{
			"set":   allowedOverlap,
			"unit":  "basis_points",
			"range": []int{2000, 8000},
		}, "merge overlap threshold is outside allowed set")
	}
	allowedSW := map[string]any{"unit": "basis_points", "range": []int{5000, 10000}, "examples": []int{6000, 7000, 8000, 9000, 10000}}
	if r.Detection.StrokeConsistencyBPS < 5000 || r.Detection.StrokeConsistencyBPS > 10000 {
		return paramError("detection.stroke_consistency_bps", allowedSW, "stroke-width consistency threshold is outside allowed set")
	}
	return nil
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
