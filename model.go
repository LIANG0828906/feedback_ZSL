package mirror

import (
	"fmt"
	"sort"
	"strings"
)

const (
	AlgorithmVersion        = "mirror-1.0"
	DefaultTickMillis int64 = 10
)

// Schema versions supported by the loader and exporter.
const (
	SchemaV1 = "1"
	SchemaV2 = "2"
	SchemaV3 = "3"
)

// Scale is a percentage. It is kept as an integer in the internal model.
type Scale int64

const (
	Scale100 Scale = 100
	Scale125 Scale = 125
	Scale150 Scale = 150
	Scale200 Scale = 200
)

func (s Scale) String() string { return fmt.Sprintf("%d%%", int64(s)) }

// Chroma identifies a YUV sampling layout.
type Chroma string

const (
	Chroma444 Chroma = "4:4:4"
	Chroma422 Chroma = "4:2:2"
	Chroma420 Chroma = "4:2:0"
	Chroma411 Chroma = "4:1:1"
)

func (c Chroma) BytesPerPixelNum() int64 {
	switch c {
	case Chroma444:
		return 3
	case Chroma422:
		return 2
	case Chroma420, Chroma411:
		return 3
	default:
		return 0
	}
}

func (c Chroma) BytesPerPixelDen() int64 {
	switch c {
	case Chroma420, Chroma411:
		return 2
	default:
		return 1
	}
}

// Align returns the chroma block alignment for horizontal and vertical axes.
func (c Chroma) Align() (int64, int64) {
	switch c {
	case Chroma444:
		return 1, 1
	case Chroma422:
		return 2, 1
	case Chroma420:
		return 2, 2
	case Chroma411:
		return 4, 1
	default:
		return 1, 1
	}
}

// Region is an axis-aligned rectangle. X2/Y2 are derived, not stored.
type Region struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	W int64 `json:"w"`
	H int64 `json:"h"`
}

func (r Region) X2() int64   { return r.X + r.W }
func (r Region) Y2() int64   { return r.Y + r.H }
func (r Region) Empty() bool { return r.W <= 0 || r.H <= 0 }
func (r Region) Area() int64 { return r.W * r.H }

// Valid reports whether the rectangle has a non-negative origin and positive
// dimensions. It does not check for overflow at the far edge.
func (r Region) Valid() bool {
	return r.X >= 0 && r.Y >= 0 && r.W > 0 && r.H > 0
}

