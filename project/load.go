// Package project decodes and validates terrain engineering samples. Decoding
// and generation are intentionally separate from validation.
package project

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"terrainbake/model"
)

var chunkIDPattern = regexp.MustCompile(`"id"\s*:\s*"([^"]+)"`)
var lastKeyPattern = regexp.MustCompile(`"([A-Za-z0-9_]+)"\s*:`)

type rawProject struct {
	Seed      *int64             `json:"seed"`
	Bounds    *model.Bounds      `json:"bounds"`
	Chunks    *[]json.RawMessage `json:"chunks"`
	Occluders *[]json.RawMessage `json:"occluders"`
	Sky       *[]json.RawMessage `json:"sky"`
	Sun       *[]json.RawMessage `json:"sun"`
	Params    *model.BakeParams  `json:"params"`
}

type rawChunk struct {
	ID        *string       `json:"id"`
	GridX     *int          `json:"grid_x"`
	GridZ     *int          `json:"grid_z"`
	Bounds    *model.Bounds `json:"bounds"`
	ResX      *int          `json:"res_x"`
	ResZ      *int          `json:"res_z"`
	Heights   *[]float64    `json:"heights"`
	Albedo    *[3]float64   `json:"albedo"`
	Occluders *[]string     `json:"occluders"`
}

type rawOccluder struct {
	ID     *string     `json:"id"`
	Center *model.Vec2 `json:"center"`
	Radius *float64    `json:"radius"`
	Base   *float64    `json:"base"`
	Height *float64    `json:"height"`
}

type rawSky struct {
	ID           *string     `json:"id"`
	AzimuthDeg   *float64    `json:"azimuth_deg"`
	ElevationDeg *float64    `json:"elevation_deg"`
	Radiance     *[3]float64 `json:"radiance"`
	Weight       *float64    `json:"weight"`
}

type rawSun struct {
	Time         *int        `json:"time"`
	AzimuthDeg   *float64    `json:"azimuth_deg"`
	ElevationDeg *float64    `json:"elevation_deg"`
	Radiance     *[3]float64 `json:"radiance"`
}

type rawBundle struct {
	Project *json.RawMessage `json:"project"`
	Truth   *json.RawMessage `json:"truth"`
}

type rawTruth struct {
	Seed   *int64             `json:"seed"`
	Points *[]json.RawMessage `json:"points"`
}

type rawTruthPoint struct {
	ChunkID       *string            `json:"chunk_id"`
	Time          *int               `json:"time"`
	Vertex        *model.VertexPoint `json:"vertex"`
	World         *model.Vec3        `json:"world"`
	DirectShadow  *bool              `json:"direct_shadow"`
	SkyVisibility *float64           `json:"sky_visibility"`
	Irradiance    *[3]float64        `json:"irradiance"`
	ShadowSteps   *int               `json:"shadow_steps"`
	ShadowBudget  *int               `json:"shadow_budget"`
	FirstHit      *model.Hit         `json:"first_hit,omitempty"`
}

