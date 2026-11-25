package kernel

import "encoding/json"

func loadV2(root map[string]json.RawMessage) (*LoadResult, error) {
	const v = SchemaV2
	transcript := Transcript{}
	levelID, err := requiredString(root, "level_id", "$.level_id", v)
	if err != nil {
		return nil, err
	}
	timing, err := requiredObject(root, "timing", "$", v)
	if err != nil {
		return nil, err
	}
	stepMS, err := requiredInt(timing, "step_milliseconds", "$.timing.step_milliseconds", v)
	if err != nil {
		return nil, err
	}
	if stepMS <= 0 {
		return nil, parameterError("$.timing.step_milliseconds", levelID, v, "must be positive")
	}
	physics, err := requiredObject(root, "physics", "$", v)
	if err != nil {
		return nil, err
	}
	gravityRaw, err := field(physics, "gravity_units_per_sec_sq", "$.physics.gravity_units_per_sec_sq", v)
	if err != nil {
		return nil, err
	}
	gravityObj, err := asObject(gravityRaw, "$.physics.gravity_units_per_sec_sq", v)
	if err != nil {
		return nil, err
	}
	gravityWorld, err := requiredXY(gravityObj, "$.physics.gravity_units_per_sec_sq", v)
	if err != nil {
		return nil, err
	}
	gravity, err := scaledWorldXY(gravityWorld, "$.physics.gravity_units_per_sec_sq", levelID, v)
	if err != nil {
		return nil, err
	}

	rawSegments, err := requiredArray(root, "chain_segments", "$", v)
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0, len(rawSegments))
	var previousEnd Vec2
	for i, raw := range rawSegments {
		path := "$.chain_segments[" + itoa(i) + "]"
		obj, err := asObject(raw, path, v)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "segment_id", path+".segment_id", v)
		if err != nil {
			return nil, err
		}
		startDeltaRaw, err := field(obj, "start_delta", path+".start_delta", v)
		if err != nil {
			return nil, err
		}
		startDeltaObj, err := asObject(startDeltaRaw, path+".start_delta", v)
		if err != nil {
			return nil, err
		}
		startDX, err := requiredInt(startDeltaObj, "dx", path+".start_delta.dx", v)
		if err != nil {
			return nil, err
		}
		startDY, err := requiredInt(startDeltaObj, "dy", path+".start_delta.dy", v)
		if err != nil {
			return nil, err
		}
		startDeltaWorld := Vec2{X: startDX, Y: startDY}
		if err != nil {
			return nil, err
		}
		startDelta, err := scaledWorldXY(startDeltaWorld, path+".start_delta", id, v)
		if err != nil {
			return nil, err
		}
		startX, err := CheckedAddField(previousEnd.X, startDelta.X, path+".start_delta.dx", id, v)
		if err != nil {
			return nil, err
		}
		startY, err := CheckedAddField(previousEnd.Y, startDelta.Y, path+".start_delta.dy", id, v)
		if err != nil {
			return nil, err
		}
		start := Vec2{X: startX, Y: startY}

		vectorRaw, err := field(obj, "vector_delta", path+".vector_delta", v)
		if err != nil {
			return nil, err
		}
		vectorObj, err := asObject(vectorRaw, path+".vector_delta", v)
		if err != nil {
			return nil, err
		}
		vectorDX, err := requiredInt(vectorObj, "dx", path+".vector_delta.dx", v)
		if err != nil {
			return nil, err
		}
		vectorDY, err := requiredInt(vectorObj, "dy", path+".vector_delta.dy", v)
		if err != nil {
			return nil, err
		}
		vectorWorld := Vec2{X: vectorDX, Y: vectorDY}
		if err != nil {
			return nil, err
		}
		vector, err := scaledWorldXY(vectorWorld, path+".vector_delta", id, v)
		if err != nil {
			return nil, err
		}
		endX, err := CheckedAddField(start.X, vector.X, path+".vector_delta.x", id, v)
		if err != nil {
			return nil, err
		}
		endY, err := CheckedAddField(start.Y, vector.Y, path+".vector_delta.y", id, v)
		if err != nil {
			return nil, err
		}
		end := Vec2{X: endX, Y: endY}
		previousEnd = end

		facing, err := requiredString(obj, "normal_facing", path+".normal_facing", v)
		if err != nil {
			return nil, err
		}
		side := ""
		switch facing {
		case "L", "left":
			side = "left"
		case "R", "right":
			side = "right"
		default:
			return nil, parameterError(path+".normal_facing", id, v, "normal facing must be L/R")
		}

		restitution, err := requiredInt(obj, "restitution_basis_points", path+".restitution_basis_points", v)
		if err != nil {
			return nil, err
		}
		if restitution < 0 || restitution > CoeffScale {
			return nil, parameterError(path+".restitution_basis_points", id, v,
				"basis-point restitution must be in [0,%d]", CoeffScale)
		}

		penetrable := false
		if rawPen, ok := optionalField(obj, "pass_through"); ok {
			penetrable, err = asBool(rawPen, path+".pass_through", v)
			if err != nil {
				return nil, err
			}
		} else {
			transcript.Fills = append(transcript.Fills,
				fillTrace(path+".pass_through", "false", "schema_v2_declared_default:impenetrable", v))
		}

		materialID := "default"
		materialVersion := int64(1)
		if rawMaterial, ok := optionalField(obj, "material_id"); ok {
			materialID, err = asString(rawMaterial, path+".material_id", v)
			if err != nil {
				return nil, err
			}
		}
		if rawVersion, ok := optionalField(obj, "material_version"); ok {
			materialVersion, err = asInt(rawVersion, path+".material_version", v)
			if err != nil {
				return nil, err
			}
		}

		segments = append(segments, Segment{
			ID:               id,
			MaterialID:       materialID,
			MaterialVersion:  materialVersion,
			A:                start,
			B:                end,
			NormalSide:       side,
			Restitution:      restitution,
			Penetrable:       penetrable,
			PenetrationDepth: 0,
		})
	}

	launchers, err := loadV2Launchers(root, v)
	if err != nil {
		return nil, err
	}
	requests, err := loadV2Requests(root, v)
	if err != nil {
		return nil, err
	}

	scene := &Scene{
		SchemaVersion:    v,
		LevelID:          levelID,
		Width:            DefaultWidthUnits,
		Height:           DefaultHeightUnits,
		MSPerTick:        DefaultTickMS,
		StepTicks:        stepMS / DefaultTickMS,
		StepMS:           stepMS,
		Gravity:          gravity,
		Segments:         segments,
		Launchers:        launchers,
		Requests:         requests,
		Versions:         SemanticVersions{Endpoints: 1, Normals: 1, Restitutions: 1, Gravity: 1, TimeStep: 1, Materials: 1},
		StructureVersion: 1,
	}
	if err := PrepareScene(scene); err != nil {
		return nil, err
	}
	return &LoadResult{Scene: scene, Transcript: transcript}, nil
}

