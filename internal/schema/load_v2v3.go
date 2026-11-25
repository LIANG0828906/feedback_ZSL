package schema

import (
	"fmt"

	"combatjudge/internal/conv"
	"combatjudge/internal/model"
)

func loadV2(f *FileV2) (*model.Project, error) {
	v := model.SchemaV2
	if f.Schema != v {
		return nil, model.NewError(model.CodeSchema, "$.schema", v, "schema marker mismatch %q", f.Schema)
	}
	if f.UnitScale != 0 && f.UnitScale != convScaleMyriad {
		return nil, model.NewError(model.CodeUnit, "$.unit_scale", v, "version %s requires myriad scale %d, found %d", v, convScaleMyriad, f.UnitScale)
	}
	if err := requiredString("$.battle_id", v, f.BattleID); err != nil {
		return nil, err
	}
	traces := []model.Trace{}
	step := DefaultStepMS
	if f.FrameStepMS == nil {
		traces = append(traces, model.Trace{SourceVersion: v, FieldPath: "$.frame_step_ms", Op: "default_fill", OldValue: "<absent>", NewValue: "16", Source: "declared default frame step", RuleVersion: model.TransformRuleVersion})
	} else {
		if *f.FrameStepMS <= 0 {
			return nil, model.NewError(model.CodeStep, "$.frame_step_ms", v, "frame step must be positive, got %d", *f.FrameStepMS)
		}
		step = *f.FrameStepMS
	}

	colliders, collTraces, err := loadCollidersV2(f.Colliders, v)
	if err != nil {
		return nil, err
	}
	traces = append(traces, collTraces...)
	sweeps, sweepTraces, err := loadSweepsV2(f.Sweeps, v, step, &traces)
	if err != nil {
		return nil, err
	}
	traces = append(traces, sweepTraces...)
	requests, reqTraces, err := loadRequestsV2(f.Requests, v, step)
	if err != nil {
		return nil, err
	}
	traces = append(traces, reqTraces...)
	for i := range sweeps {
		sweeps[i].StepMS = step
	}
	return &model.Project{SchemaVersion: v, BattleID: f.BattleID, StepMS: step, Colliders: colliders, Sweeps: sweeps, Requests: requests, Traces: traces}, nil
}

func loadCollidersV2(items []ColliderV2, version string) ([]model.Collider, []model.Trace, error) {
	out := make([]model.Collider, 0, len(items))
	traces := []model.Trace{}
	for i, d := range items {
		path := fmt.Sprintf("$.colliders[%d]", i)
		if d.No <= 0 {
			return nil, nil, model.NewError(model.CodeSchema, path+".collider_no", version, "collider number must be positive")
		}
		if err := requiredString(path+".id", version, d.ID); err != nil {
			return nil, nil, err
		}
		kind, err := mapKind(version, path+".kind", d.Kind)
		if err != nil {
			return nil, nil, err
		}
		c := model.Collider{Index: d.No, ID: d.ID, Kind: kind, Part: firstNonEmpty(d.Part, "body"), DeclaredScale: convScaleMyriad}
		if kind == model.KindAABB {
			if err := requiredPointer(path+".mins_q", version, d.MinsQ); err != nil {
				return nil, nil, err
			}
			if err := requiredPointer(path+".maxs_q", version, d.MaxsQ); err != nil {
				return nil, nil, err
			}
			c.Min, c.Max = vecFromDTO(d.MinsQ), vecFromDTO(d.MaxsQ)
		} else {
			if err := requiredPointer(path+".center_q", version, d.CenterQ); err != nil {
				return nil, nil, err
			}
			if err := requiredPointer(path+".half_axis_delta_q", version, d.HalfAxisDeltaQ); err != nil {
				return nil, nil, err
			}
			if err := requiredPointer(path+".radius_myriad", version, d.RadiusMyriad); err != nil {
				return nil, nil, err
			}
			center, half := vecFromDTO(d.CenterQ), vecFromDTO(d.HalfAxisDeltaQ)
			neg := model.Vec3{X: -half.X, Y: -half.Y, Z: -half.Z}
			c.P0, err = conv.AddVec(path+".p0", version, center, neg)
			if err != nil {
				return nil, nil, err
			}
			c.P1, err = conv.AddVec(path+".p1", version, center, half)
			if err != nil {
				return nil, nil, err
			}
			traces = append(traces, model.Trace{SourceVersion: version, FieldPath: path + ".half_axis_delta_q", Op: "orientation_delta_expand", OldValue: fmt.Sprintf("center=%+v half=%+v", center, half), NewValue: fmt.Sprintf("p0=%+v p1=%+v", c.P0, c.P1), Source: "checked integer center ± half axis", RuleVersion: model.TransformRuleVersion})
			if c.Radius, err = fixedField(path+".radius_myriad", version, d.RadiusMyriad, convScaleMyriad); err != nil {
				return nil, nil, err
			}
		}
		out = append(out, c)
	}
	return out, traces, nil
}

