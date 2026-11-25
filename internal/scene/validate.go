package scene

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"lightprobe/internal/diag"
)

func ParseScene(data []byte) (*Scene, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, diag.Schema(typeErr.Field, "invalid type: %v", err)
		}
		return nil, diag.Schema("$", "invalid JSON: %v", err)
	}
	for _, key := range []string{
		"schema_version", "seed", "sampling_parameter_version", "algorithm_version",
		"tolerance", "rooms", "probes", "photon_grid", "pixels", "truth",
	} {
		if rawMissing(top[key]) {
			return nil, diag.Schema(key, "missing required field")
		}
	}
	if err := validateRequiredShape(top); err != nil {
		return nil, err
	}

	var scene Scene
	if err := json.Unmarshal(data, &scene); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, diag.Schema(typeErr.Field, "invalid field type: %v", err)
		}
		return nil, diag.Schema("$", "invalid scene: %v", err)
	}
	if err := Validate(&scene); err != nil {
		return nil, err
	}
	return &scene, nil
}

func validateRequiredShape(top map[string]json.RawMessage) error {
	var roomRaws []json.RawMessage
	if err := json.Unmarshal(top["rooms"], &roomRaws); err != nil {
		return diag.Schema("rooms", "rooms must be an array")
	}
	for i, raw := range roomRaws {
		obj, err := rawObject(raw)
		if err != nil {
			return diag.Schema(fmt.Sprintf("rooms[%d]", i), "room must be an object")
		}
		for _, field := range []string{"id", "index", "name", "bounds", "resolution", "grid_offset"} {
			if rawMissing(obj[field]) {
				return roomSchemaError(i, field, "missing room field %q", field)
			}
		}
		bounds, err := rawObject(obj["bounds"])
		if err != nil || rawMissing(bounds["min"]) || rawMissing(bounds["max"]) {
			return roomSchemaError(i, "bounds", "bounds requires min and max")
		}
		resolution, err := rawObject(obj["resolution"])
		if err != nil {
			return roomSchemaError(i, "resolution", "resolution must be an object")
		}
		for _, field := range []string{"x", "y", "z"} {
			if rawMissing(resolution[field]) {
				return roomSchemaError(i, "resolution."+field, "missing resolution.%s", field)
			}
		}
	}

	var probeRaws []json.RawMessage
	if err := json.Unmarshal(top["probes"], &probeRaws); err != nil {
		return diag.Schema("probes", "probes must be an array")
	}
	for i, raw := range probeRaws {
		obj, err := rawObject(raw)
		if err != nil {
			return diag.Schema(fmt.Sprintf("probes[%d]", i), "probe must be an object")
		}
		for _, field := range []string{"id", "room_id", "i", "j", "k", "position", "radiance", "visibility_samples"} {
			if rawMissing(obj[field]) {
				return &diag.Error{
					Code: diag.CodeSchema, ID: jsonString(obj["id"]),
					Field: field, Message: fmt.Sprintf("probe[%d] missing field %q", i, field),
				}
			}
		}
	}
	return nil
}

