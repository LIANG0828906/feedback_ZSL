package spatialpick

import (
	"fmt"
	"math"
)

type TransformStepError struct {
	Step           int     `json:"step"`
	NodeID         string  `json:"node_id"`
	Depth          int     `json:"depth"`
	QuantumError   float64 `json:"quantum_error"`
	Cumulative     float64 `json:"cumulative"`
	MatrixInfError float64 `json:"matrix_inf_error"`
}

// Derived is immutable after publication. Unaffected nodes reuse the same
// pointer across edits.
type Derived struct {
	NodeID          string               `json:"node_id"`
	Depth           int                  `json:"depth"`
	LocalVersion    uint64               `json:"local_version"`
	Version         uint64               `json:"version"`
	World           FixedMatrix          `json:"world"`
	WorldInverse    FixedMatrix          `json:"world_inverse"`
	WorldFloat      Mat4                 `json:"world_float"`
	WorldInvFloat   Mat4                 `json:"world_inv_float"`
	AABB            AABB                 `json:"aabb"`
	Sphere          Sphere               `json:"sphere"`
	GeometryIndex   int                  `json:"geometry_index"`
	GeometryID      string               `json:"geometry_id,omitempty"`
	TransformError  float64              `json:"transform_error"`
	GeometryError   float64              `json:"geometry_error"`
	ErrorTrace      []TransformStepError `json:"error_trace"`
	Failed          bool                 `json:"failed"`
	Failure         string               `json:"failure,omitempty"`
	ExpectedVersion uint64               `json:"expected_version"`
}

func QuantizedIdentity(precision int) FixedMatrix {
	scale, _ := quantizeFloat(1, precision)
	return FixedMatrix{scale, 0, 0, 0, 0, scale, 0, 0, 0, 0, scale, 0, 0, 0, 0, scale}
}

func BuildDerived(node *Node, local FixedMatrix, localVersion, version uint64, parent *Derived, geometry *Geometry, precision int) (*Derived, error) {
	depth := 0
	if parent != nil {
		depth = parent.Depth + 1
	}
	var world FixedMatrix
	var err error
	parentNorm := 1.0
	parentErr := 0.0
	parentTrace := []TransformStepError(nil)
	if parent != nil {
		world, err = MultiplyFixed(parent.World, local, precision)
		parentNorm = parent.WorldFloat.MaxRowAbsSum()
		parentErr = parent.TransformError
		parentTrace = parent.ErrorTrace
	} else {
		parentWorld := QuantizedIdentity(precision)
		world, err = MultiplyFixed(parentWorld, local, precision)
	}
	if err != nil {
		var ce *CodedError
		if IsCoded(err, &ce) {
			ce.NodeID = node.ID
			ce.Field = "world"
		}
		return nil, err
	}
	worldInv, err := InverseFixedAffine(world, precision)
	if err != nil {
		var ce *CodedError
		if IsCoded(err, &ce) {
			ce.NodeID = node.ID
			ce.Field = "world"
		}
		return nil, err
	}
	worldFloat := world.Float(precision)
	worldInvFloat := worldInv.Float(precision)
	localFloat := local.Float(precision)
	q := math.Pow10(-precision)
	localNorm := localFloat.MaxRowAbsSum()
	stepErr := q * (0.5 + 0.5*localNorm + 0.5*parentNorm)
	cumulative := parentErr + stepErr
	trace := make([]TransformStepError, 0, len(parentTrace)+1)
	trace = append(trace, parentTrace...)
	trace = append(trace, TransformStepError{
		Step:           len(trace),
		NodeID:         node.ID,
		Depth:          depth,
		QuantumError:   0.5 * q,
		Cumulative:     cumulative,
		MatrixInfError: worldFloat.MaxRowAbsSum(),
	})

	d := &Derived{
		NodeID:         node.ID,
		Depth:          depth,
		LocalVersion:   localVersion,
		Version:        version,
		World:          world,
		WorldInverse:   worldInv,
		WorldFloat:     worldFloat,
		WorldInvFloat:  worldInvFloat,
		GeometryIndex:  node.GeometryIndex,
		TransformError: cumulative,
		ErrorTrace:     trace,
	}
	if geometry != nil {
		d.GeometryID = geometry.ID
		worldAABB, worldSphere := WorldBounds(*geometry, worldFloat)
		d.AABB = worldAABB
		d.Sphere = worldSphere
		d.GeometryError = geometryErrorBound(*geometry, d, precision)
	} else {
		center := worldFloat.TransformPoint(Vec3{})
		d.AABB = AABB{Min: center, Max: center}
		d.Sphere = Sphere{Center: center}
		d.GeometryError = cumulative
	}
	return d, nil
}

