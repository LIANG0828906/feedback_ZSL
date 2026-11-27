package volumefog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

type mediumJSON struct {
	ID              string `json:"id"`
	MinQ            Vec3i  `json:"min_q"`
	MaxQ            Vec3i  `json:"max_q"`
	DensityRes      [3]int `json:"density_res"`
	DensityB64      string `json:"density_b64"`
	DensityChecksum string `json:"density_checksum"`
	AbsorptionQ     int64  `json:"absorption_q"`
	ScatteringQ     int64  `json:"scattering_q"`
	AnisotropyQ     int64  `json:"anisotropy_q"`
}

type sceneJSON struct {
	Schema           string         `json:"schema"`
	Seed             int64          `json:"seed"`
	FixedScale       int64          `json:"fixed_scale"`
	CoefficientScale int64          `json:"coefficient_scale"`
	ChunkSize        int            `json:"chunk_size"`
	ActiveCamera     int            `json:"active_camera"`
	Media            []mediumJSON   `json:"media"`
	Cameras          []CameraSample `json:"cameras"`
	Recipe           Recipe         `json:"recipe"`
	Truth            *TruthSet      `json:"truth,omitempty"`
}

// MarshalSceneJSON serializes a scene with base64 int32 density payloads and
// integrity checksums. The truth block is included when present.
func MarshalSceneJSON(s *Scene) ([]byte, error) {
	if s == nil {
		return nil, fogErr(E_SCHEMA, "nil scene")
	}
	w := sceneJSON{
		Schema:           s.Schema,
		Seed:             s.Seed,
		FixedScale:       s.FixedScale,
		CoefficientScale: s.CoefficientScale,
		ChunkSize:        s.ChunkSize,
		ActiveCamera:     s.ActiveCamera,
		Cameras:          s.Cameras,
		Recipe:           s.Recipe,
		Truth:            s.Truth,
	}
	for i := range s.Media {
		m := &s.Media[i]
		raw := EncodeDensity(m.Density)
		checksum := DensityChecksum(raw)
		w.Media = append(w.Media, mediumJSON{
			ID:              m.ID,
			MinQ:            m.MinQ,
			MaxQ:            m.MaxQ,
			DensityRes:      m.DensityRes,
			DensityB64:      base64.StdEncoding.EncodeToString(raw),
			DensityChecksum: checksum,
			AbsorptionQ:     m.AbsorptionQ,
			ScatteringQ:     m.ScatteringQ,
			AnisotropyQ:     m.AnisotropyQ,
		})
	}
	return json.MarshalIndent(w, "", "  ")
}

type sceneWire struct {
	Schema           *string       `json:"schema"`
	Seed             *int64        `json:"seed"`
	FixedScale       *int64        `json:"fixed_scale"`
	CoefficientScale *int64        `json:"coefficient_scale"`
	ChunkSize        *int          `json:"chunk_size"`
	ActiveCamera     *int          `json:"active_camera"`
	Media            *[]mediumWire `json:"media"`
	Cameras          *[]cameraWire `json:"cameras"`
	Recipe           *recipeWire   `json:"recipe"`
	Truth            *TruthSet     `json:"truth"`
}

type mediumWire struct {
	ID              *string `json:"id"`
	MinQ            *Vec3i  `json:"min_q"`
	MaxQ            *Vec3i  `json:"max_q"`
	DensityRes      *[3]int `json:"density_res"`
	DensityB64      *string `json:"density_b64"`
	DensityChecksum *string `json:"density_checksum"`
	AbsorptionQ     *int64  `json:"absorption_q"`
	ScatteringQ     *int64  `json:"scattering_q"`
	AnisotropyQ     *int64  `json:"anisotropy_q"`
}

type cameraWire struct {
	TimeUS          *int64   `json:"time_us"`
	PositionQ       *Vec3i   `json:"position_q"`
	OrientationMdeg *Vec3i   `json:"orientation_mdeg"`
	ViewWidthQ      *int64   `json:"view_width_q"`
	ViewHeightQ     *int64   `json:"view_height_q"`
	FovDeg          *float64 `json:"fov_deg"`
	NearQ           *int64   `json:"near_q"`
	FarQ            *int64   `json:"far_q"`
	Width           *int     `json:"width"`
	Height          *int     `json:"height"`
}