func Validate(s *Scene) error {
	if s.SchemaVersion == "" {
		return diag.Schema("schema_version", "missing schema version")
	}
	if s.SchemaVersion != SchemaVersion {
		return diag.Schema("schema_version", "unsupported schema version %q", s.SchemaVersion)
	}
	if s.SamplingParameterVersion == "" {
		return diag.Schema("sampling_parameter_version", "missing sampling parameter version")
	}
	if s.AlgorithmVersion == "" {
		return diag.Schema("algorithm_version", "missing algorithm version")
	}
	if s.Tolerance.Radiance <= 0 || s.Tolerance.Color <= 0 || s.Tolerance.Jump <= 0 {
		return diag.Schema("tolerance", "all tolerances must be positive")
	}
	if len(s.Rooms) == 0 {
		return diag.Schema("rooms", "at least one room is required")
	}

	roomIDs := make(map[string]int, len(s.Rooms))
	for i := range s.Rooms {
		room := &s.Rooms[i]
		if room.ID == "" {
			return roomSchemaError(i, "id", "missing room id")
		}
		if prev, exists := roomIDs[room.ID]; exists {
			return (&diag.Error{
				Code: diag.CodeSchema, RoomID: room.ID, RoomIndex: i, Field: "id",
				Message: fmt.Sprintf("duplicate room id at indexes %d and %d", prev, i),
			})
		}
		roomIDs[room.ID] = i
		if room.Resolution.X <= 0 || room.Resolution.Y <= 0 || room.Resolution.Z <= 0 {
			return (&diag.Error{
				Code: diag.CodeSchema, RoomID: room.ID, RoomIndex: i, Field: "resolution",
				Message: "resolution x/y/z must be positive",
			})
		}
		for axis := 0; axis < 3; axis++ {
			if room.Bounds.Min[axis] > room.Bounds.Max[axis] {
				return diag.Geom(room.ID, i, "bounds", "min[%d]=%.6g is greater than max[%d]=%.6g",
					axis, room.Bounds.Min[axis], axis, room.Bounds.Max[axis])
			}
			if room.Bounds.Max[axis]-room.Bounds.Min[axis] <= 0 {
				return diag.Geom(room.ID, i, "bounds", "axis %d has non-positive extent", axis)
			}
		}
		if room.BlendRule == "" {
			room.BlendRule = "linear_half_mix"
		}
		if room.BoundaryBlendWidth <= 0 {
			room.BoundaryBlendWidth = 0.5
		}
	}

	for i := 0; i < len(s.Rooms); i++ {
		for j := i + 1; j < len(s.Rooms); j++ {
			if s.Rooms[i].Bounds.InteriorsOverlap(s.Rooms[j].Bounds) {
				err := diag.Geom(s.Rooms[i].ID, s.Rooms[i].Index, "bounds",
					"room overlaps room %s", s.Rooms[j].ID)
				err.Details = map[string]any{"other_room_id": s.Rooms[j].ID, "other_room_index": s.Rooms[j].Index}
				return err
			}
		}
	}

	counts := make(map[string]int, len(s.Rooms))
	probeIDs := make(map[string]struct{}, len(s.Probes))
	for _, probe := range s.Probes {
		if probe.ID == "" {
			return diag.Schema("probes.id", "probe is missing id")
		}
		if _, dup := probeIDs[probe.ID]; dup {
			return (&diag.Error{Code: diag.CodeSchema, ID: probe.ID, Field: "id", Message: "duplicate probe id"})
		}
		probeIDs[probe.ID] = struct{}{}
		room, roomIndex, ok := s.RoomByID(probe.RoomID)
		if !ok {
			return (&diag.Error{
				Code: diag.CodeSchema, ID: probe.ID, RoomID: probe.RoomID,
				Field: "room_id", Message: "probe references unknown room",
			})
		}
		if probe.VisibilitySamples < 0 {
			return (&diag.Error{
				Code: diag.CodeSchema, ID: probe.ID, RoomID: room.ID, RoomIndex: roomIndex,
				Field: "visibility_samples", Message: "visibility sample count must be non-negative",
			})
		}
		if probe.I < 0 || probe.I >= room.Resolution.X ||
			probe.J < 0 || probe.J >= room.Resolution.Y ||
			probe.K < 0 || probe.K >= room.Resolution.Z {
			return diag.Geom(room.ID, roomIndex, "ijk", "probe %s has out-of-range grid index (%d,%d,%d)",
				probe.ID, probe.I, probe.J, probe.K)
		}
		expectedPosition := ProbePosition(room, probe.I, probe.J, probe.K)
		if !vecNear(probe.Position, expectedPosition, 1e-7) {
			err := diag.Geom(room.ID, roomIndex, "position", "probe %s position %s does not match lattice %s",
				probe.ID, probe.Position, expectedPosition)
			err.ID = probe.ID
			return err
		}
		if !room.Bounds.Contains(probe.Position, 1e-7) {
			return diag.Geom(room.ID, roomIndex, "position", "probe %s is outside room bounds", probe.ID).
				WithID(probe.ID)
		}
		for c := 0; c < 3; c++ {
			if math.IsNaN(probe.Radiance[c]) || math.IsInf(probe.Radiance[c], 0) {
				return (&diag.Error{
					Code: diag.CodeSchema, ID: probe.ID, RoomID: room.ID, RoomIndex: roomIndex,
					Field: "radiance", Message: "radiance channels must be finite",
				})
			}
		}
		counts[probe.RoomID]++
	}
	for _, room := range s.Rooms {
		expected := room.Resolution.Product()
		actual := counts[room.ID]
		if actual != expected {
			return diag.Geom(room.ID, room.Index, "resolution",
				"probe count %d does not match resolution product %d", actual, expected)
		}
	}

	for _, boundary := range s.Boundaries {
		if _, _, ok := s.RoomByID(boundary.RoomA); !ok {
			return diag.Schema("boundaries.room_a", "boundary %s references unknown room", boundary.ID)
		}
		if _, _, ok := s.RoomByID(boundary.RoomB); !ok {
			return diag.Schema("boundaries.room_b", "boundary %s references unknown room", boundary.ID)
		}
		if boundary.Axis < 0 || boundary.Axis > 2 {
			return diag.Schema("boundaries.axis", "boundary %s axis must be 0, 1, or 2", boundary.ID)
		}
	}
	if s.PhotonGrid.Scale <= 0 {
		return diag.Schema("photon_grid.fixed_scale", "fixed point scale must be positive")
	}
	if len(s.PhotonGrid.Cells) != s.PhotonGrid.Dims[0]*s.PhotonGrid.Dims[1]*s.PhotonGrid.Dims[2] {
		return diag.Schema("photon_grid.cells", "photon cell count does not match dims product")
	}
	for _, truth := range s.Truth.Probes {
		if _, _, ok := s.ProbeByID(truth.ProbeID); !ok {
			return diag.Schema("truth.probes.probe_id", "truth references unknown probe %s", truth.ProbeID)
		}
	}
	return nil
}

func rawMissing(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func roomSchemaError(index int, field, format string, args ...any) *diag.Error {
	roomID := ""
	if field == "id" {
		roomID = ""
	}
	return &diag.Error{
		Code: diag.CodeSchema, RoomIndex: index, RoomID: roomID, Field: field,
		Message: fmt.Sprintf(format, args...),
	}
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func vecNear(a, b Vec3, eps float64) bool {
	for i := 0; i < 3; i++ {
		if math.Abs(a[i]-b[i]) > eps {
			return false
		}
	}
	return true
}
