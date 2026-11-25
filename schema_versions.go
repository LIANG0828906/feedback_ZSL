package maskqc

import "encoding/json"

func loadV1(root rawObject, report *ConversionReport) (*Project, error) {
	id, err := root.string("$", "icon_id")
	if err != nil {
		return nil, err
	}
	canvas, err := root.object("$", "canvas")
	if err != nil {
		return nil, err
	}
	width, err := canvas.int("$.canvas", "w")
	if err != nil {
		return nil, err
	}
	height, err := canvas.int("$.canvas", "h")
	if err != nil {
		return nil, err
	}
	seed := int64(root.optionalInt("seed", int(FixedSeed)))
	strokeRaw, err := root.array("$.strokes", "strokes")
	if err != nil {
		return nil, err
	}
	strokes := make([]Stroke, 0, len(strokeRaw))
	for i, raw := range strokeRaw {
		path := "$.strokes[" + itoa(i) + "]"
		obj, err := decodeObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := obj.string(path, "id")
		if err != nil {
			return nil, err
		}
		seq, err := obj.int(path, "ord")
		if err != nil {
			return nil, err
		}
		priority, err := obj.int(path, "process")
		if err != nil {
			return nil, err
		}
		fromRaw, ok := obj["from_px"]
		if !ok {
			return nil, schemaError(path+".from_px", "missing required field")
		}
		start, err := decodeIntPair(fromRaw, path+".from_px")
		if err != nil {
			return nil, err
		}
		toRaw, ok := obj["to_px"]
		if !ok {
			return nil, schemaError(path+".to_px", "missing required field")
		}
		end, err := decodeIntPair(toRaw, path+".to_px")
		if err != nil {
			return nil, err
		}
		permille, err := obj.int(path, "coverage_permille")
		if err != nil {
			return nil, err
		}
		weight, _, err := ConvertScale(int64(permille), BasisPermille, BasisTenThousand, RoundFloor,
			"strokes."+id+".weightBP", path+".coverage_permille", report)
		if err != nil {
			return nil, err
		}
		hardnessPercent, err := obj.int(path, "hardness_percent")
		if err != nil {
			return nil, err
		}
		hardness, _, err := ConvertScale(int64(hardnessPercent), 100, BasisPermille, RoundFloor,
			"strokes."+id+".hardnessPermille", path+".hardness_percent", report)
		if err != nil {
			return nil, err
		}
		radius, err := obj.int(path, "radius_px")
		if err != nil {
			return nil, err
		}
		mode := obj.optionalInt("brush_mode", 0)
		dirCode := obj.optionalInt("direction_code", 0)
		strokes = append(strokes, Stroke{
			ID: id, Seq: seq, Priority: priority,
			Op: v1Op(mode), Role: roleForOp(v1Op(mode)),
			Start: start, End: end, WeightBP: weight, HardnessPermille: hardness,
			Radius: radius, DeclaredDirection: directionFromV1(dirCode),
		})
	}
	truth, err := loadTruthV1(root)
	if err != nil {
		return nil, err
	}
	return &Project{ID: id, Seed: seed, Width: width, Height: height, Strokes: strokes, Truth: truth}, nil
}

func v1Op(code int) string {
	if code == 1 {
		return StrokeOpErase
	}
	return StrokeOpOver
}

func roleForOp(op string) string {
	if op == StrokeOpErase {
		return RoleRepair
	}
	return RolePaint
}

