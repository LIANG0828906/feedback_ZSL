package scene

import (
	"fmt"
	"math"

	"diffrender/errs"
)

// CompareSemantics compares all target-expressible canonical fields. It is used
// after cross-version export/load. Representation normalization (units, matrix,
// quaternion, look-at distance encoding) therefore does not count as a change.
func CompareSemantics(want, got *Scene, targetVersion string) error {
	if want == nil || got == nil {
		return roundError("scene", "nil", "missing")
	}
	check := func(path string, a, b, tol float64) error {
		if !nearlyEqual(a, b, tol) {
			return roundError(path, fmt.Sprintf("%.17g", a), fmt.Sprintf("%.17g", b))
		}
		return nil
	}
	checkVec := func(path string, a, b Vec3, tol float64) error {
		if err := check(path+".x", a.X, b.X, tol); err != nil {
			return err
		}
		if err := check(path+".y", a.Y, b.Y, tol); err != nil {
			return err
		}
		return check(path+".z", a.Z, b.Z, tol)
	}
	if len(want.Spheres) != len(got.Spheres) {
		return roundError("spheres", fmt.Sprint(len(want.Spheres)), fmt.Sprint(len(got.Spheres)))
	}
	for i := range want.Spheres {
		a, b := want.Spheres[i], got.Spheres[i]
		if a.ID != b.ID {
			return roundError(fmt.Sprintf("spheres[%d].id", i), a.ID, b.ID)
		}
		if err := checkVec(fmt.Sprintf("spheres[%d].center", i), a.Center, b.Center, 1e-12); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("spheres[%d].radius", i), a.Radius, b.Radius, 1e-12); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("spheres[%d].albedo", i), a.Albedo, b.Albedo, 1e-12); err != nil {
			return err
		}
		if targetVersion == Version3 {
			if err := check(fmt.Sprintf("spheres[%d].coat", i), a.Coat, b.Coat, 1e-12); err != nil {
				return err
			}
		}
	}
	if len(want.Planes) != len(got.Planes) {
		return roundError("planes", fmt.Sprint(len(want.Planes)), fmt.Sprint(len(got.Planes)))
	}
	for i := range want.Planes {
		a, b := want.Planes[i], got.Planes[i]
		if a.ID != b.ID {
			return roundError(fmt.Sprintf("planes[%d].id", i), a.ID, b.ID)
		}
		if err := checkVec(fmt.Sprintf("planes[%d].normal", i), a.Normal, b.Normal, 1e-12); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("planes[%d].offset", i), a.Offset, b.Offset, 1e-12); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("planes[%d].albedo", i), a.Albedo, b.Albedo, 1e-12); err != nil {
			return err
		}
		if targetVersion == Version3 {
			if err := check(fmt.Sprintf("planes[%d].coat", i), a.Coat, b.Coat, 1e-12); err != nil {
				return err
			}
		}
	}
	if len(want.Lights) != len(got.Lights) {
		return roundError("lights", fmt.Sprint(len(want.Lights)), fmt.Sprint(len(got.Lights)))
	}
	for i := range want.Lights {
		a, b := want.Lights[i], got.Lights[i]
		if a.ID != b.ID {
			return roundError(fmt.Sprintf("lights[%d].id", i), a.ID, b.ID)
		}
		if err := checkVec(fmt.Sprintf("lights[%d].center", i), a.Center, b.Center, 1e-12); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("lights[%d].normal", i), a.Normal, b.Normal, 1e-11); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("lights[%d].up", i), a.Up, b.Up, 1e-11); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("lights[%d].halfWidth", i), a.HalfWidth, b.HalfWidth, 1e-12); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("lights[%d].halfHeight", i), a.HalfHeight, b.HalfHeight, 1e-12); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("lights[%d].radiantPowerW", i), a.RadiantPowerW, b.RadiantPowerW, 1e-12); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("lights[%d].color", i), a.Color, b.Color, 1e-12); err != nil {
			return err
		}
	}
	if len(want.Cameras) != len(got.Cameras) {
		return roundError("cameras", fmt.Sprint(len(want.Cameras)), fmt.Sprint(len(got.Cameras)))
	}
	for i := range want.Cameras {
		a, b := want.Cameras[i], got.Cameras[i]
		af, ar, au, err := CameraBasis(a)
		if err != nil {
			return err
		}
		bf, br, bu, err := CameraBasis(b)
		if err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("cameras[%d].eye", i), a.Eye, b.Eye, 1e-11); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("cameras[%d].forward", i), af, bf, 1e-10); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("cameras[%d].right", i), ar, br, 1e-10); err != nil {
			return err
		}
		if err := checkVec(fmt.Sprintf("cameras[%d].up", i), au, bu, 1e-10); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("cameras[%d].fovYRad", i), a.FovYRad, b.FovYRad, 1e-11); err != nil {
			return err
		}
		if a.Width != b.Width || a.Height != b.Height {
			return roundError(fmt.Sprintf("cameras[%d].resolution", i), fmt.Sprintf("%dx%d", a.Width, a.Height), fmt.Sprintf("%dx%d", b.Width, b.Height))
		}
	}
	if targetVersion == Version2 || targetVersion == Version3 {
		if err := check("settings.shadowSoftness", want.Settings.ShadowSoftness, got.Settings.ShadowSoftness, 1e-12); err != nil {
			return err
		}
	}
	if targetVersion == Version3 {
		if want.Settings.ToneMapping != got.Settings.ToneMapping {
			return roundError("settings.toneMapping", want.Settings.ToneMapping, got.Settings.ToneMapping)
		}
	}
	return nil
}

// VerifyRoundTrip exports, reloads, and compares canonical semantics.
func VerifyRoundTrip(s *Scene, opts ExportOptions) (ExportResult, error) {
	result, err := Export(s, opts)
	if err != nil {
		return result, err
	}
	reloaded, err := Load(result.Data, "<roundtrip:"+opts.Version+">")
	if err != nil {
		return result, err
	}
	if err := CompareSemantics(s, reloaded, opts.Version); err != nil {
		return result, err
	}
	return result, nil
}

func nearlyEqual(a, b, tol float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return false
	}
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return math.Abs(a-b) <= tol*scale
}

func roundError(path, want, got string) error {
	e := errs.New(errs.CodeRound, "<roundtrip>", path, "round-trip mismatch: want %s, got %s", want, got)
	e.Details = map[string]string{"want": want, "got": got, "field": path}
	return e
}
