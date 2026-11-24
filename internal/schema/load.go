package schema

import (
	"encoding/json"
	"fmt"

	"cadpipe/internal/cad"
)

type LoadResult struct {
	Scene      *cad.Scene
	Transcript *Transcript
}

func Load(data []byte) (*LoadResult, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, cad.NewError(cad.CodeVersion, "invalid JSON scene: %v", err).
			WithPath("$.schema")
	}
	if head.Schema == "" {
		return nil, cad.NewError(cad.CodeVersion, "missing schema version").
			WithPath("$.schema")
	}
	switch head.Schema {
	case V1:
		return loadV1(data)
	case V2:
		return loadV2(data)
	case V3:
		return loadV3(data)
	default:
		return nil, cad.NewError(cad.CodeVersion, "unsupported schema version %q", head.Schema).
			WithVersion(head.Schema).WithPath("$.schema")
	}
}

func versionError(version, path, format string, args ...any) error {
	return cad.NewError(cad.CodeVersion, format, args...).
		WithVersion(version).WithPath(path)
}

func canonicalUnit(unit string, version, path string) (string, error) {
	switch unit {
	case "mm", "millimeter", "millimetre", "millimeters":
		return "mm", nil
	case "m", "meter", "metre", "meters":
		return "m", nil
	default:
		return "", versionError(version, path, "unsupported length unit %q", unit)
	}
}

func lengthFromUnit(v float64, unit string) cad.Length {
	if unit == "mm" {
		return cad.Millimetre(v)
	}
	return cad.Metre(v)
}

func defaultMaterial() cad.Material {
	return cad.Material{
		Name:      "transcribed-default",
		Color:     cad.RGB{R: 0.62, G: 0.72, B: 0.86},
		Metallic:  0.05,
		Roughness: 0.62,
	}
}

func appendFill(t *Transcript, patchID, path string, original any, filled any, action string) {
	t.Entries = append(t.Entries, TranscriptEntry{
		Code:        "E_TRANSCRIBE",
		PatchID:     patchID,
		Path:        path,
		FromVersion: t.SourceVersion,
		Original:    original,
		Action:      action,
		FilledValue: filled,
	})
}

func checkGrid[T any](grid [][]T, version, path string) error {
	if len(grid) == 0 || len(grid[0]) == 0 {
		return versionError(version, path, "field is required and must be a non-empty rectangular grid")
	}
	width := len(grid[0])
	for i, row := range grid {
		if len(row) == 0 || len(row) != width {
			return versionError(version, fmt.Sprintf("%s[%d]", path, i), "grid must be non-empty and rectangular")
		}
	}
	return nil
}

func checkWeights(weights [][]float64, points [][]cad.Point3, version, path string) error {
	if err := checkGrid(weights, version, path); err != nil {
		return err
	}
	if len(weights) != len(points) || len(weights[0]) != len(points[0]) {
		return versionError(version, path, "weights dimensions %dx%d do not match control grid %dx%d",
			len(weights), len(weights[0]), len(points), len(points[0]))
	}
	return nil
}

func finalizePatch(p *cad.Patch) error {
	if p.DegreeU <= 0 {
		p.DegreeU = len(p.KnotsU) - len(p.ControlPoints) - 1
	}
	if p.DegreeV <= 0 {
		p.DegreeV = len(p.KnotsV) - len(p.ControlPoints[0]) - 1
	}
	if p.Material == nil {
		m := defaultMaterial()
		p.Material = &m
	}
	if p.Subdivision == nil {
		s := cad.DefaultSubdivision()
		p.Subdivision = &s
	}
	return cad.ValidateClip(p.ID, p.Clip)
}