func loadV2(root rawObject, report *ConversionReport) (*Project, error) {
	meta, err := root.object("$", "meta")
	if err != nil {
		return nil, err
	}
	id, err := meta.string("$.meta", "icon")
	if err != nil {
		return nil, err
	}
	width, err := root.int("$", "width")
	if err != nil {
		return nil, err
	}
	height, err := root.int("$", "height")
	if err != nil {
		return nil, err
	}
	seed := int64(root.optionalInt("seed", int(FixedSeed)))
	if rawSeed, ok := meta["seed"]; ok {
		_ = json.Unmarshal(rawSeed, &seed)
	}
	strokeRaw, err := root.array("$.strokes", "strokes")
	if err != nil {
		return nil, err
	}
	strokes := make([]Stroke, 0, len(strokeRaw))
	for i, raw := range strokeRaw {
		path := "$.strokes[" + itoa(i) + "]"
		obj, err := decodeObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := obj.string(path, "stroke_id")
		if err != nil {
			return nil, err
		}
		seq, err := obj.int(path, "stroke_no")
		if err != nil {
			return nil, err
		}
		priority := obj.optionalInt("priority", 10)
		startRaw, ok := obj["start_off"]
		if !ok {
			return nil, schemaError(path+".start_off", "missing required field")
		}
		startOff, err := decodeIntPair(startRaw, path+"start_off")
		if err != nil {
			return nil, err
		}
		endRaw, ok := obj["end_off"]
		if !ok {
			return nil, schemaError(path+".end_off", "missing required field")
		}
		endOff, err := decodeIntPair(endRaw, path+".end_off")
		if err != nil {
			return nil, err
		}
		start, err := AbsoluteFromOffset(startOff, seq, "strokes."+id+".start", report)
		if err != nil {
			return nil, err
		}
		end, err := AbsoluteFromOffset(endOff, seq, "strokes."+id+".end", report)
		if err != nil {
			return nil, err
		}
		weight, err := obj.int(path, "coverage_bp")
		if err != nil {
			return nil, err
		}
		hardness, err := obj.int(path, "hardness_pm")
		if err != nil {
			return nil, err
		}
		radius, err := obj.int(path, "radius")
		if err != nil {
			return nil, err
		}
		mode, _ := obj.string(path, "mode")
		if mode == "" {
			mode = "SRC_OVER"
		}
		dirCode, _ := obj.string(path, "direction")
		op := opFromV2(mode)
		strokes = append(strokes, Stroke{
			ID: id, Seq: seq, Priority: priority, Op: op, Role: roleForOp(op),
			Start: start, End: end, WeightBP: weight, HardnessPermille: hardness,
			Radius: radius, DeclaredDirection: directionFromV2(dirCode),
		})
	}
	truth, err := loadTruthV3Key(root, "truth")
	if err != nil {
		return nil, err
	}
	return &Project{ID: id, Seed: seed, Width: width, Height: height, Strokes: strokes, Truth: truth}, nil
}

func opFromV2(mode string) string {
	if mode == "DST_OUT" || mode == "erase" {
		return StrokeOpErase
	}
	return StrokeOpOver
}

func loadV3(root rawObject, report *ConversionReport) (*Project, error) {
	project, err := root.object("$", "project")
	if err != nil {
		return nil, err
	}
	id, err := project.string("$.project", "id")
	if err != nil {
		return nil, err
	}
	seed := int64(project.optionalInt("seed", int(FixedSeed)))
	canvas, err := root.object("$", "canvas")
	if err != nil {
		return nil, err
	}
	width, err := canvas.int("$.canvas", "width")
	if err != nil {
		return nil, err
	}
	height, err := canvas.int("$.canvas", "height")
	if err != nil {
		return nil, err
	}
	strokeRaw, err := root.array("$.strokes", "strokes")
	if err != nil {
		return nil, err
	}
	strokes := make([]Stroke, 0, len(strokeRaw))
	for i, raw := range strokeRaw {
		path := "$.strokes[" + itoa(i) + "]"
		obj, err := decodeObject(raw, path)
		if err != nil {
			return nil, err
		}
		id, err := obj.string(path, "id")
		if err != nil {
			return nil, err
		}
		seq, err := obj.int(path, "sequence")
		if err != nil {
			return nil, err
		}
		priority := obj.optionalInt("priority", 10)
		geom, err := obj.object(path, "geometry")
		if err != nil {
			return nil, err
		}
		startObj, err := geom.object(path+".geometry", "start")
		if err != nil {
			return nil, err
		}
		endObj, err := geom.object(path+".geometry", "end")
		if err != nil {
			return nil, err
		}
		startX, err := startObj.int(path+".geometry.start", "x")
		if err != nil {
			return nil, err
		}
		startY, err := startObj.int(path+".geometry.start", "y")
		if err != nil {
			return nil, err
		}
		endX, err := endObj.int(path+".geometry.end", "x")
		if err != nil {
			return nil, err
		}
		endY, err := endObj.int(path+".geometry.end", "y")
		if err != nil {
			return nil, err
		}
		weight, err := loadScaledValue(obj, path, "coverage", "value", "basis", BasisTenThousand, report, id, "weightBP")
		if err != nil {
			return nil, err
		}
		hardness, err := loadScaledValue(obj, path, "hardness", "value", "basis", BasisPermille, report, id, "hardnessPermille")
		if err != nil {
			return nil, err
		}
		radius, err := obj.int(path, "radius_px")
		if err != nil {
			return nil, err
		}
		op, role := StrokeOpOver, RolePaint
		if compRaw, ok := obj["composite"]; ok {
			comp, err := decodeObject(compRaw, path+".composite")
			if err != nil {
				return nil, err
			}
			operator := comp.optionalString("operator", StrokeOpOver)
			op = operator
			role = comp.optionalString("role", roleForOp(op))
		}
		direction := DirUnknown
		if rawDir, ok := obj["direction"]; ok {
			var dir string
			if json.Unmarshal(rawDir, &dir) == nil {
				direction = directionFromV3(dir)
			}
		}
		strokes = append(strokes, Stroke{
			ID: id, Seq: seq, Priority: priority, Op: op, Role: role,
			Start: Point{X: startX, Y: startY}, End: Point{X: endX, Y: endY},
			WeightBP: weight, HardnessPermille: hardness, Radius: radius,
			DeclaredDirection: direction,
		})
	}
	truth, err := loadTruthV3Key(root, "truth")
	if err != nil {
		return nil, err
	}
	return &Project{ID: id, Seed: seed, Width: width, Height: height, Strokes: strokes, Truth: truth}, nil
}

