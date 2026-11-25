package lightshow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Version int

const (
	Version1 Version = 1
	Version2 Version = 2
	Version3 Version = 3
)

func (v Version) String() string {
	switch v {
	case Version1:
		return "v1"
	case Version2:
		return "v2"
	case Version3:
		return "v3"
	default:
		return fmt.Sprintf("v%d", int(v))
	}
}

func ParseVersion(s string) (Version, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "v1":
		return Version1, true
	case "2", "v2":
		return Version2, true
	case "3", "v3":
		return Version3, true
	default:
		return 0, false
	}
}

type Interpolation string

const (
	Hold   Interpolation = "hold"
	Linear Interpolation = "linear"
)

type TimeRounding string

const (
	RoundNearest TimeRounding = "nearest"
	RoundFloor   TimeRounding = "floor"
	RoundCeil    TimeRounding = "ceil"
)

type RGB struct {
	R uint16 `json:"r"`
	G uint16 `json:"g"`
	B uint16 `json:"b"`
}

func (c RGB) String() string {
	return fmt.Sprintf("%d/%d/%d", c.R, c.G, c.B)
}

// Values keeps "not present" distinct from a legitimate zero value.
type Values struct {
	Color     *RGB    `json:"color,omitempty"`
	Intensity *int    `json:"intensity,omitempty"`
	Position  *[3]int `json:"position,omitempty"`
	Focus     *int    `json:"focus,omitempty"`
}

func (v Values) Empty() bool {
	return v.Color == nil && v.Intensity == nil && v.Position == nil && v.Focus == nil
}

func (v Values) Clone() Values {
	var out Values
	if v.Color != nil {
		c := *v.Color
		out.Color = &c
	}
	if v.Intensity != nil {
		n := *v.Intensity
		out.Intensity = &n
	}
	if v.Position != nil {
		p := *v.Position
		out.Position = &p
	}
	if v.Focus != nil {
		n := *v.Focus
		out.Focus = &n
	}
	return out
}

type IntRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func (r IntRange) Clamp(v int) int {
	if v < r.Min {
		return r.Min
	}
	if v > r.Max {
		return r.Max
	}
	return v
}

type FixtureRanges struct {
	Intensity IntRange    `json:"intensity"`
	Position  [3]IntRange `json:"position"`
	Focus     IntRange    `json:"focus"`
}

type Fixture struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Color     RGB           `json:"color"`
	Intensity int           `json:"intensity"`
	Position  [3]int        `json:"position"`
	Focus     int           `json:"focus"`
	Ranges    FixtureRanges `json:"ranges"`
}

type Change struct {
	ID            string        `json:"id"`
	TargetKind    string        `json:"target_kind"`
	TargetID      string        `json:"target_id"`
	AtUS          int64         `json:"at_us"`
	Interpolation Interpolation `json:"interpolation"`
	Values        Values        `json:"values"`
}

type Scene struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	AtUS   int64             `json:"at_us"`
	Values map[string]Values `json:"values"`
}

type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Priority int      `json:"priority"`
	Enabled  bool     `json:"enabled"`
	Mode     string   `json:"mode"`
	Fixtures []string `json:"fixtures"`
	Groups   []string `json:"groups"`
}

type Defaults struct {
	GroupEnabled  bool          `json:"group_enabled"`
	GroupMode     string        `json:"group_mode"`
	Interpolation Interpolation `json:"interpolation"`
}

type UpgradeTrace struct {
	Path          string `json:"path"`
	OldValue      any    `json:"old_value"`
	DefaultValue  any    `json:"default_value"`
	DefaultSource string `json:"default_source"`
}

type Show struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	FPS           int64          `json:"fps"`
	DurationUS    int64          `json:"duration_us"`
	WindowUS      int64          `json:"window_us"`
	TimeRounding  TimeRounding   `json:"time_rounding"`
	ColorStep     string         `json:"color_step"`
	Fixtures      []Fixture      `json:"fixtures"`
	Changes       []Change       `json:"changes"`
	Scenes        []Scene        `json:"scenes"`
	Groups        []Group        `json:"groups"`
	Defaults      Defaults       `json:"defaults"`
	SourceVersion Version        `json:"source_version"`
	UpgradeTraces []UpgradeTrace `json:"upgrade_traces,omitempty"`
}

func (s *Show) Clone() *Show {
	if s == nil {
		return nil
	}
	b, _ := json.Marshal(s)
	var out Show
	if err := json.Unmarshal(b, &out); err != nil {
		panic("lightshow: internal clone failed: " + err.Error())
	}
	return &out
}

func (s *Show) FixtureByID(id string) *Fixture {
	for i := range s.Fixtures {
		if s.Fixtures[i].ID == id {
			return &s.Fixtures[i]
		}
	}
	return nil
}

func (s *Show) GroupByID(id string) *Group {
	for i := range s.Groups {
		if s.Groups[i].ID == id {
			return &s.Groups[i]
		}
	}
	return nil
}

