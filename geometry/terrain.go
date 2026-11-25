// Package geometry implements deterministic terrain sampling and directional
// visibility rays. All queries are pure functions of the project height field,
// occluders, and the supplied direction.
package geometry

import (
	"fmt"
	"math"
	"sort"

	"terrainbake/model"
)

// Terrain is an immutable indexed view over a validated project.
type Terrain struct {
	project    *model.Project
	chunks     []*model.Chunk
	byID       map[string]*model.Chunk
	occluders  map[string]model.Occluder
	edges      []model.SharedEdge
	edgeByPair map[string]model.SharedEdge
	maxHeight  float64
}

// New builds the terrain index and shared-edge contract.
func New(p *model.Project) (*Terrain, error) {
	if p == nil {
		return nil, model.NewError(model.CodeValue, "cannot build terrain from nil project")
	}
	t := &Terrain{
		project:    p,
		byID:       make(map[string]*model.Chunk, len(p.Chunks)),
		occluders:  make(map[string]model.Occluder, len(p.Occluders)),
		edgeByPair: make(map[string]model.SharedEdge),
		maxHeight:  math.Inf(-1),
	}
	for i := range p.Chunks {
		c := &p.Chunks[i]
		if _, exists := t.byID[c.ID]; exists {
			return nil, model.NewError(model.CodeValue, "duplicate chunk id %q", c.ID).WithChunk(c.ID)
		}
		if c.ResX < 2 || c.ResZ < 2 {
			return nil, model.NewError(model.CodeValue, "chunk resolution must be at least 2x2").WithChunk(c.ID)
		}
		if len(c.Heights) != c.ResX*c.ResZ {
			return nil, model.NewError(model.CodeValue, "chunk height buffer does not match resolution").WithChunk(c.ID)
		}
		t.chunks = append(t.chunks, c)
		t.byID[c.ID] = c
		for _, h := range c.Heights {
			if h > t.maxHeight {
				t.maxHeight = h
			}
		}
	}
	sort.Slice(t.chunks, func(i, j int) bool { return t.chunks[i].ID < t.chunks[j].ID })
	for _, o := range p.Occluders {
		t.occluders[o.ID] = o
		if o.Top() > t.maxHeight {
			t.maxHeight = o.Top()
		}
	}
	edges, err := model.BuildSharedEdges(p)
	if err != nil {
		return nil, err
	}
	t.edges = edges
	for _, e := range edges {
		key := pairKey(e.ChunkA, e.ChunkB)
		if _, exists := t.edgeByPair[key]; exists {
			return nil, model.NewError(model.CodeSeam, "duplicate shared edge %s", e.ID).WithEdge(e.ID)
		}
		t.edgeByPair[key] = e
	}
	return t, nil
}

// Project returns the immutable project view.
func (t *Terrain) Project() *model.Project { return t.project }

// Chunks returns chunks in stable ID order.
func (t *Terrain) Chunks() []*model.Chunk {
	out := make([]*model.Chunk, len(t.chunks))
	copy(out, t.chunks)
	return out
}

// Chunk returns a chunk by ID.
func (t *Terrain) Chunk(id string) (*model.Chunk, bool) {
	c, ok := t.byID[id]
	return c, ok
}

// Edges returns the shared-edge inventory.
func (t *Terrain) Edges() []model.SharedEdge {
	out := make([]model.SharedEdge, len(t.edges))
	copy(out, t.edges)
	return out
}

// Edge returns a shared edge by ID.
func (t *Terrain) Edge(id string) (model.SharedEdge, bool) {
	for _, e := range t.edges {
		if e.ID == id {
			return e, true
		}
	}
	return model.SharedEdge{}, false
}

