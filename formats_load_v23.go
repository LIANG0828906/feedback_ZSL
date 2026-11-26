package bonerig

func parsePrecision(root map[string]any, data []byte) (PrecisionParams, int64, *Error) {
	p := DefaultPrecision()
	if raw, ok := root["precision"]; ok {
		if err := remarshalInto(raw, &p); err != nil {
			return p, 0, err
		}
	}
	distScale := p.DistanceScale
	if fp, ok := root["fixed_point"].(map[string]any); ok {
		if v, exists, err := optInt(fp, "distance_scale"); err != nil {
			return p, 0, err
		} else if exists {
			distScale = v
			p.DistanceScale = v
		}
	}
	if distScale == 0 {
		distScale = 1_000_000
		p.DistanceScale = distScale
	}
	if p.WorkScale == 0 {
		p.WorkScale = DefaultPrecision().WorkScale
	}
	if p.IntervalFrames == 0 {
		p.IntervalFrames = DefaultPrecision().IntervalFrames
	}
	return p, distScale, nil
}

func parseRotScale(root map[string]any, fallback int64) int64 {
	if fp, ok := root["fixed_point"].(map[string]any); ok {
		if v, exists, oerr := optInt(fp, "rotation_scale"); oerr == nil && exists && v > 0 {
			return v
		}
	}
	return fallback
}

