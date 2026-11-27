package grid

import "fmt"

func decodeV1(root map[string]any) (*Level, ConversionReport, error) {
	rep := ConversionReport{SourceSchema: 1}
	code, err := reqString(root, "level_code", "$")
	if err != nil {
		return nil, rep, err
	}
	gm, err := reqMap(root, "gravity_mm_s2", "$")
	if err != nil {
		return nil, rep, err
	}
	gx, err := reqIntField(gm, "x_mm", "$.gravity_mm_s2", "")
	if err != nil {
		return nil, rep, err
	}
	gy, err := reqIntField(gm, "y_mm", "$.gravity_mm_s2", "")
	if err != nil {
		return nil, rep, err
	}
	gxUnits, err := MMToUnits("", "$.gravity_mm_s2.x_mm", gx)
	if err != nil {
		return nil, rep, err
	}
	gyUnits, err := MMToUnits("", "$.gravity_mm_s2.y_mm", gy)
	if err != nil {
		return nil, rep, err
	}
	gravity := Vec2{X: gxUnits, Y: gyUnits}

	ticks, err := reqIntField(root, "time_step_tick", "$", "")
	if err != nil {
		return nil, rep, err
	}
	stepMS, err := TicksToMS("", "$.time_step_tick", ticks)
	if err != nil {
		return nil, rep, err
	}
	if stepMS <= 0 {
		return nil, rep, decodeError("", "$.time_step_tick", "time step must be positive")
	}
	bounds, err := decodeBounds(root, "world_bounds_mm", "$")
	if err != nil {
		return nil, rep, err
	}

	rawObs, err := reqField(root, "obstacles", "$")
	if err != nil {
		return nil, rep, err
	}
	obsArr, ok := rawObs.([]any)
	if !ok {
		return nil, rep, decodeError("", "$.obstacles", "must be an array")
	}
	obstacles := make([]Obstacle, 0, len(obsArr))
	seen := map[string]int{}
	for i, raw := range obsArr {
		path := arrayItem("$.obstacles", i)
		om, ok := raw.(map[string]any)
		if !ok {
			return nil, rep, decodeError("", path, "obstacle must be an object")
		}
		id, err := reqString(om, "obstacle_id", path)
		if err != nil {
			return nil, rep, err
		}
		if prior, dup := seen[id]; dup {
			ge := decodeError(id, path+".obstacle_id", "duplicate obstacle id %q; first at obstacles[%d]", id, prior)
			return nil, rep, ge
		}
		seen[id] = i

		a, err := reqPointMM(om, "start_mm", path, id, "x_mm", "y_mm")
		if err != nil {
			return nil, rep, err
		}
		b, err := reqPointMM(om, "end_mm", path, id, "x_mm", "y_mm")
		if err != nil {
			return nil, rep, err
		}
		pm, err := reqIntField(om, "restitution_pm", path, id)
		if err != nil {
			return nil, rep, err
		}
		if pm < 0 || pm > 1000 {
			return nil, rep, decodeError(id, path+".restitution_pm", "must be in [0,1000] permille")
		}
		restitution, err := PermilleToBPS(id, path+".restitution_pm", pm)
		if err != nil {
			return nil, rep, err
		}
		mat, err := reqIntField(om, "material_ver", path, id)
		if err != nil {
			return nil, rep, err
		}
		seq, err := reqIntField(om, "seq_no", path, id)
		if err != nil {
			return nil, rep, err
		}
		normal := RightNormal(a, b)
		rep.Fills = append(rep.Fills, FieldTrace{
			Kind: "fill", ObjectID: id, Field: fmt.Sprintf("obstacles[%d].normal", i),
			OldValue: "<absent>", NewValue: fmt.Sprintf("(%d,%d)", normal.X, normal.Y),
			Source: "declared-default:right-normal-q1000",
		})
		rep.Fills = append(rep.Fills, FieldTrace{
			Kind: "fill", ObjectID: id, Field: fmt.Sprintf("obstacles[%d].penetrable", i),
			OldValue: "<absent>", NewValue: "false", Source: "declared-default:penetrable=false",
		})
		obstacles = append(obstacles, Obstacle{
			ID: id, Seq: int(seq), A: a, B: b, Normal: normal, Restitution: restitution,
			MaterialVer: int(mat), Penetrable: false, EndpointVer: 1, NormalVer: 1, RestitutionVer: 1,
		})
	}

	launches, err := decodeV1Launches(root)
	if err != nil {
		return nil, rep, err
	}
	return &Level{
		SchemaVersion: 3, Code: code, Gravity: gravity, StepMS: stepMS, Bounds: bounds,
		Obstacles: obstacles, Launches: launches,
	}, rep, nil
}

