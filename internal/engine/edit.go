package engine

import (
	"math"

	"volumediag/internal/diag"
	"volumediag/internal/model"
	"volumediag/internal/volume"
)

type MediumEdit struct {
	Extinction *model.Color `json:"extinction,omitempty"`
	Scattering *model.Color `json:"scattering,omitempty"`
	PhaseG     *float64     `json:"phase_g,omitempty"`
}

type EditRequest struct {
	LightID    string       `json:"light_id,omitempty"`
	Intensity  *model.Color `json:"intensity,omitempty"`
	Direction  *model.Vec3  `json:"direction,omitempty"`
	Position   *model.Vec3  `json:"position,omitempty"`
	Radius     *float64     `json:"radius,omitempty"`
	Anisotropy *float64     `json:"anisotropy,omitempty"`
	Medium     *MediumEdit  `json:"medium,omitempty"`
	OccluderID string       `json:"occluder_id,omitempty"`
	Depths     [][]float64  `json:"depths,omitempty"`
}

type EditReport struct {
	Changed        bool           `json:"changed"`
	Target         string         `json:"target"`
	AffectedBlocks int            `json:"affected_blocks"`
	ReusedBlocks   int            `json:"reused_blocks"`
	Invalidated    []Invalidation `json:"invalidated"`
	SceneHash      string         `json:"scene_hash"`
}

