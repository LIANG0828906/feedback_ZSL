package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	InternalMilliPerUnit int64 = 1000
	DefaultMaxPacketSize       = 32768
	AlgorithmVersion           = "obs-mirror-kernel/v1"
)

type Scale int

const (
	Scale100 Scale = 100
	Scale125 Scale = 125
	Scale150 Scale = 150
	Scale200 Scale = 200
)

type Chroma int

const (
	Chroma420 Chroma = 420
	Chroma422 Chroma = 422
	Chroma444 Chroma = 444
)

func (s Scale) String() string {
	switch s {
	case Scale100:
		return "1x"
	case Scale125:
		return "1.25x"
	case Scale150:
		return "1.5x"
	case Scale200:
		return "2x"
	default:
		return fmt.Sprintf("scale(%d)", int(s))
	}
}

func (c Chroma) String() string {
	switch c {
	case Chroma420:
		return "4:2:0"
	case Chroma422:
		return "4:2:2"
	case Chroma444:
		return "4:4:4"
	default:
		return fmt.Sprintf("chroma(%d)", int(c))
	}
}

// BytesPerPixel is the conservative wire budget for a chroma mode. It is an
// integer so packet splitting never depends on floating-point arithmetic.
func (c Chroma) BytesPerPixel() int64 {
	switch c {
	case Chroma420:
		return 2
	case Chroma422:
		return 3
	case Chroma444:
		return 4
	default:
		return 4
	}
}

type Point struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type Size struct {
	W int64 `json:"w"`
	H int64 `json:"h"`
}

// Rect is in internal fixed-point logical coordinates: 1/1000 logical unit.
type Rect struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	W int64 `json:"w"`
	H int64 `json:"h"`
}

type PixelRect struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	W int64 `json:"w"`
	H int64 `json:"h"`
}

type Capability struct {
	Scales        []Scale  `json:"scales"`
	Chromas       []Chroma `json:"chromas"`
	MaxPacketSize int      `json:"max_packet_size"`
}

// Screen is the current, effective topology and capability record.
type Screen struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Origin     Point      `json:"origin"`
	Resolution Size       `json:"resolution"`
	Scale      Scale      `json:"scale"`
	Chroma     Chroma     `json:"chroma"`
	Capability Capability `json:"capability"`
}

func (s Screen) Clone() Screen {
	out := s
	out.Capability.Scales = append([]Scale(nil), s.Capability.Scales...)
	out.Capability.Chromas = append([]Chroma(nil), s.Capability.Chromas...)
	return out
}

func (s Screen) AllowsScale(scale Scale) bool {
	for _, candidate := range s.Capability.Scales {
		if candidate == scale {
			return true
		}
	}
	return false
}

func (s Screen) AllowsChroma(chroma Chroma) bool {
	for _, candidate := range s.Capability.Chromas {
		if candidate == chroma {
			return true
		}
	}
	return false
}

type EventKind string

const (
	EventJoin     EventKind = "join"
	EventExit     EventKind = "exit"
	EventTopology EventKind = "topology"
)

func (k EventKind) Priority() int {
	switch k {
	case EventExit:
		return 40
	case EventJoin:
		return 30
	case EventTopology:
		return 20
	default:
		return 0
	}
}

// TopologyPatch is a sparse event document. A nil field means "unchanged".
type TopologyPatch struct {
	Origin            *Point    `json:"origin,omitempty"`
	Resolution        *Size     `json:"resolution,omitempty"`
	Scale             *Scale    `json:"scale,omitempty"`
	Chroma            *Chroma   `json:"chroma,omitempty"`
	CapabilityScales  *[]Scale  `json:"capability_scales,omitempty"`
	CapabilityChromas *[]Chroma `json:"capability_chromas,omitempty"`
	MaxPacketSize     *int      `json:"max_packet_size,omitempty"`
}

func (p TopologyPatch) Clone() TopologyPatch {
	out := p
	if p.Origin != nil {
		v := *p.Origin
		out.Origin = &v
	}
	if p.Resolution != nil {
		v := *p.Resolution
		out.Resolution = &v
	}
	if p.Scale != nil {
		v := *p.Scale
		out.Scale = &v
	}
	if p.Chroma != nil {
		v := *p.Chroma
		out.Chroma = &v
	}
	if p.CapabilityScales != nil {
		v := append([]Scale(nil), (*p.CapabilityScales)...)
		out.CapabilityScales = &v
	}
	if p.CapabilityChromas != nil {
		v := append([]Chroma(nil), (*p.CapabilityChromas)...)
		out.CapabilityChromas = &v
	}
	if p.MaxPacketSize != nil {
		v := *p.MaxPacketSize
		out.MaxPacketSize = &v
	}
	return out
}

type Event struct {
	ID       string         `json:"id"`
	Window   int            `json:"window"`
	At       int64          `json:"at_tick"`
	Kind     EventKind      `json:"kind"`
	ScreenID string         `json:"screen_id"`
	Topology *TopologyPatch `json:"topology,omitempty"`
}

