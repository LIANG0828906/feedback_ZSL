package tessellate

import (
	"fmt"
	"math"

	"cadpipe/internal/cad"
)

const (
	MinDensity       = 1
	MaxDensity       = 256
	AbsoluteMaxMesh  = 512
	AlgorithmVersion = "cadpipe-algo-v1"
)

type Vertex struct {
	Position  cad.Vec3 `json:"position"`
	Normal    cad.Vec3 `json:"normal"`
	UV        cad.Vec2 `json:"uv"`
	Curvature float64  `json:"curvature"`
}

type Mesh struct {
	PatchID   string          `json:"patch_id"`
	SegmentsU int             `json:"segments_u"`
	SegmentsV int             `json:"segments_v"`
	Vertices  []Vertex        `json:"vertices"`
	Triangles [][3]int        `json:"triangles"`
	Params    cad.Subdivision `json:"params"`
}

func ValidateParameters(p cad.Subdivision) error {
	if p.Density < MinDensity || p.Density > MaxDensity {
		return cad.NewError(cad.CodeParam, "density %d outside allowed range [%d,%d]",
			p.Density, MinDensity, MaxDensity).
			WithPath("subdivision.density").
			WithField("density")
	}
	if p.MaxDensity < p.Density || p.MaxDensity > AbsoluteMaxMesh {
		return cad.NewError(cad.CodeParam, "max_density %d must be between density %d and %d",
			p.MaxDensity, p.Density, AbsoluteMaxMesh).
			WithPath("subdivision.max_density").
			WithField("max_density")
	}
	if math.IsNaN(p.CurvatureSensitivity) || p.CurvatureSensitivity < 0 || p.CurvatureSensitivity > 100 {
		return cad.NewError(cad.CodeParam, "curvature_sensitivity %v outside [0,100]", p.CurvatureSensitivity).
			WithPath("subdivision.curvature_sensitivity").
			WithField("curvature_sensitivity")
	}
	if math.IsNaN(p.Tolerance) || p.Tolerance < 0 {
		return cad.NewError(cad.CodeParam, "tolerance %v must be zero (disabled) or positive", p.Tolerance).
			WithPath("subdivision.tolerance").
			WithField("tolerance")
	}
	return nil
}

func ValidateKnots(p *cad.Patch) error {
	if err := knotMonotonic(p.ID, "knots_u", p.KnotsU); err != nil {
		return err
	}
	if err := knotMonotonic(p.ID, "knots_v", p.KnotsV); err != nil {
		return err
	}
	expectedU := len(p.ControlPoints) + p.DegreeU + 1
	if len(p.KnotsU) != expectedU {
		return cad.NewError(cad.CodeKnot, "knots_u length %d does not match control count/degree (expected %d)",
			len(p.KnotsU), expectedU).WithPatch(p.ID).WithPath("knots_u")
	}
	expectedV := len(p.ControlPoints[0]) + p.DegreeV + 1
	if len(p.KnotsV) != expectedV {
		return cad.NewError(cad.CodeKnot, "knots_v length %d does not match control count/degree (expected %d)",
			len(p.KnotsV), expectedV).WithPatch(p.ID).WithPath("knots_v")
	}
	return nil
}

func knotMonotonic(patchID, path string, knots []float64) error {
	for i := 1; i < len(knots); i++ {
		if knots[i] < knots[i-1] {
			return cad.NewError(cad.CodeKnot, "knot vector is non-monotonic at index %d: %v < %v",
				i, knots[i], knots[i-1]).
				WithPatch(patchID).WithPath(fmt.Sprintf("%s[%d]", path, i))
		}
	}
	return nil
}

func EffectiveParameters(p *cad.Patch, override *cad.Subdivision) cad.Subdivision {
	if override != nil {
		return *override
	}
	if p.Subdivision != nil {
		return *p.Subdivision
	}
	return cad.DefaultSubdivision()
}

func Tessellate(p *cad.Patch, override *cad.Subdivision) (*Mesh, error) {
	params := EffectiveParameters(p, override)
	if err := ValidateParameters(params); err != nil {
		if ce, ok := cad.AsError(err); ok && ce.PatchID == "" {
			ce.PatchID = p.ID
		}
		return nil, err
	}
	if err := ValidateKnots(p); err != nil {
		return nil, err
	}

	maxSecond := maximumSecondDerivative(p)
	segU := segmentCount(params, maxSecond)
	segV := segU

	mesh := &Mesh{PatchID: p.ID, SegmentsU: segU, SegmentsV: segV, Params: params}
	row := segV + 1
	mesh.Vertices = make([]Vertex, 0, (segU+1)*(segV+1))
	for i := 0; i <= segU; i++ {
		u := float64(i) / float64(segU)
		for j := 0; j <= segV; j++ {
			v := float64(j) / float64(segV)
			mesh.Vertices = append(mesh.Vertices, Vertex{
				Position:  Evaluate(p, u, v),
				Normal:    NormalAt(p, u, v),
				UV:        cad.Vec2{U: u, V: v},
				Curvature: CurvatureAt(p, u, v),
			})
		}
	}
	for i := 0; i < segU; i++ {
		for j := 0; j < segV; j++ {
			a := i*row + j
			b := (i+1)*row + j
			c := i*row + j + 1
			d := (i+1)*row + j + 1
			mesh.Triangles = append(mesh.Triangles, [3]int{a, b, d}, [3]int{a, d, c})
		}
	}
	return mesh, nil
}

func maximumSecondDerivative(p *cad.Patch) float64 {
	var maxK float64
	const samples = 7
	for i := 0; i <= samples; i++ {
		u := float64(i) / samples
		for j := 0; j <= samples; j++ {
			v := float64(j) / samples
			_, _, duu, dvv, duv := partialDerivatives(p, u, v)
			k := math.Max(math.Abs(duu.X)+math.Abs(duu.Y)+math.Abs(duu.Z),
				math.Abs(dvv.X)+math.Abs(dvv.Y)+math.Abs(dvv.Z))
			k += math.Abs(duv.X) + math.Abs(duv.Y) + math.Abs(duv.Z)
			if k > maxK {
				maxK = k
			}
		}
	}
	return math.Max(maxK, 1e-9)
}

func segmentCount(params cad.Subdivision, secondDerivative float64) int {
	seg := params.Density
	if params.CurvatureSensitivity > 0 {
		curvatureRequired := int(math.Ceil(params.CurvatureSensitivity * math.Sqrt(secondDerivative)))
		if curvatureRequired > seg {
			seg = curvatureRequired
		}
	}
	if params.Tolerance > 0 {
		toleranceRequired := int(math.Ceil(math.Sqrt(secondDerivative / (8 * params.Tolerance))))
		power := 1
		for power < toleranceRequired {
			power *= 2
		}
		if power > seg {
			seg = power
		}
	}
	if seg < params.Density {
		seg = params.Density
	}
	if seg > params.MaxDensity {
		seg = params.MaxDensity
	}
	if seg < 1 {
		seg = 1
	}
	return seg
}

func FaceCount(m *Mesh) int { return len(m.Triangles) }
