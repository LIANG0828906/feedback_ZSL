package schema

import (
	"encoding/json"
	"fmt"
	"sort"

	"maskqc/model"
)

type v2PendingStroke struct {
	stroke model.Stroke
	dx0    int64
	dy0    int64
	dx1    int64
	dy1    int64
	path   string
}

func loadV2(root object, objectID string, report *model.ConversionReport) (*model.Project, model.Errors) {
	var problems model.Errors
	source := model.SchemaV2
	if _, err := root.versionField("schema_version", "$", objectID); err != nil {
		problems = append(problems, err)
	}
	objectFields, err := root.objectField("object", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	id := objectID
	if objectFields != nil {
		parsedID, idErr := objectFields.stringField("id", "$.object", objectID, true)
		if idErr != nil {
			problems = append(problems, idErr)
		} else {
			id = parsedID
		}
	}
	seed, _, err := root.intField("seed", "$", id, true)
	if err != nil {
		problems = append(problems, err)
	}
	canvas, err := root.objectField("canvas", "$", id, true)
	if err != nil {
		problems = append(problems, err)
	}
	var width64, height64 int64
	if canvas != nil {
		width64, _, err = canvas.intField("width", "$.canvas", id, true)
		if err != nil {
			problems = append(problems, err)
		}
		height64, _, err = canvas.intField("height", "$.canvas", id, true)
		if err != nil {
			problems = append(problems, err)
		}
	}
	candidateObj, err := root.objectField("candidate", "$", id, true)
	if err != nil {
		problems = append(problems, err)
	}
	var candidate []byte
	if candidateObj != nil {
		if _, encodingErr := candidateObj.stringField("encoding", "$.candidate", id, true); encodingErr != nil {
			problems = append(problems, encodingErr)
		}
		if !candidateObj.has("data") {
			problems = append(problems, missing("$.candidate.data", id))
		} else {
			candidate, err = decodeMask(candidateObj.raw("data"), "$.candidate.data", id)
			if err != nil {
				problems = append(problems, err)
			}
		}
	}
	strokeItems, err := root.arrayField("strokes", "$", id, true)
	if err != nil {
		problems = append(problems, err)
	}
	truthItems, err := root.arrayField("truth", "$", id, true)
	if err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	project := &model.Project{ID: id, Version: source, Seed: seed, Width: int(width64), Height: int(height64), CandidateMask: candidate}
	pending := make([]v2PendingStroke, 0, len(strokeItems))
	for index, item := range strokeItems {
		path := fmt.Sprintf("$.strokes[%d]", index)
		stroke, strokeProblems := loadV2Stroke(item, path, id, report)
		problems = append(problems, strokeProblems...)
		if stroke != nil {
			pending = append(pending, *stroke)
		}
	}
	if len(problems) == 0 {
		sort.SliceStable(pending, func(i, j int) bool {
			if pending[i].stroke.Seq != pending[j].stroke.Seq {
				return pending[i].stroke.Seq < pending[j].stroke.Seq
			}
			return pending[i].stroke.ID < pending[j].stroke.ID
		})
		var anchorX, anchorY int64
		for index := range pending {
			item := &pending[index]
			fields := []struct {
				name string
				rel  int64
				base int64
			}{
				{name: "relative.dx0", rel: item.dx0, base: anchorX},
				{name: "relative.dy0", rel: item.dy0, base: anchorY},
				{name: "relative.dx1", rel: item.dx1, base: anchorX},
				{name: "relative.dy1", rel: item.dy1, base: anchorY},
			}
			values := make([]int64, 4)
			for fieldIndex, field := range fields {
				value, ok := safeAdd(field.rel, field.base)
				if !ok {
					problems = append(problems, &model.Error{
						Code: model.CodeOverflow, Path: item.path + "." + field.name, ObjectID: item.stroke.ID,
						Field: field.name, Message: "relative coordinate conversion overflow",
						OperandA: fmt.Sprint(field.rel), OperandB: fmt.Sprint(field.base),
					})
					continue
				}
				values[fieldIndex] = value
				addIntTrace(report, item.path+"."+field.name, item.stroke.ID, model.SchemaV2, model.SchemaV3, field.rel, field.base, 0, value, "exact")
			}
			item.stroke.X0 = int(values[0])
			item.stroke.Y0 = int(values[1])
			item.stroke.X1 = int(values[2])
			item.stroke.Y1 = int(values[3])
			anchorX = values[2]
			anchorY = values[3]
			project.Strokes = append(project.Strokes, item.stroke)
		}
	}
	for index, item := range truthItems {
		path := fmt.Sprintf("$.truth[%d]", index)
		truth, truthProblems := loadTruthV2(item, path, id)
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

func loadV2Stroke(raw json.RawMessage, path, projectID string, report *model.ConversionReport) (*v2PendingStroke, model.Errors) {
	var problems model.Errors
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, projectID, "object")}
	}
	obj := object(fields)
	id, err := obj.stringField("strokeId", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	seq, _, err := obj.intField("seq", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	relative, err := obj.objectField("relative", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	var dx0, dy0, dx1, dy1 int64
	if relative != nil {
		dx0, _, err = relative.intField("dx0", path+".relative", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		dy0, _, err = relative.intField("dy0", path+".relative", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		dx1, _, err = relative.intField("dx1", path+".relative", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		dy1, _, err = relative.intField("dy1", path+".relative", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
	}
	radius, _, err := obj.intField("radius", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	hardness, hardnessSet, err := obj.intField("hardness", path, projectID, false)
	if err != nil {
		problems = append(problems, err)
	}
	weight, _, err := obj.intField("coverage_bp", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	priority, _, err := obj.intField("priority", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	composite, compositeSet, err := obj.intField("composite_order", path, projectID, false)
	if err != nil {
		problems = append(problems, err)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	if weight < 0 || weight > 10000 {
		problems = append(problems, &model.Error{
			Code: model.CodeParam, Path: path + ".coverage_bp", ObjectID: id,
			Field: "coverage_bp", Message: "coverage weight is outside the declared scale", Allowed: "0..10000",
		})
	}
	if !hardnessSet {
		hardness = int64(model.DefaultHardness)
		report.Traces = append(report.Traces, model.ConversionTrace{
			Field: path + ".hardness", ObjectID: id, From: "v2", To: "internal",
			OldValue: "<missing>", DefaultValue: fmt.Sprint(model.DefaultHardness),
			DefaultSource: "schema:v2.default.hardness", Handling: "fill_default",
			Result: hardness, Rounding: "default", Source: model.SchemaV2, Target: model.SchemaV3,
		})
	}
	if hardness < 0 || hardness > model.MaxHardness {
		problems = append(problems, &model.Error{
			Code: model.CodeParam, Path: path + ".hardness", ObjectID: id,
			Field: "hardness", Message: "brush hardness is outside the allowed range", Allowed: "0..100",
		})
	}
	if !compositeSet {
		composite = int64(model.DefaultComposite)
		report.Traces = append(report.Traces, model.ConversionTrace{
			Field: path + ".composite_order", ObjectID: id, From: "v2", To: "internal",
			OldValue: "<missing>", DefaultValue: fmt.Sprint(model.DefaultComposite),
			DefaultSource: "schema:v2.default.compositeOrder", Handling: "fill_default",
			Result: composite, Rounding: "default", Source: model.SchemaV2, Target: model.SchemaV3,
		})
	}
	direction := model.DefaultDirection
	report.Traces = append(report.Traces, model.ConversionTrace{
		Field: path + ".direction", ObjectID: id, From: "v2", To: "internal",
		OldValue: "<missing>", DefaultValue: string(model.DefaultDirection),
		DefaultSource: "schema:v2.default.direction", Handling: "fill_default",
		Rounding: "default", Source: model.SchemaV2, Target: model.SchemaV3,
	})
	if len(problems) > 0 {
		return nil, problems
	}
	return &v2PendingStroke{
		stroke: model.Stroke{
			ID: id, Seq: int(seq), Radius: int(radius), Hardness: int(hardness),
			Weight: int(weight), Priority: int(priority), Direction: direction,
			CompositeOrder: int(composite), SourceVersion: model.SchemaV2,
		},
		dx0: dx0, dy0: dy0, dx1: dx1, dy1: dy1, path: path,
	}, nil
}

func loadTruthV2(raw json.RawMessage, path, projectID string) (*model.DomainTruth, model.Errors) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, projectID, "object")}
	}
	obj := object(fields)
	var problems model.Errors
	id, err := obj.stringField("domainId", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	centroidRaw, ok := fields["centroid"]
	if !ok {
		problems = append(problems, missing(path+".centroid", id))
	}
	centroidValues, centroidOK := decodeIntArray(centroidRaw, 2)
	var cx, cy float64
	if ok && centroidOK {
		cx, cy = float64(centroidValues[0]), float64(centroidValues[1])
	} else if ok {
		var values []float64
		if json.Unmarshal(centroidRaw, &values) == nil && len(values) == 2 {
			cx, cy = values[0], values[1]
		} else {
			problems = append(problems, typeError(path+".centroid", id, "number array of length 2"))
		}
	}
	area, _, err := obj.intField("area", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	holes, _, err := obj.intField("hole_count", path, projectID, true)
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
	bbox, err := obj.objectField("bbox", path, projectID, true)
	var bboxValues [4]int64
	if err != nil {
		problems = append(problems, err)
	} else {
		keys := []string{"min_x", "min_y", "max_x", "max_y"}
		for index, key := range keys {
			value, _, fieldErr := bbox.intField(key, path+".bbox", id, true)
			if fieldErr != nil {
				problems = append(problems, fieldErr)
			}
			bboxValues[index] = value
		}
	}
	reference, err := obj.objectField("reference_mask", path, projectID, true)
	var mask []byte
	if err != nil {
		problems = append(problems, err)
	} else {
		if _, encodingErr := reference.stringField("encoding", path+".reference_mask", id, true); encodingErr != nil {
			problems = append(problems, encodingErr)
		}
		if !reference.has("data") {
			problems = append(problems, missing(path+".reference_mask.data", id))
		} else {
			mask, err = decodeMask(reference.raw("data"), path+".reference_mask.data", id)
			if err != nil {
				problems = append(problems, err)
			}
		}
	}
	boundaryRaw, ok := fields["boundary"]
	var segments []model.TruthSegment
	if !ok {
		problems = append(problems, missing(path+".boundary", id))
	} else {
		var items []json.RawMessage
		if json.Unmarshal(boundaryRaw, &items) != nil {
			problems = append(problems, typeError(path+".boundary", id, "array"))
		} else {
			for index, item := range items {
				segment, segmentProblems := loadTruthSegmentV2(item, fmt.Sprintf("%s.boundary[%d]", path, index), id)
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

func loadTruthSegmentV2(raw json.RawMessage, path, objectID string) (*model.TruthSegment, model.Errors) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, objectID, "object")}
	}
	obj := object(fields)
	var problems model.Errors
	coordinateRaw, ok := fields["coordinate"]
	var coordinate []int64
	var coordinateOK bool
	if !ok {
		problems = append(problems, missing(path+".coordinate", objectID))
	} else {
		coordinate, coordinateOK = decodeIntArray(coordinateRaw, 2)
		if !coordinateOK {
			problems = append(problems, typeError(path+".coordinate", objectID, "integer array of length 2"))
		}
	}
	normalRaw, ok := fields["normal"]
	var normalValues []int64
	var normalOK bool
	if !ok {
		problems = append(problems, missing(path+".normal", objectID))
	} else {
		normalValues, normalOK = decodeIntArray(normalRaw, 2)
		if !normalOK {
			problems = append(problems, typeError(path+".normal", objectID, "integer array of length 2"))
		}
	}
	normalText, err := obj.stringField("normal_class", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	directionText, err := obj.stringField("consistency", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	normal, normalClassOK := model.ParseDirection(normalText)
	direction, directionOK := model.ParseDirection(directionText)
	if !normalClassOK {
		problems = append(problems, model.NewError(model.CodeSchema, path+".normal_class", objectID, "unknown normal class"))
	}
	if !directionOK {
		problems = append(problems, model.NewError(model.CodeSchema, path+".consistency", objectID, "unknown direction category"))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &model.TruthSegment{
		X: int(coordinate[0]), Y: int(coordinate[1]),
		NX: int(normalValues[0]), NY: int(normalValues[1]),
		Normal: normal, Direction: direction,
	}, nil
}
