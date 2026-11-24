package engine

import (
	"context"
	"math"
	"sort"

	"cadlab/internal/geom"
	"cadlab/internal/model"
	"cadlab/internal/render"
)

type ProfilePlane struct {
	Normal model.Vec3 `json:"normal"`
	Offset float64    `json:"offset"`
}

type ProfileEndpoint struct {
	Label   string     `json:"label"`
	World   model.Vec3 `json:"world"`
	Param   model.Vec2 `json:"param"`
	Screen  model.Vec2 `json:"screen"`
	Visible bool       `json:"visible"`
	PatchID int        `json:"patchId"`
}

type ProfileSegment struct {
	PatchID int             `json:"patchId"`
	A       ProfileEndpoint `json:"a"`
	B       ProfileEndpoint `json:"b"`
}

type ProfileResult struct {
	Version  int64            `json:"version"`
	Plane    ProfilePlane     `json:"plane"`
	Segments []ProfileSegment `json:"segments"`
	Locked   bool             `json:"locked"`
}

func (e *Engine) Profile(ctx context.Context, normal model.Vec3, offset float64) (ProfileResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensureContext(ctx); err != nil {
		return ProfileResult{}, err
	}
	length := normal.Length()
	if length < 1e-9 || !normal.IsFinite() || math.IsNaN(offset) || math.IsInf(offset, 0) {
		return ProfileResult{}, model.Diag(model.CodePara, "profile plane is invalid")
	}
	n := normal.Mul(1 / length)
	plane := ProfilePlane{Normal: n, Offset: offset / length}
	result := ProfileResult{Version: e.state.Version, Plane: plane, Locked: true}
	for _, patch := range e.exact.Patches {
		m := patch.Mesh
		if m == nil {
			continue
		}
		for i := 0; i+2 < len(m.Indices); i += 3 {
			ia, ib, ic := m.Indices[i], m.Indices[i+1], m.Indices[i+2]
			a, b, c := m.Vertices[ia], m.Vertices[ib], m.Vertices[ic]
			da := n.Dot(a.Position) + plane.Offset
			db := n.Dot(b.Position) + plane.Offset
			dc := n.Dot(c.Position) + plane.Offset
			if math.Abs(da) < 1e-10 && math.Abs(db) < 1e-10 && math.Abs(dc) < 1e-10 {
				continue
			}
			var hits []ProfileEndpoint
			hits = append(hits, edgePlaneHit(patch.PatchID, a, b, da, db)...)
			hits = append(hits, edgePlaneHit(patch.PatchID, b, c, db, dc)...)
			hits = append(hits, edgePlaneHit(patch.PatchID, c, a, dc, da)...)
			hits = dedupeHits(hits)
			if len(hits) >= 2 {
				hits[0].Label = "A"
				hits[1].Label = "B"
				for i := range hits {
					sx, sy, visible := render.ProjectPoint(e.exact.Camera, hits[i].World)
					if visible {
						hits[i].Screen = model.Vec2{U: sx, V: sy}
						hits[i].Visible = true
					}
				}
				result.Segments = append(result.Segments, ProfileSegment{
					PatchID: patch.PatchID, A: hits[0], B: hits[1],
				})
			}
		}
	}
	sort.Slice(result.Segments, func(i, j int) bool {
		a, b := result.Segments[i], result.Segments[j]
		if a.PatchID != b.PatchID {
			return a.PatchID < b.PatchID
		}
		if a.A.World.X != b.A.World.X {
			return a.A.World.X < b.A.World.X
		}
		if a.A.World.Z != b.A.World.Z {
			return a.A.World.Z < b.A.World.Z
		}
		return a.A.World.Y < b.A.World.Y
	})
	e.profileLocks[result.Version] = result
	return result, nil
}

func (e *Engine) FrozenProfile(version int64) (ProfileResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.profileLocks[version]
	return p, ok
}

func edgePlaneHit(patchID int, a, b geom.Vertex, da, db float64) []ProfileEndpoint {
	if (da > 0 && db > 0) || (da < 0 && db < 0) {
		return nil
	}
	if math.Abs(da-db) < 1e-15 {
		return nil
	}
	t := da / (da - db)
	if t < -1e-9 || t > 1+1e-9 {
		return nil
	}
	world := a.Position.Lerp(b.Position, t)
	param := model.Vec2{
		U: a.UV.U + (b.UV.U-a.UV.U)*t,
		V: a.UV.V + (b.UV.V-a.UV.V)*t,
	}
	return []ProfileEndpoint{{World: world, Param: param, PatchID: patchID}}
}

func dedupeHits(hits []ProfileEndpoint) []ProfileEndpoint {
	var out []ProfileEndpoint
	for _, h := range hits {
		duplicate := false
		for _, v := range out {
			if h.World.Sub(v.World).Length() < 1e-8 {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, h)
		}
	}
	return out
}
