package schema

import (
	"encoding/json"

	"cadpipe/internal/cad"
)

func Export(scene *cad.Scene, target string) ([]byte, *LossReport, error) {
	report := &LossReport{TargetVersion: target}
	var doc any
	switch target {
	case V1:
		doc = exportV1(scene, report)
	case V2:
		doc = exportV2(scene, report)
	case V3:
		doc = exportV3(scene, report)
	default:
		return nil, nil, cad.NewError(cad.CodeVersion, "unsupported export schema %q", target).
			WithVersion(target).WithPath("$.schema")
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, report, err
	}
	return append(data, '\n'), report, nil
}

func exportV1(scene *cad.Scene, report *LossReport) envelopeV1 {
	env := envelopeV1{Schema: V1, SceneID: scene.SceneID, Unit: "mm"}
	for i, p := range scene.Patches {
		dst := patchV1{
			PatchID: p.ID, DegreeU: p.DegreeU, DegreeV: p.DegreeV,
			KnotsU:  append([]float64(nil), p.KnotsU...),
			KnotsV:  append([]float64(nil), p.KnotsV...),
			Weights: cloneWeights(p.Weights),
		}
		dst.ControlPoints = make([][]pointV1, len(p.ControlPoints))
		for u := range p.ControlPoints {
			dst.ControlPoints[u] = make([]pointV1, len(p.ControlPoints[u]))
			for v, q := range p.ControlPoints[u] {
				dst.ControlPoints[u][v] = pointV1{X: q.X.Millimetres(), Y: q.Y.Millimetres(), Z: q.Z.Millimetres()}
			}
		}
		if loop := p.Clip.MainLoop(); len(loop) > 0 {
			dst.ClipBoundary.UVPoints = make([]uvV1, 0, len(loop))
			for _, q := range loop {
				dst.ClipBoundary.UVPoints = append(dst.ClipBoundary.UVPoints, uvV1{U: q.U, V: q.V})
			}
		}
		if p.Material != nil {
			dst.SurfaceMaterial = &materialV1{
				MaterialID: p.Material.Name,
				BaseColor:  [3]float64{p.Material.Color.R, p.Material.Color.G, p.Material.Color.B},
				Metallic:   p.Material.Metallic,
				Roughness:  p.Material.Roughness,
			}
		}
		if p.Subdivision != nil {
			report.add(LossEntry{
				PatchID:     p.ID,
				Path:        "$.patches[" + itoa(i) + "].subdivision",
				Field:       "subdivision",
				Original:    *p.Subdivision,
				Disposition: "omit_unsupported_field_and_use_v1_importer_default",
			})
		}
		env.Patches = append(env.Patches, dst)
	}
	return env
}

func exportV2(scene *cad.Scene, report *LossReport) envelopeV2 {
	env := envelopeV2{Schema: V2, SceneID: scene.SceneID, LengthUnit: "mm"}
	for i, p := range scene.Patches {
		base := p.ControlPoints[0][0]
		dst := patchV2{
			PatchID: p.ID, DegreeU: p.DegreeU, DegreeV: p.DegreeV,
			ControlBaseMm: &pointV2{X: base.X.Millimetres(), Y: base.Y.Millimetres(), Z: base.Z.Millimetres()},
			UKnots:        append([]float64(nil), p.KnotsU...),
			VKnots:        append([]float64(nil), p.KnotsV...),
			WeightGrid:    cloneWeights(p.Weights),
		}
		dst.ControlDeltasMm = make([][]deltaV2, len(p.ControlPoints))
		for u := range p.ControlPoints {
			dst.ControlDeltasMm[u] = make([]deltaV2, len(p.ControlPoints[u]))
			for v, q := range p.ControlPoints[u] {
				dx := q.X - base.X
				dy := q.Y - base.Y
				dz := q.Z - base.Z
				dst.ControlDeltasMm[u][v] = deltaV2{DX: dx.Millimetres(), DY: dy.Millimetres(), DZ: dz.Millimetres()}
			}
		}
		if loop := p.Clip.MainLoop(); len(loop) > 0 {
			dst.Clip.FlatUV = make([]float64, 0, len(loop)*2)
			for _, q := range loop {
				dst.Clip.FlatUV = append(dst.Clip.FlatUV, q.U, q.V)
			}
		}
		if p.Material != nil {
			report.add(LossEntry{
				PatchID: p.ID, Path: "$.patches[" + itoa(i) + "].material", Field: "material",
				Original: *p.Material, Disposition: "omit_unsupported_field_and_use_v2_importer_default",
			})
		}
		if p.Subdivision != nil {
			report.add(LossEntry{
				PatchID: p.ID, Path: "$.patches[" + itoa(i) + "].subdivision", Field: "subdivision",
				Original: *p.Subdivision, Disposition: "omit_unsupported_field_and_use_v2_importer_default",
			})
		}
		env.Patches = append(env.Patches, dst)
	}
	return env
}

func exportV3(scene *cad.Scene, report *LossReport) envelopeV3 {
	env := envelopeV3{Schema: V3, SceneID: scene.SceneID, LengthUnit: "m"}
	for _, p := range scene.Patches {
		dst := patchV3{
			SurfaceID: p.ID, UDegree: p.DegreeU, VDegree: p.DegreeV,
			Weights: cloneWeights(p.Weights),
			KnotVectors: knotVectorsV3{
				U: append([]float64(nil), p.KnotsU...),
				V: append([]float64(nil), p.KnotsV...),
			},
		}
		dst.ControlGridM = make([][]pointV3, len(p.ControlPoints))
		for u := range p.ControlPoints {
			dst.ControlGridM[u] = make([]pointV3, len(p.ControlPoints[u]))
			for v, q := range p.ControlPoints[u] {
				dst.ControlGridM[u][v] = pointV3{X: q.X.Metres(), Y: q.Y.Metres(), Z: q.Z.Metres()}
			}
		}
		dst.Clipping.Rings = make([]ringV3, 0, len(p.Clip.Loops))
		for _, loop := range p.Clip.Loops {
			ring := ringV3{Vertices: make([]uvV3, 0, len(loop))}
			for _, q := range loop {
				ring.Vertices = append(ring.Vertices, uvV3{U: q.U, V: q.V})
			}
			dst.Clipping.Rings = append(dst.Clipping.Rings, ring)
		}
		if p.Material != nil {
			dst.Material = &materialV3{
				Name:           p.Material.Name,
				ColorRGB:       [3]float64{p.Material.Color.R, p.Material.Color.G, p.Material.Color.B},
				MetallicFactor: p.Material.Metallic, RoughnessFactor: p.Material.Roughness,
			}
		}
		if p.Subdivision != nil {
			dst.Subdivision = &subdivisionV3{
				Density:              p.Subdivision.Density,
				CurvatureSensitivity: p.Subdivision.CurvatureSensitivity,
				MaxDensity:           p.Subdivision.MaxDensity,
				Tolerance:            p.Subdivision.Tolerance,
			}
		}
		env.Surfaces = append(env.Surfaces, dst)
	}
	return env
}

func cloneWeights(in [][]float64) [][]float64 {
	if in == nil {
		return nil
	}
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = append([]float64(nil), in[i]...)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