func (s *Show) ChangeByID(id string) *Change {
	for i := range s.Changes {
		if s.Changes[i].ID == id {
			return &s.Changes[i]
		}
	}
	return nil
}

func (s *Show) DurationFrames() int64 {
	if s.FPS <= 0 || s.DurationUS <= 0 {
		return 0
	}
	return s.DurationUS * s.FPS / 1_000_000
}

func (s *Show) WindowCount() int64 {
	if s.WindowUS <= 0 || s.DurationUS <= 0 {
		return 0
	}
	return (s.DurationUS + s.WindowUS - 1) / s.WindowUS
}

// Normalize validates ordering, defaults, ids and references. It deliberately
// does not reorder changes: an input inversion is a semantic error (E_ORDER).
func (s *Show) Normalize() error {
	if s.FPS <= 0 {
		return engineError(CodeVersion, "fps", "fps must be positive")
	}
	if s.DurationUS <= 0 {
		return engineError(CodeVersion, "duration", "duration must be positive")
	}
	if s.WindowUS <= 0 {
		return engineError(CodeVersion, "window", "window must be positive")
	}
	if s.TimeRounding == "" {
		s.TimeRounding = RoundNearest
	}
	if s.TimeRounding != RoundNearest && s.TimeRounding != RoundFloor && s.TimeRounding != RoundCeil {
		return engineError(CodeVersion, "time_rounding", "unsupported rounding %q", s.TimeRounding)
	}
	if s.ColorStep == "" {
		s.ColorStep = "1/255"
	}
	if s.Defaults.GroupMode == "" {
		s.Defaults.GroupMode = "override"
	}
	if s.Defaults.Interpolation == "" {
		s.Defaults.Interpolation = Hold
	}
	if s.Defaults.GroupMode != "override" && s.Defaults.GroupMode != "add" {
		return engineError(CodeVersion, "defaults.group_mode", "unsupported group mode %q", s.Defaults.GroupMode)
	}
	if s.Defaults.Interpolation != Hold && s.Defaults.Interpolation != Linear {
		return engineError(CodeVersion, "defaults.interpolation", "unsupported interpolation %q", s.Defaults.Interpolation)
	}

	fixtures := make(map[string]struct{}, len(s.Fixtures))
	for i := range s.Fixtures {
		f := &s.Fixtures[i]
		if f.ID == "" {
			return engineError(CodeVersion, fmt.Sprintf("fixtures[%d].id", i), "required field is missing")
		}
		if _, ok := fixtures[f.ID]; ok {
			return engineError(CodeVersion, fmt.Sprintf("fixtures[%d].id", i), "duplicate fixture id %q", f.ID)
		}
		fixtures[f.ID] = struct{}{}
		if f.Ranges.Intensity.Max == 0 && f.Ranges.Intensity.Min == 0 {
			f.Ranges.Intensity = IntRange{Min: 0, Max: 1000}
		}
		if f.Ranges.Focus.Max == 0 && f.Ranges.Focus.Min == 0 {
			f.Ranges.Focus = IntRange{Min: 0, Max: 10000}
		}
		for a := 0; a < 3; a++ {
			if f.Ranges.Position[a].Max == 0 && f.Ranges.Position[a].Min == 0 {
				f.Ranges.Position[a] = IntRange{Min: -1000, Max: 1000}
			}
			if f.Ranges.Position[a].Min > f.Ranges.Position[a].Max {
				return engineError(CodeVersion, fmt.Sprintf("fixtures[%d].ranges.position[%d]", i, a), "min exceeds max")
			}
		}
	}

	changes := make(map[string]struct{}, len(s.Changes))
	var previous int64 = -1
	for i := range s.Changes {
		c := &s.Changes[i]
		if c.ID == "" {
			return engineError(CodeVersion, fmt.Sprintf("changes[%d].id", i), "required field is missing")
		}
		if _, ok := changes[c.ID]; ok {
			return engineError(CodeVersion, fmt.Sprintf("changes[%d].id", i), "duplicate change id %q", c.ID)
		}
		changes[c.ID] = struct{}{}
		if c.AtUS < 0 {
			return engineError(CodeVersion, fmt.Sprintf("changes[%d].at", i), "logical time must not be negative")
		}
		if c.AtUS < previous {
			return engineError(CodeOrder, fmt.Sprintf("changes[%d].at", i),
				"change points are not in logical-time order (%d < %d)", c.AtUS, previous)
		}
		previous = c.AtUS
		if c.Interpolation == "" {
			c.Interpolation = s.Defaults.Interpolation
		}
		if c.Interpolation != Hold && c.Interpolation != Linear {
			return engineError(CodeVersion, fmt.Sprintf("changes[%d].interpolation", i), "unsupported interpolation %q", c.Interpolation)
		}
		if c.Values.Empty() {
			return engineError(CodeVersion, fmt.Sprintf("changes[%d].values", i), "at least one output value is required")
		}
		switch c.TargetKind {
		case "fixture":
			if _, ok := fixtures[c.TargetID]; !ok {
				return engineError(CodeVersion, fmt.Sprintf("changes[%d].target_id", i), "unknown fixture %q", c.TargetID)
			}
		case "group":
			if s.GroupByID(c.TargetID) == nil {
				return engineError(CodeVersion, fmt.Sprintf("changes[%d].target_id", i), "unknown group %q", c.TargetID)
			}
		default:
			return engineError(CodeVersion, fmt.Sprintf("changes[%d].target_kind", i), "unsupported target kind %q", c.TargetKind)
		}
	}

	scenes := make(map[string]struct{}, len(s.Scenes))
	previous = -1
	for i := range s.Scenes {
		sc := &s.Scenes[i]
		if sc.ID == "" {
			return engineError(CodeVersion, fmt.Sprintf("scenes[%d].scene_id", i), "required field is missing")
		}
		if _, ok := scenes[sc.ID]; ok {
			return engineError(CodeVersion, fmt.Sprintf("scenes[%d].scene_id", i), "duplicate scene id %q", sc.ID)
		}
		scenes[sc.ID] = struct{}{}
		if sc.AtUS < previous {
			return engineError(CodeOrder, fmt.Sprintf("scenes[%d].at", i), "scenes are not in logical-time order")
		}
		previous = sc.AtUS
		if sc.Values == nil {
			sc.Values = map[string]Values{}
		}
		for id := range sc.Values {
			if _, ok := fixtures[id]; !ok {
				return engineError(CodeVersion, fmt.Sprintf("scenes[%d].values.%s", i, id), "unknown fixture")
			}
		}
	}

	groups := make(map[string]struct{}, len(s.Groups))
	for i := range s.Groups {
		g := &s.Groups[i]
		if g.ID == "" {
			return engineError(CodeVersion, fmt.Sprintf("groups[%d].id", i), "required field is missing")
		}
		if _, ok := groups[g.ID]; ok {
			return engineError(CodeVersion, fmt.Sprintf("groups[%d].id", i), "duplicate group id %q", g.ID)
		}
		groups[g.ID] = struct{}{}
	}
	for i := range s.Groups {
		g := &s.Groups[i]
		if g.Mode == "" {
			g.Mode = s.Defaults.GroupMode
		}
		if g.Mode != "override" && g.Mode != "add" {
			return engineError(CodeVersion, fmt.Sprintf("groups[%d].mode", i), "unsupported group mode %q", g.Mode)
		}
		for j, id := range g.Fixtures {
			if _, ok := fixtures[id]; !ok {
				return engineError(CodeVersion, fmt.Sprintf("groups[%d].fixtures[%d]", i, j), "unknown fixture %q", id)
			}
		}
		for j, id := range g.Groups {
			if _, ok := groups[id]; !ok {
				return engineError(CodeVersion, fmt.Sprintf("groups[%d].groups[%d]", i, j), "unknown group %q", id)
			}
		}
	}
	if err := s.validateGroupCycles(); err != nil {
		return err
	}
	return nil
}