func (e *Engine) ApplyEdit(req EditRequest) (*EditReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()

	targets := 0
	if req.LightID != "" {
		targets++
	}
	if req.Medium != nil {
		targets++
	}
	if req.OccluderID != "" {
		targets++
	}
	if targets != 1 {
		return nil, diag.Parameter("", "edit", "exactly one of light, medium or occluder must be edited")
	}

	oldScene := e.scene.Clone()
	next := e.scene.Clone()
	if next == nil {
		return nil, diag.New(diag.EIO, "cannot clone scene for edit")
	}
	var reason string
	var sourceLights, sourceOccluders []string
	var affected map[volume.BlockKey]struct{}
	oldCoverage := e.coverage

	if req.LightID != "" {
		light, idx := next.FindLight(req.LightID)
		if idx < 0 {
			return nil, &diag.Error{Code: diag.ERef, LightID: req.LightID, Field: "light_id", Message: "light does not exist"}
		}
		reasons := make([]string, 0, 4)
		if req.Intensity != nil {
			if !finiteColor(*req.Intensity) || req.Intensity[0] < 0 || req.Intensity[1] < 0 || req.Intensity[2] < 0 {
				return nil, diag.Parameter(req.LightID, "intensity", "intensity must be finite and non-negative")
			}
			light.Intensity = *req.Intensity
			reasons = append(reasons, "light_intensity:"+req.LightID)
		}
		if req.Direction != nil {
			if !req.Direction.IsFinite() || req.Direction.IsZero(1e-12) {
				return nil, diag.Parameter(req.LightID, "direction", "direction must be finite and non-zero")
			}
			light.Direction = *req.Direction
			reasons = append(reasons, "light_direction:"+req.LightID)
		}
		if req.Position != nil {
			if !req.Position.IsFinite() {
				return nil, diag.Parameter(req.LightID, "position", "position must be finite")
			}
			light.Position = *req.Position
			reasons = append(reasons, "light_position:"+req.LightID)
		}
		if req.Radius != nil {
			if !finite(*req.Radius) || *req.Radius <= 0 {
				return nil, diag.Parameter(req.LightID, "radius", "radius must be positive and finite")
			}
			light.Radius = *req.Radius
			reasons = append(reasons, "light_radius:"+req.LightID)
		}
		if req.Anisotropy != nil {
			if !finite(*req.Anisotropy) || *req.Anisotropy < -model.MaxAnisotropy || *req.Anisotropy > model.MaxAnisotropy {
				return nil, diag.Parameter(req.LightID, "anisotropy", "anisotropy is outside [-0.95, 0.95]")
			}
			light.Anisotropy = *req.Anisotropy
			reasons = append(reasons, "light_anisotropy:"+req.LightID)
		}
		if len(reasons) == 0 {
			return nil, diag.Parameter(req.LightID, "edit", "no light fields were supplied")
		}
		reason = joinReasons(reasons)
		sourceLights = []string{req.LightID}
	} else if req.Medium != nil {
		reasons := make([]string, 0, 3)
		if req.Medium.Extinction != nil {
			if !finiteColor(*req.Medium.Extinction) || req.Medium.Extinction[0] < 0 || req.Medium.Extinction[1] < 0 || req.Medium.Extinction[2] < 0 {
				return nil, diag.Parameter("", "medium.extinction", "extinction must be finite and non-negative")
			}
			next.Medium.Extinction = *req.Medium.Extinction
			reasons = append(reasons, "medium_extinction")
		}
		if req.Medium.Scattering != nil {
			if !finiteColor(*req.Medium.Scattering) || req.Medium.Scattering[0] < 0 || req.Medium.Scattering[1] < 0 || req.Medium.Scattering[2] < 0 {
				return nil, diag.Parameter("", "medium.scattering", "scattering must be finite and non-negative")
			}
			next.Medium.Scattering = *req.Medium.Scattering
			reasons = append(reasons, "medium_scattering")
		}
		if req.Medium.PhaseG != nil {
			if !finite(*req.Medium.PhaseG) || *req.Medium.PhaseG < -model.MaxAnisotropy || *req.Medium.PhaseG > model.MaxAnisotropy {
				return nil, diag.Parameter("", "medium.phase_g", "phase anisotropy is outside [-0.95, 0.95]")
			}
			next.Medium.PhaseG = *req.Medium.PhaseG
			reasons = append(reasons, "medium_phase_g")
		}
		if len(reasons) == 0 {
			return nil, diag.Parameter("", "medium", "no medium fields were supplied")
		}
		reason = joinReasons(reasons)
	} else {
		mesh, idx := next.FindMesh(req.OccluderID)
		if idx < 0 {
			return nil, &diag.Error{Code: diag.ERef, Field: "occluder_id", Message: "occluder does not exist"}
		}
		oldHits := geometryHitBlocks(oldScene, e.cameraIndex, req.OccluderID)
		if len(req.Depths) != mesh.Rows {
			return nil, diag.Parameter("", "occluder.depths", "depth row count does not match geometry")
		}
		for r := range req.Depths {
			if len(req.Depths[r]) != mesh.Columns {
				return nil, diag.Parameter("", "occluder.depths", "depth column count does not match geometry")
			}
			for _, d := range req.Depths[r] {
				if !finite(d) {
					return nil, diag.Parameter("", "occluder.depths", "depth values must be finite")
				}
			}
		}
		mesh.Depths = cloneDepths(req.Depths)
		reason = "occluder_geometry:" + req.OccluderID
		sourceOccluders = []string{req.OccluderID}
		sourceLights = lightsReferencingMesh(next, req.OccluderID)
		affected = unionBlockSet(affected, oldHits)
		affected = unionBlockSet(affected, geometryHitBlocks(next, e.cameraIndex, req.OccluderID))
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	newVersions := next.Versions()
	if newVersions.SceneHash == e.versions.SceneHash {
		return &EditReport{
			Changed:     false,
			Target:      editTarget(req),
			Invalidated: []Invalidation{},
			SceneHash:   e.contentHash,
		}, nil
	}
	newCoverage, covErr := computeLightCoverage(next, e.cameraIndex, e.params)
	if covErr != nil {
		return nil, covErr
	}
	newBlockGeometry := computeBlockGeometry(next, e.cameraIndex, newCoverage)

	if req.LightID != "" {
		affected = unionBlockSet(affected, oldCoverage[req.LightID])
		affected = unionBlockSet(affected, newCoverage[req.LightID])
		for key, block := range e.cache {
			if _, ok := block.LightVersions[req.LightID]; ok {
				if affected == nil {
					affected = make(map[volume.BlockKey]struct{})
				}
				affected[key] = struct{}{}
			}
		}
	} else if req.Medium != nil {
		affected = make(map[volume.BlockKey]struct{})
		for _, key := range e.allKeys() {
			affected[key] = struct{}{}
		}
	} else {
		for key, deps := range e.blockGeometry {
			if _, ok := deps[req.OccluderID]; ok {
				affected = unionBlockSet(affected, map[volume.BlockKey]struct{}{key: {}})
			}
		}
		for key, deps := range newBlockGeometry {
			if _, ok := deps[req.OccluderID]; ok {
				affected = unionBlockSet(affected, map[volume.BlockKey]struct{}{key: {}})
			}
		}
		for _, lightID := range sourceLights {
			affected = unionBlockSet(affected, oldCoverage[lightID])
			affected = unionBlockSet(affected, newCoverage[lightID])
		}
		for key, block := range e.cache {
			for _, id := range block.UsedOccluders {
				if id == req.OccluderID {
					if affected == nil {
						affected = make(map[volume.BlockKey]struct{})
					}
					affected[key] = struct{}{}
				}
			}
		}
	}
	if affected == nil {
		affected = make(map[volume.BlockKey]struct{})
	}
	if len(affected) == 0 {
		e.scene = next
		e.versions = newVersions
		e.contentHash = next.ComputeHash()
		e.coverage = newCoverage
		e.blockGeometry = newBlockGeometry
		return &EditReport{
			Changed:        true,
			Target:         editTarget(req),
			AffectedBlocks: 0,
			ReusedBlocks:   len(e.cache),
			Invalidated:    []Invalidation{},
			SceneHash:      e.contentHash,
		}, nil
	}
	if len(affected) > e.scene.BlockRows()*e.scene.BlockCols() {
		return nil, diag.Dependency(e.scene.ActiveCameraID, 0, 0, "invalidation set exceeds the active camera block grid")
	}
	e.scene = next
	e.versions = newVersions
	e.contentHash = next.ComputeHash()
	e.coverage = newCoverage
	e.blockGeometry = newBlockGeometry
	for key := range affected {
		e.markPendingLocked(key, pendingInvalidation{
			Reason:          reason,
			Stage:           "volume",
			Depth:           0,
			SourceLights:    sourceLights,
			SourceOccluders: sourceOccluders,
		})
	}
	if e.store != nil {
		if err := e.store.SetComplete(e.manifest(), false); err != nil {
			return nil, err
		}
	}
	total := e.scene.BlockRows() * e.scene.BlockCols()
	return &EditReport{
		Changed:        true,
		Target:         editTarget(req),
		AffectedBlocks: len(affected),
		ReusedBlocks:   total - len(affected),
		Invalidated:    e.invalidationListLocked(),
		SceneHash:      e.contentHash,
	}, nil
}

func (e *Engine) SetLightIntensity(id string, value model.Color) (*EditReport, error) {
	return e.ApplyEdit(EditRequest{LightID: id, Intensity: &value})
}

func unionBlockSet(dst, src map[volume.BlockKey]struct{}) map[volume.BlockKey]struct{} {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[volume.BlockKey]struct{}, len(src))
	}
	for key := range src {
		dst[key] = struct{}{}
	}
	return dst
}

func cloneDepths(in [][]float64) [][]float64 {
	out := make([][]float64, len(in))
	for i := range in {
		out[i] = append([]float64(nil), in[i]...)
	}
	return out
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func finiteColor(c model.Color) bool { return c.IsFinite() }

func joinReasons(reasons []string) string {
	out := ""
	for _, r := range reasons {
		if out == "" {
			out = r
		} else {
			out += "+" + r
		}
	}
	return out
}

func editTarget(req EditRequest) string {
	if req.LightID != "" {
		return "light:" + req.LightID
	}
	if req.Medium != nil {
		return "medium"
	}
	return "occluder:" + req.OccluderID
}

func (e *Engine) DebugCoverageCounts() map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]int, len(e.coverage))
	for id, blocks := range e.coverage {
		out[id] = len(blocks)
	}
	return out
}