type Command struct {
	Window   int    `json:"window"`
	At       int64  `json:"at_tick"`
	ScreenID string `json:"screen_id"`
	Region   Rect   `json:"region"`
}

// DefaultTrace records every field filled while upgrading a lower schema.
type DefaultTrace struct {
	Path         string `json:"path"`
	OldValue     string `json:"old_value"`
	DefaultValue string `json:"default_value"`
	Source       string `json:"source"`
}

type Session struct {
	ID            string         `json:"id"`
	Seed          int64          `json:"seed"`
	SourceVersion string         `json:"source_version"`
	TickRate      int64          `json:"tick_rate"`
	InternalRate  int64          `json:"internal_tick_rate"`
	Screens       []Screen       `json:"screens"`
	Events        []Event        `json:"events"`
	Commands      []Command      `json:"commands"`
	UpgradeTraces []DefaultTrace `json:"upgrade_traces,omitempty"`
}

func (s *Session) Clone() *Session {
	if s == nil {
		return nil
	}
	out := *s
	out.Screens = make([]Screen, len(s.Screens))
	for i := range s.Screens {
		out.Screens[i] = s.Screens[i].Clone()
	}
	out.Events = make([]Event, len(s.Events))
	for i := range s.Events {
		out.Events[i] = s.Events[i]
		if s.Events[i].Topology != nil {
			p := s.Events[i].Topology.Clone()
			out.Events[i].Topology = &p
		}
	}
	out.Commands = append([]Command(nil), s.Commands...)
	out.UpgradeTraces = append([]DefaultTrace(nil), s.UpgradeTraces...)
	return &out
}

// Normalize sorts all collections into the canonical internal order.
func (s *Session) Normalize() {
	sort.Slice(s.Screens, func(i, j int) bool { return s.Screens[i].ID < s.Screens[j].ID })
	for i := range s.Screens {
		sort.Slice(s.Screens[i].Capability.Scales, func(a, b int) bool {
			return s.Screens[i].Capability.Scales[a] < s.Screens[i].Capability.Scales[b]
		})
		sort.Slice(s.Screens[i].Capability.Chromas, func(a, b int) bool {
			return s.Screens[i].Capability.Chromas[a] < s.Screens[i].Capability.Chromas[b]
		})
	}
	sort.Slice(s.Events, func(i, j int) bool {
		a, b := s.Events[i], s.Events[j]
		if a.At != b.At {
			return a.At < b.At
		}
		if a.ScreenID != b.ScreenID {
			return a.ScreenID < b.ScreenID
		}
		if a.Kind.Priority() != b.Kind.Priority() {
			return a.Kind.Priority() > b.Kind.Priority()
		}
		return a.ID < b.ID
	})
	for i := range s.Events {
		if s.Events[i].Topology == nil {
			continue
		}
		if s.Events[i].Topology.CapabilityScales != nil {
			sort.Slice(*s.Events[i].Topology.CapabilityScales, func(a, b int) bool {
				return (*s.Events[i].Topology.CapabilityScales)[a] < (*s.Events[i].Topology.CapabilityScales)[b]
			})
		}
		if s.Events[i].Topology.CapabilityChromas != nil {
			sort.Slice(*s.Events[i].Topology.CapabilityChromas, func(a, b int) bool {
				return (*s.Events[i].Topology.CapabilityChromas)[a] < (*s.Events[i].Topology.CapabilityChromas)[b]
			})
		}
	}
	sort.Slice(s.Commands, func(i, j int) bool {
		a, b := s.Commands[i], s.Commands[j]
		if a.Window != b.Window {
			return a.Window < b.Window
		}
		return a.ScreenID < b.ScreenID
	})
}