// Intersect clips r to bounds and returns the visible area.
func (r Region) Intersect(bounds Region) Region {
	x0 := max64(r.X, bounds.X)
	y0 := max64(r.Y, bounds.Y)
	x1 := min64(r.X2(), bounds.X2())
	y1 := min64(r.Y2(), bounds.Y2())
	if x1 <= x0 || y1 <= y0 {
		return Region{X: x0, Y: y0}
	}
	return Region{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// Domain is a topology domain. Parent is empty for a root domain.
type Domain struct {
	ID      string `json:"id"`
	Parent  string `json:"parent,omitempty"`
	OriginX int64  `json:"originX"`
	OriginY int64  `json:"originY"`
}

// Screen is one controlled display in the internal model.
type Screen struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Domain             string   `json:"domain"`
	Master             bool     `json:"master"`
	OriginX            int64    `json:"originX"`
	OriginY            int64    `json:"originY"`
	Width              int64    `json:"width"`
	Height             int64    `json:"height"`
	ScaleCapabilities  []Scale  `json:"scaleCapabilities"`
	DefaultScale       Scale    `json:"defaultScale"`
	ChromaCapabilities []Chroma `json:"chromaCapabilities"`
	DefaultChroma      Chroma   `json:"defaultChroma"`
	MaxPacketBytes     int64    `json:"maxPacketBytes"`
}

func (s Screen) SupportsScale(v Scale) bool {
	for _, x := range s.ScaleCapabilities {
		if x == v {
			return true
		}
	}
	return false
}

func (s Screen) SupportsChroma(v Chroma) bool {
	for _, x := range s.ChromaCapabilities {
		if x == v {
			return true
		}
	}
	return false
}

// EventKind is represented explicitly so arbitration never depends on string
// map iteration order.
type EventKind string

const (
	EventJoin      EventKind = "join"
	EventLeave     EventKind = "leave"
	EventTopology  EventKind = "topology"
	EventNegotiate EventKind = "negotiate"
)

// Event is a logical-time endpoint or topology change.
type Event struct {
	ID           string    `json:"id"`
	Kind         EventKind `json:"kind"`
	ScreenID     string    `json:"screenId,omitempty"`
	Domain       string    `json:"domain,omitempty"`
	FromDomain   string    `json:"fromDomain,omitempty"`
	ToDomain     string    `json:"toDomain,omitempty"`
	AtTick       int64     `json:"atTick"`
	TimeoutTicks int64     `json:"timeoutTicks,omitempty"`
}

// Window is one frame-window command. Region is always expressed in the
// internal absolute logical coordinate space.
type Window struct {
	ID             string `json:"id"`
	Index          int    `json:"index"`
	StartTick      int64  `json:"startTick"`
	EndTick        int64  `json:"endTick"`
	Region         Region `json:"region"`
	Scale          Scale  `json:"scale"`
	Chroma         Chroma `json:"chroma"`
	MaxPacketBytes int64  `json:"maxPacketBytes"`
	// SourceTimeResidualMs records lossy milliseconds-to-ticks input rounding.
	// It is derived metadata and is excluded from semantic equivalence.
	SourceTimeResidualMs int64 `json:"sourceTimeResidualMs,omitempty"`
}

// ConversionTrace preserves one source-to-internal field conversion.
type ConversionTrace struct {
	Version  string `json:"version"`
	Path     string `json:"path"`
	OldValue any    `json:"oldValue,omitempty"`
	NewValue any    `json:"newValue"`
	Rule     string `json:"rule"`
	Residual int64  `json:"residual,omitempty"`
}

// Session is the only model consumed by mapping and execution code.
type Session struct {
	ID                      string            `json:"id"`
	Seed                    int64             `json:"seed"`
	SourceVersion           string            `json:"sourceVersion"`
	AlgorithmVersion        string            `json:"algorithmVersion"`
	TickMillis              int64             `json:"tickMillis"`
	TimeRounding            RoundingMode      `json:"-"`
	NegotiationTimeoutTicks int64             `json:"negotiationTimeoutTicks"`
	ReconnectTimeoutTicks   int64             `json:"reconnectTimeoutTicks"`
	MasterScreenID          string            `json:"masterScreenId"`
	Domains                 []Domain          `json:"domains"`
	Screens                 []Screen          `json:"screens"`
	Events                  []Event           `json:"events"`
	Windows                 []Window          `json:"windows"`
	Traces                  []ConversionTrace `json:"traces"`
	Warnings                []Loss            `json:"warnings,omitempty"`
}

// ScreenByID returns a screen in constant time only after the session has been
// normalized. It still validates the ID instead of silently returning zero.
func (s *Session) ScreenByID(id string) (Screen, bool) {
	for _, screen := range s.Screens {
		if screen.ID == id {
			return screen, true
		}
	}
	return Screen{}, false
}

func (s *Session) DomainByID(id string) (Domain, bool) {
	for _, domain := range s.Domains {
		if domain.ID == id {
			return domain, true
		}
	}
	return Domain{}, false
}

// NormalizeSession sorts unordered collections into a canonical order and
// validates cross references. Loaders call it before returning any session.
func NormalizeSession(s *Session) error {
	if s == nil {
		return errf(CodeSchema, "session", "session is nil")
	}
	if s.ID == "" {
		return errf(CodeSchema, "session_id", "required value is missing")
	}
	if s.TickMillis <= 0 {
		return &Error{Code: CodeSchema, Path: "tick_ms", Message: "must be greater than zero"}
	}
	if s.AlgorithmVersion == "" {
		s.AlgorithmVersion = AlgorithmVersion
	}
	if s.SourceVersion == "" {
		return errf(CodeSchema, "schema_version", "required value is missing")
	}
	seenScreens := map[string]bool{}
	for i := range s.Screens {
		sc := &s.Screens[i]
		if sc.ID == "" {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].id", i), "required value is missing")
		}
		if seenScreens[sc.ID] {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].id", i), "duplicate screen id %q", sc.ID)
		}
		seenScreens[sc.ID] = true
		if sc.Name == "" {
			sc.Name = sc.ID
		}
		if sc.Width <= 0 || sc.Height <= 0 {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].resolution", i), "width and height must be positive")
		}
		if sc.MaxPacketBytes <= 0 {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].max_packet_bytes", i), "must be positive")
		}
		if len(sc.ScaleCapabilities) == 0 {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].scale_profiles", i), "at least one scale is required")
		}
		if len(sc.ChromaCapabilities) == 0 {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].chroma_modes", i), "at least one chroma mode is required")
		}
		for j, v := range sc.ScaleCapabilities {
			if !validScale(v) {
				return errf(CodeSchema, fmt.Sprintf("screens[%d].scale_profiles[%d]", i, j), "unknown scale enum %d", v)
			}
		}
		for j, v := range sc.ChromaCapabilities {
			if !validChroma(v) {
				return errf(CodeSchema, fmt.Sprintf("screens[%d].chroma_modes[%d]", i, j), "unknown chroma enum %q", v)
			}
		}
		if sc.DefaultScale == 0 {
			sc.DefaultScale = sc.ScaleCapabilities[0]
		}
		if sc.DefaultChroma == "" {
			sc.DefaultChroma = sc.ChromaCapabilities[0]
		}
		if !sc.SupportsScale(sc.DefaultScale) {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].default_scale", i), "default is not in scale capability set")
		}
		if !sc.SupportsChroma(sc.DefaultChroma) {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].default_chroma", i), "default is not in chroma capability set")
		}
		sort.Slice(sc.ScaleCapabilities, func(a, b int) bool { return sc.ScaleCapabilities[a] < sc.ScaleCapabilities[b] })
		sort.Slice(sc.ChromaCapabilities, func(a, b int) bool {
			return chromaRank(sc.ChromaCapabilities[a]) < chromaRank(sc.ChromaCapabilities[b])
		})
	}
	if s.MasterScreenID == "" {
		for _, sc := range s.Screens {
			if sc.Master {
				s.MasterScreenID = sc.ID
				break
			}
		}
	}
	if _, ok := s.ScreenByID(s.MasterScreenID); !ok {
		return errf(CodeSchema, "master_screen_id", "unknown screen %q", s.MasterScreenID)
	}
	seenDomains := map[string]bool{}
	for i := range s.Domains {
		d := &s.Domains[i]
		if d.ID == "" {
			return errf(CodeSchema, fmt.Sprintf("domains[%d].id", i), "required value is missing")
		}
		if seenDomains[d.ID] {
			return errf(CodeSchema, fmt.Sprintf("domains[%d].id", i), "duplicate domain id %q", d.ID)
		}
		seenDomains[d.ID] = true
	}
	for i := range s.Screens {
		if !seenDomains[s.Screens[i].Domain] {
			return errf(CodeSchema, fmt.Sprintf("screens[%d].domain", i), "unknown domain %q", s.Screens[i].Domain)
		}
	}
	for i, d := range s.Domains {
		if d.Parent != "" && !seenDomains[d.Parent] {
			return errf(CodeSchema, fmt.Sprintf("domains[%d].parent", i), "unknown parent domain %q", d.Parent)
		}
	}
	for _, d := range s.Domains {
		if !domainAcyclic(s.Domains, d.ID) {
			return errf(CodeSchema, "domains."+d.ID+".parent", "domain parent cycle detected")
		}
	}
	sort.Slice(s.Domains, func(i, j int) bool { return s.Domains[i].ID < s.Domains[j].ID })
	sort.Slice(s.Screens, func(i, j int) bool { return s.Screens[i].ID < s.Screens[j].ID })
	sort.SliceStable(s.Events, func(i, j int) bool {
		if s.Events[i].AtTick != s.Events[j].AtTick {
			return s.Events[i].AtTick < s.Events[j].AtTick
		}
		if eventPriority(s.Events[i].Kind) != eventPriority(s.Events[j].Kind) {
			return eventPriority(s.Events[i].Kind) > eventPriority(s.Events[j].Kind)
		}
		return s.Events[i].ID < s.Events[j].ID
	})
	for i, ev := range s.Events {
		path := fmt.Sprintf("events[%d]", i)
		if ev.ID == "" {
			return errf(CodeSchema, path+".id", "required value is missing")
		}
		if eventPriority(ev.Kind) == 0 {
			return errf(CodeSchema, path+".kind", "unknown event kind %q", ev.Kind)
		}
		if ev.AtTick < 0 {
			return errf(CodeSchema, path+".at", "logical time must be non-negative")
		}
		if ev.TimeoutTicks < 0 {
			return errf(CodeSchema, path+".timeout", "timeout must be non-negative")
		}
		if ev.ScreenID == "" {
			return errf(CodeSchema, path+".screen_id", "required value is missing")
		}
		if !seenScreens[ev.ScreenID] {
			return errf(CodeSchema, path+".screen_id", "unknown screen %q", ev.ScreenID)
		}
		domainFields := []struct {
			key    string
			domain string
		}{
			{"domain", ev.Domain},
			{"from_domain", ev.FromDomain},
			{"to_domain", ev.ToDomain},
		}
		for _, field := range domainFields {
			if field.domain != "" && !seenDomains[field.domain] {
				return errf(CodeSchema, path+"."+field.key, "unknown domain %q", field.domain)
			}
		}
	}
	sort.SliceStable(s.Windows, func(i, j int) bool {
		if s.Windows[i].Index != s.Windows[j].Index {
			return s.Windows[i].Index < s.Windows[j].Index
		}
		return s.Windows[i].ID < s.Windows[j].ID
	})
	seenWindows := map[string]bool{}
	seenWindowIndexes := map[int]bool{}
	for i := range s.Windows {
		w := &s.Windows[i]
		if w.ID == "" {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].id", i), "required value is missing")
		}
		if w.Index < 0 {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].index", i), "must be non-negative")
		}
		if seenWindows[w.ID] {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].id", i), "duplicate window id %q", w.ID)
		}
		seenWindows[w.ID] = true
		if seenWindowIndexes[w.Index] {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].index", i), "duplicate window index %d", w.Index)
		}
		seenWindowIndexes[w.Index] = true
		if w.StartTick < 0 {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].start_tick", i), "must be non-negative")
		}
		if w.EndTick < w.StartTick {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].time", i), "end precedes start")
		}
		if !w.Region.Valid() {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].region", i), "region must have non-negative origin and positive size")
		}
		if w.MaxPacketBytes <= 0 {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].max_packet_bytes", i), "must be positive")
		}
		if !validScale(w.Scale) {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].scale", i), "unknown scale enum %d", w.Scale)
		}
		if !validChroma(w.Chroma) {
			return errf(CodeSchema, fmt.Sprintf("windows[%d].chroma", i), "unknown chroma enum %q", w.Chroma)
		}
		if _, err := AddChecked(fmt.Sprintf("windows[%d].region.x2", i), w.Region.X, w.Region.W); err != nil {
			return err
		}
		if _, err := AddChecked(fmt.Sprintf("windows[%d].region.y2", i), w.Region.Y, w.Region.H); err != nil {
			return err
		}
	}
	sort.SliceStable(s.Traces, func(i, j int) bool {
		if s.Traces[i].Version != s.Traces[j].Version {
			return s.Traces[i].Version < s.Traces[j].Version
		}
		return s.Traces[i].Path < s.Traces[j].Path
	})
	return nil
}