// ChunkAt returns the chunk containing a horizontal position. Shared bounds
// resolve to the chunk whose minimum bound equals the position, except at the
// terrain maximum. The duplicate edge heights are validated elsewhere.
func (t *Terrain) ChunkAt(x, z float64) (string, bool) {
	b := t.project.Bounds
	if x < b.MinX-1e-9 || x > b.MaxX+1e-9 || z < b.MinZ-1e-9 || z > b.MaxZ+1e-9 {
		return "", false
	}
	var fallback string
	for _, c := range t.chunks {
		cb := c.Bounds
		if x < cb.MinX-1e-9 || x > cb.MaxX+1e-9 || z < cb.MinZ-1e-9 || z > cb.MaxZ+1e-9 {
			continue
		}
		if math.Abs(x-cb.MinX) <= 1e-9 && math.Abs(z-cb.MinZ) <= 1e-9 {
			return c.ID, true
		}
		if fallback == "" {
			fallback = c.ID
		}
	}
	return fallback, fallback != ""
}

// HeightAt returns bilinearly interpolated terrain height. The containing chunk
// is selected by ChunkAt, so crossing a shared edge continues on the real
// adjacent height sequence.
func (t *Terrain) HeightAt(x, z float64) (float64, string, error) {
	id, ok := t.ChunkAt(x, z)
	if !ok {
		return 0, "", model.NewError(model.CodeValue, "height query outside terrain at %s", model.FormatWorld(x, z)).
			WithWorld(model.Vec2{X: x, Z: z})
	}
	h, err := t.HeightAtChunk(id, x, z)
	return h, id, err
}

// HeightAtChunk returns bilinear height from one named chunk, clamping to its
// footprint. It is used to verify both sides of a shared edge.
func (t *Terrain) HeightAtChunk(id string, x, z float64) (float64, error) {
	c, ok := t.byID[id]
	if !ok {
		return 0, model.NewError(model.CodeValue, "unknown chunk %q", id).WithChunk(id)
	}
	b := c.Bounds
	u := (x - b.MinX) / b.Width() * float64(c.ResX-1)
	v := (z - b.MinZ) / b.Depth() * float64(c.ResZ-1)
	u = clamp(u, 0, float64(c.ResX-1))
	v = clamp(v, 0, float64(c.ResZ-1))
	x0 := int(math.Floor(u))
	z0 := int(math.Floor(v))
	x1 := minInt(x0+1, c.ResX-1)
	z1 := minInt(z0+1, c.ResZ-1)
	tx := u - float64(x0)
	tz := v - float64(z0)
	h00 := c.Heights[z0*c.ResX+x0]
	h10 := c.Heights[z0*c.ResX+x1]
	h01 := c.Heights[z1*c.ResX+x0]
	h11 := c.Heights[z1*c.ResX+x1]
	top := h00 + (h10-h00)*tx
	bottom := h01 + (h11-h01)*tx
	return top + (bottom-top)*tz, nil
}

// Direction converts azimuth/elevation in degrees to a normalized vector
// pointing toward the light. Azimuth zero points north (+z), ninety east (+x).
func Direction(azimuthDeg, elevationDeg float64) model.Vec3 {
	az := azimuthDeg * math.Pi / 180
	el := elevationDeg * math.Pi / 180
	cosEl := math.Cos(el)
	return model.Vec3{
		X: math.Sin(az) * cosEl,
		Y: math.Sin(el),
		Z: math.Cos(az) * cosEl,
	}
}

// Normalize returns a unit vector.
func Normalize(v model.Vec3) model.Vec3 {
	n := math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z)
	if n == 0 {
		return v
	}
	return model.Vec3{X: v.X / n, Y: v.Y / n, Z: v.Z / n}
}

// TraceOptions fixes the visibility integration contract.
type TraceOptions struct {
	Budget     int
	Step       float64
	Epsilon    float64
	RecordPath bool
}

