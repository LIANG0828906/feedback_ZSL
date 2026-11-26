package bonerig

import (
	"math"
)

const (
	V1Schema    = "rig.v1"
	V2Schema    = "rig.v2"
	V3Schema    = "rig.v3"
	V1QuatScale = int64(10_000)
	V2QuatScale = int64(32_768)
	V3QuatScale = int64(1_048_576)
)

func microRadToMilliDeg(v int64) int64 {
	return int64(math.Round(float64(v) * 180.0 / (math.Pi * 1000.0)))
}

func constraintV1(c *Constraint) map[string]any {
	if c == nil || c.DefaultFilled {
		return nil
	}
	return map[string]any{
		"axis":     []int64{c.AxisX, c.AxisY, c.AxisZ},
		"min_mdeg": microRadToMilliDeg(c.MinAngleMicroRad),
		"max_mdeg": microRadToMilliDeg(c.MaxAngleMicroRad),
	}
}

func constraintV2(c *Constraint) map[string]any {
	if c == nil {
		return nil
	}
	return map[string]any{
		"axis":    []int64{c.AxisX, c.AxisY, c.AxisZ},
		"min_rad": math.Round(float64(c.MinAngleMicroRad)) / 1_000_000.0,
		"max_rad": math.Round(float64(c.MaxAngleMicroRad)) / 1_000_000.0,
	}
}

// ExportFormat serializes the asset into one release schema. The returned
// lossy list is empty for the native nested V3 format.
func ExportFormat(a Asset, schema string) ([]byte, []LossyItem, *Error) {
	var lossy []LossyItem
	var doc map[string]any
	switch schema {
	case V1Schema:
		doc, lossy = exportV1(a)
	case V2Schema:
		doc, lossy = exportV2(a)
	case V3Schema:
		doc = exportV3(a)
	default:
		e := errorf(CodeSchema, "unknown export schema")
		e.Field = "schema"
		e.OpA = schema
		e.OpB = []string{V1Schema, V2Schema, V3Schema}
		return nil, nil, e
	}
	b, err := marshalDoc(doc)
	if err != nil {
		return nil, nil, errorf(CodeSchema, err.Error())
	}
	return b, dedupeLossy(lossy), nil
}

func dedupeLossy(in []LossyItem) []LossyItem {
	seen := map[string]bool{}
	out := make([]LossyItem, 0, len(in))
	for _, item := range in {
		key := item.FieldPath + "|" + item.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		item.Code = CodeLossy
		if item.RuleVersion == "" {
			item.RuleVersion = RuleVersion
		}
		out = append(out, item)
	}
	return out
}

func childIDs(s Skeleton, parent string) []string {
	var out []string
	for _, j := range s.Joints {
		if j.ParentID == parent {
			out = append(out, j.ID)
		}
	}
	return out
}