func loadV2(root map[string]any, data []byte) (LoadResult, *Error) {
	traces := []TransformTrace{{RuleVersion: RuleVersion, Direction: "load", FieldPath: "$.schema", OldValue: V2Schema, NewValue: "unified-model"}}
	normTraces := []QuatNormTrace{}
	seed, err := reqInt(root, "seed", "$")
	if err != nil {
		return LoadResult{}, err
	}
	prec, distScale, perr := parsePrecision(root, data)
	if perr != nil {
		return LoadResult{}, perr
	}
	rotScale := parseRotScale(root, V2QuatScale)

	armature, err := reqMap(root, "armature", "$")
	if err != nil {
		return LoadResult{}, err
	}
	armID := reqStringLoose(armature, "id")
	nodesArr, err := reqSlice(armature, "nodes", "$.armature")
	if err != nil {
		return LoadResult{}, err
	}
	joints := []Joint{}
	for i, item := range nodesArr {
		path := "$.armature.nodes" + indexPath("", i)
		n, nerr := asMapAt(item, path)
		if nerr != nil {
			return LoadResult{}, nerr
		}
		id, nerr := reqString(n, "id", path)
		if nerr != nil {
			return LoadResult{}, nerr
		}
		parent, _, _ := optString(n, "parent_id")
		restRaw, nerr := reqIntArray(n["rest_position"], path+".rest_position", 3)
		if nerr != nil {
			return LoadResult{}, nerr
		}
		bindRaw, nerr := reqIntArray(n["bind_q_wxyz"], path+".bind_q_wxyz", 4)
		if nerr != nil {
			return LoadResult{}, nerr
		}
		bind, _, tr, qerr := NormalizeQuat(quatFromOrder(bindRaw, rotScale, orderWXYZ), prec, id, -1)
		if qerr != nil {
			return LoadResult{}, qerr
		}
		normTraces = append(normTraces, tr)
		c, cerr := parseRadConstraint(n, "constraint_rad", path, &traces)
		if cerr != nil {
			return LoadResult{}, cerr
		}
		joints = append(joints, Joint{ID: id, ParentID: parent, RestTranslation: vecFromArray(restRaw, distScale), BindRotation: bind, Constraint: c})
		traces = append(traces, TransformTrace{RuleVersion: RuleVersion, Direction: "load", FieldPath: path + ".id", OldValue: id, NewValue: "skeleton.joints[].id", Reason: "v2 field alias"})
	}

	anim, err := reqMap(root, "animation", "$")
	if err != nil {
		return LoadResult{}, err
	}
	clipID, err := reqString(anim, "clip_id", "$.animation")
	if err != nil {
		return LoadResult{}, err
	}
	tickRate, err := reqInt(anim, "ticks_per_second", "$.animation")
	if err != nil {
		return LoadResult{}, err
	}
	frameArr, err := reqSlice(anim, "frames", "$.animation")
	if err != nil {
		return LoadResult{}, err
	}
	frames := []Frame{}
	for fi, item := range frameArr {
		path := "$.animation.frames" + indexPath("", fi)
		fm, ferr := asMapAt(item, path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		ms, ferr := reqInt(fm, "at_ms", path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		rootRaw, ferr := reqIntArray(fm["root_position"], path+".root_position", 3)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		rotMap, ferr := reqMap(fm, "rotations", path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		poses := map[string]JointPose{}
		for _, j := range joints {
			raw, ok := rotMap[j.ID]
			if !ok {
				e := errorf(CodeSchema, "missing required joint rotation")
				e.Field = path + ".rotations." + j.ID
				return LoadResult{}, e
			}
			arr, aerr := reqIntArray(raw, path+".rotations."+j.ID, 4)
			if aerr != nil {
				return LoadResult{}, aerr
			}
			q, _, tr, qerr := NormalizeQuat(quatFromOrder(arr, rotScale, orderWXYZ), prec, j.ID, fi)
			if qerr != nil {
				return LoadResult{}, qerr
			}
			normTraces = append(normTraces, tr)
			poses[j.ID] = JointPose{Rotation: q}
		}
		tick := int64(0)
		if tickRate > 0 {
			tick = ms * tickRate / 1000
		}
		frames = append(frames, Frame{Index: fi, Tick: tick, TimeMS: ms, RootTranslation: vecFromArray(rootRaw, distScale), Poses: poses})
		traces = append(traces, TransformTrace{RuleVersion: RuleVersion, Direction: "load", FieldPath: path + ".at_ms", OldValue: ms, NewValue: tick, Reason: "millisecond to canonical tick trace"})
	}

	target, terr := parseV2Target(root, distScale)
	if terr != nil {
		return LoadResult{}, terr
	}
	mapping, merr := parseV2Mapping(root, &traces)
	if merr != nil {
		return LoadResult{}, merr
	}
	props := ProportionSet{}
	if raw, ok := root["proportion_decl"]; ok {
		if rerr := remarshalInto(raw, &props); rerr != nil {
			return LoadResult{}, rerr
		}
	}
	if props.DistanceScale == 0 {
		props.DistanceScale = distScale
	}
	a := Asset{
		Seed: seed, SourceFormat: V2Schema, SourceVersion: "v2", SchemaVersion: V2Schema,
		Skeleton: Skeleton{ID: armID, Version: V2Schema, Joints: joints}, TargetSkeleton: target,
		Clip: Clip{ID: clipID, Frames: frames, TickRate: tickRate}, Mapping: mapping,
		Proportions: props, Precision: prec,
	}
	return finishAsset(&a, traces, normTraces), nil
}

func parseV2Target(root map[string]any, distScale int64) (Skeleton, *Error) {
	raw, ok := root["target_armature"]
	if !ok {
		return Skeleton{}, nil
	}
	m, err := asMapAt(raw, "$.target_armature")
	if err != nil {
		return Skeleton{}, err
	}
	arr, err := reqSlice(m, "nodes", "$.target_armature")
	if err != nil {
		return Skeleton{}, err
	}
	joints := []Joint{}
	for i, item := range arr {
		path := "$.target_armature.nodes" + indexPath("", i)
		n, nerr := asMapAt(item, path)
		if nerr != nil {
			return Skeleton{}, nerr
		}
		id, nerr := reqString(n, "id", path)
		if nerr != nil {
			return Skeleton{}, nerr
		}
		parent, _, _ := optString(n, "parent_id")
		rest, nerr := reqIntArray(n["rest_position"], path+".rest_position", 3)
		if nerr != nil {
			return Skeleton{}, nerr
		}
		joints = append(joints, Joint{ID: id, ParentID: parent, RestTranslation: vecFromArray(rest, distScale)})
	}
	return Skeleton{ID: reqStringLoose(m, "id"), Joints: joints}, nil
}

func parseV2Mapping(root map[string]any, traces *[]TransformTrace) (Mapping, *Error) {
	arr, err := reqSlice(root, "retarget_links", "$")
	if err != nil {
		return Mapping{}, err
	}
	items := []MapItem{}
	for i, item := range arr {
		path := "$.retarget_links" + indexPath("", i)
		m, merr := asMapAt(item, path)
		if merr != nil {
			return Mapping{}, merr
		}
		from, merr := reqString(m, "source", path)
		if merr != nil {
			return Mapping{}, merr
		}
		to, merr := reqString(m, "target", path)
		if merr != nil {
			return Mapping{}, merr
		}
		weightScale := int64(1000)
		if v, ok, oerr := optInt(m, "weight_scale"); oerr != nil {
			return Mapping{}, oerr
		} else if ok {
			weightScale = v
		}
		mi := MapItem{SourceJoint: from, TargetJoint: to, WeightScale: weightScale, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1}
		if rawInf, ok := m["influences"]; ok {
			if ierr := remarshalInto(rawInf, &mi.ExtraSources); ierr != nil {
				return Mapping{}, ierr
			}
		}
		if v, ok, oerr := optInt(m, "rot_scale_pmille"); oerr != nil {
			return Mapping{}, oerr
		} else if ok {
			mi.RotScaleNum = v
			mi.RotScaleDen = 1000
		} else {
			mi.ScaleDefaultFilled = true
			*traces = append(*traces, TransformTrace{RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + ".rot_scale_pmille", Default: 1000, Reason: "optional retarget coefficient absent"})
		}
		if v, ok, oerr := optInt(m, "trans_scale_pmille"); oerr != nil {
			return Mapping{}, oerr
		} else if ok {
			mi.TransScaleNum = v
			mi.TransScaleDen = 1000
		}
		items = append(items, mi)
	}
	return Mapping{Version: "mapping-v2", Items: items}, nil
}

func loadV3(root map[string]any, data []byte) (LoadResult, *Error) {
	traces := []TransformTrace{{RuleVersion: RuleVersion, Direction: "load", FieldPath: "$.schema", OldValue: V3Schema, NewValue: "unified-model"}}
	normTraces := []QuatNormTrace{}
	seed, err := reqInt(root, "seed", "$")
	if err != nil {
		return LoadResult{}, err
	}
	prec, distScale, perr := parsePrecision(root, data)
	if perr != nil {
		return LoadResult{}, perr
	}
	rotScale := parseRotScale(root, V3QuatScale)
	rig, err := reqMap(root, "rig_tree", "$")
	if err != nil {
		return LoadResult{}, err
	}
	rootsArr, err := reqSlice(rig, "roots", "$.rig_tree")
	if err != nil {
		return LoadResult{}, err
	}
	joints := []Joint{}
	var parseNode func(item any, path, parent string) *Error
	parseNode = func(item any, path, parent string) *Error {
		n, nerr := asMapAt(item, path)
		if nerr != nil {
			return nerr
		}
		id, nerr := reqString(n, "joint_id", path)
		if nerr != nil {
			return nerr
		}
		restRaw, nerr := reqIntArray(n["rest"], path+".rest", 3)
		if nerr != nil {
			return nerr
		}
		bindMap, nerr := reqMap(n, "bind", path)
		if nerr != nil {
			return nerr
		}
		qw, nerr := reqInt(bindMap, "qw", path+".bind")
		if nerr != nil {
			return nerr
		}
		qx, nerr := reqInt(bindMap, "qx", path+".bind")
		if nerr != nil {
			return nerr
		}
		qy, nerr := reqInt(bindMap, "qy", path+".bind")
		if nerr != nil {
			return nerr
		}
		qz, nerr := reqInt(bindMap, "qz", path+".bind")
		if nerr != nil {
			return nerr
		}
		bind, _, tr, qerr := NormalizeQuat(Quat{W: qw, X: qx, Y: qy, Z: qz, Scale: rotScale}, prec, id, -1)
		if qerr != nil {
			return qerr
		}
		normTraces = append(normTraces, tr)
		c, cerr := parseRadConstraint(n, "constraint", path, &traces)
		if cerr != nil {
			return cerr
		}
		joints = append(joints, Joint{ID: id, ParentID: parent, RestTranslation: vecFromArray(restRaw, distScale), BindRotation: bind, Constraint: c})
		traces = append(traces, TransformTrace{RuleVersion: RuleVersion, Direction: "load", FieldPath: path + ".joint_id", OldValue: id, NewValue: "skeleton.joints[].id", Reason: "v3 field alias"})
		if kidsRaw, ok := n["children"]; ok && kidsRaw != nil {
			traces = append(traces, TransformTrace{RuleVersion: RuleVersion, Direction: "load", FieldPath: path + ".children", OldValue: "nested tree", NewValue: "flat parent_id=" + id, Reason: "nested joint tree flattened"})
			kids, kerr := reqSlice(n, "children", path)
			if kerr != nil {
				return kerr
			}
			for k, kid := range kids {
				if kerr := parseNode(kid, path+".children"+indexPath("", k), id); kerr != nil {
					return kerr
				}
			}
		}
		return nil
	}
	for i, r := range rootsArr {
		if err := parseNode(r, "$.rig_tree.roots"+indexPath("", i), ""); err != nil {
			return LoadResult{}, err
		}
	}

	action, err := reqMap(root, "action", "$")
	if err != nil {
		return LoadResult{}, err
	}
	clipID, err := reqString(action, "clip_id", "$.action")
	if err != nil {
		return LoadResult{}, err
	}
	tickRate, err := reqInt(action, "tick_rate", "$.action")
	if err != nil {
		return LoadResult{}, err
	}
	frameArr, err := reqSlice(action, "frames", "$.action")
	if err != nil {
		return LoadResult{}, err
	}
	frames := []Frame{}
	for fi, item := range frameArr {
		path := "$.action.frames" + indexPath("", fi)
		fm, ferr := asMapAt(item, path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		idx, ferr := reqInt(fm, "frame_index", path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		ms, ferr := reqInt(fm, "time_ms", path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		rootRaw, ferr := reqIntArray(fm["root_translation"], path+".root_translation", 3)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		jointMap, ferr := reqMap(fm, "joints", path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		poses := map[string]JointPose{}
		for _, j := range joints {
			jr, ok := jointMap[j.ID]
			if !ok {
				e := errorf(CodeSchema, "missing required joint track")
				e.Field = path + ".joints." + j.ID
				return LoadResult{}, e
			}
			jm, jerr := asMapAt(jr, path+".joints."+j.ID)
			if jerr != nil {
				return LoadResult{}, jerr
			}
			rm, jerr := reqMap(jm, "rotation", path+".joints."+j.ID)
			if jerr != nil {
				return LoadResult{}, jerr
			}
			qw, jerr := reqInt(rm, "qw", path+".joints."+j.ID+".rotation")
			if jerr != nil {
				return LoadResult{}, jerr
			}
			qx, jerr := reqInt(rm, "qx", path+".joints."+j.ID+".rotation")
			if jerr != nil {
				return LoadResult{}, jerr
			}
			qy, jerr := reqInt(rm, "qy", path+".joints."+j.ID+".rotation")
			if jerr != nil {
				return LoadResult{}, jerr
			}
			qz, jerr := reqInt(rm, "qz", path+".joints."+j.ID+".rotation")
			if jerr != nil {
				return LoadResult{}, jerr
			}
			q, _, tr, qerr := NormalizeQuat(Quat{W: qw, X: qx, Y: qy, Z: qz, Scale: rotScale}, prec, j.ID, fi)
			if qerr != nil {
				return LoadResult{}, qerr
			}
			normTraces = append(normTraces, tr)
			poses[j.ID] = JointPose{Rotation: q}
		}
		tick := int64(0)
		if tickRate > 0 {
			tick = ms * tickRate / 1000
		}
		frames = append(frames, Frame{Index: int(idx), Tick: tick, TimeMS: ms, RootTranslation: vecFromArray(rootRaw, distScale), Poses: poses})
		traces = append(traces, TransformTrace{RuleVersion: RuleVersion, Direction: "load", FieldPath: path + ".time_ms", OldValue: ms, NewValue: tick, Reason: "millisecond retained with canonical tick trace"})
	}

	target, terr := parseV3Target(root, distScale)
	if terr != nil {
		return LoadResult{}, terr
	}
	mapping, merr := parseV3Mapping(root, &traces)
	if merr != nil {
		return LoadResult{}, merr
	}
	props := ProportionSet{}
	if raw, ok := root["proportions"]; ok {
		if rerr := remarshalInto(raw, &props); rerr != nil {
			return LoadResult{}, rerr
		}
	}
	if props.DistanceScale == 0 {
		props.DistanceScale = distScale
	}
	a := Asset{
		Seed: seed, SourceFormat: V3Schema, SourceVersion: "v3", SchemaVersion: V3Schema,
		Skeleton:       Skeleton{ID: reqStringLoose(rig, "rig_id"), Version: V3Schema, Joints: joints},
		TargetSkeleton: target, Clip: Clip{ID: clipID, Frames: frames, TickRate: tickRate},
		Mapping: mapping, Proportions: props, Precision: prec,
	}
	return finishAsset(&a, traces, normTraces), nil
}

func parseV3Target(root map[string]any, distScale int64) (Skeleton, *Error) {
	raw, ok := root["target_rig"]
	if !ok {
		return Skeleton{}, nil
	}
	m, err := asMapAt(raw, "$.target_rig")
	if err != nil {
		return Skeleton{}, err
	}
	arr, err := reqSlice(m, "joints", "$.target_rig")
	if err != nil {
		return Skeleton{}, err
	}
	joints := []Joint{}
	for i, item := range arr {
		path := "$.target_rig.joints" + indexPath("", i)
		j, jerr := asMapAt(item, path)
		if jerr != nil {
			return Skeleton{}, jerr
		}
		id, jerr := reqString(j, "joint_id", path)
		if jerr != nil {
			return Skeleton{}, jerr
		}
		parent, _, _ := optString(j, "parent")
		rest, jerr := reqIntArray(j["rest"], path+".rest", 3)
		if jerr != nil {
			return Skeleton{}, jerr
		}
		joints = append(joints, Joint{ID: id, ParentID: parent, RestTranslation: vecFromArray(rest, distScale)})
	}
	return Skeleton{ID: reqStringLoose(m, "rig_id"), Joints: joints}, nil
}

func parseV3Mapping(root map[string]any, traces *[]TransformTrace) (Mapping, *Error) {
	rm, err := reqMap(root, "retarget", "$")
	if err != nil {
		return Mapping{}, err
	}
	version := reqStringLoose(rm, "version")
	arr, err := reqSlice(rm, "links", "$.retarget")
	if err != nil {
		return Mapping{}, err
	}
	items := []MapItem{}
	for i, item := range arr {
		path := "$.retarget.links" + indexPath("", i)
		m, merr := asMapAt(item, path)
		if merr != nil {
			return Mapping{}, merr
		}
		from, merr := reqString(m, "from", path)
		if merr != nil {
			return Mapping{}, merr
		}
		to, merr := reqString(m, "to", path)
		if merr != nil {
			return Mapping{}, merr
		}
		mi := MapItem{SourceJoint: from, TargetJoint: to, WeightScale: 1000, RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1}
		if v, ok, oerr := optInt(m, "weight_scale"); oerr != nil {
			return Mapping{}, oerr
		} else if ok {
			mi.WeightScale = v
		}
		if rawInf, ok := m["sources"]; ok {
			if ierr := remarshalInto(rawInf, &mi.ExtraSources); ierr != nil {
				return Mapping{}, ierr
			}
		}
		if rs, ok := m["rotation_scale"]; ok && rs != nil {
			rm2, rerr := asMapAt(rs, path+".rotation_scale")
			if rerr != nil {
				return Mapping{}, rerr
			}
			n, rerr := reqInt(rm2, "num", path+".rotation_scale")
			if rerr != nil {
				return Mapping{}, rerr
			}
			d, rerr := reqInt(rm2, "den", path+".rotation_scale")
			if rerr != nil {
				return Mapping{}, rerr
			}
			mi.RotScaleNum, mi.RotScaleDen = n, d
		} else {
			mi.ScaleDefaultFilled = true
			*traces = append(*traces, TransformTrace{RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + ".rotation_scale", Default: map[string]int64{"num": 1, "den": 1}, Reason: "v3 link missing rotation scale"})
		}
		if ts, ok := m["translation_scale"]; ok && ts != nil {
			tm, terr := asMapAt(ts, path+".translation_scale")
			if terr != nil {
				return Mapping{}, terr
			}
			n, terr := reqInt(tm, "num", path+".translation_scale")
			if terr != nil {
				return Mapping{}, terr
			}
			d, terr := reqInt(tm, "den", path+".translation_scale")
			if terr != nil {
				return Mapping{}, terr
			}
			mi.TransScaleNum, mi.TransScaleDen = n, d
		}
		items = append(items, mi)
	}
	return Mapping{Version: version, Items: items}, nil
}
