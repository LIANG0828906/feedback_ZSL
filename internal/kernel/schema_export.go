package kernel

import (
	"encoding/json"
	"strconv"
)

func Export(scene *Scene, targetVersion int) (*ExportResult, error) {
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	switch targetVersion {
	case SchemaV1:
		return exportV1(scene)
	case SchemaV2:
		return exportV2(scene)
	case SchemaV3:
		return exportV3(scene)
	default:
		return nil, schemaError("$.schema_version", targetVersion, "unknown export schema version")
	}
}

func lossIssue(field, id string, version int, oldValue, defaultValue, disposition string) Issue {
	return Issue{
		Code:          CodeLossy,
		Message:       "field cannot be represented losslessly in target schema",
		Field:         field,
		SourceVersion: version,
		ID:            id,
		OldValue:      oldValue,
		DefaultValue:  defaultValue,
		Disposition:   disposition,
	}
}

func worldComponent(v int64) (world int64, remainder int64, err error) {
	q, r, err := DivScalar(v, PosScale, RoundingTruncate)
	return q, r, err
}

func worldPoint(p Vec2, fieldPath, id string, version int, losses *[]Issue) (map[string]int64, error) {
	x, rx, err := worldComponent(p.X)
	if err != nil {
		return nil, err
	}
	y, ry, err := worldComponent(p.Y)
	if err != nil {
		return nil, err
	}
	if rx != 0 {
		*losses = append(*losses, lossIssue(fieldPath+".x", id, version,
			strconv.FormatInt(p.X, 10), strconv.FormatInt(x, 10),
			"truncate_fixed_point_to_world_integer_remainder="+strconv.FormatInt(rx, 10)))
	}
	if ry != 0 {
		*losses = append(*losses, lossIssue(fieldPath+".y", id, version,
			strconv.FormatInt(p.Y, 10), strconv.FormatInt(y, 10),
			"truncate_fixed_point_to_world_integer_remainder="+strconv.FormatInt(ry, 10)))
	}
	return map[string]int64{"x": x, "y": y}, nil
}

func mustWorldPoint(p Vec2) Vec2 {
	x, _, _ := worldComponent(p.X)
	y, _, _ := worldComponent(p.Y)
	return Vec2{X: x, Y: y}
}