func (s *Show) validateGroupCycles() error {
	const (
		unseen = iota
		visiting
		done
	)
	state := make(map[string]int, len(s.Groups))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return engineError(CodeGroup, "groups", "group control cycle detected")
		case done:
			return nil
		}
		state[id] = visiting
		g := s.GroupByID(id)
		if g != nil {
			for _, child := range g.Groups {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		state[id] = done
		return nil
	}
	for _, g := range s.Groups {
		if err := visit(g.ID); err != nil {
			return err
		}
	}
	return nil
}

// ResolveGroupFixtures returns the deterministic transitive fixture set.
func (s *Show) ResolveGroupFixtures(groupID string) ([]string, error) {
	order := make(map[string]int, len(s.Fixtures))
	for i, f := range s.Fixtures {
		order[f.ID] = i
	}
	seenFixtures := make(map[string]bool)
	seenGroups := make(map[string]bool)
	var walk func(string) error
	walk = func(id string) error {
		if seenGroups[id] {
			return nil
		}
		seenGroups[id] = true
		g := s.GroupByID(id)
		if g == nil {
			return engineError(CodeVersion, "groups", "unknown group %q", id)
		}
		for _, f := range g.Fixtures {
			seenFixtures[f] = true
		}
		for _, child := range g.Groups {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(groupID); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seenFixtures))
	for _, f := range s.Fixtures {
		if seenFixtures[f.ID] {
			out = append(out, f.ID)
		}
	}
	return out, nil
}

func (s *Show) NormalizeMemberships() (map[string][]string, error) {
	out := make(map[string][]string, len(s.Groups))
	for _, g := range s.Groups {
		fixtures, err := s.ResolveGroupFixtures(g.ID)
		if err != nil {
			return nil, err
		}
		out[g.ID] = fixtures
	}
	return out, nil
}

func sortedGroupIDs(groups []Group) []string {
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	sort.Strings(ids)
	return ids
}