func loadV1(data []byte) (*LoadResult, error) {
	var env envelopeV1
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, versionError(V1, "$", "invalid v1 document: %v", err)
	}
	if env.SceneID == "" {
		return nil, versionError(V1, "$.scene_id", "missing required field scene_id")
	}
	unit, err := canonicalUnit(env.Unit, V1, "$.unit")
	if err != nil {
		return nil, err
	}
	if len(env.Patches) == 0 {
		return nil, versionError(V1, "$.patches", "at least one patch is required")
	}

	tr := &Transcript{SourceVersion: V1}
	scene := &cad.Scene{Schema: V1, SchemaVersion: 1, SceneID: env.SceneID, SourceUnit: unit}
	for i, src := range env.Patches {
		pp := fmt.Sprintf("$.patches[%d]", i)
		if src.PatchID == "" {
			return nil, versionError(V1, pp+".patch_id", "missing required field patch_id")
		}
		if err := checkGrid(src.ControlPoints, V1, pp+".control_points"); err != nil {
			return nil, err
		}
		points := make([][]cad.Point3, len(src.ControlPoints))
		for u := range src.ControlPoints {
			points[u] = make([]cad.Point3, len(src.ControlPoints[u]))
			for v, q := range src.ControlPoints[u] {
				points[u][v] = cad.Point3{
					X: lengthFromUnit(q.X, unit),
					Y: lengthFromUnit(q.Y, unit),
					Z: lengthFromUnit(q.Z, unit),
				}
			}
		}
		if err := checkWeights(src.Weights, points, V1, pp+".weights"); err != nil {
			return nil, err
		}
		if len(src.KnotsU) == 0 {
			return nil, versionError(V1, pp+".knots_u", "missing required field knots_u")
		}
		if len(src.KnotsV) == 0 {
			return nil, versionError(V1, pp+".knots_v", "missing required field knots_v")
		}
		if len(src.ClipBoundary.UVPoints) < 3 {
			return nil, versionError(V1, pp+".clip_boundary.uv_points", "clip boundary requires at least three points")
		}

		patch := &cad.Patch{
			ID: src.PatchID, SourceSchema: V1,
			DegreeU: src.DegreeU, DegreeV: src.DegreeV,
			ControlPoints: points, Weights: src.Weights,
			KnotsU: src.KnotsU, KnotsV: src.KnotsV,
			Clip: cad.ClipBoundary{Loops: [][]cad.Vec2{make([]cad.Vec2, 0, len(src.ClipBoundary.UVPoints))}},
		}
		for _, q := range src.ClipBoundary.UVPoints {
			patch.Clip.Loops[0] = append(patch.Clip.Loops[0], cad.Vec2{U: q.U, V: q.V})
		}
		if src.SurfaceMaterial != nil {
			patch.Material = &cad.Material{
				Name: src.SurfaceMaterial.MaterialID,
				Color: cad.RGB{
					R: src.SurfaceMaterial.BaseColor[0],
					G: src.SurfaceMaterial.BaseColor[1],
					B: src.SurfaceMaterial.BaseColor[2],
				},
				Metallic:  src.SurfaceMaterial.Metallic,
				Roughness: src.SurfaceMaterial.Roughness,
			}
		} else {
			m := defaultMaterial()
			patch.Material = &m
			appendFill(tr, patch.ID, pp+".surface_material", nil, m, "fill_default_material_for_v1")
		}
		s := cad.DefaultSubdivision()
		patch.Subdivision = &s
		appendFill(tr, patch.ID, pp+".subdivision", nil, s, "fill_default_subdivision_absent_in_v1")
		if err := finalizePatch(patch); err != nil {
			return nil, err
		}
		scene.Patches = append(scene.Patches, patch)
	}
	return &LoadResult{Scene: scene, Transcript: tr}, nil
}