func loadSweepsV2(items []SweepV2, version string, stepMS int64, globalTraces *[]model.Trace) ([]model.Sweep, []model.Trace, error) {
	out := make([]model.Sweep, 0, len(items))
	traces := []model.Trace{}
	for i, d := range items {
		path := fmt.Sprintf("$.sweeps[%d]", i)
		if err := requiredString(path+".sweep_id", version, d.ID); err != nil {
			return nil, nil, err
		}
		if err := requiredPointer(path+".start_ms", version, d.StartMS); err != nil {
			return nil, nil, err
		}
		if err := requiredPointer(path+".end_ms", version, d.EndMS); err != nil {
			return nil, nil, err
		}
		startFrame, startResidual, err := conv.MSToFrame(path+".start_ms", version, *d.StartMS, stepMS, conv.RoundFloor)
		if err != nil {
			return nil, nil, err
		}
		endFrame, endResidual, err := conv.MSToFrame(path+".end_ms", version, *d.EndMS, stepMS, conv.RoundFloor)
		if err != nil {
			return nil, nil, err
		}
		traces = append(traces,
			model.Trace{SourceVersion: version, FieldPath: path + ".start_ms", Op: "time_to_frame_floor", OldValue: fmt.Sprint(*d.StartMS), NewValue: fmt.Sprint(startFrame), Source: "one-based frame 1 starts at 0ms", RuleVersion: model.TransformRuleVersion, Detail: fmt.Sprintf("residual_ms=%d", startResidual)},
			model.Trace{SourceVersion: version, FieldPath: path + ".end_ms", Op: "time_to_frame_floor", OldValue: fmt.Sprint(*d.EndMS), NewValue: fmt.Sprint(endFrame), Source: "one-based frame 1 starts at 0ms", RuleVersion: model.TransformRuleVersion, Detail: fmt.Sprintf("residual_ms=%d", endResidual)},
		)
		radius, err := fixedField(path+".radius_myriad", version, d.RadiusMyriad, convScaleMyriad)
		if err != nil {
			return nil, nil, err
		}
		damage, err := fixedField(path+".damage_myriad", version, d.DamageMyriad, convScaleMyriad)
		if err != nil {
			return nil, nil, err
		}
		from := vecFromDTO(&d.OriginQ)
		to, err := conv.AddVec(path+".advance_delta_q", version, from, vecFromDTO(&d.AdvanceDeltaQ))
		if err != nil {
			return nil, nil, err
		}
		traces = append(traces, model.Trace{SourceVersion: version, FieldPath: path + ".advance_delta_q", Op: "delta_expand", OldValue: fmt.Sprintf("origin=%+v delta=%+v", from, d.AdvanceDeltaQ), NewValue: fmt.Sprintf("%+v", to), Source: "checked integer addition", RuleVersion: model.TransformRuleVersion})

		pen := true
		if d.Penetrate == nil {
			*globalTraces = append(*globalTraces, model.Trace{SourceVersion: version, FieldPath: path + ".penetrate", Op: "default_fill", OldValue: "<absent>", NewValue: "true", Source: "declared default penetration policy", RuleVersion: model.TransformRuleVersion})
		} else {
			pen = *d.Penetrate
		}
		decay := int64(500_000)
		if d.DecayMyriad == nil {
			*globalTraces = append(*globalTraces, model.Trace{SourceVersion: version, FieldPath: path + ".decay_myriad", Op: "default_fill", OldValue: "<absent>", NewValue: "5000", Source: "declared default 50% retention", RuleVersion: model.TransformRuleVersion})
		} else {
			decay, err = fixedField(path+".decay_myriad", version, d.DecayMyriad, convScaleMyriad)
			if err != nil {
				return nil, nil, err
			}
		}
		multi, maxSeg, repeat := defaultSegmentPolicy(version, path, globalTraces)
		out = append(out, model.Sweep{
			Index: i + 1, ID: d.ID, From: from, To: to, Radius: radius,
			StartFrame: startFrame, EndFrame: endFrame, DamageQ: damage, StepMS: stepMS,
			Penetration: pen, DecayQ: decay, MultiHit: multi, MaxSegments: maxSeg, RepeatPolicy: repeat,
			EndpointMode: "relative_delta",
		})
	}
	return out, traces, nil
}

