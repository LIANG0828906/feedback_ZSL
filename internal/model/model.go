package model

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaVersion    = "roomreverb.level.v1"
	TruthSchema      = "roomreverb.truth.v1"
	AlgorithmVersion = "roomreverb-solver-v1"
	Precision        = "Q16.16"
	SpeedOfSoundMMPS = 343000
)

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

func (r Rect) Right() float64  { return r.X + r.W }
func (r Rect) Bottom() float64 { return r.Y + r.H }
func (r Rect) Center() Point   { return Point{X: r.X + r.W/2, Y: r.Y + r.H/2} }

func (r Rect) ContainsInclusive(p Point) bool {
	const eps = 1e-9
	return p.X >= r.X-eps && p.X <= r.Right()+eps && p.Y >= r.Y-eps && p.Y <= r.Bottom()+eps
}

func (r Rect) Equal(o Rect) bool {
	return r.X == o.X && r.Y == o.Y && r.W == o.W && r.H == o.H
}

type RoomParams struct {
	T60MS        float64 `json:"t60_ms"`
	EarlyDensity float64 `json:"early_density"`
	ReverbGain   float64 `json:"reverb_gain"`
}

type Room struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Rect   Rect       `json:"rect"`
	Params RoomParams `json:"params"`
}

func (r Room) ValidateParams() error {
	if r.Params.T60MS < 50 || r.Params.T60MS > 10000 {
		return fmt.Errorf("t60_ms must be in [50,10000]")
	}
	if r.Params.EarlyDensity < 0 || r.Params.EarlyDensity > 1 {
		return fmt.Errorf("early_density must be in [0,1]")
	}
	if r.Params.ReverbGain < 0 || r.Params.ReverbGain > 4 {
		return fmt.Errorf("reverb_gain must be in [0,4]")
	}
	return nil
}

type Wall struct {
	ID    string   `json:"id"`
	Start Point    `json:"start"`
	End   Point    `json:"end"`
	Rooms []string `json:"rooms"`
}

func (w Wall) Length() float64 {
	dx := w.End.X - w.Start.X
	dy := w.End.Y - w.Start.Y
	return dx*dx + dy*dy
}

type Opening struct {
	ID     string  `json:"id"`
	WallID string  `json:"wall_id"`
	Center Point   `json:"center"`
	Width  float64 `json:"width"`
	Active bool    `json:"active"`
}

type Source struct {
	ID  string `json:"id"`
	Pos Point  `json:"pos"`
}

type Listener struct {
	ID  string `json:"id"`
	Pos Point  `json:"pos"`
}

type Level struct {
	SchemaVersion string     `json:"schema_version"`
	ID            string     `json:"id"`
	Seed          int64      `json:"seed"`
	Units         string     `json:"units"`
	Rooms         []Room     `json:"rooms"`
	Walls         []Wall     `json:"walls"`
	Openings      []Opening  `json:"openings"`
	Sources       []Source   `json:"sources"`
	Listeners     []Listener `json:"listeners"`
}

func (l Level) Room(id string) (Room, bool) {
	for _, r := range l.Rooms {
		if r.ID == id {
			return r, true
		}
	}
	return Room{}, false
}

func (l Level) Wall(id string) (Wall, bool) {
	for _, w := range l.Walls {
		if w.ID == id {
			return w, true
		}
	}
	return Wall{}, false
}

func (l Level) Source(id string) (Source, bool) {
	for _, s := range l.Sources {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

func (l Level) Listener(id string) (Listener, bool) {
	for _, v := range l.Listeners {
		if v.ID == id {
			return v, true
		}
	}
	return Listener{}, false
}

func (l Level) Opening(id string) (Opening, bool) {
	for _, o := range l.Openings {
		if o.ID == id {
			return o, true
		}
	}
	return Opening{}, false
}

type Tolerance struct {
	PathMM      float64 `json:"path_mm"`
	DecayMS     float64 `json:"decay_ms"`
	EarlyEnergy float64 `json:"early_energy"`
}

type PairTruth struct {
	SourceID    string   `json:"source_id"`
	ListenerID  string   `json:"listener_id"`
	Reachable   bool     `json:"reachable"`
	RoomPath    []string `json:"room_path"`
	DoorPath    []string `json:"door_path"`
	PathMM      float64  `json:"path_mm"`
	DecayMS     float64  `json:"decay_ms"`
	EarlyEnergy float64  `json:"early_energy"`
}

type Truth struct {
	SchemaVersion    string            `json:"schema_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	Precision        string            `json:"precision"`
	SpeedOfSoundMMPS int               `json:"speed_of_sound_mm_s"`
	Tolerance        Tolerance         `json:"tolerance"`
	ListenerRooms    map[string]string `json:"listener_rooms"`
	Pairs            []PairTruth       `json:"pairs"`
}

func (t Truth) Pair(sourceID, listenerID string) (PairTruth, bool) {
	for _, p := range t.Pairs {
		if p.SourceID == sourceID && p.ListenerID == listenerID {
			return p, true
		}
	}
	return PairTruth{}, false
}

// Clone returns a deep copy so edits can be validated without mutating the
// caller's level.
func (l Level) Clone() Level {
	out := l
	out.Rooms = append([]Room(nil), l.Rooms...)
	out.Walls = make([]Wall, len(l.Walls))
	for i, w := range l.Walls {
		out.Walls[i] = w
		out.Walls[i].Rooms = append([]string(nil), w.Rooms...)
	}
	out.Openings = append([]Opening(nil), l.Openings...)
	out.Sources = append([]Source(nil), l.Sources...)
	out.Listeners = append([]Listener(nil), l.Listeners...)
	return out
}

// Canonical returns a stable JSON representation used for content hashes.
func (l Level) Canonical() []byte {
	c := l.Clone()
	sort.Slice(c.Rooms, func(i, j int) bool { return c.Rooms[i].ID < c.Rooms[j].ID })
	sort.Slice(c.Walls, func(i, j int) bool { return c.Walls[i].ID < c.Walls[j].ID })
	sort.Slice(c.Openings, func(i, j int) bool { return c.Openings[i].ID < c.Openings[j].ID })
	sort.Slice(c.Sources, func(i, j int) bool { return c.Sources[i].ID < c.Sources[j].ID })
	sort.Slice(c.Listeners, func(i, j int) bool { return c.Listeners[i].ID < c.Listeners[j].ID })
	b, _ := json.Marshal(c)
	return b
}