func validScale(v Scale) bool {
	switch v {
	case Scale100, Scale125, Scale150, Scale200:
		return true
	default:
		return false
	}
}

func validChroma(c Chroma) bool {
	switch c {
	case Chroma444, Chroma422, Chroma420, Chroma411:
		return true
	default:
		return false
	}
}

func domainAcyclic(domains []Domain, start string) bool {
	parent := map[string]string{}
	for _, d := range domains {
		parent[d.ID] = d.Parent
	}
	seen := map[string]bool{}
	for cur := start; cur != ""; cur = parent[cur] {
		if seen[cur] {
			return false
		}
		seen[cur] = true
	}
	return true
}

func chromaRank(c Chroma) int {
	switch c {
	case Chroma444:
		return 0
	case Chroma422:
		return 1
	case Chroma420:
		return 2
	case Chroma411:
		return 3
	default:
		return 99
	}
}

func eventPriority(k EventKind) int {
	switch k {
	case EventLeave:
		return 400
	case EventTopology:
		return 300
	case EventNegotiate:
		return 200
	case EventJoin:
		return 100
	default:
		return 0
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func abs64(v int64) int64 {
	if v < 0 {
		if v == -1<<63 {
			return 1<<63 - 1
		}
		return -v
	}
	return v
}

func joinStrings(values []string) string {
	return strings.Join(values, ", ")
}
