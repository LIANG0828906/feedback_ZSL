package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"maskqc/model"
)

type ExportResult struct {
	Artifact []byte
	Report   *model.ConversionReport
	Issues   []*model.Error
}

func ExportFile(project *model.Project, target model.SchemaVersion, path string) (*ExportResult, error) {
	result, err := Export(project, target)
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(path, result.Artifact, 0o644); err != nil {
		return result, err
	}
	return result, nil
}

func Export(project *model.Project, target model.SchemaVersion) (*ExportResult, error) {
	if target.Rank() == 0 {
		return nil, model.NewError(model.CodeSchema, "$.targetSchema", project.ID, "unknown target schema version")
	}
	problems := validateProject(project)
	if len(problems) > 0 {
		return nil, problems
	}
	report := &model.ConversionReport{ParamsVersion: model.ConversionVersion}
	issues := make([]*model.Error, 0)
	var document any
	switch target {
	case model.SchemaV1:
		document, issues = exportV1(project, report)
	case model.SchemaV2:
		document, issues = exportV2(project, report)
	case model.SchemaV3:
		document, issues = exportV3(project, report)
	}
	report.Finish()
	for _, issue := range issues {
		if issue.Code == model.CodeOverflow {
			return &ExportResult{Report: report, Issues: issues}, model.Errors(issues)
		}
	}
	artifact, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	artifact = append(artifact, '\n')
	reloaded, _, reloadErr := Load(artifact)
	if reloadErr != nil {
		return &ExportResult{Artifact: artifact, Report: report, Issues: issues}, fmt.Errorf("exported %s artifact failed read-back: %w", target, reloadErr)
	}
	if reloaded.ID != project.ID || reloaded.Width != project.Width || reloaded.Height != project.Height {
		return &ExportResult{Artifact: artifact, Report: report, Issues: issues},
			model.NewError(model.CodeSchema, "$", project.ID, "exported artifact identity or dimensions changed during read-back")
	}
	return &ExportResult{Artifact: artifact, Report: report, Issues: issues}, nil
}

func Upgrade(data []byte, target model.SchemaVersion) (*ExportResult, error) {
	project, _, err := Load(data)
	if err != nil {
		return nil, err
	}
	if target.Rank() < project.Version.Rank() {
		return nil, model.NewError(model.CodeSchema, "$.targetSchema", project.ID, "upgrade target is lower than source version")
	}
	return Export(project, target)
}