func loadV2Launchers(root map[string]json.RawMessage, version int) ([]Launcher, error) {
	raws, err := requiredArray(root, "launch_table", "$", version)
	if err != nil {
		return nil, err
	}
	result := make([]Launcher, 0, len(raws))
	for i, raw := range raws {
		path := "$.launch_table[" + itoa(i) + "]"
		obj, err := asObject(raw, path, version)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "launch_id", path+".launch_id", version)
		if err != nil {
			return nil, err
		}
		posRaw, err := field(obj, "position", path+".position", version)
		if err != nil {
			return nil, err
		}
		posObj, err := asObject(posRaw, path+".position", version)
		if err != nil {
			return nil, err
		}
		posWorld, err := requiredXY(posObj, path+".position", version)
		if err != nil {
			return nil, err
		}
		pos, err := scaledWorldXY(posWorld, path+".position", id, version)
		if err != nil {
			return nil, err
		}
		velRaw, err := field(obj, "velocity", path+".velocity", version)
		if err != nil {
			return nil, err
		}
		velObj, err := asObject(velRaw, path+".velocity", version)
		if err != nil {
			return nil, err
		}
		vx, err := requiredInt(velObj, "x_per_sec", path+".velocity.x_per_sec", version)
		if err != nil {
			return nil, err
		}
		vy, err := requiredInt(velObj, "y_per_sec", path+".velocity.y_per_sec", version)
		if err != nil {
			return nil, err
		}
		vel, err := scaledWorldXY(Vec2{X: vx, Y: vy}, path+".velocity", id, version)
		if err != nil {
			return nil, err
		}
		result = append(result, Launcher{ID: id, Position: pos, Velocity: vel})
	}
	return result, nil
}

func loadV2Requests(root map[string]json.RawMessage, version int) ([]PredictionRequest, error) {
	raws, err := requiredArray(root, "prediction_queue", "$", version)
	if err != nil {
		return nil, err
	}
	result := make([]PredictionRequest, 0, len(raws))
	for i, raw := range raws {
		path := "$.prediction_queue[" + itoa(i) + "]"
		obj, err := asObject(raw, path, version)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "request_id", path+".request_id", version)
		if err != nil {
			return nil, err
		}
		launcherID, err := requiredString(obj, "launch_id", path+".launch_id", version)
		if err != nil {
			return nil, err
		}
		frame, err := requiredInt(obj, "logical_frame", path+".logical_frame", version)
		if err != nil {
			return nil, err
		}
		maxFrames := int(DefaultMaxFrames)
		if rawMax, ok := optionalField(obj, "max_frames"); ok {
			maxValue, err := asInt(rawMax, path+".max_frames", version)
			if err != nil {
				return nil, err
			}
			maxFrames = int(maxValue)
		}
		result = append(result, PredictionRequest{ID: id, LogicalFrame: frame, LauncherID: launcherID, MaxFrames: maxFrames})
	}
	return result, nil
}

var _ = json.RawMessage(nil)
