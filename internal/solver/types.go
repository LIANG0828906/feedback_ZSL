package solver

import (
	"encoding/json"
	"sort"

	"roomreverb/internal/fixed"
	"roomreverb/internal/model"
)

const DefaultPathBufferLimit = 4096

// PortalRef is the immutable door snapshot stored with each committed room.
type PortalRef struct {
	ID     string      `json:"id"`
	WallID string      `json:"wall_id"`
	Rooms  []string    `json:"rooms"`
	Center model.Point `json:"center"`
	Width  float64     `json:"width"`
}

func (p PortalRef) Clone() PortalRef {
	p.Rooms = append([]string(nil), p.Rooms...)
	return p
}

// PairResult is the per-listener solved path and acoustic result.
type PairResult struct {
	SourceID     string   `json:"source_id"`
	ListenerID   string   `json:"listener_id"`
	Reachable    bool     `json:"reachable"`
	RoomPath     []string `json:"room_path"`
	PortalPath   []string `json:"portal_path"`
	PathFixed    int64    `json:"path_fixed"`
	DecayFixed   int64    `json:"decay_fixed"`
	EnergyFixed  int64    `json:"energy_fixed"`
	PathMM       float64  `json:"path_mm"`
	DecayMS      float64  `json:"decay_ms"`
	EarlyEnergy  float64  `json:"early_energy"`
	ErrorCode    string   `json:"error_code,omitempty"`
	ErrorMessage string   `json:"error_message,omitempty"`
}

func (p PairResult) Clone() PairResult {
	p.RoomPath = append([]string(nil), p.RoomPath...)
	p.PortalPath = append([]string(nil), p.PortalPath...)
	return p
}

func (p PairResult) CanonicalJSONBytes() []byte {
	b, _ := json.Marshal(p)
	return b
}

type RoomStats struct {
	Pairs          int   `json:"pairs"`
	Reachable      int   `json:"reachable"`
	Unreachable    int   `json:"unreachable"`
	DecaySumFixed  int64 `json:"decay_sum_fixed"`
	EnergySumFixed int64 `json:"energy_sum_fixed"`
}

// RoomResult contains all listener work owned by one room. A room owns the
// listeners physically located inside it and every source-to-that-listener
// path, which makes room commits independent and stable.
type RoomResult struct {
	RoomID           string           `json:"room_id"`
	Params           model.RoomParams `json:"params"`
	Portals          []PortalRef      `json:"portals"`
	Neighbors        []string         `json:"neighbors"`
	PortalIDs        []string         `json:"portal_ids"`
	Pairs            []PairResult     `json:"pairs"`
	Stats            RoomStats        `json:"stats"`
	AlgorithmVersion string           `json:"algorithm_version"`
	Precision        string           `json:"precision"`
}

func (r RoomResult) Clone() RoomResult {
	out := r
	out.Portals = make([]PortalRef, len(r.Portals))
	for i, p := range r.Portals {
		out.Portals[i] = p.Clone()
	}
	out.Neighbors = append([]string(nil), r.Neighbors...)
	out.PortalIDs = append([]string(nil), r.PortalIDs...)
	out.Pairs = make([]PairResult, len(r.Pairs))
	for i, p := range r.Pairs {
		out.Pairs[i] = p.Clone()
	}
	return out
}

// CanonicalJSON emits deterministic bytes for hashing and comparisons.
func (r RoomResult) CanonicalJSON() []byte {
	c := r.Clone()
	sort.Slice(c.Pairs, func(i, j int) bool {
		if c.Pairs[i].ListenerID != c.Pairs[j].ListenerID {
			return c.Pairs[i].ListenerID < c.Pairs[j].ListenerID
		}
		return c.Pairs[i].SourceID < c.Pairs[j].SourceID
	})
	b, _ := json.Marshal(c)
	return b
}

func (r RoomResult) Pair(source, listener string) (PairResult, bool) {
	for _, p := range r.Pairs {
		if p.SourceID == source && p.ListenerID == listener {
			return p, true
		}
	}
	return PairResult{}, false
}

func FillPairNumbers(p *PairResult) {
	p.PathMM = fixed.Value(p.PathFixed).Float()
	p.DecayMS = fixed.Value(p.DecayFixed).Float()
	p.EarlyEnergy = fixed.Value(p.EnergyFixed).Float()
}