// DecodeProject decodes a project without performing semantic validation.
// Missing required fields and truncated payloads return E_DECODE.
func DecodeProject(data []byte) (*model.Project, error) {
	var rp rawProject
	if err := json.Unmarshal(data, &rp); err != nil {
		id := inferChunkID(data)
		return nil, model.NewError(model.CodeDecode, "project JSON is truncated or malformed: %v", err).
			WithChunk(id).
			WithField(inferFieldPath(data))
	}
	if rp.Seed == nil {
		return nil, decodeMissing("", "seed")
	}
	if rp.Bounds == nil {
		return nil, decodeMissing("", "bounds")
	}
	if rp.Chunks == nil {
		return nil, decodeMissing("", "chunks")
	}
	if rp.Occluders == nil {
		return nil, decodeMissing("", "occluders")
	}
	if rp.Sky == nil {
		return nil, decodeMissing("", "sky")
	}
	if rp.Sun == nil {
		return nil, decodeMissing("", "sun")
	}
	if rp.Params == nil {
		return nil, decodeMissing("", "params")
	}

	p := &model.Project{
		Seed:   *rp.Seed,
		Bounds: *rp.Bounds,
		Params: *rp.Params,
	}
	for i, raw := range *rp.Chunks {
		var c rawChunk
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, model.NewError(model.CodeDecode, "chunk %d is malformed: %v", i, err).
				WithChunk(fmt.Sprintf("#%d", i)).
				WithField(fmt.Sprintf("chunks[%d]", i))
		}
		chunkID := ""
		if c.ID != nil {
			chunkID = *c.ID
		}
		path := fmt.Sprintf("chunks[%d]", i)
		if c.ID == nil {
			return nil, decodeMissing(chunkID, path+".id")
		}
		path = fmt.Sprintf("chunks[%d]", i)
		if c.GridX == nil {
			return nil, decodeMissing(chunkID, path+".grid_x")
		}
		if c.GridZ == nil {
			return nil, decodeMissing(chunkID, path+".grid_z")
		}
		if c.Bounds == nil {
			return nil, decodeMissing(chunkID, path+".bounds")
		}
		if c.ResX == nil {
			return nil, decodeMissing(chunkID, path+".res_x")
		}
		if c.ResZ == nil {
			return nil, decodeMissing(chunkID, path+".res_z")
		}
		if c.Heights == nil {
			return nil, decodeMissing(chunkID, path+".heights")
		}
		if c.Albedo == nil {
			return nil, decodeMissing(chunkID, path+".albedo")
		}
		if c.Occluders == nil {
			return nil, decodeMissing(chunkID, path+".occluders")
		}
		p.Chunks = append(p.Chunks, model.Chunk{
			ID:        *c.ID,
			GridX:     *c.GridX,
			GridZ:     *c.GridZ,
			Bounds:    *c.Bounds,
			ResX:      *c.ResX,
			ResZ:      *c.ResZ,
			Heights:   append([]float64(nil), (*c.Heights)...),
			Albedo:    *c.Albedo,
			Occluders: append([]string(nil), (*c.Occluders)...),
		})
	}

	for i, raw := range *rp.Occluders {
		var o rawOccluder
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, model.NewError(model.CodeDecode, "occluder %d is malformed: %v", i, err).
				WithField(fmt.Sprintf("occluders[%d]", i))
		}
		path := fmt.Sprintf("occluders[%d]", i)
		if o.ID == nil {
			return nil, decodeMissing("", path+".id")
		}
		if o.Center == nil {
			return nil, decodeMissing("", path+".center")
		}
		if o.Radius == nil {
			return nil, decodeMissing("", path+".radius")
		}
		if o.Base == nil {
			return nil, decodeMissing("", path+".base")
		}
		if o.Height == nil {
			return nil, decodeMissing("", path+".height")
		}
		p.Occluders = append(p.Occluders, model.Occluder{
			ID: *o.ID, Center: *o.Center, Radius: *o.Radius, Base: *o.Base, Height: *o.Height,
		})
	}

	for i, raw := range *rp.Sky {
		var s rawSky
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, model.NewError(model.CodeDecode, "sky sample %d is malformed: %v", i, err).
				WithField(fmt.Sprintf("sky[%d]", i))
		}
		path := fmt.Sprintf("sky[%d]", i)
		if s.ID == nil {
			return nil, decodeMissing("", path+".id")
		}
		if s.AzimuthDeg == nil {
			return nil, decodeMissing("", path+".azimuth_deg")
		}
		if s.ElevationDeg == nil {
			return nil, decodeMissing("", path+".elevation_deg")
		}
		if s.Radiance == nil {
			return nil, decodeMissing("", path+".radiance")
		}
		if s.Weight == nil {
			return nil, decodeMissing("", path+".weight")
		}
		p.Sky = append(p.Sky, model.SkySample{
			ID: *s.ID, AzimuthDeg: *s.AzimuthDeg, ElevationDeg: *s.ElevationDeg,
			Radiance: *s.Radiance, Weight: *s.Weight,
		})
	}

	for i, raw := range *rp.Sun {
		var s rawSun
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, model.NewError(model.CodeDecode, "sun sample %d is malformed: %v", i, err).
				WithField(fmt.Sprintf("sun[%d]", i))
		}
		path := fmt.Sprintf("sun[%d]", i)
		if s.Time == nil {
			return nil, decodeMissing("", path+".time")
		}
		if s.AzimuthDeg == nil {
			return nil, decodeMissing("", path+".azimuth_deg")
		}
		if s.ElevationDeg == nil {
			return nil, decodeMissing("", path+".elevation_deg")
		}
		if s.Radiance == nil {
			return nil, decodeMissing("", path+".radiance")
		}
		p.Sun = append(p.Sun, model.SunSample{
			Time: *s.Time, AzimuthDeg: *s.AzimuthDeg, ElevationDeg: *s.ElevationDeg, Radiance: *s.Radiance,
		})
	}
	return p, nil
}

