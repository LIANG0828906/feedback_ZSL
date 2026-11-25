package probe

import (
	"math"

	"lightprobe/internal/diag"
	"lightprobe/internal/scene"
)

type cornerRef struct {
	ID     string
	I      int
	J      int
	K      int
	Weight float64
}

type InterpolationResult struct {
	QueryID        string     `json:"query_id,omitempty"`
	RoomID         string     `json:"room_id"`
	Position       scene.Vec3 `json:"position"`
	Radiance       scene.Vec3 `json:"radiance"`
	OwnerRadiance  scene.Vec3 `json:"owner_radiance"`
	OtherRadiance  scene.Vec3 `json:"other_radiance,omitempty"`
	Alpha          float64    `json:"blend_alpha"`
	BoundaryID     string     `json:"boundary_id,omitempty"`
	AdjacentRoomID string     `json:"adjacent_room_id,omitempty"`
	Corners        []string   `json:"corners"`
}

func rangeError(roomID string, roomIndex int, p scene.Vec3, format string, args ...any) error {
	err := diag.New(diag.CodeRange, format, args...)
	err.Code = diag.CodeRange
	err.RoomID = roomID
	err.RoomIndex = roomIndex
	err.Field = "position"
	err.Details = map[string]any{"position": p}
	return err
}

func (e *Engine) trilinearCornersLocked(room scene.Room, p scene.Vec3) ([]cornerRef, error) {
	if !room.Bounds.Contains(p, 1e-7) {
		return nil, rangeError(room.ID, room.Index, p,
			"query position %s is outside room %s bounds", p, room.ID)
	}
	grid := e.lattice[room.ID]
	res := [3]int{room.Resolution.X, room.Resolution.Y, room.Resolution.Z}
	step := scene.ProbeStep(room)
	indices := [3][2]int{}
	weights := [3][2]float64{}

	for axis := 0; axis < 3; axis++ {
		n := res[axis]
		if n == 1 || step[axis] == 0 {
			indices[axis] = [2]int{0, 0}
			weights[axis] = [2]float64{1, 0}
			continue
		}
		raw := (p[axis] - room.Bounds.Min[axis] - room.GridOffset[axis]) / step[axis]
		if raw < 0 && raw > -1e-8 {
			raw = 0
		}
		if raw > float64(n-1) && raw < float64(n-1)+1e-8 {
			raw = float64(n - 1)
		}
		if raw < -1e-8 || raw > float64(n-1)+1e-8 {
			return nil, rangeError(room.ID, room.Index, p,
				"query coordinate axis %d=%.9g is outside probe lattice [0,%d]", axis, raw, n-1)
		}
		i0 := int(math.Floor(raw))
		if i0 < 0 {
			i0 = 0
		}
		t := raw - float64(i0)
		if i0 >= n-1 {
			i0 = n - 2
			t = 1
		}
		if i0 < 0 {
			i0 = 0
		}
		i1 := i0 + 1
		if i1 >= n {
			i1 = n - 1
		}
		indices[axis] = [2]int{i0, i1}
		weights[axis] = [2]float64{1 - t, t}
	}

	corners := make([]cornerRef, 0, 8)
	for di := 0; di < 2; di++ {
		for dj := 0; dj < 2; dj++ {
			for dk := 0; dk < 2; dk++ {
				i := indices[0][di]
				j := indices[1][dj]
				k := indices[2][dk]
				id := grid[i][j][k]
				if id == "" {
					return nil, diag.Geom(room.ID, room.Index, "resolution",
						"missing lattice probe at (%d,%d,%d)", i, j, k)
				}
				weight := weights[0][di] * weights[1][dj] * weights[2][dk]
				corners = append(corners, cornerRef{ID: id, I: i, J: j, K: k, Weight: weight})
			}
		}
	}
	return corners, nil
}

func (e *Engine) trilinearCornersByRoomIDLocked(roomID string, p scene.Vec3) ([]cornerRef, error) {
	room, index, ok := e.scene.RoomByID(roomID)
	if !ok {
		return nil, rangeError(roomID, -1, p, "unknown room %q", roomID)
	}
	_ = index
	return e.trilinearCornersLocked(room, p)
}

