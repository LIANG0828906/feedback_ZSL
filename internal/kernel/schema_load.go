package kernel

import (
	"encoding/json"
	"strconv"
)

func LoadDocument(data []byte) (*LoadResult, error) {
	root, err := decodeRootObject(data, 0)
	if err != nil {
		return nil, err
	}
	versionRaw, err := field(root, "schema_version", "$.schema_version", 0)
	if err != nil {
		return nil, err
	}
	version, err := asInt(versionRaw, "$.schema_version", 0)
	if err != nil {
		return nil, err
	}
	if version < SchemaV1 || version > SchemaV3 {
		return nil, schemaError("$.schema_version", int(version), "unknown schema version %d", version)
	}

	switch version {
	case SchemaV1:
		return loadV1(root)
	case SchemaV2:
		return loadV2(root)
	default:
		return loadV3(root)
	}
}

func loadV1(root map[string]json.RawMessage) (*LoadResult, error) {
	const v = SchemaV1
	transcript := Transcript{}
	levelID, err := requiredString(root, "level_id", "$.level_id", v)
	if err != nil {
		return nil, err
	}
	msPerTick, err := requiredInt(root, "ms_per_tick", "$.ms_per_tick", v)
	if err != nil {
		return nil, err
	}
	if msPerTick <= 0 {
		return nil, parameterError("$.ms_per_tick", levelID, v, "must be positive")
	}
	stepTicks, err := requiredInt(root, "step_ticks", "$.step_ticks", v)
	if err != nil {
		return nil, err
	}
	if stepTicks <= 0 {
		return nil, parameterError("$.step_ticks", levelID, v, "must be positive")
	}
	gravity, err := requiredInt(root, "gravity_units_per_sec2", "$.gravity_units_per_sec2", v)
	if err != nil {
		return nil, err
	}
	gravityScaled, err := ScaleWorld(gravity, "$.gravity_units_per_sec2", levelID, v)
	if err != nil {
		return nil, err
	}

	wallRaws, err := requiredArray(root, "walls", "$", v)
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0, len(wallRaws))
	for i, raw := range wallRaws {
		path := "$.walls[" + itoa(i) + "]"
		obj, err := asObject(raw, path, v)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "wall_id", path+".wall_id", v)
		if err != nil {
			return nil, err
		}
		x1, err := requiredInt(obj, "x1", path+".x1", v)
		if err != nil {
			return nil, err
		}
		y1, err := requiredInt(obj, "y1", path+".y1", v)
		if err != nil {
			return nil, err
		}
		x2, err := requiredInt(obj, "x2", path+".x2", v)
		if err != nil {
			return nil, err
		}
		y2, err := requiredInt(obj, "y2", path+".y2", v)
		if err != nil {
			return nil, err
		}
		bounce, err := requiredInt(obj, "bounce_permille", path+".bounce_permille", v)
		if err != nil {
			return nil, err
		}
		if bounce < 0 || bounce > 1000 {
			return nil, parameterError(path+".bounce_permille", id, v, "permille restitution must be in [0,1000]")
		}
		restitution, ok := PermilleToMyriad(bounce)
		if !ok {
			return nil, overflowError(path+".bounce_permille", id, v, bounce, 10)
		}
		a, err := scaledWorldXY(Vec2{X: x1, Y: y1}, path+".start", id, v)
		if err != nil {
			return nil, err
		}
		b, err := scaledWorldXY(Vec2{X: x2, Y: y2}, path+".end", id, v)
		if err != nil {
			return nil, err
		}
		normalPath := path + ".normal_side"
		transcript.Fills = append(transcript.Fills,
			fillTrace(normalPath, "left", "schema_v1_declared_default:left_computed_segment_normal", v))
		penPath := path + ".penetrable"
		transcript.Fills = append(transcript.Fills,
			fillTrace(penPath, "false", "schema_v1_declared_default:impenetrable", v))

		segments = append(segments, Segment{
			ID:               id,
			MaterialID:       "default",
			MaterialVersion:  1,
			A:                a,
			B:                b,
			NormalSide:       "left",
			Restitution:      restitution,
			Penetrable:       false,
			PenetrationDepth: 0,
		})
	}

	launchers, err := loadV1Launchers(root, v)
	if err != nil {
		return nil, err
	}
	requests, err := loadV1Requests(root, v)
	if err != nil {
		return nil, err
	}

	stepMS, ok := TicksToMS(stepTicks, msPerTick)
	if !ok {
		return nil, overflowError("$.step_ticks", levelID, v, stepTicks, msPerTick)
	}
	scene := &Scene{
		SchemaVersion:    v,
		LevelID:          levelID,
		Width:            DefaultWidthUnits,
		Height:           DefaultHeightUnits,
		MSPerTick:        msPerTick,
		StepTicks:        stepTicks,
		StepMS:           stepMS,
		Gravity:          Vec2{X: 0, Y: gravityScaled},
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

func loadV1Launchers(root map[string]json.RawMessage, version int) ([]Launcher, error) {
	raws, err := requiredArray(root, "guns", "$", version)
	if err != nil {
		return nil, err
	}
	result := make([]Launcher, 0, len(raws))
	for i, raw := range raws {
		path := "$.guns[" + itoa(i) + "]"
		obj, err := asObject(raw, path, version)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "gun_id", path+".gun_id", version)
		if err != nil {
			return nil, err
		}
		x, err := requiredInt(obj, "origin_x", path+".origin_x", version)
		if err != nil {
			return nil, err
		}
		y, err := requiredInt(obj, "origin_y", path+".origin_y", version)
		if err != nil {
			return nil, err
		}
		vx, err := requiredInt(obj, "velocity_x_per_sec", path+".velocity_x_per_sec", version)
		if err != nil {
			return nil, err
		}
		vy, err := requiredInt(obj, "velocity_y_per_sec", path+".velocity_y_per_sec", version)
		if err != nil {
			return nil, err
		}
		pos, err := scaledWorldXY(Vec2{X: x, Y: y}, path+".origin", id, version)
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

func loadV1Requests(root map[string]json.RawMessage, version int) ([]PredictionRequest, error) {
	raws, err := requiredArray(root, "frame_requests", "$", version)
	if err != nil {
		return nil, err
	}
	result := make([]PredictionRequest, 0, len(raws))
	for i, raw := range raws {
		path := "$.frame_requests[" + itoa(i) + "]"
		obj, err := asObject(raw, path, version)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "request_id", path+".request_id", version)
		if err != nil {
			return nil, err
		}
		launcherID, err := requiredString(obj, "gun_id", path+".gun_id", version)
		if err != nil {
			return nil, err
		}
		frame, err := requiredInt(obj, "frame", path+".frame", version)
		if err != nil {
			return nil, err
		}
		maxFrames, _ := optionalField(obj, "max_frames")
		max := int(DefaultMaxFrames)
		if maxFrames != nil {
			value, err := asInt(maxFrames, path+".max_frames", version)
			if err != nil {
				return nil, err
			}
			max = int(value)
		}
		result = append(result, PredictionRequest{ID: id, LogicalFrame: frame, LauncherID: launcherID, MaxFrames: max})
	}
	return result, nil
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