func exportV1(project *model.Project, report *model.ConversionReport) (map[string]any, []*model.Error) {
	issues := make([]*model.Error, 0)
	strokes := make([]map[string]any, 0, len(project.Strokes))
	ordered := append([]model.Stroke(nil), project.Strokes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	for index, stroke := range ordered {
		path := fmt.Sprintf("$.brush_log[%d]", index)
		quotient, remainder, weight, ok := roundHalfUp(int64(stroke.Weight), 10)
		if !ok {
			issues = append(issues, &model.Error{
				Code: model.CodeOverflow, Path: path + ".weight_permille", ObjectID: stroke.ID,
				Field: "weight", Message: "coverage conversion overflow",
				OperandA: fmt.Sprint(stroke.Weight), OperandB: "10",
			})
			continue
		}
		addScaleTrace(report, path+".weight_permille", stroke.ID, model.SchemaV3, model.SchemaV1, int64(stroke.Weight), 10, quotient, remainder, weight, RoundNearestHalfUp)
		if stroke.Direction != model.DefaultDirection {
			issues = append(issues, &model.Error{
				Code: model.CodeLossy, Path: path + ".direction", ObjectID: stroke.ID,
				Field: "direction", Message: "v1 cannot express stroke direction",
				Handling: "reset_to_unknown",
			})
		}
		if stroke.GroupID != "" {
			issues = append(issues, &model.Error{
				Code: model.CodeLossy, Path: path + ".groupId", ObjectID: stroke.ID,
				Field: "groupId", Message: "v1 cannot express stroke groups",
				Handling: "flatten_group",
			})
		}
		if stroke.Hardness != model.DefaultHardness {
			issues = append(issues, &model.Error{
				Code: model.CodeLossy, Path: path + ".hardness", ObjectID: stroke.ID,
				Field: "hardness", Message: "v1 cannot express brush hardness",
				Handling: "reset_to_default", DefaultValue: fmt.Sprint(model.DefaultHardness),
			})
		}
		if stroke.CompositeOrder != model.DefaultComposite {
			issues = append(issues, &model.Error{
				Code: model.CodeLossy, Path: path + ".composite_order", ObjectID: stroke.ID,
				Field: "compositeOrder", Message: "v1 cannot express composite order",
				Handling: "flatten_to_default", DefaultValue: fmt.Sprint(model.DefaultComposite),
			})
		}
		strokes = append(strokes, map[string]any{
			"id": stroke.ID, "stroke_no": stroke.Seq,
			"x0": stroke.X0, "y0": stroke.Y0, "x1": stroke.X1, "y1": stroke.Y1,
			"radius": stroke.Radius, "weight_permille": weight, "op_priority": stroke.Priority,
		})
	}
	for _, group := range project.Groups {
		issues = append(issues, &model.Error{
			Code: model.CodeLossy, Path: "$.strokeGroups", ObjectID: project.ID,
			Field: "groups", Message: "v1 cannot express stroke group " + group.ID,
			Handling: "flatten_group",
		})
	}
	return map[string]any{
		"schema": "v1", "project_id": project.ID, "seed": project.Seed,
		"width": project.Width, "height": project.Height,
		"candidate_mask": encodeRawMask(project.CandidateMask),
		"brush_log":      strokes,
		"truth":          truthV1(project),
	}, issues
}

func exportV2(project *model.Project, report *model.ConversionReport) (map[string]any, []*model.Error) {
	issues := make([]*model.Error, 0)
	ordered := append([]model.Stroke(nil), project.Strokes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	strokes := make([]map[string]any, 0, len(ordered))
	var anchorX, anchorY int64
	for index, stroke := range ordered {
		path := fmt.Sprintf("$.strokes[%d]", index)
		dx0, ok0 := safeSub(int64(stroke.X0), anchorX)
		dy0, ok1 := safeSub(int64(stroke.Y0), anchorY)
		dx1, ok2 := safeSub(int64(stroke.X1), anchorX)
		dy1, ok3 := safeSub(int64(stroke.Y1), anchorY)
		if !ok0 || !ok1 || !ok2 || !ok3 {
			issues = append(issues, &model.Error{
				Code: model.CodeOverflow, Path: path + ".relative", ObjectID: stroke.ID,
				Field: "relative", Message: "absolute-to-relative coordinate conversion overflow",
				OperandA: fmt.Sprint(stroke.X1), OperandB: fmt.Sprint(anchorX),
			})
			continue
		}
		addIntTrace(report, path+".relative.dx0", stroke.ID, model.SchemaV3, model.SchemaV2, int64(stroke.X0), anchorX, 0, dx0, "exact")
		addIntTrace(report, path+".relative.dy0", stroke.ID, model.SchemaV3, model.SchemaV2, int64(stroke.Y0), anchorY, 0, dy0, "exact")
		addIntTrace(report, path+".relative.dx1", stroke.ID, model.SchemaV3, model.SchemaV2, int64(stroke.X1), anchorX, 0, dx1, "exact")
		addIntTrace(report, path+".relative.dy1", stroke.ID, model.SchemaV3, model.SchemaV2, int64(stroke.Y1), anchorY, 0, dy1, "exact")
		if stroke.Direction != model.DefaultDirection {
			issues = append(issues, &model.Error{
				Code: model.CodeLossy, Path: path + ".direction", ObjectID: stroke.ID,
				Field: "direction", Message: "v2 cannot express stroke direction",
				Handling: "reset_to_unknown",
			})
		}
		if stroke.GroupID != "" {
			issues = append(issues, &model.Error{
				Code: model.CodeLossy, Path: path + ".groupId", ObjectID: stroke.ID,
				Field: "groupId", Message: "v2 cannot express stroke groups",
				Handling: "flatten_group",
			})
		}
		strokes = append(strokes, map[string]any{
			"strokeId": stroke.ID, "seq": stroke.Seq,
			"relative": map[string]any{"dx0": dx0, "dy0": dy0, "dx1": dx1, "dy1": dy1},
			"radius":   stroke.Radius, "hardness": stroke.Hardness, "coverage_bp": stroke.Weight,
			"priority": stroke.Priority, "composite_order": stroke.CompositeOrder,
		})
		anchorX, anchorY = int64(stroke.X1), int64(stroke.Y1)
	}
	for _, group := range project.Groups {
		issues = append(issues, &model.Error{
			Code: model.CodeLossy, Path: "$.strokeGroups", ObjectID: project.ID,
			Field: "groups", Message: "v2 cannot express stroke group " + group.ID,
			Handling: "flatten_group",
		})
	}
	return map[string]any{
		"schema_version": 2,
		"object":         map[string]any{"id": project.ID},
		"seed":           project.Seed,
		"canvas":         map[string]any{"width": project.Width, "height": project.Height},
		"candidate":      maskObject(project.CandidateMask),
		"strokes":        strokes,
		"truth":          truthV2(project),
	}, issues
}

func exportV3(project *model.Project, report *model.ConversionReport) (map[string]any, []*model.Error) {
	ordered := append([]model.Stroke(nil), project.Strokes...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	strokes := make([]map[string]any, 0, len(ordered))
	for index, stroke := range ordered {
		path := fmt.Sprintf("$.strokes[%d]", index)
		addIntTrace(report, path+".start.x", stroke.ID, model.SchemaV3, model.SchemaV3, int64(stroke.X0), 0, 0, int64(stroke.X0), "exact")
		addIntTrace(report, path+".start.y", stroke.ID, model.SchemaV3, model.SchemaV3, int64(stroke.Y0), 0, 0, int64(stroke.Y0), "exact")
		addIntTrace(report, path+".end.x", stroke.ID, model.SchemaV3, model.SchemaV3, int64(stroke.X1), 0, 0, int64(stroke.X1), "exact")
		addIntTrace(report, path+".end.y", stroke.ID, model.SchemaV3, model.SchemaV3, int64(stroke.Y1), 0, 0, int64(stroke.Y1), "exact")
		strokes = append(strokes, map[string]any{
			"id": stroke.ID, "seq": stroke.Seq,
			"start":             map[string]any{"x": stroke.X0, "y": stroke.Y0},
			"end":               map[string]any{"x": stroke.X1, "y": stroke.Y1},
			"brush":             map[string]any{"radius": stroke.Radius, "hardness": stroke.Hardness},
			"coveragePermyriad": stroke.Weight,
			"priority":          stroke.Priority,
			"direction":         string(stroke.Direction),
			"groupId":           stroke.GroupID,
			"compositeOrder":    stroke.CompositeOrder,
		})
	}
	groups := make([]map[string]any, 0, len(project.Groups))
	for _, group := range project.Groups {
		groups = append(groups, map[string]any{
			"id": group.ID, "compositeOrder": group.CompositeOrder, "strokeIds": group.StrokeIDs,
		})
	}
	return map[string]any{
		"schemaVersion": "v3",
		"project":       map[string]any{"id": project.ID, "seed": project.Seed},
		"canvas":        map[string]any{"width": project.Width, "height": project.Height},
		"candidateMask": maskObject(project.CandidateMask),
		"strokeGroups":  groups,
		"strokes":       strokes,
		"truth":         truthV3(project),
	}, nil
}

func truthV1(project *model.Project) []map[string]any {
	items := make([]map[string]any, 0, len(project.Truth))
	for _, truth := range project.Truth {
		segments := make([]map[string]any, 0, len(truth.BoundarySegments))
		for _, segment := range truth.BoundarySegments {
			segments = append(segments, map[string]any{
				"x": segment.X, "y": segment.Y, "nx": segment.NX, "ny": segment.NY,
				"normal": string(segment.Normal), "direction": string(segment.Direction),
			})
		}
		items = append(items, map[string]any{
			"id": truth.ID, "cx": truth.CentroidX, "cy": truth.CentroidY, "area": truth.Area,
			"bbox":  []int{truth.BBox.MinX, truth.BBox.MinY, truth.BBox.MaxX, truth.BBox.MaxY},
			"holes": truth.Holes, "ring_width": truth.RingWidth, "perimeter": truth.Perimeter,
			"ref_mask":  encodeRawMask(truth.ReferenceMask),
			"direction": string(truth.Direction), "segments": segments,
		})
	}
	return items
}

func truthV2(project *model.Project) []map[string]any {
	items := make([]map[string]any, 0, len(project.Truth))
	for _, truth := range project.Truth {
		boundary := make([]map[string]any, 0, len(truth.BoundarySegments))
		for _, segment := range truth.BoundarySegments {
			boundary = append(boundary, map[string]any{
				"coordinate":   []int{segment.X, segment.Y},
				"normal":       []int{segment.NX, segment.NY},
				"normal_class": string(segment.Normal),
				"consistency":  string(segment.Direction),
			})
		}
		items = append(items, map[string]any{
			"domainId": truth.ID, "centroid": []float64{truth.CentroidX, truth.CentroidY},
			"area": truth.Area, "bbox": map[string]int{
				"min_x": truth.BBox.MinX, "min_y": truth.BBox.MinY, "max_x": truth.BBox.MaxX, "max_y": truth.BBox.MaxY,
			},
			"hole_count": truth.Holes, "ring_width": truth.RingWidth, "perimeter": truth.Perimeter,
			"reference_mask": maskObject(truth.ReferenceMask),
			"direction":      string(truth.Direction), "boundary": boundary,
		})
	}
	return items
}

func truthV3(project *model.Project) []map[string]any {
	items := make([]map[string]any, 0, len(project.Truth))
	for _, truth := range project.Truth {
		segments := make([]map[string]any, 0, len(truth.BoundarySegments))
		for _, segment := range truth.BoundarySegments {
			segments = append(segments, map[string]any{
				"at":          map[string]int{"x": segment.X, "y": segment.Y},
				"normal":      map[string]int{"x": segment.NX, "y": segment.NY},
				"normalClass": string(segment.Normal), "direction": string(segment.Direction),
			})
		}
		items = append(items, map[string]any{
			"id": truth.ID, "centroid": map[string]float64{"x": truth.CentroidX, "y": truth.CentroidY},
			"area": truth.Area, "bbox": map[string]int{
				"minX": truth.BBox.MinX, "minY": truth.BBox.MinY, "maxX": truth.BBox.MaxX, "maxY": truth.BBox.MaxY,
			},
			"holes": truth.Holes, "ringWidth": truth.RingWidth, "perimeter": truth.Perimeter,
			"refMask": maskObject(truth.ReferenceMask), "direction": string(truth.Direction),
			"segments": segments,
		})
	}
	return items
}

func roundHalfUp(value, divisor int64) (quotient, remainder, result int64, ok bool) {
	if divisor <= 0 {
		return 0, 0, 0, false
	}
	quotient = value / divisor
	remainder = value % divisor
	result = quotient
	if remainder*2 >= divisor {
		result++
	}
	return quotient, remainder, result, true
}