// TraceVisibility marches from start toward direction. Visibility is a pure
// function of the terrain, occluders, direction, and trace options.
func (t *Terrain) TraceVisibility(start, direction model.Vec3, opts TraceOptions) (model.ShadowResult, error) {
	if opts.Budget <= 0 {
		return model.ShadowResult{}, model.NewError(model.CodeValue, "shadow step budget must be positive")
	}
	if !(opts.Step > 0) || math.IsNaN(opts.Step) || math.IsInf(opts.Step, 0) {
		return model.ShadowResult{}, model.NewError(model.CodeValue, "ray step must be positive and finite")
	}
	dir := Normalize(direction)
	if dir.X == 0 && dir.Y == 0 && dir.Z == 0 {
		return model.ShadowResult{}, model.NewError(model.CodeValue, "ray direction is zero").
			WithWorld(model.Vec2{X: start.X, Z: start.Z})
	}
	if dir.Y < 0 {
		// A downward light direction is invalid for terrain illumination.
		return model.ShadowResult{}, model.NewError(model.CodeValue, "light direction points below horizon").
			WithWorld(model.Vec2{X: start.X, Z: start.Z})
	}

	res := model.ShadowResult{Visible: true, StepBudget: opts.Budget}
	origin := model.Vec3{X: start.X, Y: start.Y + opts.Epsilon, Z: start.Z}
	prevPos := origin
	prevChunk, prevOK := t.ChunkAt(origin.X, origin.Z)
	if opts.RecordPath && prevOK {
		res.SourceChunks = append(res.SourceChunks, prevChunk)
	}
	seenSources := make(map[string]struct{})
	if prevOK {
		seenSources[prevChunk] = struct{}{}
	}
	seenEdges := make(map[string]struct{})

	for step := 1; step <= opts.Budget; step++ {
		distance := opts.Step * float64(step)
		pos := model.Vec3{
			X: origin.X + dir.X*distance,
			Y: origin.Y + dir.Y*distance,
			Z: origin.Z + dir.Z*distance,
		}
		res.Steps = step
		if pos.Y > t.maxHeight+opts.Epsilon+1e-9 {
			return res, nil
		}
		if pos.X < t.project.Bounds.MinX || pos.X > t.project.Bounds.MaxX ||
			pos.Z < t.project.Bounds.MinZ || pos.Z > t.project.Bounds.MaxZ {
			return res, nil
		}

		chunkID, ok := t.ChunkAt(pos.X, pos.Z)
		if !ok {
			return res, nil
		}
		if opts.RecordPath && prevOK && chunkID != prevChunk {
			cross := boundaryCrossing(t, prevPos, pos, prevChunk)
			edgeID := model.EdgeID(prevChunk, chunkID)
			_, adjacent := t.edgeByPair[pairKey(prevChunk, chunkID)]
			if !adjacent {
				cross = t.snapCorner(prevChunk, chunkID, cross)
				edgeID = "corner:" + edgeID
			}
			hFrom, err := t.HeightAtChunk(prevChunk, cross.X, cross.Z)
			if err != nil {
				return model.ShadowResult{}, err
			}
			hTo, err := t.HeightAtChunk(chunkID, cross.X, cross.Z)
			if err != nil {
				return model.ShadowResult{}, err
			}
			if !adjacent && math.Abs(hFrom-hTo) > 1e-7 {
				return model.ShadowResult{}, model.NewError(model.CodeSeam, "ray crosses non-adjacent or inconsistent chunks %s -> %s", prevChunk, chunkID).
					WithChunk(chunkID).
					WithEdge(edgeID).
					WithWorld(cross)
			}
			consistent := math.Abs(hFrom-hTo) <= 1e-7
			res.Handoffs = append(res.Handoffs, model.Handoff{
				EdgeID:      edgeID,
				FromChunk:   prevChunk,
				ToChunk:     chunkID,
				Position:    cross,
				HeightFrom:  hFrom,
				HeightTo:    hTo,
				Consistent:  consistent,
				SourceChunk: prevChunk,
			})
			if !consistent {
				return model.ShadowResult{}, model.NewError(model.CodeSeam,
					"shared edge %s has mismatched heights %.9f vs %.9f", edgeID, hFrom, hTo).
					WithChunk(chunkID).
					WithEdge(edgeID).
					WithWorld(cross)
			}
			if _, exists := seenEdges[edgeID]; !exists {
				seenEdges[edgeID] = struct{}{}
				res.EdgeIDs = append(res.EdgeIDs, edgeID)
			}
			if _, exists := seenSources[chunkID]; !exists {
				seenSources[chunkID] = struct{}{}
				res.SourceChunks = append(res.SourceChunks, chunkID)
			}
		}
		prevChunk, prevOK, prevPos = chunkID, true, pos

		terrainHeight, err := t.HeightAtChunk(chunkID, pos.X, pos.Z)
		if err != nil {
			return model.ShadowResult{}, err
		}
		if pos.Y <= terrainHeight-opts.Epsilon {
			hit := model.Hit{Step: step, Position: pos, ChunkID: chunkID, Height: terrainHeight, Kind: "terrain"}
			res.Visible = false
			res.Shadow = true
			res.FirstHit = &hit
			res.Flip = &model.ShadowFlip{AtStep: step, Position: pos}
			sort.Strings(res.SourceChunks)
			sort.Strings(res.EdgeIDs)
			return res, nil
		}
		if hit, ok := t.occluderHit(pos); ok {
			hit.Step = step
			res.Visible = false
			res.Shadow = true
			res.FirstHit = &hit
			res.Flip = &model.ShadowFlip{AtStep: step, Position: pos}
			sort.Strings(res.SourceChunks)
			sort.Strings(res.EdgeIDs)
			return res, nil
		}
	}
	sort.Strings(res.SourceChunks)
	sort.Strings(res.EdgeIDs)
	return res, nil
}

