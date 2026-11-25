package schema

import (
	"encoding/json"
	"fmt"

	"combatjudge/internal/conv"
	"combatjudge/internal/model"
)

func Export(project *model.Project, version string) (data []byte, losses []model.LossNotice, err error) {
	losses = []model.LossNotice{}
	defer func() {
		if recovered := recover(); recovered != nil {
			if e, ok := recovered.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("export failed: %v", recovered)
			}
			data = nil
		}
	}()
	var doc any
	switch version {
	case model.SchemaV1:
		doc, losses = exportV1(project, &losses)
	case model.SchemaV2:
		doc, losses = exportV2(project, &losses)
	case model.SchemaV3:
		doc = exportV3(project)
	default:
		return nil, nil, model.NewError(model.CodeSchema, "$.schema", version, "unknown export version")
	}
	data, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return data, losses, nil
}

func loss(path string, losses *[]model.LossNotice, version, value, disposition string) {
	*losses = append(*losses, model.LossNotice{
		Code: model.CodeLossy, FieldPath: path, SourceVersion: version, Value: value,
		Disposition: disposition, RuleVersion: model.TransformRuleVersion,
	})
}

func mustFixed(path string, losses *[]model.LossNotice, targetVersion string, q, scale int64) *int64 {
	raw, remainder, err := conv.ToFixed(path, targetVersion, q, scale, conv.RoundFloor)
	if err != nil {
		panic(err)
	}
	if remainder != 0 {
		loss(path, losses, targetVersion, fmt.Sprintf("q=%d remainder=%d/%d", q, remainder, model.InternalScale), "floor_quantize_and_retain_residual_trace")
	}
	return &raw
}

func exportV1(p *model.Project, losses *[]model.LossNotice) (FileV1, []model.LossNotice) {
	v := model.SchemaV1
	step := p.StepMS
	doc := FileV1{Schema: v, BattleID: p.BattleID, FrameStepMS: &step, UnitScale: convScalePermille}
	for _, c := range p.Colliders {
		d := ColliderV1{No: c.Index, ID: c.ID, Part: c.Part}
		if c.Kind == model.KindAABB {
			d.Kind = "AABB"
			min, max := Vec3DTO(c.Min.Array()), Vec3DTO(c.Max.Array())
			d.Min, d.Max = &min, &max
		} else {
			d.Kind = "CAPSULE"
			ps, pe := Vec3DTO(c.P0.Array()), Vec3DTO(c.P1.Array())
			d.CapStart, d.CapEnd = &ps, &pe
			d.RadiusPermille = mustFixed(fmt.Sprintf("$.Colliders[%d].RadiusPermille", c.Index-1), losses, v, c.Radius, convScalePermille)
		}
		doc.Colliders = append(doc.Colliders, d)
	}
	for i, s := range p.Sweeps {
		rp := mustFixed(fmt.Sprintf("$.Sweeps[%d].RadiusPermille", i), losses, v, s.Radius, convScalePermille)
		dp := mustFixed(fmt.Sprintf("$.Sweeps[%d].DamagePermille", i), losses, v, s.DamageQ, convScalePermille)
		d := SweepV1{No: s.Index, ID: s.ID, StartPosQ: Vec3DTO(s.From.Array()), EndPosQ: Vec3DTO(s.To.Array()), RadiusPermille: rp, DamagePermille: dp, StartFrame: &s.StartFrame, EndFrame: &s.EndFrame}
		doc.Sweeps = append(doc.Sweeps, d)
		path := fmt.Sprintf("$.Sweeps[%d]", i)
		if s.EndpointMode == "relative_delta" {
			loss(path+".EndPosQ", losses, v, fmt.Sprintf("delta=%+v", subVec(s.To, s.From)), "flatten_incremental_endpoint_to_absolute_start_end")
		}
		loss(path+".Penetration", losses, v, fmt.Sprintf("enabled=%t decayQ=%d", s.Penetration, s.DecayQ), "omit_fields_and_apply_v1_declared_default_policy")
		loss(path+".MultiHit", losses, v, fmt.Sprintf("multi=%t maxSegments=%d repeat=%s", s.MultiHit, s.MaxSegments, s.RepeatPolicy), "omit_multi_segment_fields_and_apply_v1_defaults")
	}
	for _, r := range p.Requests {
		frame := r.Frame
		doc.Requests = append(doc.Requests, RequestV1{Frame: &frame, SweepID: r.SweepID})
	}
	return doc, *losses
}