func loadV2(data []byte) (*LoadResult, error) {
	var env envelopeV2
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, versionError(V2, "$", "invalid v2 document: %v", err)
	}
	if env.SceneID == "" {
		return nil, versionError(V2, "$.sceneId", "missing required field sceneId")
	}
	unit, err := canonicalUnit(env.LengthUnit, V2, "$.lengthUnit")
	if err != nil {
		return nil, err
	}
	if len(env.Patches) == 0 {
		return nil, versionError(V2, "$.patches", "at least one patch is required")
	}

	tr := &Transcript{SourceVersion: V2}
	scene := &cad.Scene{Schema: V2, SchemaVersion: 2, SceneID: env.SceneID, SourceUnit: unit}
	for i, src := range env.Patches {
		pp := fmt.Sprintf("$.patches[%d]", i)
		if src.PatchID == "" {
			return nil, versionError(V2, pp+".patchId", "missing required field patchId")
		}
		if src.ControlBaseMm == nil {
			return nil, versionError(V2, pp+".controlBaseMm", "missing required field controlBaseMm")
		}
		if err := checkGrid(src.ControlDeltasMm, V2, pp+".controlDeltasMm"); err != nil {
			return nil, err
		}
		base := cad.Point3{
			X: lengthFromUnit(src.ControlBaseMm.X, unit),
			Y: lengthFromUnit(src.ControlBaseMm.Y, unit),
			Z: lengthFromUnit(src.ControlBaseMm.Z, unit),
		}
		points := make([][]cad.Point3, len(src.ControlDeltasMm))
		for u := range src.ControlDeltasMm {
			points[u] = make([]cad.Point3, len(src.ControlDeltasMm[u]))
			for v, d := range src.ControlDeltasMm[u] {
				points[u][v] = cad.Point3{
					X: base.X + lengthFromUnit(d.DX, unit),
					Y: base.Y + lengthFromUnit(d.DY, unit),
					Z: base.Z + lengthFromUnit(d.DZ, unit),
				}
			}
		}
		if err := checkWeights(src.WeightGrid, points, V2, pp+".weightGrid"); err != nil {
			return nil, err
		}
		if len(src.UKnots) == 0 {
			return nil, versionError(V2, pp+".uKnots", "missing required field uKnots")
		}
		if len(src.VKnots) == 0 {
			return nil, versionError(V2, pp+".vKnots", "missing required field vKnots")
		}
		if len(src.Clip.FlatUV) < 6 || len(src.Clip.FlatUV)%2 != 0 {
			return nil, versionError(V2, pp+".clip.flatUV", "clip.flatUV must contain an even number of at least six coordinates")
		}

		patch := &cad.Patch{
			ID: src.PatchID, SourceSchema: V2,
			DegreeU: src.DegreeU, DegreeV: src.DegreeV,
			ControlPoints: points, Weights: src.WeightGrid,
			KnotsU: src.UKnots, KnotsV: src.VKnots,
			Clip: cad.ClipBoundary{Loops: [][]cad.Vec2{{}}},
		}
		for k := 0; k+1 < len(src.Clip.FlatUV); k += 2 {
			patch.Clip.Loops[0] = append(patch.Clip.Loops[0],
				cad.Vec2{U: src.Clip.FlatUV[k], V: src.Clip.FlatUV[k+1]})
		}
		m := defaultMaterial()
		patch.Material = &m
		appendFill(tr, patch.ID, pp+".material", nil, m, "fill_default_material_absent_in_v2")
		s := cad.DefaultSubdivision()
		patch.Subdivision = &s
		appendFill(tr, patch.ID, pp+".subdivision", nil, s, "fill_default_subdivision_absent_in_v2")
		if err := finalizePatch(patch); err != nil {
			return nil, err
		}
		scene.Patches = append(scene.Patches, patch)
	}
	return &LoadResult{Scene: scene, Transcript: tr}, nil
}

