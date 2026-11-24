package render

import (
	"fmt"
	"math"

	"diffrender/errs"
	"diffrender/scene"
)

// ParamKind identifies a differentiable scene property.
type ParamKind uint8

const (
	ParamCenterX ParamKind = iota
	ParamCenterY
	ParamCenterZ
	ParamRadius
	ParamAlbedoR
	ParamAlbedoG
	ParamAlbedoB
	ParamPower
)

// ParamSpec maps one entry of the flat gradient vector to scene storage.
type ParamSpec struct {
	ID      string
	Kind    ParamKind
	Sphere  int
	Plane   int
	Light   int
	Channel int
}

// ParameterSpecs returns the stable optimization order:
// sphere center xyz, radius, sphere albedo rgb, plane albedo rgb, light power.
func ParameterSpecs(s *scene.Scene) ([]ParamSpec, error) {
	specs := make([]ParamSpec, 0)
	for i, sp := range s.Spheres {
		specs = append(specs,
			ParamSpec{ID: sp.ID + ".center.x", Kind: ParamCenterX, Sphere: i, Plane: -1, Light: -1, Channel: 0},
			ParamSpec{ID: sp.ID + ".center.y", Kind: ParamCenterY, Sphere: i, Plane: -1, Light: -1, Channel: 1},
			ParamSpec{ID: sp.ID + ".center.z", Kind: ParamCenterZ, Sphere: i, Plane: -1, Light: -1, Channel: 2},
			ParamSpec{ID: sp.ID + ".radius", Kind: ParamRadius, Sphere: i, Plane: -1, Light: -1},
			ParamSpec{ID: sp.ID + ".albedo.r", Kind: ParamAlbedoR, Sphere: i, Plane: -1, Light: -1, Channel: 0},
			ParamSpec{ID: sp.ID + ".albedo.g", Kind: ParamAlbedoG, Sphere: i, Plane: -1, Light: -1, Channel: 1},
			ParamSpec{ID: sp.ID + ".albedo.b", Kind: ParamAlbedoB, Sphere: i, Plane: -1, Light: -1, Channel: 2},
		)
	}
	for i, pl := range s.Planes {
		specs = append(specs,
			ParamSpec{ID: pl.ID + ".albedo.r", Kind: ParamAlbedoR, Plane: i, Sphere: -1, Light: -1, Channel: 0},
			ParamSpec{ID: pl.ID + ".albedo.g", Kind: ParamAlbedoG, Plane: i, Sphere: -1, Light: -1, Channel: 1},
			ParamSpec{ID: pl.ID + ".albedo.b", Kind: ParamAlbedoB, Plane: i, Sphere: -1, Light: -1, Channel: 2},
		)
	}
	for i, li := range s.Lights {
		specs = append(specs, ParamSpec{ID: li.ID + ".radiantPowerW", Kind: ParamPower, Sphere: -1, Plane: -1, Light: i})
	}
	if len(specs) > MaxParams {
		return nil, errs.New(errs.CodeParam, "<render>", "parameters", "%d differentiable parameters exceed limit %d", len(specs), MaxParams)
	}
	return specs, nil
}

// ParameterValues extracts values in ParameterSpecs order.
func ParameterValues(s *scene.Scene, specs []ParamSpec) []float64 {
	values := make([]float64, len(specs))
	for i, spec := range specs {
		switch {
		case spec.Light >= 0:
			values[i] = s.Lights[spec.Light].RadiantPowerW
		case spec.Plane >= 0:
			v := s.Planes[spec.Plane].Albedo
			values[i] = vecChannel(v, spec.Channel)
		case spec.Sphere >= 0:
			sp := s.Spheres[spec.Sphere]
			switch spec.Kind {
			case ParamCenterX:
				values[i] = sp.Center.X
			case ParamCenterY:
				values[i] = sp.Center.Y
			case ParamCenterZ:
				values[i] = sp.Center.Z
			case ParamRadius:
				values[i] = sp.Radius
			case ParamAlbedoR, ParamAlbedoG, ParamAlbedoB:
				values[i] = vecChannel(sp.Albedo, spec.Channel)
			}
		}
	}
	return values
}

// ApplyParameterValues validates the candidate vector before mutating the scene.
func ApplyParameterValues(s *scene.Scene, specs []ParamSpec, values []float64) error {
	if len(values) != len(specs) {
		return errs.New(errs.CodeParam, "<render>", "parameters", "expected %d values, got %d", len(specs), len(values))
	}
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errs.New(errs.CodeGradient, "<render>", specs[i].ID, "parameter is non-finite")
		}
		switch {
		case specs[i].Light >= 0:
			if v <= 0 {
				return errs.New(errs.CodeParam, "<render>", specs[i].ID, "light power must be > 0")
			}
		case specs[i].Plane >= 0:
			if v < 0 || v > 1 {
				return errs.New(errs.CodeParam, "<render>", specs[i].ID, "albedo must be in [0,1]")
			}
		case specs[i].Sphere >= 0:
			switch specs[i].Kind {
			case ParamRadius:
				if v <= 1e-8 {
					return errs.New(errs.CodeParam, "<render>", specs[i].ID, "radius must be > 1e-8")
				}
			case ParamAlbedoR, ParamAlbedoG, ParamAlbedoB:
				if v < 0 || v > 1 {
					return errs.New(errs.CodeParam, "<render>", specs[i].ID, "albedo must be in [0,1]")
				}
			}
		}
	}
	for i, spec := range specs {
		v := values[i]
		switch {
		case spec.Light >= 0:
			s.Lights[spec.Light].RadiantPowerW = v
		case spec.Plane >= 0:
			setVecChannel(&s.Planes[spec.Plane].Albedo, spec.Channel, v)
		case spec.Sphere >= 0:
			switch spec.Kind {
			case ParamCenterX:
				s.Spheres[spec.Sphere].Center.X = v
			case ParamCenterY:
				s.Spheres[spec.Sphere].Center.Y = v
			case ParamCenterZ:
				s.Spheres[spec.Sphere].Center.Z = v
			case ParamRadius:
				s.Spheres[spec.Sphere].Radius = v
			case ParamAlbedoR, ParamAlbedoG, ParamAlbedoB:
				setVecChannel(&s.Spheres[spec.Sphere].Albedo, spec.Channel, v)
			}
		}
	}
	return scene.Validate(s, "<render>")
}

func vecChannel(v scene.Vec3, c int) float64 {
	switch c {
	case 0:
		return v.X
	case 1:
		return v.Y
	default:
		return v.Z
	}
}

func setVecChannel(v *scene.Vec3, c int, value float64) {
	switch c {
	case 0:
		v.X = value
	case 1:
		v.Y = value
	default:
		v.Z = value
	}
}

func parameterIndex(specs []ParamSpec, match func(ParamSpec) bool) int {
	for i, s := range specs {
		if match(s) {
			return i
		}
	}
	return -1
}

func paramField(spec ParamSpec) string { return fmt.Sprintf("%s", spec.ID) }
