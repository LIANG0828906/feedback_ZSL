package model

import (
	"fmt"
	"math"

	"volumediag/internal/diag"
)

const (
	MaxViewportDimension = 4096
	MaxBlockDimension    = 512
	MaxMeshDimension     = 256
	MaxAnisotropy        = 0.95
)

func (s *Scene) Validate() error { return validateScene(s) }

func validateScene(s *Scene) error {
	if s.Viewport.Width <= 0 || s.Viewport.Height <= 0 ||
		s.Viewport.Width > MaxViewportDimension || s.Viewport.Height > MaxViewportDimension {
		return diag.Parameter("", "scene.viewport", "viewport dimensions out of range")
	}
	if s.BlockSize.Width <= 0 || s.BlockSize.Height <= 0 ||
		s.BlockSize.Width > MaxBlockDimension || s.BlockSize.Height > MaxBlockDimension {
		return diag.Parameter("", "scene.block_size", "block dimensions out of range")
	}
	if len(s.CameraPath) == 0 {
		return diag.Schema("scene.camera_path", "at least one camera is required")
	}
	cameraIDs := make(map[string]struct{}, len(s.CameraPath))
	for i := range s.CameraPath {
		c := &s.CameraPath[i]
		path := fmt.Sprintf("scene.camera_path[%d]", i)
		if c.ID == "" {
			return diag.Schema(path+".id", "camera id is required")
		}
		if _, ok := cameraIDs[c.ID]; ok {
			return diag.Schema(path+".id", "duplicate camera id")
		}
		cameraIDs[c.ID] = struct{}{}
		if !c.Position.IsFinite() || !c.Target.IsFinite() || !c.Up.IsFinite() {
			return diag.Parameter("", path, "camera vector is not finite")
		}
		if c.Target.Sub(c.Position).IsZero(1e-12) {
			return diag.Parameter("", path+".target", "camera target equals position")
		}
		if c.Up.IsZero(1e-12) {
			return diag.Parameter("", path+".up", "camera up vector is zero")
		}
		if !finite(c.FovYDeg) || c.FovYDeg <= 1 || c.FovYDeg >= 179 {
			return diag.Parameter("", path+".fov_y_deg", "field of view out of range")
		}
		if !finite(c.Near) || c.Near < 0 || !finite(c.Far) || c.Far <= c.Near {
			return diag.Parameter("", path+".far", "camera clip planes are invalid")
		}
	}
	if _, idx := s.ActiveCamera(); idx < 0 {
		return &diag.Error{
			Code:    diag.ERef,
			Field:   "scene.active_camera_id",
			Message: "active camera reference does not exist",
		}
	}
	if len(s.Geometry) == 0 {
		return diag.Schema("scene.geometry", "at least one depth mesh is required")
	}
	if len(s.Lights) == 0 {
		return diag.Schema("scene.lights", "at least one light is required")
	}

	meshIDs := make(map[string]struct{}, len(s.Geometry))
	for i := range s.Geometry {
		m := &s.Geometry[i]
		path := fmt.Sprintf("scene.geometry[%d]", i)
		if m.ID == "" {
			return diag.Schema(path+".id", "geometry id is required")
		}
		if _, ok := meshIDs[m.ID]; ok {
			return diag.Schema(path+".id", "duplicate geometry id")
		}
		meshIDs[m.ID] = struct{}{}
		if !m.Origin.IsFinite() || !m.AxisU.IsFinite() || !m.AxisV.IsFinite() || !m.Normal.IsFinite() {
			return diag.Parameter("", path, "geometry vector is not finite")
		}
		if m.AxisU.Length() <= 1e-12 || m.AxisV.Length() <= 1e-12 || m.Normal.Length() <= 1e-12 {
			return diag.Parameter("", path, "geometry axis or normal is zero")
		}
		if math.Abs(m.AxisU.Dot(m.AxisV)) > 1e-6*m.AxisU.Length()*m.AxisV.Length() {
			return diag.Parameter("", path+".axis_v", "geometry axes are not orthogonal")
		}
		if math.Abs(m.Normal.Dot(m.AxisU)) > 1e-6*m.Normal.Length()*m.AxisU.Length() ||
			math.Abs(m.Normal.Dot(m.AxisV)) > 1e-6*m.Normal.Length()*m.AxisV.Length() {
			return diag.Parameter("", path+".normal", "normal is not orthogonal to axes")
		}
		if !m.Color.IsFinite() || m.Color[0] < 0 || m.Color[1] < 0 || m.Color[2] < 0 {
			return diag.Parameter("", path+".color", "geometry color must be finite and non-negative")
		}
		if m.Columns <= 0 || m.Rows <= 0 || m.Columns > MaxMeshDimension || m.Rows > MaxMeshDimension {
			return diag.Decode(path, "depth mesh dimensions are invalid", nil)
		}
		if len(m.Depths) != m.Rows {
			return diag.Decode(path+".depths", "depth row count does not match rows", nil)
		}
		for r := range m.Depths {
			if len(m.Depths[r]) != m.Columns {
				return diag.Decode(path+".depths", "depth column count does not match columns", nil)
			}
			for _, d := range m.Depths[r] {
				if !finite(d) {
					return diag.Decode(path+".depths", "depth value is not finite", nil)
				}
			}
		}
	}

	lightIDs := make(map[string]struct{}, len(s.Lights))
	for i := range s.Lights {
		l := &s.Lights[i]
		path := fmt.Sprintf("scene.lights[%d]", i)
		if l.ID == "" {
			return diag.Schema(path+".id", "light id is required")
		}
		if _, ok := lightIDs[l.ID]; ok {
			return diag.Schema(path+".id", "duplicate light id")
		}
		lightIDs[l.ID] = struct{}{}
		switch l.Type {
		case "point", "spot", "area":
		default:
			return diag.Schema(path+".type", "light type must be point, spot or area")
		}
		if !l.Position.IsFinite() {
			return diag.Parameter(l.ID, path+".position", "light position is not finite")
		}
		if !l.Direction.IsFinite() || l.Direction.IsZero(1e-12) {
			return diag.Parameter(l.ID, path+".direction", "light direction is not finite or zero")
		}
		if !l.Intensity.IsFinite() || l.Intensity[0] < 0 || l.Intensity[1] < 0 || l.Intensity[2] < 0 {
			return diag.Parameter(l.ID, path+".intensity", "intensity must be finite and non-negative")
		}
		if !finite(l.Radius) || l.Radius <= 0 {
			return diag.Parameter(l.ID, path+".radius", "light radius must be positive and finite")
		}
		if !finite(l.Anisotropy) || l.Anisotropy < -MaxAnisotropy || l.Anisotropy > MaxAnisotropy {
			return diag.Parameter(l.ID, path+".anisotropy", "anisotropy is outside [-0.95, 0.95]")
		}
		for _, id := range l.OccluderIDs {
			if _, ok := meshIDs[id]; !ok {
				return &diag.Error{
					Code:    diag.ERef,
					LightID: l.ID,
					Field:   path + ".occluder_ids",
					Message: "referenced occluder does not exist: " + id,
				}
			}
		}
		if l.Type == "spot" {
			if !finite(l.ConeInnerDeg) || !finite(l.ConeOuterDeg) ||
				l.ConeInnerDeg < 0 || l.ConeOuterDeg <= 0 ||
				l.ConeInnerDeg > l.ConeOuterDeg || l.ConeOuterDeg >= 180 {
				return diag.Parameter(l.ID, path+".cone_outer_deg", "spot cone angles are invalid")
			}
		}
		if l.Type == "area" {
			if !finite(l.HalfExtents[0]) || !finite(l.HalfExtents[1]) ||
				l.HalfExtents[0] <= 0 || l.HalfExtents[1] <= 0 {
				return diag.Parameter(l.ID, path+".half_extents", "area light extents must be positive")
			}
		}
	}

	m := &s.Medium
	if !m.Extinction.IsFinite() || m.Extinction[0] < 0 || m.Extinction[1] < 0 || m.Extinction[2] < 0 {
		return diag.Parameter("", "scene.medium.extinction", "extinction must be finite and non-negative")
	}
	if !m.Scattering.IsFinite() || m.Scattering[0] < 0 || m.Scattering[1] < 0 || m.Scattering[2] < 0 {
		return diag.Parameter("", "scene.medium.scattering", "scattering must be finite and non-negative")
	}
	if m.PhaseModel != "hg" {
		return diag.Schema("scene.medium.phase_model", "only the hg phase model is supported")
	}
	if !finite(m.PhaseG) || m.PhaseG < -MaxAnisotropy || m.PhaseG > MaxAnisotropy {
		return diag.Parameter("", "scene.medium.phase_g", "phase anisotropy is outside [-0.95, 0.95]")
	}
	return nil
}

