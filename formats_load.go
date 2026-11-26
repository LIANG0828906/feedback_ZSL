package bonerig

import (
	"encoding/json"
	"math"
)

// LoadResult contains the unified asset plus per-frame normalization traces.
type LoadResult struct {
	Asset      Asset           `json:"asset"`
	NormTraces []QuatNormTrace `json:"norm_traces"`
}

// LoadDocument detects and loads one of the three release schemas.
func LoadDocument(data []byte) (LoadResult, *Error) {
	root, err := decodeObject(data)
	if err != nil {
		return LoadResult{}, err
	}
	schema, err := reqString(root, "schema", "$")
	if err != nil {
		return LoadResult{}, err
	}
	switch schema {
	case V1Schema:
		return loadV1(root, data)
	case V2Schema:
		return loadV2(root, data)
	case V3Schema:
		return loadV3(root, data)
	default:
		e := errorf(CodeSchema, "unknown schema version")
		e.Field = "$.schema"
		e.OpA = schema
		e.OpB = []string{V1Schema, V2Schema, V3Schema}
		return LoadResult{}, e
	}
}

func remarshalInto(v any, out any) *Error {
	b, err := json.Marshal(v)
	if err != nil {
		return errorf(CodeSchema, err.Error())
	}
	if err := json.Unmarshal(b, out); err != nil {
		return errorf(CodeSchema, err.Error())
	}
	return nil
}

func defaultConstraint() Constraint {
	return Constraint{
		Enabled: true, AxisX: 0, AxisY: 0, AxisZ: 1,
		MinAngleMicroRad: -3_141_593, MaxAngleMicroRad: 3_141_593,
		RuleVersion: RuleVersion, DefaultFilled: true,
	}
}

func mdegToMicroRad(v int64) int64 {
	return int64(math.Round(float64(v) * math.Pi * 1000.0 / 180.0))
}

func radToMicroRad(v float64) int64 {
	return int64(math.Round(v * 1_000_000.0))
}

