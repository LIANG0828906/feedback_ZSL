package grid

import (
	"encoding/json"
	"fmt"
)

// ValidateNormals checks that unit normals are actually perpendicular to
// their obstacle segment. Non-unit input remains E_DECODE at schema load.
func ValidateNormals(level *Level, cam Camera) error {
	for i := range level.Obstacles {
		o := &level.Obstacles[i]
		if NormalMatchesSegment(o.A, o.B, o.Normal) {
			continue
		}
		mid := Point{X: (o.A.X + o.B.X) / 2, Y: (o.A.Y + o.B.Y) / 2}
		ge := &Error{
			Code: CodeNorm, ObjectID: o.ID, Field: fmt.Sprintf("obstacles[%d].normal", i),
			Message:  "unit normal is not perpendicular to obstacle segment",
			Measured: fmt.Sprintf("normal=(%d,%d)", o.Normal.X, o.Normal.Y),
			Expected: "normal perpendicular to A->B",
		}
		if tile, ok := cam.TileAt(mid); ok {
			ge.Tile = &tile
		}
		return ge
	}
	return nil
}

type EndpointRevision struct {
	ObstacleID string `json:"obstacleId"`
	Frame      int    `json:"frame"`
	X          int64  `json:"x"`
	Y          int64  `json:"y"`
}

// ValidateEndpointTimeline rejects effective endpoint revisions whose logical
// frame moves backwards.
func ValidateEndpointTimeline(level *Level, cam Camera, revisions []EndpointRevision) error {
	lastFrame := -1 << 30
	lastObstacle := ""
	for i, rev := range revisions {
		if lastObstacle == rev.ObstacleID && rev.Frame < lastFrame {
			o := level.ObstacleByID(rev.ObstacleID)
			ge := &Error{
				Code: CodeOrder, ObjectID: rev.ObstacleID,
				Field:    fmt.Sprintf("endpointRevisions[%d].frame", i),
				Message:  fmt.Sprintf("endpoint effective frame went backwards: %d < %d", rev.Frame, lastFrame),
				Measured: fmt.Sprint(rev.Frame), Expected: fmt.Sprintf(">= %d", lastFrame),
			}
			if o != nil {
				mid := Point{X: (o.A.X + o.B.X) / 2, Y: (o.A.Y + o.B.Y) / 2}
				if tile, ok := cam.TileAt(mid); ok {
					ge.Tile = &tile
				}
			}
			return ge
		}
		lastFrame = rev.Frame
		lastObstacle = rev.ObstacleID
	}
	return nil
}

// InjectDecodeFault deletes a required field from a v1 document and returns
// the stable E_DECODE produced by the unified loader.
func InjectDecodeFault(level *Level) ([]byte, *Error) {
	art, err := ExportVersion(level, 1)
	if err != nil {
		return nil, &Error{Code: CodeDecode, Field: "export", Message: err.Error()}
	}
	var root map[string]any
	if err := json.Unmarshal(art.Data, &root); err != nil {
		return art.Data, &Error{Code: CodeDecode, Field: "$", Message: err.Error()}
	}
	obs, _ := root["obstacles"].([]any)
	if len(obs) > 0 {
		first, _ := obs[0].(map[string]any)
		delete(first, "start_mm")
		obs[0] = first
	}
	corrupt, _ := json.Marshal(root)
	_, _, err = DecodeVersion(corrupt)
	ge, ok := AsError(err, CodeDecode)
	if !ok {
		return corrupt, decodeError("OB01", "$.obstacles[0].start_mm", "injected corruption was not rejected")
	}
	return corrupt, ge
}

// InjectDuplicateFault duplicates the second obstacle ID and returns E_DECODE.
func InjectDuplicateFault(level *Level) *Error {
	art, err := ExportVersion(level, 3)
	if err != nil {
		return &Error{Code: CodeDecode, Message: err.Error()}
	}
	var root map[string]any
	_ = json.Unmarshal(art.Data, &root)
	barriers := root["barriers"].([]any)
	first := barriers[0].(map[string]any)
	second := barriers[1].(map[string]any)
	second["key"] = first["key"]
	barriers[1] = second
	root["barriers"] = barriers
	data, _ := json.Marshal(root)
	_, _, err = DecodeVersion(data)
	ge, ok := AsError(err, CodeDecode)
	if !ok {
		return decodeError("OB01", "$.barriers[1].key", "duplicate was not rejected")
	}
	return ge
}

func InjectOrderFault(level *Level, cam Camera) *Error {
	revisions := []EndpointRevision{
		{ObstacleID: "OB01", Frame: 0, X: 1000, Y: 12000},
		{ObstacleID: "OB01", Frame: 5, X: 1200, Y: 12000},
		{ObstacleID: "OB01", Frame: 3, X: 1400, Y: 12000},
	}
	err := ValidateEndpointTimeline(level, cam, revisions)
	ge, _ := AsError(err, CodeOrder)
	return ge
}

func InjectNormFault(level *Level, cam Camera) (*Level, *Error) {
	mutated := cloneLevel(level)
	o := mutated.ObstacleByID("OB01")
	if o != nil {
		o.Normal = Vec2{X: 500, Y: 866} // unit-length, but not perpendicular to horizontal OB01
	}
	err := ValidateNormals(mutated, cam)
	ge, _ := AsError(err, CodeNorm)
	return mutated, ge
}

func InjectBlockFault(cam Camera) *Error {
	tile := TileID{Col: 1, Row: 1}
	return BlockError(tile, "OB01", "tile.projection.rayBuffer", "injected single-tile ray intersection failure")
}

func InjectBounceFault(level *Level) (*PredictResult, *Error) {
	launch := *level.LaunchByID("LN05")
	launch.BounceLimit = 1
	result, err := Predict(level, "REQ-INJECT-BOUNCE", launch)
	ge, _ := AsError(err, CodeBounce)
	return result, ge
}