func decodeV1Launches(root map[string]any) ([]Launch, error) {
	raw, err := reqField(root, "launches", "$")
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, decodeError("", "$.launches", "must be an array")
	}
	out := make([]Launch, 0, len(arr))
	seen := map[string]bool{}
	for i, rawL := range arr {
		path := arrayItem("$.launches", i)
		m, ok := rawL.(map[string]any)
		if !ok {
			return nil, decodeError("", path, "launch must be an object")
		}
		id, err := reqString(m, "launch_id", path)
		if err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, decodeError(id, path+".launch_id", "duplicate launch id")
		}
		seen[id] = true
		origin, err := reqPointMM(m, "origin_mm", path, id, "x_mm", "y_mm")
		if err != nil {
			return nil, err
		}
		vel, err := reqPointMM(m, "speed_mm_s", path, id, "x_mm", "y_mm")
		if err != nil {
			return nil, err
		}
		bl, err := reqIntField(m, "bounce_limit", path, id)
		if err != nil {
			return nil, err
		}
		fl, err := reqIntField(m, "frame_limit", path, id)
		if err != nil {
			return nil, err
		}
		out = append(out, Launch{ID: id, Origin: origin, Velocity: vel, BounceLimit: int(bl), FrameLimit: int(fl)})
	}
	return out, nil
}

func decodeV2(root map[string]any) (*Level, ConversionReport, error) {
	rep := ConversionReport{SourceSchema: 2}
	code, err := reqString(root, "levelId", "$")
	if err != nil {
		return nil, rep, err
	}
	gravity, err := reqMMPointPlain(root, "gravity", "$", "")
	if err != nil {
		return nil, rep, err
	}
	stepMS, err := reqIntField(root, "timeStepMs", "$", "")
	if err != nil {
		return nil, rep, err
	}
	if stepMS <= 0 {
		return nil, rep, decodeError("", "$.timeStepMs", "time step must be positive")
	}
	bounds, err := decodePlainBounds(root, "bounds", "$")
	if err != nil {
		return nil, rep, err
	}

	rawObs, err := reqField(root, "obstacleList", "$")
	if err != nil {
		return nil, rep, err
	}
	arr, ok := rawObs.([]any)
	if !ok {
		return nil, rep, decodeError("", "$.obstacleList", "must be an array")
	}
	obstacles := make([]Obstacle, 0, len(arr))
	seen := map[string]int{}
	for i, rawO := range arr {
		path := arrayItem("$.obstacleList", i)
		om, ok := rawO.(map[string]any)
		if !ok {
			return nil, rep, decodeError("", path, "obstacle must be an object")
		}
		id, err := reqString(om, "id", path)
		if err != nil {
			return nil, rep, err
		}
		if prior, dup := seen[id]; dup {
			return nil, rep, decodeError(id, path+".id", "duplicate obstacle id %q; first index %d", id, prior)
		}
		seen[id] = i
		a, err := reqMMPointPlain(om, "from", path, id)
		if err != nil {
			return nil, rep, err
		}
		b, err := reqMMPointPlain(om, "to", path, id)
		if err != nil {
			return nil, rep, err
		}
		n, err := reqPointPlain(om, "normal", path, id)
		if err != nil {
			return nil, rep, err
		}
		nPath := path + ".normal"
		if err := validateNormal(n, id, nPath); err != nil {
			return nil, rep, err
		}
		pm, err := reqIntField(om, "restitutionPermille", path, id)
		if err != nil {
			return nil, rep, err
		}
		if pm < 0 || pm > 1000 {
			return nil, rep, decodeError(id, path+".restitutionPermille", "must be in [0,1000]")
		}
		mat, err := reqIntField(om, "materialVersion", path, id)
		if err != nil {
			return nil, rep, err
		}
		seq, err := reqIntField(om, "seq", path, id)
		if err != nil {
			return nil, rep, err
		}
		rep.Fills = append(rep.Fills, FieldTrace{
			Kind: "fill", ObjectID: id, Field: fmt.Sprintf("obstacleList[%d].allowsPenetration", i),
			OldValue: "<absent>", NewValue: "false", Source: "declared-default:penetrable=false",
		})
		obstacles = append(obstacles, Obstacle{
			ID: id, Seq: int(seq), A: a, B: b, Normal: n,
			Restitution: pm * 10, MaterialVer: int(mat), Penetrable: false,
			EndpointVer: 1, NormalVer: 1, RestitutionVer: 1,
		})
	}
	launches, err := decodeV2Launches(root)
	if err != nil {
		return nil, rep, err
	}
	return &Level{SchemaVersion: 3, Code: code, Gravity: gravity, StepMS: stepMS, Bounds: bounds, Obstacles: obstacles, Launches: launches}, rep, nil
}