func parseV1Constraint(bone map[string]any, path string, traces *[]TransformTrace) *Constraint {
	raw, ok := bone["limit_deg"]
	if !ok || raw == nil {
		c := defaultConstraint()
		*traces = append(*traces, TransformTrace{
			RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + ".limit_deg",
			OldValue: nil, NewValue: c, Default: "[-180deg,180deg], z axis", Reason: "missing joint constraint",
		})
		return &c
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	c := defaultConstraint()
	c.DefaultFilled = false
	if axis, ok := m["axis"]; ok {
		if arr, err := reqIntArray(axis, path+".limit_deg.axis", 3); err == nil {
			c.AxisX, c.AxisY, c.AxisZ = arr[0], arr[1], arr[2]
		}
	}
	if v, ok, _ := optInt(m, "min_mdeg"); ok {
		c.MinAngleMicroRad = mdegToMicroRad(v)
	} else {
		*traces = append(*traces, TransformTrace{RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + ".limit_deg.min_mdeg", Default: -180000, Reason: "missing constraint minimum"})
	}
	if v, ok, _ := optInt(m, "max_mdeg"); ok {
		c.MaxAngleMicroRad = mdegToMicroRad(v)
	} else {
		*traces = append(*traces, TransformTrace{RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + ".limit_deg.max_mdeg", Default: 180000, Reason: "missing constraint maximum"})
	}
	return &c
}

func parseRadConstraint(parent map[string]any, key, path string, traces *[]TransformTrace) (*Constraint, *Error) {
	raw, ok := parent[key]
	if !ok || raw == nil {
		c := defaultConstraint()
		*traces = append(*traces, TransformTrace{
			RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + "." + key,
			Default: "[-pi,pi] radians, z axis", Reason: "missing joint constraint",
		})
		return &c, nil
	}
	m, err := asMapAt(raw, path+"."+key)
	if err != nil {
		return nil, err
	}
	c := defaultConstraint()
	c.DefaultFilled = false
	if axis, ok := m["axis"]; ok {
		arr, aerr := reqIntArray(axis, path+"."+key+".axis", 3)
		if aerr != nil {
			return nil, aerr
		}
		c.AxisX, c.AxisY, c.AxisZ = arr[0], arr[1], arr[2]
	}
	if v, ok := m["min_rad"]; ok {
		f, ferr := asFloat(v, path+"."+key+".min_rad")
		if ferr != nil {
			return nil, ferr
		}
		c.MinAngleMicroRad = radToMicroRad(f)
	} else {
		*traces = append(*traces, TransformTrace{RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + "." + key + ".min_rad", Default: -math.Pi, Reason: "missing constraint minimum"})
	}
	if v, ok := m["max_rad"]; ok {
		f, ferr := asFloat(v, path+"."+key+".max_rad")
		if ferr != nil {
			return nil, ferr
		}
		c.MaxAngleMicroRad = radToMicroRad(f)
	} else {
		*traces = append(*traces, TransformTrace{RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + "." + key + ".max_rad", Default: math.Pi, Reason: "missing constraint maximum"})
	}
	return &c, nil
}

func quatFromOrder(arr []int64, scale int64, order [4]int) Quat {
	q := [4]int64{}
	for wirePos, internalPos := range order {
		q[internalPos] = arr[wirePos]
	}
	return Quat{W: q[0], X: q[1], Y: q[2], Z: q[3], Scale: scale}
}

var orderXYZWtoWXYZ = [4]int{1, 2, 3, 0} // wire x,y,z,w -> internal w,x,y,z
var orderWXYZ = [4]int{0, 1, 2, 3}

func vecFromArray(arr []int64, scale int64) Vec3 {
	return Vec3{X: arr[0], Y: arr[1], Z: arr[2], Scale: scale}
}

func finishAsset(a *Asset, traces []TransformTrace, norm []QuatNormTrace) LoadResult {
	a.Traces = traces
	a.NormTraces = norm
	a.ContentHash = CanonicalHash(a.Clip)
	a.TopologyVersion = TopologyHash(a.Skeleton)
	return LoadResult{Asset: *a, NormTraces: norm}
}

func loadV1(root map[string]any, data []byte) (LoadResult, *Error) {
	traces := []TransformTrace{{RuleVersion: RuleVersion, Direction: "load", FieldPath: "$.schema", OldValue: V1Schema, NewValue: "unified-model"}}
	normTraces := []QuatNormTrace{}
	seed, err := reqInt(root, "seed", "$")
	if err != nil {
		return LoadResult{}, err
	}
	fp, err := reqMap(root, "fixed_point", "$")
	if err != nil {
		return LoadResult{}, err
	}
	rotScale, err := reqInt(fp, "rotation_scale", "$.fixed_point")
	if err != nil {
		return LoadResult{}, err
	}
	distScale, err := reqInt(fp, "distance_scale", "$.fixed_point")
	if err != nil {
		return LoadResult{}, err
	}
	prec := DefaultPrecision()
	prec.DistanceScale = distScale
	if v, ok, _ := optInt(root, "interval_frames"); ok && v > 0 {
		prec.IntervalFrames = int(v)
	}

	boneArr, err := reqSlice(root, "bones", "$")
	if err != nil {
		return LoadResult{}, err
	}
	joints := make([]Joint, 0, len(boneArr))
	for i, item := range boneArr {
		path := "$" + indexPath(".bones", i)
		b, berr := asMapAt(item, path)
		if berr != nil {
			return LoadResult{}, berr
		}
		id, berr := reqString(b, "name", path)
		if berr != nil {
			return LoadResult{}, berr
		}
		parent, _, _ := optString(b, "parent")
		restRaw, berr := reqIntArray(b["rest_xyz"], path+".rest_xyz", 3)
		if berr != nil {
			return LoadResult{}, berr
		}
		bindRaw, berr := reqIntArray(b["bind_xyzw"], path+".bind_xyzw", 4)
		if berr != nil {
			return LoadResult{}, berr
		}
		bindQ, _, tr, qerr := NormalizeQuat(quatFromOrder(bindRaw, rotScale, orderXYZWtoWXYZ), prec, id, -1)
		if qerr != nil {
			return LoadResult{}, qerr
		}
		normTraces = append(normTraces, tr)
		joints = append(joints, Joint{
			ID: id, ParentID: parent, RestTranslation: vecFromArray(restRaw, distScale),
			BindRotation: bindQ, Constraint: parseV1Constraint(b, path, &traces),
		})
		traces = append(traces, TransformTrace{RuleVersion: RuleVersion, Direction: "load", FieldPath: path + ".name", OldValue: id, NewValue: "skeleton.joints[].id", Reason: "v1 field alias"})
	}

	action, err := reqMap(root, "action", "$")
	if err != nil {
		return LoadResult{}, err
	}
	clipID, err := reqString(action, "clip", "$.action")
	if err != nil {
		return LoadResult{}, err
	}
	tickRate, err := reqInt(action, "ticks_per_second", "$.action")
	if err != nil {
		return LoadResult{}, err
	}
	frameArr, err := reqSlice(action, "frames", "$.action")
	if err != nil {
		return LoadResult{}, err
	}
	frames := make([]Frame, 0, len(frameArr))
	for fi, item := range frameArr {
		path := "$.action" + indexPath(".frames", fi)
		fm, ferr := asMapAt(item, path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		tick, ferr := reqInt(fm, "tick", path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		rootRaw, ferr := reqIntArray(fm["root_xyz"], path+".root_xyz", 3)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		pose, ferr := reqMap(fm, "pose", path)
		if ferr != nil {
			return LoadResult{}, ferr
		}
		poses := map[string]JointPose{}
		for _, j := range joints {
			raw, ok := pose[j.ID]
			if !ok {
				e := errorf(CodeSchema, "missing required joint rotation")
				e.Field = path + ".pose." + j.ID
				return LoadResult{}, e
			}
			arr, aerr := reqIntArray(raw, path+".pose."+j.ID, 4)
			if aerr != nil {
				return LoadResult{}, aerr
			}
			q, _, tr, qerr := NormalizeQuat(quatFromOrder(arr, rotScale, orderXYZWtoWXYZ), prec, j.ID, fi)
			if qerr != nil {
				return LoadResult{}, qerr
			}
			normTraces = append(normTraces, tr)
			poses[j.ID] = JointPose{Rotation: q}
		}
		timeMS := int64(0)
		if tickRate > 0 {
			timeMS = tick * 1000 / tickRate
		}
		frames = append(frames, Frame{Index: fi, Tick: tick, TimeMS: timeMS, RootTranslation: vecFromArray(rootRaw, distScale), Poses: poses})
		traces = append(traces, TransformTrace{RuleVersion: RuleVersion, Direction: "load", FieldPath: path + ".tick", OldValue: tick, NewValue: timeMS, Reason: "tick to millisecond canonical trace"})
	}

	targetSkel, terr := parseV1Target(root, distScale)
	if terr != nil {
		return LoadResult{}, terr
	}
	mapping, mtraces, merr := parseV1Mapping(root, &traces)
	if merr != nil {
		return LoadResult{}, merr
	}
	traces = append(traces, mtraces...)
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
		Seed: seed, SourceFormat: V1Schema, SourceVersion: "v1", SchemaVersion: V1Schema,
		Skeleton:       Skeleton{ID: reqStringLoose(root, "rig"), Version: V1Schema, Joints: joints},
		TargetSkeleton: targetSkel, Clip: Clip{ID: clipID, Frames: frames, TickRate: tickRate},
		Mapping: mapping, Proportions: props, Precision: prec,
	}
	return finishAsset(&a, traces, normTraces), nil
}

func optString(m map[string]any, key string) (string, bool, *Error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, typeError(key, "string", v)
	}
	return s, true, nil
}

func reqStringLoose(m map[string]any, key string) string {
	s, _, _ := optString(m, key)
	return s
}

func parseV1Target(root map[string]any, distScale int64) (Skeleton, *Error) {
	arrAny, ok := root["target_bones"]
	if !ok {
		return Skeleton{}, nil
	}
	arr, err := reqSlice(root, "target_bones", "$")
	if err != nil {
		return Skeleton{}, err
	}
	_ = arrAny
	joints := []Joint{}
	for i, item := range arr {
		path := "$" + indexPath(".target_bones", i)
		m, merr := asMapAt(item, path)
		if merr != nil {
			return Skeleton{}, merr
		}
		id, merr := reqString(m, "name", path)
		if merr != nil {
			return Skeleton{}, merr
		}
		parent, _, _ := optString(m, "parent")
		rest, merr := reqIntArray(m["rest_xyz"], path+".rest_xyz", 3)
		if merr != nil {
			return Skeleton{}, merr
		}
		joints = append(joints, Joint{ID: id, ParentID: parent, RestTranslation: vecFromArray(rest, distScale)})
	}
	return Skeleton{ID: "target", Version: V1Schema, Joints: joints}, nil
}

func parseV1Mapping(root map[string]any, traces *[]TransformTrace) (Mapping, []TransformTrace, *Error) {
	arr, err := reqSlice(root, "retarget", "$")
	if err != nil {
		return Mapping{}, nil, err
	}
	items := []MapItem{}
	var local []TransformTrace
	for i, item := range arr {
		path := "$.retarget" + indexPath("", i)
		m, merr := asMapAt(item, path)
		if merr != nil {
			return Mapping{}, nil, merr
		}
		from, merr := reqString(m, "from", path)
		if merr != nil {
			return Mapping{}, nil, merr
		}
		to, merr := reqString(m, "to", path)
		if merr != nil {
			return Mapping{}, nil, merr
		}
		weightScale := int64(1000)
		if v, ok, oerr := optInt(m, "power_scale"); oerr != nil {
			return Mapping{}, nil, oerr
		} else if ok {
			weightScale = v
		}
		var extras []WeightedJoint
		if rawBlend, ok := m["blend"]; ok && rawBlend != nil {
			blend, berr := reqSlice(m, "blend", path)
			if berr != nil {
				return Mapping{}, nil, berr
			}
			_ = rawBlend
			for bi, item := range blend {
				bp := path + ".blend" + indexPath("", bi)
				bm, berr := asMapAt(item, bp)
				if berr != nil {
					return Mapping{}, nil, berr
				}
				joint, berr := reqString(bm, "joint", bp)
				if berr != nil {
					return Mapping{}, nil, berr
				}
				power, berr := reqInt(bm, "power", bp)
				if berr != nil {
					return Mapping{}, nil, berr
				}
				extras = append(extras, WeightedJoint{SourceJoint: joint, Weight: power})
			}
		}
		tr := TransformTrace{
			RuleVersion: RuleVersion, Direction: "upgrade", FieldPath: path + ".rot_scale",
			OldValue: nil, NewValue: map[string]int64{"num": 1, "den": 1}, Default: "identity",
			Reason: "v1 has no retarget rotation scale field",
		}
		*traces = append(*traces, tr)
		local = append(local, tr)
		items = append(items, MapItem{
			SourceJoint: from, TargetJoint: to, WeightScale: weightScale, ExtraSources: extras,
			RotScaleNum: 1, RotScaleDen: 1, TransScaleNum: 1, TransScaleDen: 1,
			ScaleDefaultFilled: true,
		})
	}
	return Mapping{Version: "mapping-v1", Items: items}, local, nil
}
