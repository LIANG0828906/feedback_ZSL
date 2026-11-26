package schemax

import (
	"encoding/json"
	"strings"

	"iconflow/errs"
	"iconflow/model"
)

type LossReport struct {
	Target string        `json:"target"`
	Items  []*errs.Error `json:"items"`
}

func (r *LossReport) HasLoss() bool { return r != nil && len(r.Items) > 0 }

type ExportResult struct {
	Version  string                   `json:"version"`
	Data     []byte                   `json:"-"`
	Document map[string]any           `json:"document"`
	Loss     *LossReport              `json:"loss,omitempty"`
	Traces   []model.ConversionRecord `json:"traces,omitempty"`
}

func Export(project *model.Project, target string) (*ExportResult, *errs.Error) {
	switch target {
	case model.SchemaV1:
		return exportV1(project)
	case model.SchemaV2:
		return exportV2(project)
	case model.SchemaV3:
		return exportV3(project)
	default:
		return nil, errs.Schema("$.schema", "unknown export target %q", target)
	}
}

func marshalDoc(doc map[string]any) ([]byte, *errs.Error) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, errs.New(errs.EInternal, "$", "marshal schema document: %v", err)
	}
	return data, nil
}

func rulesMap(r model.Rules) map[string]any {
	return map[string]any{
		"operator":                 r.GradientOperator,
		"operator_version":         r.OperatorVersion,
		"min_contrast":             r.MinContrast,
		"threshold_min":            r.ThresholdMin,
		"threshold_max":            r.ThresholdMax,
		"min_component_area":       r.MinComponentArea,
		"connectivity":             r.Connectivity,
		"fill_holes":               r.FillHoles,
		"area_tolerance":           r.Tolerances.AreaAbs,
		"centroid_tolerance_milli": r.Tolerances.CentroidMilliAbs,
	}
}

func exportV1(p *model.Project) (*ExportResult, *errs.Error) {
	loss := &LossReport{Target: model.SchemaV1}
	traces := []model.ConversionRecord{}
	permille, trace, err := permyriadToPermille(p.ThresholdPermyriad, model.RoundFloor, "$.threshold_permille")
	if err != nil {
		return nil, err
	}
	trace.FromVersion = model.SchemaV3
	trace.ToVersion = model.SchemaV1
	traces = append(traces, trace)

	if p.ChannelLayout == model.LayoutGrouped {
		loss.Items = append(loss.Items, &errs.Error{
			Code: errs.ELossy, Field: "$.images[*].pixels_flat",
			Message: "schema v1 cannot express channel-grouped raster arrangement",
			Details: map[string]any{"source_layout": p.ChannelLayout, "disposition": "flatten_to_RGBA_interleaved"},
		})
	}
	if p.ColorTolerance != nil {
		loss.Items = append(loss.Items, &errs.Error{
			Code: errs.ELossy, Field: "$.color_tolerance",
			Message: "schema v1 has no optional color difference tolerance field",
			Details: map[string]any{"value": *p.ColorTolerance, "disposition": "drop_optional_field"},
		})
	}

	images := []any{}
	for _, img := range p.Images {
		r := img.Raster
		flat := r.Flat
		if len(flat) == 0 {
			flat = []int{}
			for pixel := 0; pixel < r.Width*r.Height; pixel++ {
				for ch := range r.Channels {
					flat = append(flat, r.Planes[ch][pixel])
				}
			}
		}
		images = append(images, map[string]any{
			"id":          img.ID,
			"path":        img.Path,
			"width":       r.Width,
			"height":      r.Height,
			"channels":    strings.Join(r.Channels, ""),
			"pixels_flat": flat,
		})
	}
	doc := map[string]any{
		"schema":             model.SchemaV1,
		"project_id":         p.ProjectID,
		"seed":               p.Seed,
		"tile":               map[string]any{"width": p.BlockWidth, "height": p.BlockHeight},
		"threshold_permille": permille,
		"channel_sequence":   strings.Join(p.ChannelOrder, ""),
		"raster_layout":      model.LayoutFlat,
		"quant_step":         p.Quant.Step,
		"quant_rounding":     p.Quant.Rounding,
		"extent_divisible":   p.Quant.ExtentDivisible,
		"param_version":      p.ParamVersion,
		"rules":              rulesMap(p.Rules),
		"images":             images,
		"windows":            p.Panels,
		"dirty_blocks":       p.Dirty,
		"expected_blocks":    p.Truth,
	}
	data, merr := marshalDoc(doc)
	if merr != nil {
		return nil, merr
	}
	return &ExportResult{Version: model.SchemaV1, Data: data, Document: doc, Loss: loss, Traces: traces}, nil
}