func decodeV2Launches(root map[string]any) ([]Launch, error) {
	raw, err := reqField(root, "launchList", "$")
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, decodeError("", "$.launchList", "must be array")
	}
	out := make([]Launch, 0, len(arr))
	for i, rawL := range arr {
		path := arrayItem("$.launchList", i)
		m, ok := rawL.(map[string]any)
		if !ok {
			return nil, decodeError("", path, "launch must be object")
		}
		id, err := reqString(m, "id", path)
		if err != nil {
			return nil, err
		}
		origin, err := reqMMPointPlain(m, "origin", path, id)
		if err != nil {
			return nil, err
		}
		vel, err := reqMMPointPlain(m, "velocity", path, id)
		if err != nil {
			return nil, err
		}
		bl, err := reqIntField(m, "bounceLimit", path, id)
		if err != nil {
			return nil, err
		}
		fl, err := reqIntField(m, "frameLimit", path, id)
		if err != nil {
			return nil, err
		}
		out = append(out, Launch{ID: id, Origin: origin, Velocity: vel, BounceLimit: int(bl), FrameLimit: int(fl)})
	}
	return out, nil
}

func decodeV3(root map[string]any) (*Level, ConversionReport, error) {
	rep := ConversionReport{SourceSchema: 3}
	code, err := reqString(root, "sceneId", "$")
	if err != nil {
		return nil, rep, err
	}
	gravity, err := reqMMPointPlain(root, "acceleration", "$", "")
	if err != nil {
		return nil, rep, err
	}
	stepMS, err := reqIntField(root, "stepMillis", "$", "")
	if err != nil {
		return nil, rep, err
	}
	if stepMS <= 0 {
		return nil, rep, decodeError("", "$.stepMillis", "step must be positive")
	}
	bounds, err := decodePlainBounds(root, "sceneBounds", "$")
	if err != nil {
		return nil, rep, err
	}
	rawObs, err := reqField(root, "barriers", "$")
	if err != nil {
		return nil, rep, err
	}
	arr, ok := rawObs.([]any)
	if !ok {
		return nil, rep, decodeError("", "$.barriers", "must be array")
	}
	obstacles := make([]Obstacle, 0, len(arr))
	seen := map[string]int{}
	var prevA Point
	for i, rawO := range arr {
		path := arrayItem("$.barriers", i)
		om, ok := rawO.(map[string]any)
		if !ok {
			return nil, rep, decodeError("", path, "barrier must be object")
		}
		id, err := reqString(om, "key", path)
		if err != nil {
			return nil, rep, err
		}
		if prior, dup := seen[id]; dup {
			return nil, rep, decodeError(id, path+".key", "duplicate barrier key %q; first index %d", id, prior)
		}
		seen[id] = i
		anchor, err := reqMMPointPlain(om, "anchorDelta", path, id)
		if err != nil {
			return nil, rep, err
		}
		delta, err := reqMMPointPlain(om, "endpointDelta", path, id)
		if err != nil {
			return nil, rep, err
		}
		a, err := addChecked(path+".anchorDelta.x", prevA.X, anchor.X)
		if err != nil {
			return nil, rep, withObject(err, id)
		}
		ay, err := addChecked(path+".anchorDelta.y", prevA.Y, anchor.Y)
		if err != nil {
			return nil, rep, withObject(err, id)
		}
		bx, err := addChecked(path+".endpointDelta.x", a, delta.X)
		if err != nil {
			return nil, rep, withObject(err, id)
		}
		by, err := addChecked(path+".endpointDelta.y", ay, delta.Y)
		if err != nil {
			return nil, rep, withObject(err, id)
		}
		aPoint := Point{X: a, Y: ay}
		bPoint := Point{X: bx, Y: by}
		face, err := reqPointPlain(om, "face", path, id)
		if err != nil {
			return nil, rep, err
		}
		if err := validateNormal(face, id, path+".face"); err != nil {
			return nil, rep, err
		}
		bps, err := reqIntField(om, "bounceBasisPoints", path, id)
		if err != nil {
			return nil, rep, err
		}
		if bps < 0 || bps > 10000 {
			return nil, rep, decodeError(id, path+".bounceBasisPoints", "must be in [0,10000]")
		}
		mat, err := reqIntField(om, "materialRevision", path, id)
		if err != nil {
			return nil, rep, err
		}
		order, err := reqIntField(om, "order", path, id)
		if err != nil {
			return nil, rep, err
		}
		penetrable := false
		if rawPen, exists := om["allowsPenetration"]; exists && rawPen != nil {
			b, ok := rawPen.(bool)
			if !ok {
				return nil, rep, decodeError(id, path+".allowsPenetration", "must be boolean")
			}
			penetrable = b
		} else {
			rep.Fills = append(rep.Fills, FieldTrace{
				Kind: "fill", ObjectID: id, Field: path + ".allowsPenetration",
				OldValue: "<absent>", NewValue: "false", Source: "declared-default:penetrable=false",
			})
		}
		obstacles = append(obstacles, Obstacle{
			ID: id, Seq: int(order), A: aPoint, B: bPoint, Normal: face,
			Restitution: bps, MaterialVer: int(mat), Penetrable: penetrable,
			EndpointVer: 1, NormalVer: 1, RestitutionVer: 1,
		})
		prevA = aPoint
	}
	launches, err := decodeV3Emitters(root)
	if err != nil {
		return nil, rep, err
	}
	return &Level{SchemaVersion: 3, Code: code, Gravity: gravity, StepMS: stepMS, Bounds: bounds, Obstacles: obstacles, Launches: launches}, rep, nil
}

