package lightprobe

import (
	"fmt"
	"math"
)

func ValidateScene(s *Scene) error {
	if s == nil {
		return domainError(ESchema, "scene", "$", "nil scene")
	}
	if err := s.Params.Validate(); err != nil {
		return schemaFrom(err)
	}
	if len(s.Rooms) == 0 {
		return domainError(ESchema, "scene", "rooms", "at least one room is required")
	}

	roomIDs := map[string]bool{}
	for i := range s.Rooms {
		r := &s.Rooms[i]
		if r.ID == "" {
			return domainError(ESchema, fmt.Sprintf("rooms[%d]", i), "id", "room id is empty")
		}
		if roomIDs[r.ID] {
			return domainError(ESchema, r.ID, "id", "duplicate room id")
		}
		roomIDs[r.ID] = true
		if err := validateBounds(r.ID, r.Bounds); err != nil {
			return err
		}
		for axis, n := range r.Resolution {
			if n < 2 {
				return domainError(ESchema, r.ID, fmt.Sprintf("resolution[%d]", axis), "must be at least 2")
			}
		}
	}

	probeIDs := map[string]bool{}
	coordinates := map[string]string{}
	for i := range s.Probes {
		p := &s.Probes[i]
		if p.ID == "" {
			return domainError(ESchema, fmt.Sprintf("probes[%d]", i), "id", "probe id is empty")
		}
		if probeIDs[p.ID] {
			return domainError(ESchema, p.ID, "id", "duplicate probe id")
		}
		probeIDs[p.ID] = true
		room := s.Room(p.RoomID)
		if room == nil {
			return domainError(ESchema, p.ID, "room_id", "unknown room %q", p.RoomID)
		}
		if !room.Bounds.Contains(p.Position) {
			return domainError(ESchema, p.ID, "position", "probe %v is outside room %s bounds %v", p.Position, room.ID, room.Bounds)
		}
		coordKey := fmt.Sprintf("%.9f/%.9f/%.9f", p.Position.X, p.Position.Y, p.Position.Z)
		if other, ok := coordinates[coordKey]; ok {
			return domainError(ESchema, p.ID, "position", "probe coordinate duplicates %s", other)
		}
		coordinates[coordKey] = p.ID
		if p.SHScale != s.Params.FixedScale {
			return domainError(ESchema, p.ID, "sh_scale", "probe scale %d does not match declared %d", p.SHScale, s.Params.FixedScale)
		}
		for ch := 0; ch < 3; ch++ {
			for k := 0; k < 9; k++ {
				v := p.SH[ch][k]
				if v < s.Params.SHMinFixed || v > s.Params.SHMaxFixed {
					return domainError(ESchema, p.ID, fmt.Sprintf("sh[%d][%d]", ch, k),
						"fixed coefficient %d outside declared range [%d,%d]", v, s.Params.SHMinFixed, s.Params.SHMaxFixed)
				}
			}
		}
		if p.SampleCount < 0 {
			return domainError(ESchema, p.ID, "sample_count", "must be non-negative")
		}
		for ch := 0; ch < 3; ch++ {
			if math.IsNaN(p.Radiance[ch]) || math.IsInf(p.Radiance[ch], 0) {
				return domainError(ESchema, p.ID, fmt.Sprintf("radiance[%d]", ch), "must be finite")
			}
		}
	}

	if len(s.Directives) == 0 {
		return domainError(ESchema, "scene", "directives", "at least one resample directive is required")
	}
	for i, d := range s.Directives {
		if d.ID == "" {
			return domainError(ESchema, fmt.Sprintf("directives[%d]", i), "id", "directive id is empty")
		}
		if i > 0 && d.Time < s.Directives[i-1].Time {
			return domainError(ESchema, d.ID, "time", "directive time %.6f is earlier than %s time %.6f",
				d.Time, s.Directives[i-1].ID, s.Directives[i-1].Time)
		}
		if s.QueryByID(d.QueryID) == nil {
			return domainError(ESchema, d.ID, "query_id", "unknown query %q", d.QueryID)
		}
	}

	for i := range s.Queries {
		q := &s.Queries[i]
		if q.ID == "" {
			return domainError(ESchema, fmt.Sprintf("queries[%d]", i), "id", "query id is empty")
		}
		if s.Room(q.PrimaryRoomID) == nil {
			return domainError(ESchema, q.ID, "primary_room_id", "unknown room %q", q.PrimaryRoomID)
		}
		if q.NeighborRoomID != "" && s.Room(q.NeighborRoomID) == nil {
			return domainError(ESchema, q.ID, "neighbor_room_id", "unknown room %q", q.NeighborRoomID)
		}
	}
	return nil
}

func validateBounds(id string, b Bounds) error {
	if b.Min.X > b.Max.X || b.Min.Y > b.Max.Y || b.Min.Z > b.Max.Z {
		return domainError(ESchema, id, "bounds.min", "minimum corner %v must not exceed maximum %v", b.Min, b.Max)
	}
	if b.Min.X == b.Max.X || b.Min.Y == b.Max.Y || b.Min.Z == b.Max.Z {
		return domainError(ESchema, id, "bounds", "room extent must be positive: %v", b)
	}
	return nil
}

func schemaFrom(err error) error {
	if de, ok := AsDomainError(err); ok {
		de.Code = ESchema
		return de
	}
	return domainError(ESchema, "scene", "params", "%v", err)
}