func validateTruth(t *Truth, scene *Scene) error {
	tol := t.Tolerance
	if !finite(tol.TransmittanceAbs) || tol.TransmittanceAbs < 0 ||
		!finite(tol.ScatterAbs) || tol.ScatterAbs < 0 ||
		!finite(tol.PixelAbs) || tol.PixelAbs < 0 ||
		!finite(tol.Relative) || tol.Relative < 0 {
		return diag.Parameter("", "truth.tolerance", "truth tolerance must be finite and non-negative")
	}
	if len(t.Rays) == 0 {
		return diag.Schema("truth.rays", "truth must contain at least one sampling ray")
	}
	cameras := make(map[string]*Camera, len(scene.CameraPath))
	for i := range scene.CameraPath {
		cameras[scene.CameraPath[i].ID] = &scene.CameraPath[i]
	}
	rayIDs := make(map[string]struct{}, len(t.Rays))
	for i := range t.Rays {
		r := &t.Rays[i]
		path := fmt.Sprintf("truth.rays[%d]", i)
		if r.ID == "" {
			return diag.Schema(path+".id", "sampling ray id is required")
		}
		if _, ok := rayIDs[r.ID]; ok {
			return diag.Schema(path+".id", "duplicate sampling ray id")
		}
		rayIDs[r.ID] = struct{}{}
		cam, ok := cameras[r.CameraID]
		if !ok {
			return &diag.Error{Code: diag.ERef, CameraID: r.CameraID, Field: path + ".camera_id", Message: "truth ray camera does not exist"}
		}
		if r.PixelX < 0 || r.PixelY < 0 || r.PixelX >= scene.Viewport.Width || r.PixelY >= scene.Viewport.Height {
			return diag.Schema(path+".pixel_x", "truth ray pixel is outside the viewport")
		}
		if !r.Origin.IsFinite() || !r.Direction.IsFinite() || r.Direction.IsZero(1e-12) {
			return diag.Parameter("", path, "truth ray origin/direction is invalid")
		}
		if !finite(r.TNear) || !finite(r.TFar) || r.TNear < 0 || r.TFar <= r.TNear || r.TFar > cam.Far+1e-9 {
			return diag.Parameter("", path+".t_far", "truth ray interval is invalid")
		}
		if !r.Transmittance.IsFinite() || !r.Scatter.IsFinite() ||
			r.Transmittance[0] < 0 || r.Transmittance[1] < 0 || r.Transmittance[2] < 0 ||
			r.Scatter[0] < 0 || r.Scatter[1] < 0 || r.Scatter[2] < 0 {
			return diag.Parameter("", path, "truth ray value is invalid")
		}
	}

	activeID := scene.ActiveCameraID
	expected := scene.Viewport.Width * scene.Viewport.Height
	seenByCamera := make(map[string]map[int]struct{}, len(cameras))
	for i := range t.Pixels {
		p := &t.Pixels[i]
		path := fmt.Sprintf("truth.pixels[%d]", i)
		if _, ok := cameras[p.CameraID]; !ok {
			return &diag.Error{Code: diag.ERef, CameraID: p.CameraID, Field: path + ".camera_id", Message: "truth pixel camera does not exist"}
		}
		if p.X < 0 || p.Y < 0 || p.X >= scene.Viewport.Width || p.Y >= scene.Viewport.Height {
			return diag.Schema(path+".x", "truth pixel coordinate is outside viewport")
		}
		key := p.Y*scene.Viewport.Width + p.X
		seen := seenByCamera[p.CameraID]
		if seen == nil {
			seen = make(map[int]struct{}, expected)
			seenByCamera[p.CameraID] = seen
		}
		if _, ok := seen[key]; ok {
			return diag.Schema(path, "duplicate truth pixel coverage")
		}
		seen[key] = struct{}{}
		if !p.Volume.IsFinite() || p.Volume[0] < 0 || p.Volume[1] < 0 || p.Volume[2] < 0 {
			return diag.Parameter("", path+".volume", "truth pixel value is invalid")
		}
	}
	activeSeen := seenByCamera[activeID]
	if len(activeSeen) != expected {
		for y := 0; y < scene.Viewport.Height; y++ {
			for x := 0; x < scene.Viewport.Width; x++ {
				if _, ok := activeSeen[y*scene.Viewport.Width+x]; !ok {
					return &diag.Error{
						Code:    diag.ESchema,
						Field:   "truth.pixels",
						PixelX:  x,
						PixelY:  y,
						Message: "truth manifest does not cover every screen coordinate",
					}
				}
			}
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