func (t *Terrain) occluderHit(p model.Vec3) (model.Hit, bool) {
	for _, o := range t.project.Occluders {
		dx := p.X - o.Center.X
		dz := p.Z - o.Center.Z
		if dx*dx+dz*dz <= o.Radius*o.Radius && p.Y >= o.Base-t.project.Params.RayEpsilon && p.Y <= o.Top() {
			return model.Hit{
				Position: p,
				ChunkID:  chunkIDAt(t, p.X, p.Z),
				Height:   o.Top(),
				Kind:     "occluder",
				Occluder: o.ID,
			}, true
		}
	}
	return model.Hit{}, false
}

func chunkIDAt(t *Terrain, x, z float64) string {
	id, _ := t.ChunkAt(x, z)
	return id
}

func boundaryCrossing(t *Terrain, from, to model.Vec3, fromChunk string) model.Vec2 {
	lo, hi := 0.0, 1.0
	for i := 0; i < 48; i++ {
		mid := (lo + hi) / 2
		x := from.X + (to.X-from.X)*mid
		z := from.Z + (to.Z-from.Z)*mid
		id, ok := t.ChunkAt(x, z)
		if ok && id == fromChunk {
			lo = mid
		} else {
			hi = mid
		}
	}
	return model.Vec2{
		X: from.X + (to.X-from.X)*hi,
		Z: from.Z + (to.Z-from.Z)*hi,
	}
}

func (t *Terrain) snapCorner(fromID, toID string, cross model.Vec2) model.Vec2 {
	from, okFrom := t.byID[fromID]
	to, okTo := t.byID[toID]
	if !okFrom || !okTo {
		return cross
	}
	if to.Bounds.MinX >= from.Bounds.MaxX-1e-7 {
		cross.X = from.Bounds.MaxX
	} else if to.Bounds.MaxX <= from.Bounds.MinX+1e-7 {
		cross.X = from.Bounds.MinX
	}
	if to.Bounds.MinZ >= from.Bounds.MaxZ-1e-7 {
		cross.Z = from.Bounds.MaxZ
	} else if to.Bounds.MaxZ <= from.Bounds.MinZ+1e-7 {
		cross.Z = from.Bounds.MinZ
	}
	return cross
}

func pairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return fmt.Sprintf("%s\x00%s", a, b)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