func decodeV3Emitters(root map[string]any) ([]Launch, error) {
	raw, err := reqField(root, "emitters", "$")
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, decodeError("", "$.emitters", "must be array")
	}
	out := make([]Launch, 0, len(arr))
	for i, rawE := range arr {
		path := arrayItem("$.emitters", i)
		m, ok := rawE.(map[string]any)
		if !ok {
			return nil, decodeError("", path, "emitter must be object")
		}
		id, err := reqString(m, "key", path)
		if err != nil {
			return nil, err
		}
		start, err := reqMMPointPlain(m, "start", path, id)
		if err != nil {
			return nil, err
		}
		vec, err := reqMMPointPlain(m, "initialVector", path, id)
		if err != nil {
			return nil, err
		}
		bl, err := reqIntField(m, "bounceCeiling", path, id)
		if err != nil {
			return nil, err
		}
		fl, err := reqIntField(m, "frameCeiling", path, id)
		if err != nil {
			return nil, err
		}
		out = append(out, Launch{ID: id, Origin: start, Velocity: vec, BounceLimit: int(bl), FrameLimit: int(fl)})
	}
	return out, nil
}

func decodePlainBounds(root map[string]any, key, path string) ([2]Point, error) {
	raw, err := reqField(root, key, path)
	if err != nil {
		return [2]Point{}, err
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) != 2 {
		return [2]Point{}, decodeError("", join(path, key), "must contain two points")
	}
	var out [2]Point
	for i, rawP := range arr {
		pm, ok := rawP.(map[string]any)
		if !ok {
			return [2]Point{}, decodeError("", fmt.Sprintf("%s.%s[%d]", path, key, i), "point must be object")
		}
		p, err := pointFromPlainMM(pm, fmt.Sprintf("%s.%s[%d]", path, key, i), "")
		if err != nil {
			return [2]Point{}, err
		}
		out[i] = p
	}
	return out, nil
}

func pointFromPlainMM(m map[string]any, path, objectID string) (Point, error) {
	x, err := reqIntField(m, "x", path, objectID)
	if err != nil {
		return Point{}, err
	}
	y, err := reqIntField(m, "y", path, objectID)
	if err != nil {
		return Point{}, err
	}
	ux, err := MMToUnits(objectID, join(path, "x"), x)
	if err != nil {
		return Point{}, err
	}
	uy, err := MMToUnits(objectID, join(path, "y"), y)
	if err != nil {
		return Point{}, err
	}
	return Point{X: ux, Y: uy}, nil
}