// LoadProject decodes and validates a project.
func LoadProject(data []byte) (*model.Project, error) {
	p, err := DecodeProject(data)
	if err != nil {
		return nil, err
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

// LoadBundle decodes and validates the generated sample bundle.
func LoadBundle(data []byte) (*model.SampleBundle, error) {
	var rb rawBundle
	if err := json.Unmarshal(data, &rb); err != nil {
		return nil, model.NewError(model.CodeDecode, "sample bundle is truncated or malformed: %v", err).
			WithChunk(inferChunkID(data)).
			WithField(inferFieldPath(data))
	}
	if rb.Project == nil {
		return nil, decodeMissing("", "project")
	}
	p, err := LoadProject(*rb.Project)
	if err != nil {
		return nil, err
	}
	if rb.Truth == nil {
		return nil, decodeMissing("", "truth")
	}
	truth, err := DecodeTruth(*rb.Truth)
	if err != nil {
		return nil, err
	}
	if err := ValidateTruth(p, truth); err != nil {
		return nil, err
	}
	return &model.SampleBundle{Project: *p, Truth: *truth}, nil
}

// DecodeTruth decodes the truth inventory with required-field diagnostics.
func DecodeTruth(data []byte) (*model.Truth, error) {
	var raw rawTruth
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, model.NewError(model.CodeDecode, "truth inventory is truncated or malformed: %v", err).
			WithField(inferFieldPath(data))
	}
	if raw.Seed == nil {
		return nil, decodeMissing("", "truth.seed")
	}
	if raw.Points == nil {
		return nil, decodeMissing("", "truth.points")
	}
	truth := &model.Truth{Seed: *raw.Seed}
	for i, item := range *raw.Points {
		var point rawTruthPoint
		if err := json.Unmarshal(item, &point); err != nil {
			return nil, model.NewError(model.CodeDecode, "truth point %d is malformed: %v", i, err).
				WithField(fmt.Sprintf("truth.points[%d]", i))
		}
		chunkID := ""
		if point.ChunkID != nil {
			chunkID = *point.ChunkID
		}
		path := fmt.Sprintf("truth.points[%d]", i)
		switch {
		case point.ChunkID == nil:
			return nil, decodeMissing(chunkID, path+".chunk_id")
		case point.Time == nil:
			return nil, decodeMissing(chunkID, path+".time")
		case point.Vertex == nil:
			return nil, decodeMissing(chunkID, path+".vertex")
		case point.World == nil:
			return nil, decodeMissing(chunkID, path+".world")
		case point.DirectShadow == nil:
			return nil, decodeMissing(chunkID, path+".direct_shadow")
		case point.SkyVisibility == nil:
			return nil, decodeMissing(chunkID, path+".sky_visibility")
		case point.Irradiance == nil:
			return nil, decodeMissing(chunkID, path+".irradiance")
		case point.ShadowSteps == nil:
			return nil, decodeMissing(chunkID, path+".shadow_steps")
		case point.ShadowBudget == nil:
			return nil, decodeMissing(chunkID, path+".shadow_budget")
		}
		truth.Points = append(truth.Points, model.TruthPoint{
			ChunkID: *point.ChunkID, Time: *point.Time, Vertex: *point.Vertex, World: *point.World,
			DirectShadow: *point.DirectShadow, SkyVisibility: *point.SkyVisibility,
			Irradiance: *point.Irradiance, ShadowSteps: *point.ShadowSteps,
			ShadowBudget: *point.ShadowBudget, FirstHit: point.FirstHit,
		})
	}
	return truth, nil
}

// ValidateTruth verifies that the truth inventory covers every chunk vertex at
// every logical solar time exactly once.
func ValidateTruth(p *model.Project, truth *model.Truth) error {
	if truth == nil {
		return model.NewError(model.CodeDecode, "truth inventory is missing").WithField("truth")
	}
	if truth.Seed != p.Seed {
		return model.NewError(model.CodeValue, "truth seed %d does not match project seed %d", truth.Seed, p.Seed).
			WithField("truth.seed")
	}
	expected := 0
	chunks := make(map[string]model.Chunk, len(p.Chunks))
	for _, chunk := range p.Chunks {
		chunks[chunk.ID] = chunk
		expected += chunk.ResX * chunk.ResZ
	}
	expected *= len(p.Sun)
	if len(truth.Points) != expected {
		return model.NewError(model.CodeValue, "truth point count %d does not match expected %d", len(truth.Points), expected).
			WithField("truth.points")
	}
	sunTimes := make(map[int]struct{}, len(p.Sun))
	for _, sun := range p.Sun {
		sunTimes[sun.Time] = struct{}{}
	}
	seen := make(map[string]struct{}, len(truth.Points))
	for i, point := range truth.Points {
		path := fmt.Sprintf("truth.points[%d]", i)
		chunk, ok := chunks[point.ChunkID]
		if !ok {
			return model.NewError(model.CodeValue, "truth references unknown chunk %q", point.ChunkID).
				WithChunk(point.ChunkID).WithField(path + ".chunk_id")
		}
		if _, ok := sunTimes[point.Time]; !ok {
			return model.NewError(model.CodeValue, "truth references unknown solar time %d", point.Time).
				WithChunk(point.ChunkID).WithTime(point.Time).WithField(path + ".time")
		}
		if point.Vertex.X < 0 || point.Vertex.X >= chunk.ResX || point.Vertex.Z < 0 || point.Vertex.Z >= chunk.ResZ {
			return model.NewError(model.CodeValue, "truth vertex is outside chunk resolution").
				WithChunk(point.ChunkID).WithPoint(point.Vertex).WithField(path + ".vertex")
		}
		key := fmt.Sprintf("%s@%d#%d,%d", point.ChunkID, point.Time, point.Vertex.X, point.Vertex.Z)
		if _, exists := seen[key]; exists {
			return model.NewError(model.CodeValue, "duplicate truth point %s", key).
				WithChunk(point.ChunkID).WithPoint(point.Vertex).WithField(path)
		}
		seen[key] = struct{}{}
		if !finite(point.World.X) || !finite(point.World.Y) || !finite(point.World.Z) ||
			!finite(point.SkyVisibility) || point.SkyVisibility < 0 || point.SkyVisibility > 1 {
			return model.NewError(model.CodeValue, "truth contains non-finite geometry or invalid sky visibility").
				WithChunk(point.ChunkID).WithPoint(point.Vertex).WithField(path)
		}
		for ch, value := range point.Irradiance {
			if !finite(value) || value < 0 {
				return model.NewError(model.CodeValue, "truth irradiance is invalid").
					WithChunk(point.ChunkID).WithPoint(point.Vertex).
					WithField(fmt.Sprintf("%s.irradiance[%d]", path, ch)).WithChannel(channelName(ch))
			}
		}
	}
	return nil
}

// ValidateProject performs all generation-independent engineering checks.
func ValidateProject(p *model.Project) error {
	if p == nil {
		return model.NewError(model.CodeValue, "project is nil")
	}
	if err := validateBounds(p.Bounds); err != nil {
		return err
	}
	if err := validateParams(p.Params); err != nil {
		return err
	}
	if len(p.Chunks) == 0 {
		return model.NewError(model.CodeValue, "project has no chunks").WithField("chunks")
	}
	if len(p.Sun) == 0 {
		return model.NewError(model.CodeValue, "solar sequence is empty").WithField("sun")
	}
	if len(p.Sky) == 0 {
		return model.NewError(model.CodeValue, "sky sample set is empty").WithField("sky")
	}

	occluders := make(map[string]model.Occluder, len(p.Occluders))
	for i, o := range p.Occluders {
		path := fmt.Sprintf("occluders[%d]", i)
		if o.ID == "" {
			return model.NewError(model.CodeValue, "occluder id is empty").WithField(path + ".id")
		}
		if _, exists := occluders[o.ID]; exists {
			return model.NewError(model.CodeValue, "duplicate occluder id %q", o.ID).WithField(path + ".id")
		}
		if !finite(o.Center.X) || !finite(o.Center.Z) || !finite(o.Radius) || !finite(o.Base) || !finite(o.Height) {
			return model.NewError(model.CodeValue, "occluder has non-finite values").WithField(path)
		}
		if o.Radius <= 0 || o.Height <= 0 {
			return model.NewError(model.CodeValue, "occluder radius and height must be positive").WithField(path)
		}
		occluders[o.ID] = o
	}

	ids := make(map[string]struct{}, len(p.Chunks))
	grid := make(map[string]struct{}, len(p.Chunks))
	for i := range p.Chunks {
		c := &p.Chunks[i]
		path := fmt.Sprintf("chunks[%d]", i)
		if c.ID == "" {
			return model.NewError(model.CodeValue, "chunk id is empty").WithChunk(fmt.Sprintf("#%d", i)).WithField(path + ".id")
		}
		path = fmt.Sprintf("chunks[%d]", i)
		if _, exists := ids[c.ID]; exists {
			return model.NewError(model.CodeValue, "duplicate chunk id %q", c.ID).WithChunk(c.ID).WithField(path + ".id")
		}
		ids[c.ID] = struct{}{}
		gridKey := fmt.Sprintf("%d,%d", c.GridX, c.GridZ)
		if _, exists := grid[gridKey]; exists {
			return model.NewError(model.CodeValue, "duplicate chunk grid coordinate %s", gridKey).WithChunk(c.ID).WithField(path + ".grid_x")
		}
		grid[gridKey] = struct{}{}
		if err := validateBounds(c.Bounds); err != nil {
			return err.WithChunk(c.ID).WithField(path + ".bounds")
		}
		if c.Bounds.MinX < p.Bounds.MinX-1e-9 || c.Bounds.MinZ < p.Bounds.MinZ-1e-9 ||
			c.Bounds.MaxX > p.Bounds.MaxX+1e-9 || c.Bounds.MaxZ > p.Bounds.MaxZ+1e-9 {
			return model.NewError(model.CodeValue, "chunk lies outside terrain bounds").
				WithChunk(c.ID).WithField(path + ".bounds")
		}
		if c.ResX < 2 || c.ResZ < 2 {
			return model.NewError(model.CodeValue, "chunk resolution must be at least 2x2").
				WithChunk(c.ID).WithField(path + ".res_x")
		}
		want := c.ResX * c.ResZ
		if len(c.Heights) != want {
			return model.NewError(model.CodeValue, "height sample count %d does not match resolution %dx%d (%d)", len(c.Heights), c.ResX, c.ResZ, want).
				WithChunk(c.ID).WithField(path + ".heights")
		}
		for j, h := range c.Heights {
			if !finite(h) {
				return model.NewError(model.CodeValue, "height sample is non-finite").
					WithChunk(c.ID).WithField(fmt.Sprintf("%s.heights[%d]", path, j))
			}
		}
		for ch, a := range c.Albedo {
			if !finite(a) || a < 0 || a > 1 {
				return model.NewError(model.CodeValue, "albedo must be finite and in [0,1]").
					WithChunk(c.ID).WithField(fmt.Sprintf("%s.albedo[%d]", path, ch)).WithChannel(channelName(ch))
			}
		}
		seenRefs := make(map[string]struct{}, len(c.Occluders))
		for j, ref := range c.Occluders {
			if _, exists := occluders[ref]; !exists {
				return model.NewError(model.CodeValue, "occluder reference %q does not exist", ref).
					WithChunk(c.ID).WithField(fmt.Sprintf("%s.occluders[%d]", path, j))
			}
			if _, dup := seenRefs[ref]; dup {
				return model.NewError(model.CodeValue, "duplicate occluder reference %q", ref).
					WithChunk(c.ID).WithField(fmt.Sprintf("%s.occluders[%d]", path, j))
			}
			seenRefs[ref] = struct{}{}
		}
	}

	area := 0.0
	for i := 0; i < len(p.Chunks); i++ {
		area += p.Chunks[i].Bounds.Area()
		for j := i + 1; j < len(p.Chunks); j++ {
			if overlapArea(p.Chunks[i].Bounds, p.Chunks[j].Bounds) > 1e-8 {
				return model.NewError(model.CodeSeam, "chunks %s and %s overlap", p.Chunks[i].ID, p.Chunks[j].ID).
					WithChunk(p.Chunks[j].ID).
					WithField("chunks.bounds").
					WithEdge(model.EdgeID(p.Chunks[i].ID, p.Chunks[j].ID))
			}
		}
	}
	if math.Abs(area-p.Bounds.Area()) > 1e-7*math.Max(1, p.Bounds.Area()) {
		return model.NewError(model.CodeSeam, "chunk area sum %.9f does not cover terrain area %.9f", area, p.Bounds.Area()).
			WithField("chunks.bounds")
	}

	for i, s := range p.Sky {
		path := fmt.Sprintf("sky[%d]", i)
		if s.ID == "" {
			return model.NewError(model.CodeValue, "sky sample id is empty").WithField(path + ".id")
		}
		if !finite(s.AzimuthDeg) || !finite(s.ElevationDeg) || s.ElevationDeg < 0 || s.ElevationDeg > 90 {
			return model.NewError(model.CodeValue, "sky direction is outside [0,90] elevation").WithField(path)
		}
		if !finite(s.Weight) || s.Weight <= 0 {
			return model.NewError(model.CodeValue, "sky weight must be positive and finite").WithField(path + ".weight")
		}
		for ch, r := range s.Radiance {
			if !finite(r) || r < 0 {
				return model.NewError(model.CodeValue, "sky radiance must be finite and non-negative").
					WithField(fmt.Sprintf("%s.radiance[%d]", path, ch)).WithChannel(channelName(ch))
			}
			if r*float64(p.Params.FixedScale) > float64(p.Params.MaxChannelFixed) {
				return model.NewError(model.CodeValue, "sky radiance exceeds declared channel limit").
					WithField(fmt.Sprintf("%s.radiance[%d]", path, ch)).WithChannel(channelName(ch))
			}
		}
	}

	for i, s := range p.Sun {
		path := fmt.Sprintf("sun[%d]", i)
		if !finite(s.AzimuthDeg) || !finite(s.ElevationDeg) || s.ElevationDeg < 0 || s.ElevationDeg > 90 {
			return model.NewError(model.CodeValue, "solar direction is invalid").WithField(path)
		}
		if i > 0 {
			if s.Time <= p.Sun[i-1].Time {
				return model.NewError(model.CodeValue, "solar logical times must be strictly increasing").WithField(path + ".time")
			}
			if s.AzimuthDeg+1e-9 < p.Sun[i-1].AzimuthDeg {
				return model.NewError(model.CodeValue, "solar azimuth must be monotonic").WithField(path + ".azimuth_deg")
			}
		}
		for ch, r := range s.Radiance {
			if !finite(r) || r < 0 {
				return model.NewError(model.CodeValue, "solar radiance must be finite and non-negative").
					WithField(fmt.Sprintf("%s.radiance[%d]", path, ch)).WithChannel(channelName(ch))
			}
			if r*float64(p.Params.FixedScale) > float64(p.Params.MaxChannelFixed) {
				return model.NewError(model.CodeValue, "solar radiance exceeds declared channel limit").
					WithField(fmt.Sprintf("%s.radiance[%d]", path, ch)).WithChannel(channelName(ch))
			}
		}
	}

	if _, err := model.BuildSharedEdges(p); err != nil {
		return err
	}
	return nil
}

func validateBounds(b model.Bounds) *model.CodedError {
	if !finite(b.MinX) || !finite(b.MinZ) || !finite(b.MaxX) || !finite(b.MaxZ) {
		return model.NewError(model.CodeValue, "bounds contain non-finite values")
	}
	if b.MaxX <= b.MinX || b.MaxZ <= b.MinZ {
		return model.NewError(model.CodeValue, "bounds must have positive width and depth")
	}
	return nil
}

func validateParams(p model.BakeParams) error {
	if p.FixedScale <= 0 {
		return model.NewError(model.CodeValue, "fixed_scale must be positive").WithField("params.fixed_scale")
	}
	if p.MaxChannelFixed < p.FixedScale {
		return model.NewError(model.CodeValue, "max_channel_fixed must be at least fixed_scale").WithField("params.max_channel_fixed")
	}
	if p.ShadowStepBudget <= 0 {
		return model.NewError(model.CodeValue, "shadow_step_budget must be positive").WithField("params.shadow_step_budget")
	}
	if !finite(p.RayStep) || p.RayStep <= 0 {
		return model.NewError(model.CodeValue, "ray_step must be positive").WithField("params.ray_step")
	}
	if !finite(p.RayEpsilon) || p.RayEpsilon < 0 {
		return model.NewError(model.CodeValue, "ray_epsilon must be non-negative").WithField("params.ray_epsilon")
	}
	if !finite(p.IrradianceTolerance) || p.IrradianceTolerance < 0 {
		return model.NewError(model.CodeValue, "irradiance_tolerance must be non-negative").WithField("params.irradiance_tolerance")
	}
	if p.MaxParallelism <= 0 || p.VertexBufferCap <= 0 || p.QueueBudget <= 0 {
		return model.NewError(model.CodeValue, "scheduler budgets must be positive").WithField("params")
	}
	if p.AlgorithmVersion == "" {
		return model.NewError(model.CodeValue, "algorithm_version is empty").WithField("params.algorithm_version")
	}
	return nil
}

func overlapArea(a, b model.Bounds) float64 {
	w := math.Min(a.MaxX, b.MaxX) - math.Max(a.MinX, b.MinX)
	d := math.Min(a.MaxZ, b.MaxZ) - math.Max(a.MinZ, b.MinZ)
	if w <= 0 || d <= 0 {
		return 0
	}
	return w * d
}

func decodeMissing(chunkID, field string) error {
	return model.NewError(model.CodeDecode, "required field %s is missing", field).
		WithChunk(chunkID).
		WithField(field)
}

func inferChunkID(data []byte) string {
	matches := chunkIDPattern.FindAllSubmatch(data, -1)
	if len(matches) == 0 {
		return ""
	}
	id := string(matches[len(matches)-1][1])
	if strings.HasPrefix(id, "chunk-") {
		return id
	}
	return ""
}

func inferFieldPath(data []byte) string {
	matches := lastKeyPattern.FindAllSubmatch(data, -1)
	if len(matches) == 0 {
		return "$"
	}
	return "$." + string(matches[len(matches)-1][1])
}

func channelName(i int) string {
	return []string{"r", "g", "b"}[i]
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// SortedChunkIDs returns stable chunk ordering.
func SortedChunkIDs(p *model.Project) []string {
	out := make([]string, 0, len(p.Chunks))
	for _, c := range p.Chunks {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out
}

// SortedTimes returns stable logical-time ordering.
func SortedTimes(p *model.Project) []int {
	out := make([]int, 0, len(p.Sun))
	for _, s := range p.Sun {
		out = append(out, s.Time)
	}
	sort.Ints(out)
	return out
}