func exportV1(a Asset) (map[string]any, []LossyItem) {
	var lossy []LossyItem
	bones := make([]any, 0, len(a.Skeleton.Joints))
	for i, j := range a.Skeleton.Joints {
		path := "$.bones" + indexPath("", i)
		if kids := childIDs(a.Skeleton, j.ID); len(kids) > 0 {
			lossy = append(lossy, LossyItem{
				FieldPath: path + ".children", Kind: "nested_joint_tree",
				OldValue: kids, Default: "flat parent reference",
				Disposition: "flatten_and_keep_parent_id",
			})
		}
		bone := map[string]any{
			"name":      j.ID,
			"parent":    j.ParentID,
			"rest_xyz":  vecToScale(j.RestTranslation, a.Precision.DistanceScale),
			"bind_xyzw": quatToScale(j.BindRotation, V1QuatScale),
		}
		if lim := constraintV1(j.Constraint); lim != nil {
			bone["limit_deg"] = lim
		}
		bones = append(bones, bone)
	}

	frames := make([]any, 0, len(a.Clip.Frames))
	for _, f := range a.Clip.Frames {
		poses := map[string]any{}
		for _, j := range a.Skeleton.Joints {
			if p, ok := f.Poses[j.ID]; ok {
				q := quatToScale(p.Rotation, V1QuatScale)
				poses[j.ID] = []int64{q[1], q[2], q[3], q[0]} // x,y,z,w
			}
		}
		frames = append(frames, map[string]any{
			"tick":     f.Tick,
			"root_xyz": vecToScale(f.RootTranslation, a.Precision.DistanceScale),
			"pose":     poses,
		})
	}

	links := make([]any, 0, len(a.Mapping.Items))
	for i, m := range a.Mapping.Items {
		path := "$.retarget" + indexPath("", i) + ".rot_scale"
		lossy = append(lossy, LossyItem{
			FieldPath: path, Kind: "retarget_scale",
			OldValue:    map[string]int64{"num": m.RotScaleNum, "den": m.RotScaleDen},
			Default:     map[string]int64{"num": 1, "den": 1},
			Disposition: "omit_field_then_loader_defaults_to_identity",
		})
		link := map[string]any{"from": m.SourceJoint, "to": m.TargetJoint}
		if len(m.ExtraSources) > 0 {
			blend := make([]any, 0, len(m.ExtraSources))
			for _, inf := range m.ExtraSources {
				blend = append(blend, map[string]any{"joint": inf.SourceJoint, "power": inf.Weight})
			}
			link["blend"] = blend
			link["power_scale"] = m.WeightScale
		}
		links = append(links, link)
	}

	targets := make([]any, 0, len(a.TargetSkeleton.Joints))
	for _, j := range a.TargetSkeleton.Joints {
		targets = append(targets, map[string]any{
			"name": j.ID, "parent": j.ParentID,
			"rest_xyz": vecToScale(j.RestTranslation, a.Precision.DistanceScale),
		})
	}

	return map[string]any{
		"schema": V1Schema,
		"seed":   a.Seed,
		"rig":    a.Skeleton.ID,
		"fixed_point": map[string]any{
			"rotation_scale": V1QuatScale,
			"distance_scale": a.Precision.DistanceScale,
		},
		"bones": bones,
		"action": map[string]any{
			"clip": a.Clip.ID, "ticks_per_second": a.Clip.TickRate, "frames": frames,
		},
		"target_bones":    targets,
		"retarget":        links,
		"proportions":     a.Proportions,
		"interval_frames": a.Precision.IntervalFrames,
	}, lossy
}

func exportV2(a Asset) (map[string]any, []LossyItem) {
	var lossy []LossyItem
	nodes := make([]any, 0, len(a.Skeleton.Joints))
	for i, j := range a.Skeleton.Joints {
		path := "$.armature.nodes" + indexPath("", i)
		if kids := childIDs(a.Skeleton, j.ID); len(kids) > 0 {
			lossy = append(lossy, LossyItem{
				FieldPath: path + ".children", Kind: "nested_joint_tree", OldValue: kids,
				Default: "flat parent_id reference", Disposition: "flatten_and_keep_parent_id",
			})
		}
		node := map[string]any{
			"id":             j.ID,
			"parent_id":      j.ParentID,
			"rest_position":  vecToScale(j.RestTranslation, a.Precision.DistanceScale),
			"bind_q_wxyz":    quatToScale(j.BindRotation, V2QuatScale),
			"constraint_rad": constraintV2(j.Constraint),
		}
		if j.Constraint == nil {
			delete(node, "constraint_rad")
		}
		nodes = append(nodes, node)
	}

	frames := make([]any, 0, len(a.Clip.Frames))
	for _, f := range a.Clip.Frames {
		rots := map[string]any{}
		for _, j := range a.Skeleton.Joints {
			if p, ok := f.Poses[j.ID]; ok {
				q := quatToScale(p.Rotation, V2QuatScale)
				rots[j.ID] = []int64{q[0], q[1], q[2], q[3]} // w,x,y,z
			}
		}
		frames = append(frames, map[string]any{
			"at_ms":         f.TimeMS,
			"root_position": vecToScale(f.RootTranslation, a.Precision.DistanceScale),
			"rotations":     rots,
		})
	}

	links := make([]any, 0, len(a.Mapping.Items))
	for _, m := range a.Mapping.Items {
		link := map[string]any{
			"source": m.SourceJoint, "target": m.TargetJoint,
			"weight_scale": m.WeightScale,
		}
		if len(m.ExtraSources) > 0 {
			link["influences"] = m.ExtraSources
		}
		if m.RotScaleNum != m.RotScaleDen && m.RotScaleDen != 0 {
			link["rot_scale_pmille"] = m.RotScaleNum * 1000 / m.RotScaleDen
		}
		if m.TransScaleNum != 1 || m.TransScaleDen != 1 {
			link["trans_scale_pmille"] = m.TransScaleNum * 1000 / m.TransScaleDen
		}
		links = append(links, link)
	}

	targetNodes := make([]any, 0, len(a.TargetSkeleton.Joints))
	for _, j := range a.TargetSkeleton.Joints {
		targetNodes = append(targetNodes, map[string]any{
			"id": j.ID, "parent_id": j.ParentID, "rest_position": vecToScale(j.RestTranslation, a.Precision.DistanceScale),
		})
	}

	return map[string]any{
		"schema": V2Schema,
		"seed":   a.Seed,
		"fixed_point": map[string]any{
			"rotation_scale": V2QuatScale,
			"distance_scale": a.Precision.DistanceScale,
		},
		"armature": map[string]any{"id": a.Skeleton.ID, "nodes": nodes},
		"animation": map[string]any{
			"clip_id": a.Clip.ID, "ticks_per_second": a.Clip.TickRate, "frames": frames,
		},
		"target_armature": map[string]any{"id": a.TargetSkeleton.ID, "nodes": targetNodes},
		"retarget_links":  links,
		"proportion_decl": a.Proportions,
		"precision":       a.Precision,
	}, lossy
}