func loadRequestsV2(items []RequestV2, version string, stepMS int64) ([]model.FrameRequest, []model.Trace, error) {
	out := make([]model.FrameRequest, 0, len(items))
	traces := []model.Trace{}
	for i, d := range items {
		path := fmt.Sprintf("$.requests[%d]", i)
		if err := requiredPointer(path+".at_ms", version, d.AtMS); err != nil {
			return nil, nil, err
		}
		if err := requiredString(path+".sweep_id", version, d.SweepID); err != nil {
			return nil, nil, err
		}
		frame, residual, err := conv.MSToFrame(path+".at_ms", version, *d.AtMS, stepMS, conv.RoundFloor)
		if err != nil {
			return nil, nil, err
		}
		traces = append(traces, model.Trace{SourceVersion: version, FieldPath: path + ".at_ms", Op: "time_to_frame_floor", OldValue: fmt.Sprint(*d.AtMS), NewValue: fmt.Sprint(frame), RuleVersion: model.TransformRuleVersion, Detail: fmt.Sprintf("residual_ms=%d", residual)})
		out = append(out, model.FrameRequest{Index: i + 1, Frame: frame, SweepID: d.SweepID, AtMS: *d.AtMS})
	}
	return out, traces, nil
}

func loadV3(f *FileV3) (*model.Project, error) {
	v := model.SchemaV3
	if f.SchemaVersion != v {
		return nil, model.NewError(model.CodeSchema, "$.schemaVersion", v, "schema marker mismatch %q", f.SchemaVersion)
	}
	if err := requiredString("$.battleId", v, f.BattleID); err != nil {
		return nil, err
	}
	if err := requiredPointer("$.frameStepMs", v, f.FrameStepMS); err != nil {
		return nil, model.NewError(model.CodeStep, "$.frameStepMs", v, "frame step is required")
	}
	if *f.FrameStepMS <= 0 {
		return nil, model.NewError(model.CodeStep, "$.frameStepMs", v, "frame step must be positive, got %d", *f.FrameStepMS)
	}
	traces := []model.Trace{}
	colliders, err := loadCollidersV3(f.Colliders, v)
	if err != nil {
		return nil, err
	}
	sweeps, err := loadSweepsV3(f.Sweeps, v, *f.FrameStepMS, &traces)
	if err != nil {
		return nil, err
	}
	requests := make([]model.FrameRequest, 0, len(f.Requests))
	for i, d := range f.Requests {
		path := fmt.Sprintf("$.requests[%d]", i)
		if d.Frame <= 0 {
			return nil, model.NewError(model.CodeSchema, path+".frame", v, "frame must be positive")
		}
		if err := requiredString(path+".sweepId", v, d.SweepID); err != nil {
			return nil, err
		}
		ms, err := conv.FrameToMS(int64(d.Frame), *f.FrameStepMS)
		if err != nil {
			return nil, err
		}
		requests = append(requests, model.FrameRequest{Index: i + 1, Frame: d.Frame, SweepID: d.SweepID, AtMS: ms})
	}
	return &model.Project{SchemaVersion: v, BattleID: f.BattleID, StepMS: *f.FrameStepMS, Colliders: colliders, Sweeps: sweeps, Requests: requests, Traces: traces}, nil
}