func loadV3(data []byte) (*LoadResult, error) {
	var env envelopeV3
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, versionError(V3, "$", "invalid v3 document: %v", err)
	}
	if env.SceneID == "" {
		return nil, versionError(V3, "$.SceneID", "missing required field SceneID")
	}
	unit, err := canonicalUnit(env.LengthUnit, V3, "$.LengthUnit")
	if err != nil {
		return nil, err
	}
	if len(env.Surfaces) == 0 {
		return nil, versionError(V3, "$.Surfaces", "at least one surface is required")
	}

	tr := &Transcript{SourceVersion: V3}
	scene := &cad.Scene{Schema: V3, SchemaVersion: 3, SceneID: env.SceneID, SourceUnit: unit}
	for i, src := range env.Surfaces {
		pp := fmt.Sprintf("$.Surfaces[%d]", i)
		if src.SurfaceID == "" {
			return nil, versionError(V3, pp+".SurfaceID", "missing required field SurfaceID")
		}
		if err := checkGrid(src.ControlGridM, V3, pp+".ControlGridM"); err != nil {
			return nil, err
		}
		points := make([][]cad.Point3, len(src.ControlGridM))
		for u := range src.ControlGridM {
			points[u] = make([]cad.Point3, len(src.ControlGridM[u]))
			for v, q := range src.ControlGridM[u] {
				points[u][v] = cad.Point3{
					X: lengthFromUnit(q.X, unit),
					Y: lengthFromUnit(q.Y, unit),
					Z: lengthFromUnit(q.Z, unit),
				}
			}
		}
		if err := checkWeights(src.Weights, points, V3, pp+".Weights"); err != nil {
			return nil, err
		}
		if len(src.KnotVectors.U) == 0 {
			return nil, versionError(V3, pp+".KnotVectors.U", "missing required field KnotVectors.U")
		}
		if len(src.KnotVectors.V) == 0 {
			return nil, versionError(V3, pp+".KnotVectors.V", "missing required field KnotVectors.V")
		}
		if len(src.Clipping.Rings) == 0 || len(src.Clipping.Rings[0].Vertices) < 3 {
			return nil, versionError(V3, pp+".Clipping.Rings[0].Vertices", "clipping ring requires at least three vertices")
		}

		patch := &cad.Patch{
			ID: src.SurfaceID, SourceSchema: V3,
			DegreeU: src.UDegree, DegreeV: src.VDegree,
			ControlPoints: points, Weights: src.Weights,
			KnotsU: src.KnotVectors.U, KnotsV: src.KnotVectors.V,
			Clip: cad.ClipBoundary{Loops: make([][]cad.Vec2, 0, len(src.Clipping.Rings))},
		}
		for _, ring := range src.Clipping.Rings {
			loop := make([]cad.Vec2, 0, len(ring.Vertices))
			for _, q := range ring.Vertices {
				loop = append(loop, cad.Vec2{U: q.U, V: q.V})
			}
			patch.Clip.Loops = append(patch.Clip.Loops, loop)
		}
		if src.Material != nil {
			patch.Material = &cad.Material{
				Name: src.Material.Name,
				Color: cad.RGB{
					R: src.Material.ColorRGB[0],
					G: src.Material.ColorRGB[1],
					B: src.Material.ColorRGB[2],
				},
				Metallic:  src.Material.MetallicFactor,
				Roughness: src.Material.RoughnessFactor,
			}
		} else {
			m := defaultMaterial()
			patch.Material = &m
			appendFill(tr, patch.ID, pp+".Material", nil, m, "fill_default_material")
		}
		if src.Subdivision != nil {
			patch.Subdivision = &cad.Subdivision{
				Density:              src.Subdivision.Density,
				CurvatureSensitivity: src.Subdivision.CurvatureSensitivity,
				MaxDensity:           src.Subdivision.MaxDensity,
				Tolerance:            src.Subdivision.Tolerance,
			}
		} else {
			s := cad.DefaultSubdivision()
			patch.Subdivision = &s
			appendFill(tr, patch.ID, pp+".Subdivision", nil, s, "fill_default_subdivision")
		}
		if err := finalizePatch(patch); err != nil {
			return nil, err
		}
		scene.Patches = append(scene.Patches, patch)
	}
	return &LoadResult{Scene: scene, Transcript: tr}, nil
}