func exportV3(a Asset) map[string]any {
	var build func(j Joint) map[string]any
	build = func(j Joint) map[string]any {
		q := quatToScale(j.BindRotation, V3QuatScale)
		node := map[string]any{
			"joint_id": j.ID,
			"rest":     vecToScale(j.RestTranslation, a.Precision.DistanceScale),
			"bind":     map[string]int64{"qw": q[0], "qx": q[1], "qy": q[2], "qz": q[3]},
		}
		if j.Constraint != nil {
			node["constraint"] = constraintV2(j.Constraint)
		}
		kids := childIDs(a.Skeleton, j.ID)
		if len(kids) > 0 {
			children := make([]any, 0, len(kids))
			idx := a.Skeleton.Index()
			for _, id := range kids {
				children = append(children, build(a.Skeleton.Joints[idx[id]]))
			}
			node["children"] = children
		}
		return node
	}
	roots := make([]any, 0)
	for _, j := range a.Skeleton.Joints {
		if j.ParentID == "" {
			roots = append(roots, build(j))
		}
	}

	frames := make([]any, 0, len(a.Clip.Frames))
	for _, f := range a.Clip.Frames {
		joints := map[string]any{}
		for _, j := range a.Skeleton.Joints {
			if p, ok := f.Poses[j.ID]; ok {
				q := quatToScale(p.Rotation, V3QuatScale)
				joints[j.ID] = map[string]any{
					"rotation": map[string]int64{"qw": q[0], "qx": q[1], "qy": q[2], "qz": q[3]},
				}
			}
		}
		frames = append(frames, map[string]any{
			"frame_index": f.Index, "time_ms": f.TimeMS,
			"root_translation": vecToScale(f.RootTranslation, a.Precision.DistanceScale),
			"joints":           joints,
		})
	}

	links := make([]any, 0, len(a.Mapping.Items))
	for _, m := range a.Mapping.Items {
		links = append(links, map[string]any{
			"from": m.SourceJoint, "to": m.TargetJoint,
			"sources": m.ExtraSources, "weight_scale": m.WeightScale,
			"rotation_scale":    map[string]int64{"num": m.RotScaleNum, "den": m.RotScaleDen},
			"translation_scale": map[string]int64{"num": m.TransScaleNum, "den": m.TransScaleDen},
		})
	}

	targets := make([]any, 0, len(a.TargetSkeleton.Joints))
	for _, j := range a.TargetSkeleton.Joints {
		targets = append(targets, map[string]any{
			"joint_id": j.ID, "parent": j.ParentID,
			"rest": vecToScale(j.RestTranslation, a.Precision.DistanceScale),
		})
	}

	return map[string]any{
		"schema": V3Schema,
		"seed":   a.Seed,
		"fixed_point": map[string]any{
			"rotation_scale": V3QuatScale,
			"distance_scale": a.Precision.DistanceScale,
		},
		"rig_tree":    map[string]any{"rig_id": a.Skeleton.ID, "roots": roots},
		"action":      map[string]any{"clip_id": a.Clip.ID, "tick_rate": a.Clip.TickRate, "frames": frames},
		"target_rig":  map[string]any{"rig_id": a.TargetSkeleton.ID, "joints": targets},
		"retarget":    map[string]any{"version": a.Mapping.Version, "links": links},
		"proportions": a.Proportions,
		"precision":   a.Precision,
	}
}