func loadCollidersV3(items []ColliderV3, version string) ([]model.Collider, error) {
	out := make([]model.Collider, 0, len(items))
	for i, d := range items {
		path := fmt.Sprintf("$.colliders[%d]", i)
		if d.No <= 0 {
			return nil, model.NewError(model.CodeSchema, path+".colliderNo", version, "collider number must be positive")
		}
		if err := requiredString(path+".id", version, d.ID); err != nil {
			return nil, err
		}
		kind, err := mapKind(version, path+".kind", d.Kind)
		if err != nil {
			return nil, err
		}
		c := model.Collider{Index: d.No, ID: d.ID, Kind: kind, Part: firstNonEmpty(d.Part, "body"), Radius: d.Radius, DeclaredScale: model.InternalScale}
		if kind == model.KindAABB {
			if d.Min == nil || d.Max == nil {
				return nil, model.NewError(model.CodeSchema, path, version, "aabb requires min and max")
			}
			c.Min, c.Max = *d.Min, *d.Max
		} else {
			if d.P0 == nil || d.P1 == nil {
				return nil, model.NewError(model.CodeSchema, path, version, "capsule requires p0 and p1")
			}
			if d.Radius <= 0 {
				return nil, model.NewError(model.CodeSchema, path+".radiusQ", version, "capsule radius must be positive")
			}
			c.P0, c.P1 = *d.P0, *d.P1
		}
		out = append(out, c)
	}
	return out, nil
}

func loadSweepsV3(items []SweepV3, version string, stepMS int64, traces *[]model.Trace) ([]model.Sweep, error) {
	out := make([]model.Sweep, 0, len(items))
	for i, d := range items {
		path := fmt.Sprintf("$.sweeps[%d]", i)
		if err := requiredString(path+".id", version, d.ID); err != nil {
			return nil, err
		}
		if d.StartFrame <= 0 || d.EndFrame < d.StartFrame {
			return nil, model.NewError(model.CodeSchema, path+".frameRange", version, "invalid frame range %d..%d", d.StartFrame, d.EndFrame)
		}
		if d.Radius <= 0 {
			return nil, model.NewError(model.CodeSchema, path+".radiusQ", version, "sweep radius must be positive")
		}
		s := model.Sweep{
			Index: i + 1, ID: d.ID, From: d.From, To: d.To, Radius: d.Radius,
			StartFrame: d.StartFrame, EndFrame: d.EndFrame, StepMS: stepMS, DamageQ: d.Damage,
			EndpointMode: firstNonEmpty(d.EndpointMode, "absolute"),
		}
		if d.Penetration == nil {
			s.Penetration, s.DecayQ, s.MultiHit, s.MaxSegments, s.RepeatPolicy = defaultPenetration(version, path, traces)
		} else {
			s.Penetration = *d.Penetration
			s.MultiHit, s.MaxSegments, s.RepeatPolicy = defaultSegmentPolicy(version, path, traces)
			if d.Decay != nil {
				s.DecayQ = *d.Decay
			} else {
				s.DecayQ = 500_000
			}
			if d.MultiHit != nil {
				s.MultiHit = *d.MultiHit
			}
			if d.MaxSegments != nil {
				s.MaxSegments = *d.MaxSegments
			}
			if d.RepeatPolicy != "" {
				switch model.RepeatPolicy(d.RepeatPolicy) {
				case model.RepeatOncePerSweep, model.RepeatAllowed:
					s.RepeatPolicy = model.RepeatPolicy(d.RepeatPolicy)
				default:
					return nil, model.NewError(model.CodeSchema, path+".repeatPolicy", version, "unknown repeat policy %q", d.RepeatPolicy)
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}