func exportV2(p *model.Project) (*ExportResult, *errs.Error) {
	images := []any{}
	for _, img := range p.Images {
		r := img.Raster
		planes := r.Planes
		if len(planes) == 0 {
			planes = make([][]int, len(r.Channels))
			for ch := range r.Channels {
				planes[ch] = make([]int, r.Width*r.Height)
				for pixel := 0; pixel < r.Width*r.Height; pixel++ {
					planes[ch][pixel] = r.Flat[pixel*len(r.Channels)+ch]
				}
			}
		}
		planeDocs := []any{}
		for ch, name := range r.Channels {
			planeDocs = append(planeDocs, map[string]any{"channel": name, "values": planes[ch]})
		}
		images = append(images, map[string]any{
			"id": img.ID, "path": img.Path, "width": r.Width, "height": r.Height,
			"channel_planes": planeDocs,
		})
	}
	doc := map[string]any{
		"schema":             model.SchemaV2,
		"project_id":         p.ProjectID,
		"seed":               p.Seed,
		"tile_size":          map[string]any{"w": p.BlockWidth, "h": p.BlockHeight},
		"cut_permyriad":      p.ThresholdPermyriad,
		"channel_order":      p.ChannelOrder,
		"pixel_organization": "by_channel",
		"quantization": map[string]any{
			"step": p.Quant.Step, "round": p.Quant.Rounding,
			"divides_extent": p.Quant.ExtentDivisible,
		},
		"color_tolerance": p.ColorTolerance,
		"param_revision":  p.ParamVersion,
		"segmentation":    rulesMap(p.Rules),
		"images":          images,
		"panels":          p.Panels,
		"dirty_blocks":    p.Dirty,
		"expected_blocks": p.Truth,
	}
	data, err := marshalDoc(doc)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: model.SchemaV2, Data: data, Document: doc}, nil
}

func exportV3(p *model.Project) (*ExportResult, *errs.Error) {
	images := []any{}
	for _, img := range p.Images {
		r := img.Raster
		images = append(images, map[string]any{
			"id":   img.ID,
			"path": img.Path,
			"raster": map[string]any{
				"width":    r.Width,
				"height":   r.Height,
				"channels": r.Channels,
				"grouped":  r.Planes,
				"flat":     r.Flat,
			},
		})
	}
	doc := map[string]any{
		"schema": model.SchemaV3,
		"metadata": map[string]any{
			"id": p.ProjectID, "seed": p.Seed, "param_version": p.ParamVersion,
		},
		"grid":  map[string]any{"block_width": p.BlockWidth, "block_height": p.BlockHeight},
		"scale": map[string]any{"threshold_per_10k": p.ThresholdPermyriad},
		"color": map[string]any{
			"order": p.ChannelOrder, "organization": model.LayoutGrouped,
			"tolerance": p.ColorTolerance,
		},
		"quantization": map[string]any{
			"step": p.Quant.Step, "remainder_direction": p.Quant.Rounding,
			"extent_divisible": p.Quant.ExtentDivisible,
		},
		"algorithm": rulesMap(p.Rules),
		"images":    images,
		"panels":    p.Panels,
		"dirty":     p.Dirty,
		"truth":     p.Truth,
	}
	data, err := marshalDoc(doc)
	if err != nil {
		return nil, err
	}
	return &ExportResult{Version: model.SchemaV3, Data: data, Document: doc}, nil
}

func JSONRoundTrip(data []byte) (map[string]any, *errs.Error) {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, errs.New(errs.EInternal, "$", "json round trip: %v", err)
	}
	return doc, nil
}
