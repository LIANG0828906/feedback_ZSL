package kernel

import "encoding/json"

func loadV3(root map[string]json.RawMessage) (*LoadResult, error) {
	const v = SchemaV3
	transcript := Transcript{}
	levelID, err := requiredString(root, "level_id", "$.level_id", v)
	if err != nil {
		return nil, err
	}
	stepMS, err := requiredInt(root, "time_step_ms", "$.time_step_ms", v)
	if err != nil {
		return nil, err
	}
	if stepMS <= 0 {
		return nil, parameterError("$.time_step_ms", levelID, v, "must be positive")
	}
	gravityObj, err := requiredObject(root, "gravity", "$", v)
	if err != nil {
		return nil, err
	}
	gravityWorld, err := requiredXY(gravityObj, "$.gravity", v)
	if err != nil {
		return nil, err
	}
	gravity, err := scaledWorldXY(gravityWorld, "$.gravity", levelID, v)
	if err != nil {
		return nil, err
	}

	obstacleRaws, err := requiredArray(root, "obstacles", "$", v)
	if err != nil {
		return nil, err
	}
	segments := make([]Segment, 0, len(obstacleRaws))
	for i, raw := range obstacleRaws {
		path := "$.obstacles[" + itoa(i) + "]"
		obj, err := asObject(raw, path, v)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "id", path+".id", v)
		if err != nil {
			return nil, err
		}
		endpoints, err := requiredObject(obj, "endpoints", path, v)
		if err != nil {
			return nil, err
		}
		aRaw, err := field(endpoints, "a", path+".endpoints.a", v)
		if err != nil {
			return nil, err
		}
		aObj, err := asObject(aRaw, path+".endpoints.a", v)
		if err != nil {
			return nil, err
		}
		bRaw, err := field(endpoints, "b", path+".endpoints.b", v)
		if err != nil {
			return nil, err
		}
		bObj, err := asObject(bRaw, path+".endpoints.b", v)
		if err != nil {
			return nil, err
		}
		aWorld, err := requiredXY(aObj, path+".endpoints.a", v)
		if err != nil {
			return nil, err
		}
		bWorld, err := requiredXY(bObj, path+".endpoints.b", v)
		if err != nil {
			return nil, err
		}
		a, err := scaledWorldXY(aWorld, path+".endpoints.a", id, v)
		if err != nil {
			return nil, err
		}
		b, err := scaledWorldXY(bWorld, path+".endpoints.b", id, v)
		if err != nil {
			return nil, err
		}

		normalObj, err := requiredObject(obj, "normal", path, v)
		if err != nil {
			return nil, err
		}
		sideText, err := requiredString(normalObj, "side", path+".normal.side", v)
		if err != nil {
			return nil, err
		}
		side := ""
		switch sideText {
		case "left", "L":
			side = "left"
		case "right", "R":
			side = "right"
		default:
			return nil, parameterError(path+".normal.side", id, v, "normal side must be left/right")
		}

		restitutionObj, err := requiredObject(obj, "restitution", path, v)
		if err != nil {
			return nil, err
		}
		restitution, err := requiredInt(restitutionObj, "per_ten_thousand",
			path+".restitution.per_ten_thousand", v)
		if err != nil {
			return nil, err
		}
		if restitution < 0 || restitution > CoeffScale {
			return nil, parameterError(path+".restitution.per_ten_thousand", id, v,
				"restitution must be in [0,%d]", CoeffScale)
		}

		penetrable := false
		depth := int64(0)
		if penRaw, ok := optionalField(obj, "penetration"); ok {
			penObj, err := asObject(penRaw, path+".penetration", v)
			if err != nil {
				return nil, err
			}
			if allowedRaw, exists := optionalField(penObj, "allowed"); exists {
				penetrable, err = asBool(allowedRaw, path+".penetration.allowed", v)
				if err != nil {
					return nil, err
				}
			} else {
				transcript.Fills = append(transcript.Fills,
					fillTrace(path+".penetration.allowed", "false", "schema_v3_declared_default:impenetrable", v))
			}
			if depthRaw, exists := optionalField(penObj, "max_depth_units"); exists {
				depth, err = asInt(depthRaw, path+".penetration.max_depth_units", v)
				if err != nil {
					return nil, err
				}
			}
		} else {
			transcript.Fills = append(transcript.Fills,
				fillTrace(path+".penetration", "{allowed:false}", "schema_v3_declared_default:impenetrable", v))
		}

		materialID := "default"
		materialVersion := int64(1)
		if materialRaw, ok := optionalField(obj, "material"); ok {
			materialObj, err := asObject(materialRaw, path+".material", v)
			if err != nil {
				return nil, err
			}
			if idRaw, exists := optionalField(materialObj, "id"); exists {
				materialID, err = asString(idRaw, path+".material.id", v)
				if err != nil {
					return nil, err
				}
			}
			if versionRaw, exists := optionalField(materialObj, "version"); exists {
				materialVersion, err = asInt(versionRaw, path+".material.version", v)
				if err != nil {
					return nil, err
				}
			}
		}

		segments = append(segments, Segment{
			ID:               id,
			MaterialID:       materialID,
			MaterialVersion:  materialVersion,
			A:                a,
			B:                b,
			NormalSide:       side,
			Restitution:      restitution,
			Penetrable:       penetrable,
			PenetrationDepth: depth * PosScale,
		})
	}

	launchers, err := loadV3Launchers(root, v)
	if err != nil {
		return nil, err
	}
	requests, err := loadV3Requests(root, v)
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

func loadV3Launchers(root map[string]json.RawMessage, version int) ([]Launcher, error) {
	raws, err := requiredArray(root, "launchers", "$", version)
	if err != nil {
		return nil, err
	}
	result := make([]Launcher, 0, len(raws))
	for i, raw := range raws {
		path := "$.launchers[" + itoa(i) + "]"
		obj, err := asObject(raw, path, version)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "id", path+".id", version)
		if err != nil {
			return nil, err
		}
		posObj, err := requiredObject(obj, "position", path, version)
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
		velObj, err := requiredObject(obj, "velocity", path, version)
		if err != nil {
			return nil, err
		}
		velWorld, err := requiredXY(velObj, path+".velocity", version)
		if err != nil {
			return nil, err
		}
		vel, err := scaledWorldXY(velWorld, path+".velocity", id, version)
		if err != nil {
			return nil, err
		}
		result = append(result, Launcher{ID: id, Position: pos, Velocity: vel})
	}
	return result, nil
}

func loadV3Requests(root map[string]json.RawMessage, version int) ([]PredictionRequest, error) {
	raws, err := requiredArray(root, "requests", "$", version)
	if err != nil {
		return nil, err
	}
	result := make([]PredictionRequest, 0, len(raws))
	for i, raw := range raws {
		path := "$.requests[" + itoa(i) + "]"
		obj, err := asObject(raw, path, version)
		if err != nil {
			return nil, err
		}
		id, err := requiredString(obj, "id", path+".id", version)
		if err != nil {
			return nil, err
		}
		launcherID, err := requiredString(obj, "launcher_id", path+".launcher_id", version)
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