func sumTrilinear(records map[string]*Record, corners []cornerRef) (scene.Vec3, []string, error) {
	var out scene.Vec3
	ids := make([]string, 0, len(corners))
	for _, corner := range corners {
		record, ok := records[corner.ID]
		if !ok {
			return out, ids, diag.New(diag.CodeSchema, "%s", "missing reconstructed probe "+corner.ID).WithID(corner.ID)
		}
		if record.Status != ProbeReconstructed {
			return out, ids, (&diag.Error{
				Code: diag.CodeState, ID: corner.ID, RoomID: record.RoomID,
				Field: "status", Message: "probe is not reconstructed: " + string(record.Status),
			})
		}
		for channel := 0; channel < 3; channel++ {
			out[channel] += corner.Weight * record.Radiance[channel]
		}
		ids = append(ids, corner.ID)
	}
	return out, ids, nil
}

func (e *Engine) interpolateRawLocked(roomID string, p scene.Vec3) (scene.Vec3, []cornerRef, []string, error) {
	room, _, ok := e.scene.RoomByID(roomID)
	if !ok {
		return scene.Vec3{}, nil, nil, rangeError(roomID, -1, p, "unknown room %q", roomID)
	}
	corners, err := e.trilinearCornersLocked(room, p)
	if err != nil {
		return scene.Vec3{}, nil, nil, err
	}
	radiance, ids, err := sumTrilinear(e.records, corners)
	return radiance, corners, ids, err
}

func (e *Engine) interpolateBlendLocked(roomID string, p scene.Vec3) (InterpolationResult, error) {
	var room scene.Room
	var roomIndex int
	var ok bool
	if roomID == "" {
		room, roomIndex, ok = scene.FindRoomAt(e.scene, p, 1e-7)
		if !ok {
			err := diag.New(diag.CodeRange, "position %s is not inside any declared room", p)
			err.Field = "position"
			err.Details = map[string]any{"position": p, "room_id": ""}
			return InterpolationResult{}, err
		}
		roomID = room.ID
	} else {
		room, roomIndex, ok = e.scene.RoomByID(roomID)
		if !ok {
			return InterpolationResult{}, rangeError(roomID, -1, p, "unknown room %q", roomID)
		}
		if !room.Bounds.Contains(p, 1e-7) {
			return InterpolationResult{}, rangeError(roomID, room.Index, p,
				"query position %s is outside declared room %s", p, roomID)
		}
	}
	_ = roomIndex

	ownerRadiance, _, cornerIDs, err := e.interpolateRawLocked(roomID, p)
	if err != nil {
		return InterpolationResult{}, err
	}
	result := InterpolationResult{
		RoomID: roomID, Position: p, Radiance: ownerRadiance,
		OwnerRadiance: ownerRadiance, Corners: cornerIDs,
	}

	blend, hasBlend := scene.BoundarySampleAt(e.scene, roomID, p)
	if hasBlend && blend.Alpha > 0 {
		otherRadiance, _, otherCornerIDs, err := e.interpolateRawLocked(blend.Other.ID, blend.OtherAt)
		if err != nil {
			return InterpolationResult{}, err
		}
		result.OtherRadiance = otherRadiance
		result.Alpha = blend.Alpha
		result.BoundaryID = blend.Rule.ID
		result.AdjacentRoomID = blend.Other.ID
		result.Corners = append(result.Corners, otherCornerIDs...)
		for channel := 0; channel < 3; channel++ {
			result.Radiance[channel] = (1-blend.Alpha)*ownerRadiance[channel] + blend.Alpha*otherRadiance[channel]
		}
	}
	return result, nil
}

func (e *Engine) Interpolate(roomID string, p scene.Vec3) (InterpolationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.interpolateBlendLocked(roomID, p)
}

func (e *Engine) QueryRadiance(queryID string, roomID string, p scene.Vec3) (InterpolationResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result, err := e.interpolateBlendLocked(roomID, p)
	if err != nil {
		return result, err
	}
	result.QueryID = queryID
	if queryID != "" {
		queryNode := InterpNode("adhoc:" + queryID)
		e.graph.AddNode(Node{ID: queryNode, Kind: "interpolation", RoomID: result.RoomID, Label: queryID})
		for _, cornerID := range result.Corners {
			e.graph.AddDependency(queryNode, ReconNode(cornerID))
		}
	}
	return result, nil
}
