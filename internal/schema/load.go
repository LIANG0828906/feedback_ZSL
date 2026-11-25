package schema

import (
	"encoding/json"
	"fmt"
	"strings"

	"combatjudge/internal/conv"
	"combatjudge/internal/model"
)

const DefaultStepMS int64 = 16

func Load(data []byte) (*model.Project, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, &model.Error{Code: model.CodeDecode, FieldPath: "$", Message: "JSON decode failed: " + err.Error()}
	}
	version := firstNonEmpty(env.Schema, env.SchemaCamel, env.SchemaPascal)
	switch version {
	case model.SchemaV1:
		var f FileV1
		if err := unmarshal(data, &f, model.SchemaV1); err != nil {
			return nil, err
		}
		return loadV1(&f)
	case model.SchemaV2:
		var f FileV2
		if err := unmarshal(data, &f, model.SchemaV2); err != nil {
			return nil, err
		}
		return loadV2(&f)
	case model.SchemaV3:
		var f FileV3
		if err := unmarshal(data, &f, model.SchemaV3); err != nil {
			return nil, err
		}
		return loadV3(&f)
	default:
		return nil, model.NewError(model.CodeSchema, "$.schema", version, "unknown schema version %q", version)
	}
}

func unmarshal(data []byte, dst any, version string) error {
	if err := json.Unmarshal(data, dst); err != nil {
		if typeErr, ok := err.(*json.UnmarshalTypeError); ok {
			return model.NewError(model.CodeSchema, "$."+typeErr.Field, version, "field type mismatch: expected %s", typeErr.Type.String())
		}
		return &model.Error{Code: model.CodeDecode, FieldPath: "$", SourceVersion: version, Message: "JSON decode failed: " + err.Error()}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func requiredString(path, version, value string) error {
	if strings.TrimSpace(value) == "" {
		return model.NewError(model.CodeSchema, path, version, "required field is missing or empty")
	}
	return nil
}

func requiredPointer[T any](path, version string, value *T) error {
	if value == nil {
		return model.NewError(model.CodeSchema, path, version, "required field is missing")
	}
	return nil
}

func vecFromDTO(v *Vec3DTO) model.Vec3 {
	if v == nil {
		return model.Vec3{}
	}
	return model.Vec3{X: v[0], Y: v[1], Z: v[2]}
}

func mapKind(version, path, kind string) (model.ColliderKind, error) {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	switch normalized {
	case "aabb", "axis_aligned_box", "box":
		return model.KindAABB, nil
	case "capsule", "oriented_capsule", "oriented capsule":
		return model.KindCapsule, nil
	default:
		return "", model.NewError(model.CodeSchema, path, version, "unknown collider kind %q", kind)
	}
}

func fixedField(path, version string, raw *int64, scale int64) (int64, error) {
	if err := requiredPointer(path, version, raw); err != nil {
		return 0, err
	}
	result, err := conv.FromFixed(path, version, *raw, scale, conv.RoundFloor)
	if err != nil {
		return 0, err
	}
	return result.Value, nil
}

func defaultPenetration(version, prefix string, traces *[]model.Trace) (bool, int64, bool, int, model.RepeatPolicy) {
	*traces = append(*traces, model.Trace{SourceVersion: version, FieldPath: prefix + ".penetration", Op: "default_fill", OldValue: "<absent>", NewValue: "true", Source: "declared default: penetration continues while damage > 0", RuleVersion: model.TransformRuleVersion})
	*traces = append(*traces, model.Trace{SourceVersion: version, FieldPath: prefix + ".decayQ", Op: "default_fill", OldValue: "<absent>", NewValue: "500000", Source: "declared default: retain 50% damage after each segment", RuleVersion: model.TransformRuleVersion})
	multi, maxSeg, repeat := defaultSegmentPolicy(version, prefix, traces)
	return true, 500_000, multi, maxSeg, repeat
}

func defaultSegmentPolicy(version, prefix string, traces *[]model.Trace) (bool, int, model.RepeatPolicy) {
	*traces = append(*traces,
		model.Trace{SourceVersion: version, FieldPath: prefix + ".multiHit", Op: "default_fill", OldValue: "<absent>", NewValue: "true", Source: model.DefaultPolicyVersion + ": deterministic ordered multi-segment settlement", RuleVersion: model.TransformRuleVersion},
		model.Trace{SourceVersion: version, FieldPath: prefix + ".maxSegments", Op: "default_fill", OldValue: "<absent>", NewValue: "12", Source: model.DefaultPolicyVersion + ": bounded segment count", RuleVersion: model.TransformRuleVersion},
		model.Trace{SourceVersion: version, FieldPath: prefix + ".repeatPolicy", Op: "default_fill", OldValue: "<absent>", NewValue: string(model.RepeatOncePerSweep), Source: model.DefaultPolicyVersion + ": one collider is hit at most once per sweep", RuleVersion: model.TransformRuleVersion},
	)
	return true, 12, model.RepeatOncePerSweep
}

func loadV1(f *FileV1) (*model.Project, error) {
	v := model.SchemaV1
	if f.Schema != v {
		return nil, model.NewError(model.CodeSchema, "$.Schema", v, "schema marker mismatch %q", f.Schema)
	}
	if f.UnitScale != 0 && f.UnitScale != convScalePermille {
		return nil, model.NewError(model.CodeUnit, "$.UnitScale", v, "version %s requires permille scale %d, found %d", v, convScalePermille, f.UnitScale)
	}
	if err := requiredString("$.BattleId", v, f.BattleID); err != nil {
		return nil, err
	}
	traces := []model.Trace{}
	step := DefaultStepMS
	if f.FrameStepMS == nil {
		traces = append(traces, model.Trace{SourceVersion: v, FieldPath: "$.FrameStepMs", Op: "default_fill", OldValue: "<absent>", NewValue: "16", Source: "declared default frame step", RuleVersion: model.TransformRuleVersion})
	} else {
		if *f.FrameStepMS <= 0 {
			return nil, model.NewError(model.CodeStep, "$.FrameStepMs", v, "frame step must be positive, got %d", *f.FrameStepMS)
		}
		step = *f.FrameStepMS
	}

	colliders, err := loadCollidersV1(f.Colliders, v)
	if err != nil {
		return nil, err
	}
	sweeps, sweepTraces, err := loadSweepsV1(f.Sweeps, v)
	if err != nil {
		return nil, err
	}
	traces = append(traces, sweepTraces...)
	requests, err := loadRequestsV1(f.Requests, v, step)
	if err != nil {
		return nil, err
	}
	return &model.Project{SchemaVersion: v, BattleID: f.BattleID, StepMS: step, Colliders: colliders, Sweeps: sweeps, Requests: requests, Traces: traces}, nil
}

const convScalePermille int64 = 1000
const convScaleMyriad int64 = 10000

func loadCollidersV1(items []ColliderV1, version string) ([]model.Collider, error) {
	out := make([]model.Collider, 0, len(items))
	for i, d := range items {
		path := fmt.Sprintf("$.Colliders[%d]", i)
		if d.No <= 0 {
			return nil, model.NewError(model.CodeSchema, path+".No", version, "collider number must be positive")
		}
		if err := requiredString(path+".Id", version, d.ID); err != nil {
			return nil, err
		}
		kind, err := mapKind(version, path+".Kind", d.Kind)
		if err != nil {
			return nil, err
		}
		c := model.Collider{Index: d.No, ID: d.ID, Kind: kind, Part: firstNonEmpty(d.Part, "body"), DeclaredScale: convScalePermille}
		if kind == model.KindAABB {
			if err := requiredPointer(path+".Min", version, d.Min); err != nil {
				return nil, err
			}
			if err := requiredPointer(path+".Max", version, d.Max); err != nil {
				return nil, err
			}
			c.Min, c.Max = vecFromDTO(d.Min), vecFromDTO(d.Max)
		} else {
			if err := requiredPointer(path+".CapStart", version, d.CapStart); err != nil {
				return nil, err
			}
			if err := requiredPointer(path+".CapEnd", version, d.CapEnd); err != nil {
				return nil, err
			}
			if err := requiredPointer(path+".RadiusPermille", version, d.RadiusPermille); err != nil {
				return nil, err
			}
			c.P0, c.P1 = vecFromDTO(d.CapStart), vecFromDTO(d.CapEnd)
			if c.Radius, err = fixedField(path+".RadiusPermille", version, d.RadiusPermille, convScalePermille); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func loadSweepsV1(items []SweepV1, version string) ([]model.Sweep, []model.Trace, error) {
	out := make([]model.Sweep, 0, len(items))
	traces := []model.Trace{}
	for i, d := range items {
		path := fmt.Sprintf("$.Sweeps[%d]", i)
		if err := requiredString(path+".Id", version, d.ID); err != nil {
			return nil, nil, err
		}
		if err := requiredPointer(path+".StartFrame", version, d.StartFrame); err != nil {
			return nil, nil, err
		}
		if err := requiredPointer(path+".EndFrame", version, d.EndFrame); err != nil {
			return nil, nil, err
		}
		radius, err := fixedField(path+".RadiusPermille", version, d.RadiusPermille, convScalePermille)
		if err != nil {
			return nil, nil, err
		}
		damage, err := fixedField(path+".DamagePermille", version, d.DamagePermille, convScalePermille)
		if err != nil {
			return nil, nil, err
		}
		pen, decay, multi, maxSeg, repeat := defaultPenetration(version, path, &traces)
		out = append(out, model.Sweep{
			Index: i + 1, ID: d.ID, From: vecFromDTO(&d.StartPosQ), To: vecFromDTO(&d.EndPosQ), Radius: radius,
			StartFrame: *d.StartFrame, EndFrame: *d.EndFrame, DamageQ: damage, StepMS: DefaultStepMS,
			Penetration: pen, DecayQ: decay, MultiHit: multi, MaxSegments: maxSeg, RepeatPolicy: repeat,
			EndpointMode: "absolute",
		})
	}
	return out, traces, nil
}

func loadRequestsV1(items []RequestV1, version string, stepMS int64) ([]model.FrameRequest, error) {
	out := make([]model.FrameRequest, 0, len(items))
	for i, d := range items {
		path := fmt.Sprintf("$.Requests[%d]", i)
		if err := requiredPointer(path+".Frame", version, d.Frame); err != nil {
			return nil, err
		}
		if err := requiredString(path+".SweepId", version, d.SweepID); err != nil {
			return nil, err
		}
		ms, err := conv.FrameToMS(int64(*d.Frame), stepMS)
		if err != nil {
			return nil, err
		}
		out = append(out, model.FrameRequest{Index: i + 1, Frame: *d.Frame, SweepID: d.SweepID, AtMS: ms})
	}
	return out, nil
}