func geometryErrorBound(g Geometry, d *Derived, precision int) float64 {
	q := math.Pow10(-precision)
	lb := GeometryLocalBounds(g)
	localSize := lb.Diagonal()
	maxAbs := 0.0
	for _, v := range []float64{lb.Min.X, lb.Min.Y, lb.Min.Z, lb.Max.X, lb.Max.Y, lb.Max.Z} {
		if a := math.Abs(v); a > maxAbs {
			maxAbs = a
		}
	}
	return d.TransformError*(3+3*localSize+3*maxAbs) + q*float64(d.Depth+2)
}

func GeometryLocalBounds(g Geometry) AABB {
	switch g.Kind {
	case GeometryAABB:
		return AABB{Min: g.Min, Max: g.Max}
	case GeometrySphere:
		r := V3(g.Radius, g.Radius, g.Radius)
		return AABB{Min: g.Center.Sub(r), Max: g.Center.Add(r)}
	case GeometryMesh:
		if len(g.Vertices) == 0 {
			return AABB{Min: Vec3{}, Max: Vec3{}}
		}
		a := AABB{Min: g.Vertices[0], Max: g.Vertices[0]}
		for _, v := range g.Vertices[1:] {
			a = a.Union(AABB{Min: v, Max: v})
		}
		return a
	default:
		return AABB{Min: Vec3{}, Max: Vec3{}}
	}
}

func WorldBounds(g Geometry, world Mat4) (AABB, Sphere) {
	lb := GeometryLocalBounds(g)
	corners := []Vec3{
		{lb.Min.X, lb.Min.Y, lb.Min.Z},
		{lb.Max.X, lb.Min.Y, lb.Min.Z},
		{lb.Min.X, lb.Max.Y, lb.Min.Z},
		{lb.Max.X, lb.Max.Y, lb.Min.Z},
		{lb.Min.X, lb.Min.Y, lb.Max.Z},
		{lb.Max.X, lb.Min.Y, lb.Max.Z},
		{lb.Min.X, lb.Max.Y, lb.Max.Z},
		{lb.Max.X, lb.Max.Y, lb.Max.Z},
	}
	a := EmptyAABB()
	worldCorners := make([]Vec3, 0, len(corners))
	for _, c := range corners {
		wc := world.TransformPoint(c)
		worldCorners = append(worldCorners, wc)
		a = a.Union(AABB{Min: wc, Max: wc})
	}
	center := a.Center()
	radius := 0.0
	for _, c := range worldCorners {
		if r := c.Sub(center).Length(); r > radius {
			radius = r
		}
	}
	if g.Kind == GeometrySphere {
		center = world.TransformPoint(g.Center)
		sx := V3(world[0], world[4], world[8]).Length()
		sy := V3(world[1], world[5], world[9]).Length()
		sz := V3(world[2], world[6], world[10]).Length()
		radius = g.Radius * math.Max(sx, math.Max(sy, sz))
	}
	return a, Sphere{Center: center, Radius: radius}
}

func validateDerived(d *Derived, scene *Scene) error {
	if d == nil {
		return coded(CodeSchema, "validate_snapshot", "nil derived entry")
	}
	if !finite(d.TransformError) || d.TransformError < 0 || !finite(d.GeometryError) || d.GeometryError < 0 {
		return &CodedError{Code: CodeSchema, Op: "validate_snapshot", NodeID: d.NodeID, Field: "error_bound", Message: "invalid error bound"}
	}
	if !d.AABB.IsValid() || !finite(d.Sphere.Radius) {
		return &CodedError{Code: CodeSchema, Op: "validate_snapshot", NodeID: d.NodeID, Field: "bounds", Message: "invalid bounds"}
	}
	if !d.World.IsAffine(scene.Precision) || !d.WorldInverse.IsAffine(scene.Precision) {
		return &CodedError{Code: CodeSchema, Op: "validate_snapshot", NodeID: d.NodeID, Field: "world", Message: "non-affine fixed matrix"}
	}
	if d.Depth < 0 || len(d.ErrorTrace) == 0 {
		return &CodedError{Code: CodeSchema, Op: "validate_snapshot", NodeID: d.NodeID, Field: "error_trace", Message: "invalid transform trace"}
	}
	return nil
}

func traceString(d *Derived) string {
	if d == nil || len(d.ErrorTrace) == 0 {
		return ""
	}
	last := d.ErrorTrace[len(d.ErrorTrace)-1]
	return fmt.Sprintf("depth=%d cumulative=%.12g quantum=%.12g", last.Depth, last.Cumulative, last.QuantumError)
}