func exportV1(scene *Scene) (*ExportResult, error) {
	const target = SchemaV1
	tr := Transcript{}
	msPerTick := scene.MSPerTick
	if msPerTick == 0 {
		msPerTick = DefaultTickMS
	}
	stepTicks, rem, err := DivScalar(scene.StepMS, msPerTick, RoundingFloor)
	if err != nil {
		return nil, err
	}
	if rem != 0 {
		tr.Losses = append(tr.Losses, lossIssue("$.step_ticks", scene.LevelID, target,
			strconv.FormatInt(scene.StepMS, 10)+"ms", strconv.FormatInt(stepTicks, 10)+"ticks",
			"floor_tick_conversion_remainder_ms="+strconv.FormatInt(rem, 10)))
	}

	gravity := mustWorldPoint(scene.Gravity)
	if gravity.X != 0 {
		tr.Losses = append(tr.Losses, lossIssue("$.gravity_x_units_per_sec2", scene.LevelID, target,
			strconv.FormatInt(gravity.X, 10), "0", "v1_supports_only_scalar_downward_gravity_drop_x"))
	}

	walls := make([]any, 0, len(scene.Segments))
	for i, seg := range scene.Segments {
		path := "$.walls[" + itoa(i) + "]"
		a, err := worldPoint(seg.A, path+".x1", seg.ID, target, &tr.Losses)
		if err != nil {
			return nil, err
		}
		b, err := worldPoint(seg.B, path+".x2", seg.ID, target, &tr.Losses)
		if err != nil {
			return nil, err
		}
		permille, rem10, err := DivScalar(seg.Restitution, 10, RoundingNearest)
		if err != nil {
			return nil, err
		}
		if rem10 != 0 {
			tr.Losses = append(tr.Losses, lossIssue(path+".bounce_permille", seg.ID, target,
				strconv.FormatInt(seg.Restitution, 10), strconv.FormatInt(permille, 10),
				"round_myriad_to_permille_half_away_remainder="+strconv.FormatInt(rem10, 10)))
		}
		tr.Losses = append(tr.Losses,
			lossIssue(path+".normal_side", seg.ID, target, seg.NormalSide, "left",
				"omit_normal_and_restore_schema_v1_default_left_on_load"),
			lossIssue(path+".penetrable", seg.ID, target, strconv.FormatBool(seg.Penetrable), "false",
				"omit_penetration_and_restore_schema_v1_default_false_on_load"),
			lossIssue(path+".material", seg.ID, target, seg.MaterialID+"@"+strconv.FormatInt(seg.MaterialVersion, 10),
				"default@1", "omit_material_and_restore_default_material_on_load"),
		)
		if scene.SchemaVersion == SchemaV2 {
			tr.Losses = append(tr.Losses, lossIssue(path+".start_delta", seg.ID, target,
				"relative_chain_delta", "absolute_x1_y1_x2_y2",
				"flatten_incremental_chain_to_absolute_endpoints"))
		}
		walls = append(walls, map[string]any{
			"wall_id":         seg.ID,
			"x1":              a["x"],
			"y1":              a["y"],
			"x2":              b["x"],
			"y2":              b["y"],
			"bounce_permille": permille,
		})
	}

	guns := make([]any, 0, len(scene.Launchers))
	for _, launcher := range scene.Launchers {
		p := mustWorldPoint(launcher.Position)
		v := mustWorldPoint(launcher.Velocity)
		guns = append(guns, map[string]any{
			"gun_id":             launcher.ID,
			"origin_x":           p.X,
			"origin_y":           p.Y,
			"velocity_x_per_sec": v.X,
			"velocity_y_per_sec": v.Y,
		})
	}
	requests := make([]any, 0, len(scene.Requests))
	for _, req := range scene.Requests {
		requests = append(requests, map[string]any{
			"request_id": req.ID,
			"frame":      req.LogicalFrame,
			"gun_id":     req.LauncherID,
			"max_frames": req.MaxFrames,
		})
	}

	doc := map[string]any{
		"schema_version":         SchemaV1,
		"level_id":               scene.LevelID,
		"ms_per_tick":            msPerTick,
		"step_ticks":             stepTicks,
		"gravity_units_per_sec2": gravity.Y,
		"walls":                  walls,
		"guns":                   guns,
		"frame_requests":         requests,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{SchemaVersion: SchemaV1, Data: data, Transcript: tr}, nil
}

func exportV2(scene *Scene) (*ExportResult, error) {
	const target = SchemaV2
	tr := Transcript{}
	gravity := mustWorldPoint(scene.Gravity)
	var previous Vec2
	segments := make([]any, 0, len(scene.Segments))
	for i, seg := range scene.Segments {
		path := "$.chain_segments[" + itoa(i) + "]"
		startDeltaInternal := Vec2{X: seg.A.X - previous.X, Y: seg.A.Y - previous.Y}
		startDelta, err := worldPoint(startDeltaInternal, path+".start_delta", seg.ID, target, &tr.Losses)
		if err != nil {
			return nil, err
		}
		vectorInternal := Vec2{X: seg.B.X - seg.A.X, Y: seg.B.Y - seg.A.Y}
		vector, err := worldPoint(vectorInternal, path+".vector_delta", seg.ID, target, &tr.Losses)
		if err != nil {
			return nil, err
		}
		previous = seg.B
		facing := "L"
		if seg.NormalSide == "right" {
			facing = "R"
		}
		segments = append(segments, map[string]any{
			"segment_id":               seg.ID,
			"start_delta":              map[string]int64{"dx": startDelta["x"], "dy": startDelta["y"]},
			"vector_delta":             map[string]int64{"dx": vector["x"], "dy": vector["y"]},
			"normal_facing":            facing,
			"restitution_basis_points": seg.Restitution,
			"pass_through":             seg.Penetrable,
			"material_id":              seg.MaterialID,
			"material_version":         seg.MaterialVersion,
		})
	}

	launchers := make([]any, 0, len(scene.Launchers))
	for _, launcher := range scene.Launchers {
		p := mustWorldPoint(launcher.Position)
		v := mustWorldPoint(launcher.Velocity)
		launchers = append(launchers, map[string]any{
			"launch_id": launcher.ID,
			"position":  map[string]int64{"x": p.X, "y": p.Y},
			"velocity":  map[string]int64{"x_per_sec": v.X, "y_per_sec": v.Y},
		})
	}
	queue := make([]any, 0, len(scene.Requests))
	for i, req := range scene.Requests {
		queue = append(queue, map[string]any{
			"request_id":    req.ID,
			"sequence":      i,
			"logical_frame": req.LogicalFrame,
			"launch_id":     req.LauncherID,
			"max_frames":    req.MaxFrames,
		})
	}

	doc := map[string]any{
		"schema_version": SchemaV2,
		"level_id":       scene.LevelID,
		"timing":         map[string]int64{"step_milliseconds": scene.StepMS},
		"physics": map[string]any{
			"gravity_units_per_sec_sq": map[string]int64{"x": gravity.X, "y": gravity.Y},
		},
		"chain_segments":   segments,
		"launch_table":     launchers,
		"prediction_queue": queue,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{SchemaVersion: SchemaV2, Data: data, Transcript: tr}, nil
}

func exportV3(scene *Scene) (*ExportResult, error) {
	const target = SchemaV3
	tr := Transcript{}
	gravity := mustWorldPoint(scene.Gravity)
	obstacles := make([]any, 0, len(scene.Segments))
	for i, seg := range scene.Segments {
		path := "$.obstacles[" + itoa(i) + "]"
		a, err := worldPoint(seg.A, path+".endpoints.a", seg.ID, target, &tr.Losses)
		if err != nil {
			return nil, err
		}
		b, err := worldPoint(seg.B, path+".endpoints.b", seg.ID, target, &tr.Losses)
		if err != nil {
			return nil, err
		}
		side := seg.NormalSide
		if side == "" {
			side = "left"
		}
		obstacles = append(obstacles, map[string]any{
			"id": seg.ID,
			"endpoints": map[string]any{
				"a": map[string]int64{"x": a["x"], "y": a["y"]},
				"b": map[string]int64{"x": b["x"], "y": b["y"]},
			},
			"normal":      map[string]string{"side": side},
			"restitution": map[string]int64{"per_ten_thousand": seg.Restitution},
			"penetration": map[string]any{
				"allowed":         seg.Penetrable,
				"max_depth_units": seg.PenetrationDepth / PosScale,
			},
			"material": map[string]any{"id": seg.MaterialID, "version": seg.MaterialVersion},
		})
	}
	launchers := make([]any, 0, len(scene.Launchers))
	for _, launcher := range scene.Launchers {
		p := mustWorldPoint(launcher.Position)
		v := mustWorldPoint(launcher.Velocity)
		launchers = append(launchers, map[string]any{
			"id":       launcher.ID,
			"position": map[string]int64{"x": p.X, "y": p.Y},
			"velocity": map[string]int64{"x": v.X, "y": v.Y},
		})
	}
	requests := make([]any, 0, len(scene.Requests))
	for _, req := range scene.Requests {
		requests = append(requests, map[string]any{
			"id":            req.ID,
			"logical_frame": req.LogicalFrame,
			"launcher_id":   req.LauncherID,
			"max_frames":    req.MaxFrames,
		})
	}
	doc := map[string]any{
		"schema_version": SchemaV3,
		"level_id":       scene.LevelID,
		"time_step_ms":   scene.StepMS,
		"gravity":        map[string]int64{"x": gravity.X, "y": gravity.Y},
		"obstacles":      obstacles,
		"launchers":      launchers,
		"requests":       requests,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return &ExportResult{SchemaVersion: SchemaV3, Data: data, Transcript: tr}, nil
}