type recipeWire struct {
	Steps                *int        `json:"steps"`
	PreviewSteps         *int        `json:"preview_steps"`
	PhaseType            *string     `json:"phase_type"`
	ShadowSamples        *int        `json:"shadow_samples"`
	PreviewShadowSamples *int        `json:"preview_shadow_samples"`
	TransmittanceCutoff  *float64    `json:"transmittance_cutoff"`
	MaxParallel          *int        `json:"max_parallel"`
	RayBuffer            *int        `json:"ray_buffer"`
	ShadowBudget         *int        `json:"shadow_budget"`
	PreviewLatencyMS     *int        `json:"preview_latency_ms"`
	LightDir             *[3]float64 `json:"light_dir"`
	AmbientColor         *[3]float64 `json:"ambient_color"`
}

// LoadSceneJSON performs strict required-field decoding before semantic
// validation. Missing fields produce E_SCHEMA; corrupt payloads produce
// E_DECODE; non-monotonic camera times produce E_ORDER; out-of-range density
// values produce E_DENS.
func LoadSceneJSON(data []byte) (*Scene, error) {
	var w sceneWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, withCause(E_DECODE, "scene JSON decode failed", err)
	}
	if w.Schema == nil {
		return nil, schemaErr("", "schema", "required field is missing")
	}
	if w.Seed == nil {
		return nil, schemaErr("", "seed", "required field is missing")
	}
	if w.FixedScale == nil {
		return nil, schemaErr("", "fixed_scale", "required field is missing")
	}
	if w.CoefficientScale == nil {
		return nil, schemaErr("", "coefficient_scale", "required field is missing")
	}
	if w.ChunkSize == nil {
		return nil, schemaErr("", "chunk_size", "required field is missing")
	}
	if w.ActiveCamera == nil {
		return nil, schemaErr("", "active_camera", "required field is missing")
	}
	if w.Media == nil {
		return nil, schemaErr("", "media", "required field is missing")
	}
	if w.Cameras == nil {
		return nil, schemaErr("", "cameras", "required field is missing")
	}
	if w.Recipe == nil {
		return nil, schemaErr("", "recipe", "required field is missing")
	}

	scene := &Scene{
		Schema:           *w.Schema,
		Seed:             *w.Seed,
		FixedScale:       *w.FixedScale,
		CoefficientScale: *w.CoefficientScale,
		ChunkSize:        *w.ChunkSize,
		ActiveCamera:     *w.ActiveCamera,
		Truth:            w.Truth,
	}
	for i, mw := range *w.Media {
		id := "<unknown>"
		if mw.ID != nil {
			id = *mw.ID
		}
		required := []struct {
			ok    bool
			field string
		}{
			{mw.ID != nil, "id"}, {mw.MinQ != nil, "min_q"}, {mw.MaxQ != nil, "max_q"},
			{mw.DensityRes != nil, "density_res"}, {mw.DensityB64 != nil, "density_b64"},
			{mw.DensityChecksum != nil, "density_checksum"}, {mw.AbsorptionQ != nil, "absorption_q"},
			{mw.ScatteringQ != nil, "scattering_q"}, {mw.AnisotropyQ != nil, "anisotropy_q"},
		}
		for _, f := range required {
			if !f.ok {
				return nil, schemaErr(id, f.field, "required field is missing")
			}
		}
		if *mw.DensityRes == [3]int{} {
			return nil, schemaErr(id, "density_res", "density resolution must be positive")
		}
		raw, err := base64.StdEncoding.DecodeString(*mw.DensityB64)
		if err != nil {
			return nil, withCause(E_DECODE, "density payload is not valid base64", err).withMedium(id, "density_b64")
		}
		if got := DensityChecksum(raw); got != *mw.DensityChecksum {
			e := codeErr(E_DECODE, id, "density_checksum", "density payload checksum mismatch")
			e.Cause = fmt.Sprintf("want %s got %s", got, *mw.DensityChecksum)
			return nil, e
		}
		density, err := DecodeDensity(raw)
		if err != nil {
			return nil, withCause(E_DECODE, "density payload decode failed", err).withMedium(id, "density_b64")
		}
		m := Medium{
			ID:              *mw.ID,
			MinQ:            *mw.MinQ,
			MaxQ:            *mw.MaxQ,
			DensityRes:      *mw.DensityRes,
			Density:         density,
			DensityChecksum: *mw.DensityChecksum,
			AbsorptionQ:     *mw.AbsorptionQ,
			ScatteringQ:     *mw.ScatteringQ,
			AnisotropyQ:     *mw.AnisotropyQ,
		}
		m.setDensityRaw(raw)
		scene.Media = append(scene.Media, m)
		_ = i
	}
	for i, cw := range *w.Cameras {
		id := fmt.Sprintf("camera[%d]", i)
		required := []struct {
			ok    bool
			field string
		}{
			{cw.TimeUS != nil, "time_us"}, {cw.PositionQ != nil, "position_q"},
			{cw.OrientationMdeg != nil, "orientation_mdeg"}, {cw.ViewWidthQ != nil, "view_width_q"},
			{cw.ViewHeightQ != nil, "view_height_q"}, {cw.FovDeg != nil, "fov_deg"},
			{cw.NearQ != nil, "near_q"}, {cw.FarQ != nil, "far_q"},
			{cw.Width != nil, "width"}, {cw.Height != nil, "height"},
		}
		for _, f := range required {
			if !f.ok {
				return nil, schemaErr(id, f.field, "required field is missing")
			}
		}
		scene.Cameras = append(scene.Cameras, CameraSample{
			TimeUS: *cw.TimeUS, PositionQ: *cw.PositionQ, OrientationMdeg: *cw.OrientationMdeg,
			ViewWidthQ: *cw.ViewWidthQ, ViewHeightQ: *cw.ViewHeightQ, FovDeg: *cw.FovDeg,
			NearQ: *cw.NearQ, FarQ: *cw.FarQ, Width: *cw.Width, Height: *cw.Height,
		})
	}
	rw := w.Recipe
	requiredRecipe := []struct {
		ok    bool
		field string
	}{
		{rw.Steps != nil, "steps"}, {rw.PreviewSteps != nil, "preview_steps"},
		{rw.PhaseType != nil, "phase_type"}, {rw.ShadowSamples != nil, "shadow_samples"},
		{rw.PreviewShadowSamples != nil, "preview_shadow_samples"},
		{rw.TransmittanceCutoff != nil, "transmittance_cutoff"},
		{rw.MaxParallel != nil, "max_parallel"}, {rw.RayBuffer != nil, "ray_buffer"},
		{rw.ShadowBudget != nil, "shadow_budget"}, {rw.PreviewLatencyMS != nil, "preview_latency_ms"},
		{rw.LightDir != nil, "light_dir"}, {rw.AmbientColor != nil, "ambient_color"},
	}
	for _, f := range requiredRecipe {
		if !f.ok {
			return nil, schemaErr("", "recipe."+f.field, "required field is missing")
		}
	}
	scene.Recipe = Recipe{
		Steps: *rw.Steps, PreviewSteps: *rw.PreviewSteps, PhaseType: *rw.PhaseType,
		ShadowSamples: *rw.ShadowSamples, PreviewShadowSamples: *rw.PreviewShadowSamples,
		TransmittanceCutoff: *rw.TransmittanceCutoff, MaxParallel: *rw.MaxParallel,
		RayBuffer: *rw.RayBuffer, ShadowBudget: *rw.ShadowBudget,
		PreviewLatencyMS: *rw.PreviewLatencyMS, LightDir: *rw.LightDir,
		AmbientColor: *rw.AmbientColor,
	}
	if err := ValidateScene(scene); err != nil {
		return nil, err
	}
	return scene, nil
}

func LoadSceneFile(path string) (*Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, withCause(E_DECODE, "scene file read failed", err)
	}
	return LoadSceneJSON(data)
}

func SaveSceneFile(path string, scene *Scene) error {
	data, err := MarshalSceneJSON(scene)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (e *FogError) withMedium(mediaID, field string) *FogError {
	e.MediaID = mediaID
	e.Field = field
	return e
}
