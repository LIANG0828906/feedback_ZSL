package model

type EditKind string

const (
	EditSetRoomParams EditKind = "set_room_params"
	EditAddPortal     EditKind = "add_portal"
	EditRemovePortal  EditKind = "remove_portal"
	EditMoveRoomEdge  EditKind = "move_room_edge"
)

// Edit is the serialized editor operation used by both the preview path and
// the deterministic verification command.
type Edit struct {
	Kind     EditKind    `json:"kind"`
	RoomID   string      `json:"room_id,omitempty"`
	Params   *RoomParams `json:"params,omitempty"`
	Portal   *Opening    `json:"portal,omitempty"`
	PortalID string      `json:"portal_id,omitempty"`
	Edge     string      `json:"edge,omitempty"`
	DeltaMM  float64     `json:"delta_mm,omitempty"`
}

type EditFile struct {
	LevelID          string `json:"level_id"`
	AlgorithmVersion string `json:"algorithm_version"`
	Edits            []Edit `json:"edits"`
}
