package schema

import (
	"encoding/json"
	"fmt"

	"maskqc/model"
)

func loadV3(root object, objectID string, report *model.ConversionReport) (*model.Project, model.Errors) {
	var problems model.Errors
	source := model.SchemaV3
	if _, err := root.stringField("schemaVersion", "$", objectID, true); err != nil {
		problems = append(problems, err)
	}
	projectFields, err := root.objectField("project", "$", objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	id := objectID
	var seed int64
	if projectFields != nil {
		parsedID, idErr := projectFields.stringField("id", "$.project", objectID, true)
		if idErr != nil {
			problems = append(problems, idErr)
		} else {
			id = parsedID
		}
		seed, _, err = projectFields.intField("seed", "$.project", id, true)
		if err != nil {
			problems = append(problems, err)
		}
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
	candidateObj, err := root.objectField("candidateMask", "$", id, true)
	if err != nil {
		problems = append(problems, err)
	}
	var candidate []byte
	if candidateObj != nil {
		if _, encodingErr := candidateObj.stringField("encoding", "$.candidateMask", id, true); encodingErr != nil {
			problems = append(problems, encodingErr)
		}
		if !candidateObj.has("data") {
			problems = append(problems, missing("$.candidateMask.data", id))
		} else {
			candidate, err = decodeMask(candidateObj.raw("data"), "$.candidateMask.data", id)
			if err != nil {
				problems = append(problems, err)
			}
		}
	}
	strokeItems, err := root.arrayField("strokes", "$", id, true)
	if err != nil {
		problems = append(problems, err)
	}
	groupItems, err := root.arrayField("strokeGroups", "$", id, false)
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
	for index, item := range strokeItems {
		path := fmt.Sprintf("$.strokes[%d]", index)
		stroke, strokeProblems := loadV3Stroke(item, path, id, report)
		problems = append(problems, strokeProblems...)
		if stroke != nil {
			project.Strokes = append(project.Strokes, *stroke)
		}
	}
	for index, item := range groupItems {
		path := fmt.Sprintf("$.strokeGroups[%d]", index)
		group, groupProblems := loadV3Group(item, path, id)
		problems = append(problems, groupProblems...)
		if group != nil {
			project.Groups = append(project.Groups, *group)
		}
	}
	for index, item := range truthItems {
		path := fmt.Sprintf("$.truth[%d]", index)
		truth, truthProblems := loadTruthV3(item, path, id)
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

func loadV3Stroke(raw json.RawMessage, path, projectID string, report *model.ConversionReport) (*model.Stroke, model.Errors) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, projectID, "object")}
	}
	obj := object(fields)
	var problems model.Errors
	id, err := obj.stringField("id", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	seq, _, err := obj.intField("seq", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	start, err := obj.objectField("start", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	end, err := obj.objectField("end", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	var x0, y0, x1, y1 int64
	if start != nil {
		x0, _, err = start.intField("x", path+".start", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		y0, _, err = start.intField("y", path+".start", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
	}
	if end != nil {
		x1, _, err = end.intField("x", path+".end", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		y1, _, err = end.intField("y", path+".end", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
	}
	brush, err := obj.objectField("brush", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	var radius, hardness int64
	if brush != nil {
		radius, _, err = brush.intField("radius", path+".brush", projectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		var hardnessSet bool
		hardness, hardnessSet, err = brush.intField("hardness", path+".brush", projectID, false)
		if err != nil {
			problems = append(problems, err)
		}
		if !hardnessSet {
			hardness = int64(model.DefaultHardness)
			report.Traces = append(report.Traces, model.ConversionTrace{
				Field: path + ".brush.hardness", ObjectID: id, From: "v3", To: "internal",
				OldValue: "<missing>", DefaultValue: fmt.Sprint(model.DefaultHardness),
				DefaultSource: "schema:v3.default.hardness", Handling: "fill_default",
				Result: hardness, Rounding: "default", Source: model.SchemaV3, Target: model.SchemaV3,
			})
		}
	}
	weight, _, err := obj.intField("coveragePermyriad", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	priority, _, err := obj.intField("priority", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	composite, compositeSet, err := obj.intField("compositeOrder", path, projectID, false)
	if err != nil {
		problems = append(problems, err)
	}
	if !compositeSet {
		composite = int64(model.DefaultComposite)
		report.Traces = append(report.Traces, model.ConversionTrace{
			Field: path + ".compositeOrder", ObjectID: id, From: "v3", To: "internal",
			OldValue: "<missing>", DefaultValue: fmt.Sprint(model.DefaultComposite),
			DefaultSource: "schema:v3.default.compositeOrder", Handling: "fill_default",
			Result: composite, Rounding: "default", Source: model.SchemaV3, Target: model.SchemaV3,
		})
	}
	directionText, directionSet, err := obj.stringFieldOptional("direction", path, projectID)
	if err != nil {
		problems = append(problems, err)
	}
	direction := model.DefaultDirection
	if directionSet {
		parsed, valid := model.ParseDirection(directionText)
		if !valid {
			problems = append(problems, model.NewError(model.CodeSchema, path+".direction", id, "unknown direction category"))
		}
		direction = parsed
	} else {
		report.Traces = append(report.Traces, model.ConversionTrace{
			Field: path + ".direction", ObjectID: id, From: "v3", To: "internal",
			OldValue: "<missing>", DefaultValue: string(model.DefaultDirection),
			DefaultSource: "schema:v3.default.direction", Handling: "fill_default",
			Rounding: "default", Source: model.SchemaV3, Target: model.SchemaV3,
		})
	}
	groupID, _, err := obj.stringFieldOptional("groupId", path, projectID)
	if err != nil {
		problems = append(problems, err)
	}
	if weight < 0 || weight > model.MaxCoverage {
		problems = append(problems, &model.Error{
			Code: model.CodeParam, Path: path + ".coveragePermyriad", ObjectID: id,
			Field: "coveragePermyriad", Message: "coverage weight is outside the declared scale", Allowed: "0..10000",
		})
	}
	if hardness < 0 || hardness > model.MaxHardness {
		problems = append(problems, &model.Error{
			Code: model.CodeParam, Path: path + ".brush.hardness", ObjectID: id,
			Field: "hardness", Message: "brush hardness is outside the allowed range", Allowed: "0..100",
		})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &model.Stroke{
		ID: id, Seq: int(seq), X0: int(x0), Y0: int(y0), X1: int(x1), Y1: int(y1),
		Radius: int(radius), Hardness: int(hardness), Weight: int(weight),
		Priority: int(priority), Direction: direction, GroupID: groupID,
		CompositeOrder: int(composite), SourceVersion: model.SchemaV3,
	}, nil
}

func (o object) stringFieldOptional(key, path, objectID string) (string, bool, *model.Error) {
	raw, ok := o[key]
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, typeError(path+"."+key, objectID, "string")
	}
	return value, true, nil
}

func loadV3Group(raw json.RawMessage, path, projectID string) (*model.StrokeGroup, model.Errors) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, projectID, "object")}
	}
	obj := object(fields)
	var problems model.Errors
	id, err := obj.stringField("id", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	order, _, err := obj.intField("compositeOrder", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	idsRaw, ok := fields["strokeIds"]
	var ids []string
	if !ok {
		problems = append(problems, missing(path+".strokeIds", id))
	} else if json.Unmarshal(idsRaw, &ids) != nil {
		problems = append(problems, typeError(path+".strokeIds", id, "string array"))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &model.StrokeGroup{ID: id, CompositeOrder: int(order), StrokeIDs: ids}, nil
}

func loadTruthV3(raw json.RawMessage, path, projectID string) (*model.DomainTruth, model.Errors) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, projectID, "object")}
	}
	obj := object(fields)
	var problems model.Errors
	id, err := obj.stringField("id", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	centroid, err := obj.objectField("centroid", path, projectID, true)
	var cx, cy float64
	if err != nil {
		problems = append(problems, err)
	} else {
		cx, _, err = centroid.floatField("x", path+".centroid", id, true)
		if err != nil {
			problems = append(problems, err)
		}
		cy, _, err = centroid.floatField("y", path+".centroid", id, true)
		if err != nil {
			problems = append(problems, err)
		}
	}
	area, _, err := obj.intField("area", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	holes, _, err := obj.intField("holes", path, projectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	ringWidth, _, err := obj.intField("ringWidth", path, projectID, true)
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
		keys := []string{"minX", "minY", "maxX", "maxY"}
		for index, key := range keys {
			value, _, fieldErr := bbox.intField(key, path+".bbox", id, true)
			if fieldErr != nil {
				problems = append(problems, fieldErr)
			}
			bboxValues[index] = value
		}
	}
	reference, err := obj.objectField("refMask", path, projectID, true)
	var mask []byte
	if err != nil {
		problems = append(problems, err)
	} else {
		if _, encodingErr := reference.stringField("encoding", path+".refMask", id, true); encodingErr != nil {
			problems = append(problems, encodingErr)
		}
		if !reference.has("data") {
			problems = append(problems, missing(path+".refMask.data", id))
		} else {
			mask, err = decodeMask(reference.raw("data"), path+".refMask.data", id)
			if err != nil {
				problems = append(problems, err)
			}
		}
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
				segment, segmentProblems := loadTruthSegmentV3(item, fmt.Sprintf("%s.segments[%d]", path, index), id)
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

func loadTruthSegmentV3(raw json.RawMessage, path, objectID string) (*model.TruthSegment, model.Errors) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, model.Errors{typeError(path, objectID, "object")}
	}
	obj := object(fields)
	var problems model.Errors
	at, err := obj.objectField("at", path, objectID, true)
	var x, y int64
	if err != nil {
		problems = append(problems, err)
	} else {
		x, _, err = at.intField("x", path+".at", objectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		y, _, err = at.intField("y", path+".at", objectID, true)
		if err != nil {
			problems = append(problems, err)
		}
	}
	normal, err := obj.objectField("normal", path, objectID, true)
	var nx, ny int64
	if err != nil {
		problems = append(problems, err)
	} else {
		nx, _, err = normal.intField("x", path+".normal", objectID, true)
		if err != nil {
			problems = append(problems, err)
		}
		ny, _, err = normal.intField("y", path+".normal", objectID, true)
		if err != nil {
			problems = append(problems, err)
		}
	}
	normalText, err := obj.stringField("normalClass", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	directionText, err := obj.stringField("direction", path, objectID, true)
	if err != nil {
		problems = append(problems, err)
	}
	normalClass, normalOK := model.ParseDirection(normalText)
	direction, directionOK := model.ParseDirection(directionText)
	if !normalOK {
		problems = append(problems, model.NewError(model.CodeSchema, path+".normalClass", objectID, "unknown normal class"))
	}
	if !directionOK {
		problems = append(problems, model.NewError(model.CodeSchema, path+".direction", objectID, "unknown direction category"))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return &model.TruthSegment{X: int(x), Y: int(y), NX: int(nx), NY: int(ny), Normal: normalClass, Direction: direction}, nil
}
