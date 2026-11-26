package schema

import (
	"encoding/json"
	"fmt"

	"maskqc/model"
)

func loadV1(root object, objectID string, report *model.ConversionReport) (*model.Project, model.Errors) {
	var problems model.Errors
	source := model.SchemaV1
	if _, err := root.stringField("schema", "$", objectID, true); err != nil {
		problems = append(problems, err)
	}
	id, err := root.stringField("project_id", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	if objectID == "" {
		objectID = id
	}
	seed, _, err := root.intField("seed", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	width64, _, err := root.intField("width", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	height64, _, err := root.intField("height", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	candidate, err := requiredMaskV1(root, "candidate_mask", "$", objectID)
	if err != nil {
		problems = append(problems, err)
	}
	strokeItems, err := root.arrayField("brush_log", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	truthItems, err := root.arrayField("truth", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	if width64 <= 0 || height64 <= 0 || width64 > 1<<20 || height64 > 1<<20 {
		problems = append(problems, model.NewError(model.CodeSchema, "$.width|$.height", objectID, "dimensions must be in 1..1048576"))
		return nil, problems
	}
	project := &model.Project{
		ID: id, Version: source, Seed: seed,
		Width: int(width64), Height: int(height64), CandidateMask: candidate,
	}
	for index, item := range strokeItems {
		path := fmt.Sprintf("$.brush_log[%d]", index)
		stroke, strokeProblems := loadV1Stroke(item, path, objectID, report)
		problems = append(problems, strokeProblems...)
		if stroke != nil {
			project.Strokes = append(project.Strokes, *stroke)
		}
	}
	for index, item := range truthItems {
		path := fmt.Sprintf("$.truth[%d]", index)
		truth, truthProblems := loadTruthV1(item, path, objectID)
		problems = append(problems, truthProblems...)
		if truth != nil {
			project.Truth = append(project.Truth, *truth)
		}
	}
	if len(problems) > 0 {
		return project, problems
	}
	project.Normalize()
	return project, nil
}

func requiredMaskV1(root object, key, path, objectID string) ([]byte, *model.Error) {
	raw, ok := root[key]
	if !ok {
		return nil, missing(path+"."+key, objectID)
	}
	return decodeMask(raw, path+"."+key, objectID)
}

func loadV1Stroke(raw json.RawMessage, path, projectID string, report *model.ConversionReport) (*model.Stroke, model.Errors) {
	var problems model.Errors
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, projectID, "object")}
	}
	obj := object(fields)
	id, err := obj.stringField("id", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	seq, _, err := obj.intField("stroke_no", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	x0, _, err := obj.intField("x0", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	y0, _, err := obj.intField("y0", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	x1, _, err := obj.intField("x1", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	y1, _, err := obj.intField("y1", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	radius, _, err := obj.intField("radius", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	weight, _, err := obj.intField("weight_permille", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	priority, _, err := obj.intField("op_priority", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	if weight < 0 || weight > 1000 {
		problems = append(problems, &model.Error{
			Code: model.CodeParam, Path: path + ".weight_permille", ObjectID: id,
			Field: "weight_permille", Message: "coverage weight is outside the declared scale",
			Allowed: "0..1000",
		})
		return nil, problems
	}
	canonicalWeight, ok := safeMul(weight, 10)
	if !ok {
		problems = append(problems, &model.Error{
			Code: model.CodeOverflow, Path: path + ".weight_permille", ObjectID: id,
			Field: "weight_permille", Message: "integer conversion overflow",
			OperandA: fmt.Sprint(weight), OperandB: "10",
		})
		return nil, problems
	}
	addScaleTrace(report, path+".weight_permille", id, model.SchemaV1, model.SchemaV3, weight, 10, weight, 0, canonicalWeight, "exact")
	hardness := int64(model.DefaultHardness)
	report.Traces = append(report.Traces, model.ConversionTrace{
		Field: path + ".hardness", ObjectID: id, From: "v1", To: "internal",
		OldValue: "<missing>", DefaultValue: fmt.Sprint(model.DefaultHardness),
		DefaultSource: "schema:v1.default.hardness", Handling: "fill_default",
		Result: hardness, Rounding: "default", Source: model.SchemaV1, Target: model.SchemaV3,
	})
	direction := model.DefaultDirection
	report.Traces = append(report.Traces, model.ConversionTrace{
		Field: path + ".direction", ObjectID: id, From: "v1", To: "internal",
		OldValue: "<missing>", DefaultValue: string(model.DefaultDirection),
		DefaultSource: "schema:v1.default.direction", Handling: "fill_default",
		Rounding: "default", Source: model.SchemaV1, Target: model.SchemaV3,
	})
	composite := int64(model.DefaultComposite)
	report.Traces = append(report.Traces, model.ConversionTrace{
		Field: path + ".composite_order", ObjectID: id, From: "v1", To: "internal",
		OldValue: "<missing>", DefaultValue: fmt.Sprint(model.DefaultComposite),
		DefaultSource: "schema:v1.default.compositeOrder", Handling: "fill_default",
		Result: composite, Rounding: "default", Source: model.SchemaV1, Target: model.SchemaV3,
	})
	return &model.Stroke{
		ID: id, Seq: int(seq), X0: int(x0), Y0: int(y0), X1: int(x1), Y1: int(y1),
		Radius: int(radius), Hardness: int(hardness), Weight: int(canonicalWeight),
		Priority: int(priority), Direction: direction, CompositeOrder: int(composite),
		SourceVersion: model.SchemaV1,
	}, nil
}

func loadTruthV1(raw json.RawMessage, path, projectID string) (*model.DomainTruth, model.Errors) {
	var problems model.Errors
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, projectID, "object")}
	}
	obj := object(fields)
	id, err := obj.stringField("id", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	cx, _, err := obj.floatField("cx", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	cy, _, err := obj.floatField("cy", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	area, _, err := obj.intField("area", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	holes, _, err := obj.intField("holes", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	ringWidth, _, err := obj.intField("ring_width", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	perimeter, _, err := obj.intField("perimeter", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	directionText, err := obj.stringField("direction", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	direction, valid := model.ParseDirection(directionText)
	if directionText != "" && !valid {
		problems = append(problems, model.NewError(model.CodeSchema, path+".direction", id, "unknown direction category"))
	}
	bboxRaw, ok := fields["bbox"]
	if !ok {
		problems = append(problems, missing(path+".bbox", id))
	}
	bboxValues, validBBox := decodeIntArray(bboxRaw, 4)
	if ok && !validBBox {
		problems = append(problems, typeError(path+".bbox", id, "integer array of length 4"))
	}
	mask, err := requiredMaskV1(obj, "ref_mask", path, projectID)
	if err != nil {
		problems = append(problems, err)
	}
	segmentsRaw, ok := fields["segments"]
	var segments []model.TruthSegment
	if !ok {
		problems = append(problems, missing(path+".segments", id))
	} else {
		var items []json.RawMessage
		if json.Unmarshal(segmentsRaw, &items) != nil {
			problems = append(problems, typeError(path+".segments", id, "array"))
		} else {
			for index, item := range items {
				segment, segmentProblems := loadTruthSegmentV1(item, fmt.Sprintf("%s.segments[%d]", path, index), id)
				problems = append(problems, segmentProblems...)
				if segment != nil {
					segments = append(segments, *segment)
				}
			}
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &model.DomainTruth{
		ID: id, CentroidX: cx, CentroidY: cy, Area: int(area),
		BBox:  model.Rect{MinX: int(bboxValues[0]), MinY: int(bboxValues[1]), MaxX: int(bboxValues[2]), MaxY: int(bboxValues[3])},
		Holes: int(holes), RingWidth: int(ringWidth), Perimeter: int(perimeter),
		ReferenceMask: mask, Direction: direction, BoundarySegments: segments,
	}, nil
}

func loadTruthSegmentV1(raw json.RawMessage, path, objectID string) (*model.TruthSegment, model.Errors) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, objectID, "object")}
	}
	obj := object(fields)
	var problems model.Errors
	x, _, err := obj.intField("x", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	y, _, err := obj.intField("y", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	nx, _, err := obj.intField("nx", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	ny, _, err := obj.intField("ny", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	normalText, err := obj.stringField("normal", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	directionText, err := obj.stringField("direction", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	normal, normalOK := model.ParseDirection(normalText)
	direction, directionOK := model.ParseDirection(directionText)
	if !normalOK {
		problems = append(problems, model.NewError(model.CodeSchema, path+".normal", objectID, "unknown normal class"))
	}
	if !directionOK {
		problems = append(problems, model.NewError(model.CodeSchema, path+".direction", objectID, "unknown direction category"))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &model.TruthSegment{X: int(x), Y: int(y), NX: int(nx), NY: int(ny), Normal: normal, Direction: direction}, nil
}
