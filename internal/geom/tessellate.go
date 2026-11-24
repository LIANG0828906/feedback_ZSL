package geom

import (
	"math"

	"cadlab/internal/model"
)

const (
	MinDensity = 2
	MaxDensity = 128
)

type TessOptions struct {
	Density          int
	CurvatureScale   float64
	NormalTolDegrees float64
	MaxDensity       int
}

type Vertex struct {
	Position  model.Vec3 `json:"position"`
	Normal    model.Vec3 `json:"normal"`
	UV        model.Vec2 `json:"uv"`
	Curvature float64    `json:"curvature"`
}

type SurfaceMesh struct {
	PatchID        int      `json:"patchId"`
	BaseDensity    int      `json:"baseDensity"`
	DensityU       int      `json:"densityU"`
	DensityV       int      `json:"densityV"`
	Vertices       []Vertex `json:"vertices"`
	Indices        []int    `json:"indices"`
	TrimmedTris    int      `json:"trimmedTriangles"`
	TriangleCount  int      `json:"triangleCount"`
	EstimatedBytes int64    `json:"estimatedBytes"`
}

func ValidateTessOptions(o TessOptions) *model.Diagnostic {
	if o.Density < MinDensity || o.Density > MaxDensity {
		return model.DiagPatch(model.CodePara, -1, "density", "density must be within [2,128]")
	}
	if math.IsNaN(o.CurvatureScale) || math.IsInf(o.CurvatureScale, 0) || o.CurvatureScale < 0 || o.CurvatureScale > 10 {
		return model.DiagPatch(model.CodePara, -1, "curvatureScale", "curvatureScale must be within [0,10]")
	}
	if math.IsNaN(o.NormalTolDegrees) || math.IsInf(o.NormalTolDegrees, 0) ||
		o.NormalTolDegrees <= 0 || o.NormalTolDegrees > 45 {
		return model.DiagPatch(model.CodePara, -1, "normalToleranceDeg", "normalToleranceDeg must be within (0,45]")
	}
	if o.MaxDensity != 0 && (o.MaxDensity < MinDensity || o.MaxDensity > MaxDensity) {
		return model.DiagPatch(model.CodePara, -1, "maxDensity", "maxDensity must be within [2,128]")
	}
	return nil
}

func Tessellate(p model.SurfacePatch, o TessOptions) (*SurfaceMesh, *model.Diagnostic) {
	if d := ValidateTessOptions(o); d != nil {
		d.PatchID = p.ID
		return nil, d
	}
	if o.MaxDensity == 0 {
		o.MaxDensity = MaxDensity
	}
	curv := maxCurvature(p)
	factor := 1 + o.CurvatureScale*curv/(1+curv)
	density := int(math.Ceil(float64(o.Density) * factor))
	if density < o.Density {
		density = o.Density
	}
	if density > o.MaxDensity {
		density = o.MaxDensity
	}
	du0, du1, dv0, dv1 := p.Domain()
	mesh := &SurfaceMesh{
		PatchID: p.ID, BaseDensity: o.Density, DensityU: density, DensityV: density,
		Vertices: make([]Vertex, (density+1)*(density+1)),
	}
	for iv := 0; iv <= density; iv++ {
		v := dv0 + (dv1-dv0)*float64(iv)/float64(density)
		for iu := 0; iu <= density; iu++ {
			u := du0 + (du1-du0)*float64(iu)/float64(density)
			e := Evaluate(p, u, v)
			mesh.Vertices[iv*(density+1)+iu] = Vertex{
				Position: e.Position, Normal: e.Normal, UV: model.Vec2{U: u, V: v}, Curvature: e.Curvature,
			}
		}
	}
	for iv := 0; iv < density; iv++ {
		for iu := 0; iu < density; iu++ {
			i0 := iv*(density+1) + iu
			i1 := i0 + 1
			i2 := i0 + density + 1
			i3 := i2 + 1
			if !triangleVisible(mesh.Vertices[i0], mesh.Vertices[i1], mesh.Vertices[i3], p.Trim) {
				mesh.TrimmedTris++
			} else {
				mesh.Indices = append(mesh.Indices, i0, i2, i3)
			}
			if !triangleVisible(mesh.Vertices[i0], mesh.Vertices[i3], mesh.Vertices[i2], p.Trim) {
				mesh.TrimmedTris++
			} else {
				mesh.Indices = append(mesh.Indices, i0, i3, i1)
			}
		}
	}
	mesh.TriangleCount = len(mesh.Indices) / 3
	mesh.EstimatedBytes = int64(len(mesh.Vertices)*9*8 + len(mesh.Indices)*4)
	return mesh, nil
}

func triangleVisible(a, b, c Vertex, trim []model.Vec2) bool {
	if len(trim) < 4 {
		return true
	}
	center := model.Vec2{U: (a.UV.U + b.UV.U + c.UV.U) / 3, V: (a.UV.V + b.UV.V + c.UV.V) / 3}
	if PointInTrim(center, trim) {
		return true
	}
	return PointInTrim(a.UV, trim) || PointInTrim(b.UV, trim) || PointInTrim(c.UV, trim)
}

func PointInTrim(p model.Vec2, poly []model.Vec2) bool {
	inside := false
	j := len(poly) - 1
	for i := 0; i < len(poly); i++ {
		yi, yj := poly[i].V, poly[j].V
		xi, xj := poly[i].U, poly[j].U
		if (yi > p.V) != (yj > p.V) {
			x := (xj-xi)*(p.V-yi)/(yj-yi) + xi
			if p.U < x {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

func maxCurvature(p model.SurfacePatch) float64 {
	du0, du1, dv0, dv1 := p.Domain()
	maxK := 0.0
	for iv := 0; iv <= 8; iv++ {
		v := dv0 + (dv1-dv0)*float64(iv)/8
		for iu := 0; iu <= 8; iu++ {
			u := du0 + (du1-du0)*float64(iu)/8
			k := Evaluate(p, u, v).Curvature
			if k > maxK {
				maxK = k
			}
		}
	}
	return maxK
}

func Bounds(mesh *SurfaceMesh) model.BBox {
	b := model.EmptyBBox()
	for _, v := range mesh.Vertices {
		b.Add(v.Position)
	}
	return b
}