func (o rawObject) optionalString(key, def string) string {
	v, err := o.string("$", key)
	if err != nil {
		return def
	}
	return v
}

func loadScaledValue(parent rawObject, parentPath, objectKey, valueKey, basisKey string, expectedBasis int, report *ConversionReport, strokeID, fieldName string) (int, error) {
	scaled, err := parent.object(parentPath, objectKey)
	if err != nil {
		return 0, err
	}
	value, err := scaled.int(parentPath+"."+objectKey, valueKey)
	if err != nil {
		return 0, err
	}
	basis := scaled.optionalInt(basisKey, expectedBasis)
	if basis == expectedBasis {
		return value, nil
	}
	out, _, err := ConvertScale(int64(value), basis, expectedBasis, RoundFloor,
		"strokes."+strokeID+"."+fieldName, parentPath+"."+objectKey+"."+valueKey, report)
	return out, err
}

func loadTruthV1(root rawObject) (Truth, error) {
	t := Truth{}
	truth, err := root.object("$", "truth")
	if err != nil {
		return t, err
	}
	domainRaw, err := truth.array("$.truth", "domains")
	if err != nil {
		return t, err
	}
	for i, raw := range domainRaw {
		path := "$.truth.domains[" + itoa(i) + "]"
		obj, err := decodeObject(raw, path)
		if err != nil {
			return t, err
		}
		id, err := obj.int(path, "index")
		if err != nil {
			return t, err
		}
		cx, err := obj.int64Value(path, "cx1000")
		if err != nil {
			return t, err
		}
		cy, err := obj.int64Value(path, "cy1000")
		if err != nil {
			return t, err
		}
		area, err := obj.int(path, "area")
		if err != nil {
			return t, err
		}
		bboxRaw, ok := obj["bbox"]
		if !ok {
			return t, schemaError(path+".bbox", "missing required field")
		}
		bbox, err := decodeBBox(bboxRaw, path+".bbox")
		if err != nil {
			return t, err
		}
		holes, err := obj.int(path, "holes")
		if err != nil {
			return t, err
		}
		band, err := obj.int(path, "band_px")
		if err != nil {
			return t, err
		}
		t.Domains = append(t.Domains, DomainTruth{
			DomainID: id, CentroidX1000: cx, CentroidY1000: cy, Area: area, BBox: bbox,
			Holes: holes, BandWidthPixels: band,
			HoleTolerance: obj.optionalInt("hole_tol", 0), BandTolerancePM: obj.optionalInt("band_tol_pm", 0),
		})
	}
	segRaw, err := truth.array("$.truth", "boundary_segments")
	if err != nil {
		return t, err
	}
	for i, raw := range segRaw {
		path := "$.truth.boundary_segments[" + itoa(i) + "]"
		obj, err := decodeObject(raw, path)
		if err != nil {
			return t, err
		}
		segID, err := obj.string(path, "id")
		if err != nil {
			return t, err
		}
		domainID, err := obj.int(path, "domain_index")
		if err != nil {
			return t, err
		}
		strokeID, err := obj.string(path, "stroke")
		if err != nil {
			return t, err
		}
		code, err := obj.int(path, "dir_code")
		if err != nil {
			return t, err
		}
		nx := obj.optionalInt("nx", 0)
		ny := obj.optionalInt("ny", 0)
		points := obj.optionalInt("points", 0)
		bboxRaw, ok := obj["bbox"]
		if !ok {
			return t, schemaError(path+".bbox", "missing required field")
		}
		bbox, err := decodeBBox(bboxRaw, path+".bbox")
		if err != nil {
			return t, err
		}
		t.Segments = append(t.Segments, SegmentTruth{
			SegmentID: segID, DomainID: domainID, StrokeID: strokeID,
			Class: directionFromV1(code), NormalX: nx, NormalY: ny,
			PointCount: points, BBox: bbox,
		})
	}
	return t, nil
}

func loadTruthV3Key(root rawObject, key string) (Truth, error) {
	raw, ok := root[key]
	if !ok || string(raw) == "null" {
		return Truth{}, schemaError("$."+key, "missing required field")
	}
	var wire struct {
		Domains  []DomainTruth  `json:"domains"`
		Segments []SegmentTruth `json:"segments"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Truth{}, schemaError("$."+key, "invalid truth object: %v", err)
	}
	if wire.Domains == nil {
		return Truth{}, schemaError("$.truth.domains", "missing required field")
	}
	return Truth{Domains: wire.Domains, Segments: wire.Segments}, nil
}