// Fingerprint hashes the normalized semantic model. Source version and upgrade
// traces are intentionally excluded, so all three schemas compare equal.
func (s *Session) Fingerprint() string {
	type canonical struct {
		ID           string    `json:"id"`
		Seed         int64     `json:"seed"`
		TickRate     int64     `json:"tick_rate"`
		InternalRate int64     `json:"internal_tick_rate"`
		Screens      []Screen  `json:"screens"`
		Events       []Event   `json:"events"`
		Commands     []Command `json:"commands"`
	}
	c := canonical{
		ID:           s.ID,
		Seed:         s.Seed,
		TickRate:     s.TickRate,
		InternalRate: s.InternalRate,
		Screens:      s.Screens,
		Events:       s.Events,
		Commands:     s.Commands,
	}
	data, _ := json.Marshal(c)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type SessionState string

const (
	SessionEstablished SessionState = "established"
	SessionNegotiating SessionState = "negotiating"
	SessionStreaming   SessionState = "streaming"
	SessionPaused      SessionState = "paused"
	SessionEnded       SessionState = "ended"
)

type ScreenState string

const (
	ScreenPending     ScreenState = "pending"
	ScreenNegotiating ScreenState = "negotiating"
	ScreenCommitted   ScreenState = "committed"
	ScreenFailed      ScreenState = "failed"
)

type WindowState string

const (
	WindowPending      WindowState = "pending"
	WindowDistributing WindowState = "distributing"
	WindowCommitted    WindowState = "committed"
	WindowFailed       WindowState = "failed"
)

type Decision struct {
	Window     int       `json:"window"`
	At         int64     `json:"at_tick"`
	ScreenID   string    `json:"screen_id"`
	Category   EventKind `json:"category"`
	Winner     string    `json:"winner"`
	Suppressed []string  `json:"suppressed,omitempty"`
	Rationale  string    `json:"rationale"`
}

type MappingParams struct {
	ScreenID         string `json:"screen_id"`
	OriginX          int64  `json:"origin_x"`
	OriginY          int64  `json:"origin_y"`
	ResolutionW      int64  `json:"resolution_w"`
	ResolutionH      int64  `json:"resolution_h"`
	Scale            Scale  `json:"scale"`
	ScaleNumerator   int64  `json:"scale_numerator"`
	ScaleDenominator int64  `json:"scale_denominator"`
	Chroma           Chroma `json:"chroma"`
	MaxPacketSize    int    `json:"max_packet_size"`
}

type MappedRegion struct {
	ScreenID   string        `json:"screen_id"`
	Source     Rect          `json:"source"`
	RawPixels  PixelRect     `json:"raw_pixels"`
	Pixels     PixelRect     `json:"pixels"`
	CropLeft   int64         `json:"crop_left"`
	CropTop    int64         `json:"crop_top"`
	CropRight  int64         `json:"crop_right"`
	CropBottom int64         `json:"crop_bottom"`
	Params     MappingParams `json:"params"`
}

type ErrorMetrics struct {
	CoordinateResidual int64 `json:"coordinate_residual"`
	ScaleResidual      int64 `json:"scale_residual"`
	CropError          int64 `json:"crop_error"`
	TimeResidual       int64 `json:"time_residual_ticks"`
	CoordinateBound    int64 `json:"coordinate_upper_bound"`
	ScaleBound         int64 `json:"scale_upper_bound"`
	CropBound          int64 `json:"crop_upper_bound"`
	TimeBound          int64 `json:"time_upper_bound"`
	TotalBound         int64 `json:"total_upper_bound"`
	ActualDeviation    int64 `json:"actual_deviation"`
	WithinBound        bool  `json:"within_bound"`
}

type Packet struct {
	Sequence     int       `json:"sequence"`
	GlobalSeq    int       `json:"global_sequence"`
	Coverage     PixelRect `json:"coverage"`
	ByteLength   int64     `json:"byte_length"`
	PayloadHash  string    `json:"payload_hash"`
	Replay       bool      `json:"replay"`
	SourceWindow int       `json:"source_window"`
}

type ScreenCommit struct {
	ScreenID      string       `json:"screen_id"`
	SourceWindow  int          `json:"source_window"`
	Mapped        MappedRegion `json:"mapped"`
	Metrics       ErrorMetrics `json:"metrics"`
	Packets       []Packet     `json:"packets"`
	ReplayWindows []int        `json:"replay_windows,omitempty"`
	ContentHash   string       `json:"content_hash"`
}

type RuntimeScreen struct {
	Screen     Screen      `json:"screen"`
	State      ScreenState `json:"state"`
	Active     bool        `json:"active"`
	LastWindow int         `json:"last_window"`
}

type RuntimeSnapshot struct {
	Window              int             `json:"window"`
	SessionState        SessionState    `json:"session_state"`
	ActiveScreens       []string        `json:"active_screens"`
	Screens             []RuntimeScreen `json:"screens"`
	TopologyVersion     int64           `json:"topology_version"`
	LastSourceRegion    *Rect           `json:"last_source_region,omitempty"`
	LastCommittedWindow int             `json:"last_committed_window"`
	EventCursor         int             `json:"event_cursor"`
}

type WindowCommit struct {
	Window           int             `json:"window"`
	At               int64           `json:"at_tick"`
	SourceRegion     Rect            `json:"source_region"`
	ActiveScreens    []string        `json:"active_screens"`
	Decisions        []Decision      `json:"decisions"`
	TopologyVersion  int64           `json:"topology_version"`
	Screens          []ScreenCommit  `json:"screens"`
	Snapshot         RuntimeSnapshot `json:"snapshot"`
	SessionHash      string          `json:"session_hash"`
	AlgorithmVersion string          `json:"algorithm_version"`
	ContentHash      string          `json:"content_hash"`
}

type QueueObservation struct {
	Window     int    `json:"window"`
	ScreenID   string `json:"screen_id"`
	Reason     string `json:"reason"`
	WaitMicros int64  `json:"wait_micros"`
}

type RunResult struct {
	Commits []WindowCommit     `json:"commits"`
	Final   RuntimeSnapshot    `json:"final"`
	Queue   []QueueObservation `json:"queue,omitempty"`
}

type RecoveryReport struct {
	ValidRecords    int    `json:"valid_records"`
	InvalidatedFrom int    `json:"invalidated_from,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

func HashJSON(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
