package web

import (
	"context"
	"net/http"
	"strconv"

	"maskpipe/internal/model"
	"maskpipe/internal/pipeline"
	"maskpipe/internal/vision"
)

func paramsFromRequest(r *http.Request, base model.ParameterSet) (model.ParameterSet, error) {
	p := base
	q := r.URL.Query()
	if v := q.Get("color_tolerance"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return p, model.NewError(model.CodePara, "color_tolerance", "must be number: %v", err)
		}
		p.ColorTolerance = f
	}
	if v := q.Get("gradient_threshold"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return p, model.NewError(model.CodePara, "gradient_threshold", "must be number: %v", err)
		}
		p.GradientThreshold = f
	}
	if v := q.Get("structure_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, model.NewError(model.CodePara, "structure_size", "must be integer: %v", err)
		}
		p.StructureSize = n
	}
	p.ParamVersion = ""
	p = vision.WithVersion(p)
	return p, vision.ValidateParameters(p)
}

func (m *Manager) PreviewWithParams(ctx context.Context, p model.ParameterSet, compare bool) (*pipeline.PreviewResult, error) {
	preview, err := pipeline.RunPreview(ctx, m.loaded, p)
	if err != nil {
		return nil, err
	}
	if compare {
		result := m.currentResult()
		if result != nil && result.ParamVersion == p.ParamVersion {
			preview.Comparison = pipeline.ComparePreview(
				result.Mask, preview.Mask, m.loaded.Scene.Width, m.loaded.Scene.Height,
				result.Regions, preview.Regions, m.loaded.Scene.Declared.Divergence)
		}
	}
	return preview, nil
}