func exportV2(p *model.Project, losses *[]model.LossNotice) (FileV2, []model.LossNotice) {
	v := model.SchemaV2
	step := p.StepMS
	doc := FileV2{Schema: v, BattleID: p.BattleID, FrameStepMS: &step, UnitScale: convScaleMyriad}
	for _, c := range p.Colliders {
		d := ColliderV2{No: c.Index, ID: c.ID, Part: c.Part}
		if c.Kind == model.KindAABB {
			d.Kind = "axis_aligned_box"
			mins, maxs := Vec3DTO(c.Min.Array()), Vec3DTO(c.Max.Array())
			d.MinsQ, d.MaxsQ = &mins, &maxs
		} else {
			d.Kind = "oriented_capsule"
			center := midpoint(c.P0, c.P1)
			half := halfDelta(c.P0, c.P1)
			cq, hq := Vec3DTO(center.Array()), Vec3DTO(half.Array())
			d.CenterQ, d.HalfAxisDeltaQ = &cq, &hq
			d.RadiusMyriad = mustFixed(fmt.Sprintf("$.colliders[%d].radius_myriad", c.Index-1), losses, v, c.Radius, convScaleMyriad)
		}
		doc.Colliders = append(doc.Colliders, d)
	}
	for i, s := range p.Sweeps {
		startMS, err := conv.FrameToMS(int64(s.StartFrame), step)
		if err != nil {
			panic(err)
		}
		endMS, err := conv.FrameToMS(int64(s.EndFrame), step)
		if err != nil {
			panic(err)
		}
		rp := mustFixed(fmt.Sprintf("$.sweeps[%d].radius_myriad", i), losses, v, s.Radius, convScaleMyriad)
		dp := mustFixed(fmt.Sprintf("$.sweeps[%d].damage_myriad", i), losses, v, s.DamageQ, convScaleMyriad)
		decay := mustFixed(fmt.Sprintf("$.sweeps[%d].decay_myriad", i), losses, v, s.DecayQ, convScaleMyriad)
		pen := s.Penetration
		d := SweepV2{No: s.Index, ID: s.ID, OriginQ: Vec3DTO(s.From.Array()), AdvanceDeltaQ: Vec3DTO(subVec(s.To, s.From).Array()), RadiusMyriad: rp, DamageMyriad: dp, StartMS: &startMS, EndMS: &endMS, Penetrate: &pen, DecayMyriad: decay}
		doc.Sweeps = append(doc.Sweeps, d)
		path := fmt.Sprintf("$.sweeps[%d]", i)
		loss(path+".multi_hit", losses, v, fmt.Sprintf("multi=%t maxSegments=%d repeat=%s", s.MultiHit, s.MaxSegments, s.RepeatPolicy), "omit_multi_segment_fields_and_apply_v2_defaults")
	}
	for _, r := range p.Requests {
		atMS, err := conv.FrameToMS(int64(r.Frame), step)
		if err != nil {
			panic(err)
		}
		doc.Requests = append(doc.Requests, RequestV2{AtMS: &atMS, SweepID: r.SweepID})
	}
	return doc, *losses
}

func exportV3(p *model.Project) FileV3 {
	step := p.StepMS
	doc := FileV3{SchemaVersion: model.SchemaV3, BattleID: p.BattleID, FrameStepMS: &step}
	for _, c := range p.Colliders {
		d := ColliderV3{No: c.Index, ID: c.ID, Kind: string(c.Kind), Part: c.Part, Radius: c.Radius}
		if c.Kind == model.KindAABB {
			min, max := c.Min, c.Max
			d.Min, d.Max = &min, &max
		} else {
			p0, p1 := c.P0, c.P1
			d.P0, d.P1 = &p0, &p1
		}
		doc.Colliders = append(doc.Colliders, d)
	}
	for _, s := range p.Sweeps {
		pen := s.Penetration
		multi := s.MultiHit
		maxSeg := s.MaxSegments
		d := SweepV3{
			No: s.Index, ID: s.ID, From: s.From, To: s.To, Radius: s.Radius,
			StartFrame: s.StartFrame, EndFrame: s.EndFrame, Damage: s.DamageQ,
			Penetration: &pen, Decay: &s.DecayQ, MultiHit: &multi, MaxSegments: &maxSeg,
			RepeatPolicy: string(s.RepeatPolicy), EndpointMode: s.EndpointMode,
		}
		doc.Sweeps = append(doc.Sweeps, d)
	}
	for _, r := range p.Requests {
		doc.Requests = append(doc.Requests, RequestV3{Frame: r.Frame, SweepID: r.SweepID})
	}
	return doc
}

func subVec(a, b model.Vec3) model.Vec3 {
	return model.Vec3{X: a.X - b.X, Y: a.Y - b.Y, Z: a.Z - b.Z}
}

func midpoint(a, b model.Vec3) model.Vec3 {
	return model.Vec3{X: (a.X + b.X) / 2, Y: (a.Y + b.Y) / 2, Z: (a.Z + b.Z) / 2}
}

func halfDelta(a, b model.Vec3) model.Vec3 {
	return model.Vec3{X: (b.X - a.X) / 2, Y: (b.Y - a.Y) / 2, Z: (b.Z - a.Z) / 2}
}
