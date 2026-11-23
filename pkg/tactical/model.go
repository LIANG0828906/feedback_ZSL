package tactical

import (
	"strconv"
	"time"
)

const (
	// AlgorithmVersion is part of every checkpoint validity key.
	AlgorithmVersion = "formation-search-v1"
	// FixedParamsVersion is part of every checkpoint validity key.
	FixedParamsVersion = "q1000-millirad-v1"

	MaxCellCost int64 = 1_000_000_000
	MaxPathCost int64 = 1 << 60
)

// GridPoint is an integral grid-cell coordinate.
type GridPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func (p GridPoint) Key() string {
	return gridKey(p.X, p.Y)
}

func (p GridPoint) InBounds(width, height int) bool {
	return p.X >= 0 && p.Y >= 0 && p.X < width && p.Y < height
}

func gridKey(x, y int) string {
	return strconv.Itoa(x) + "," + strconv.Itoa(y)
}

// Scenario is the complete serializable tactical input document.
type Scenario struct {
	SchemaVersion int           `json:"schema_version"`
	Seed          int64         `json:"seed"`
	Map           MapDef        `json:"map"`
	Formation     FormationDef  `json:"formation"`
	Instructions  []Instruction `json:"instructions"`
	GroundTruth   GroundTruth   `json:"ground_truth"`
}

type MapDef struct {
	ID             string          `json:"id"`
	Width          int             `json:"width"`
	Height         int             `json:"height"`
	Cells          []Cell          `json:"cells"`
	BlockedRegions []BlockedRegion `json:"blocked_regions"`
}

type Cell struct {
	Cost      int64  `json:"cost"`
	Terrain   string `json:"terrain"`
	Passable  bool   `json:"passable"`
	BlockedBy string `json:"blocked_by,omitempty"`
}

type BlockedRegion struct {
	ID     string `json:"id"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Active bool   `json:"active"`
}

type FormationDef struct {
	ID                string   `json:"id"`
	SlotVersion       string   `json:"slot_version"`
	Slots             []Slot   `json:"slots"`
	Members           []Member `json:"members"`
	MaxSpacingMilli   int64    `json:"max_spacing_milli"`
	DefaultEpsilon    int64    `json:"default_epsilon_milli"`
	PairToleranceMult int64    `json:"pair_tolerance_multiplier"`
}

type Slot struct {
	ID          string  `json:"id"`
	Anchor      string  `json:"anchor"`
	DX          float64 `json:"dx"`
	DY          float64 `json:"dy"`
	Tolerance   float64 `json:"tolerance"`
	FacingMilli int64   `json:"facing_milli"`
}

type Member struct {
	ID                   string    `json:"id"`
	Callsign             string    `json:"callsign"`
	Group                string    `json:"group"`
	SlotID               string    `json:"slot_id"`
	Start                GridPoint `json:"start"`
	End                  GridPoint `json:"end"`
	StepMilli            int64     `json:"step_milli"`
	TurnRateMilli        int64     `json:"turn_rate_milli"`
	HeuristicWeightMilli int64     `json:"heuristic_weight_milli"`
	Neighbors            int       `json:"neighbors"`
	DependsOn            []string  `json:"depends_on"`
}

type Instruction struct {
	Tick     int    `json:"tick"`
	MemberID string `json:"member_id"`
	Kind     string `json:"kind"`
	Value    int    `json:"value"`
}

type GroundTruth struct {
	MinCosts            []CostTruth      `json:"min_costs"`
	PathLengths         []PathTruth      `json:"path_lengths"`
	Corners             []CornerTruth    `json:"corners"`
	FormationDeviations []DeviationTruth `json:"formation_deviations"`
}

type CostTruth struct {
	MemberID string    `json:"member_id"`
	From     GridPoint `json:"from"`
	To       GridPoint `json:"to"`
	Cost     int64     `json:"cost"`
}

type PathTruth struct {
	MemberID    string `json:"member_id"`
	Cost        int64  `json:"cost"`
	Steps       int    `json:"steps"`
	Corners     int    `json:"corners"`
	LengthMilli int64  `json:"length_milli"`
}

type CornerTruth struct {
	MemberID string     `json:"member_id"`
	Grid     GridPoint  `json:"grid"`
	World    WorldPoint `json:"world"`
}

type DeviationTruth struct {
	Tick           int    `json:"tick"`
	MemberID       string `json:"member_id"`
	DeviationMilli int64  `json:"deviation_milli"`
}

type PathDependencies struct {
	Cells          []GridPoint `json:"cells"`
	Terrains       []string    `json:"terrains"`
	BlockedRegions []string    `json:"blocked_regions"`
}

type MemberPath struct {
	MemberID             string           `json:"member_id"`
	Cells                []GridPoint      `json:"cells"`
	Cost                 int64            `json:"cost"`
	Expanded             int              `json:"expanded"`
	HeuristicWeightMilli int64            `json:"heuristic_weight_milli"`
	Neighbors            int              `json:"neighbors"`
	Dependencies         PathDependencies `json:"dependencies"`
}

type Corner struct {
	Grid  GridPoint  `json:"grid"`
	Fixed FixPt      `json:"fixed"`
	World WorldPoint `json:"world"`
}

type SmoothedPath struct {
	MemberID       string   `json:"member_id"`
	SourceCost     int64    `json:"source_cost"`
	ToleranceMilli int64    `json:"tolerance_milli"`
	Corners        []Corner `json:"corners"`
}

type TickState struct {
	Tick                    int        `json:"tick"`
	MemberID                string     `json:"member_id"`
	Position                FixPt      `json:"position"`
	World                   WorldPoint `json:"world"`
	HeadingMilli            int64      `json:"heading_milli"`
	Segment                 int        `json:"segment"`
	FormationDeviationMilli int64      `json:"formation_deviation_milli"`
	AtCorner                bool       `json:"at_corner"`
}

type Trace struct {
	MemberID string         `json:"member_id"`
	States   []TickState    `json:"states"`
	Stats    FormationStats `json:"stats"`
}

type FormationStats struct {
	MaxDeviationMilli int64 `json:"max_deviation_milli"`
	SumDeviationMilli int64 `json:"sum_deviation_milli"`
	Samples           int64 `json:"samples"`
}

type TickInterval struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type InvalidationKind string

const (
	InvalidatePath       InvalidationKind = "path"
	InvalidateSimulation InvalidationKind = "simulation"
)

type Invalidation struct {
	MemberID string           `json:"member_id"`
	Interval TickInterval     `json:"interval"`
	Kind     InvalidationKind `json:"kind"`
	Reason   string           `json:"reason"`
}

type MemberDependency struct {
	MemberID       string      `json:"member_id"`
	PathCells      []GridPoint `json:"path_cells"`
	Terrains       []string    `json:"terrains"`
	BlockedRegions []string    `json:"blocked_regions"`
	CoupledMembers []string    `json:"coupled_members"`
}

type CouplingInterval struct {
	MemberID        string       `json:"member_id"`
	CoupledMemberID string       `json:"coupled_member_id"`
	Interval        TickInterval `json:"interval"`
	Reason          string       `json:"reason"`
}

type DependencyGraph struct {
	Members       []MemberDependency  `json:"members"`
	CellToMembers map[string][]string `json:"cell_to_members"`
	Couplings     []CouplingInterval  `json:"couplings"`
}

type CellChange struct {
	Point        GridPoint `json:"point"`
	OldCost      int64     `json:"old_cost"`
	NewCost      int64     `json:"new_cost"`
	OldPassable  bool      `json:"old_passable"`
	NewPassable  bool      `json:"new_passable"`
	OldBlockedBy string    `json:"old_blocked_by,omitempty"`
	NewBlockedBy string    `json:"new_blocked_by,omitempty"`
	Reason       string    `json:"reason"`
}

type InvalidationReport struct {
	ChangedCells    []CellChange   `json:"changed_cells"`
	Entries         []Invalidation `json:"entries"`
	Topology        []string       `json:"topology"`
	CacheHits       int            `json:"cache_hits"`
	CacheMisses     int            `json:"cache_misses"`
	ReusedIntervals int            `json:"reused_intervals"`
}

type ConsistencyResult struct {
	Equal       bool   `json:"equal"`
	Item        string `json:"item,omitempty"`
	MemberID    string `json:"member_id,omitempty"`
	Tick        int    `json:"tick,omitempty"`
	Incremental string `json:"incremental,omitempty"`
	Full        string `json:"full,omitempty"`
}

type EditResult struct {
	Snapshot     *Snapshot          `json:"snapshot"`
	Invalidation InvalidationReport `json:"invalidation"`
	Consistency  ConsistencyResult  `json:"consistency"`
	Stats        RunStats           `json:"stats"`
}

type MemberTiming struct {
	MemberID     string `json:"member_id"`
	PathMicros   int64  `json:"path_micros"`
	SmoothMicros int64  `json:"smooth_micros"`
	TraceMicros  int64  `json:"trace_micros"`
}

type RunStats struct {
	Mode            string         `json:"mode"`
	CacheHits       int            `json:"cache_hits"`
	CacheMisses     int            `json:"cache_misses"`
	ReusedIntervals int            `json:"reused_intervals"`
	Invalidated     int            `json:"invalidated"`
	MaxParallel     int            `json:"max_parallel"`
	Timings         []MemberTiming `json:"timings"`
	TotalMicros     int64          `json:"total_micros"`
}

type Snapshot struct {
	MapHash          string                  `json:"map_hash"`
	SlotVersion      string                  `json:"slot_version"`
	FixedVersion     string                  `json:"fixed_version"`
	AlgorithmVersion string                  `json:"algorithm_version"`
	CommitSeq        int64                   `json:"commit_seq"`
	MemberOrder      []string                `json:"member_order"`
	Paths            map[string]MemberPath   `json:"paths"`
	Smooth           map[string]SmoothedPath `json:"smooth"`
	Traces           map[string]Trace        `json:"traces"`
	Dependencies     DependencyGraph         `json:"dependencies"`
	Invalidation     []Invalidation          `json:"invalidation"`
	Stats            RunStats                `json:"stats"`
	SavedAt          time.Time               `json:"saved_at"`
}

type CheckpointMeta struct {
	MapHash          string    `json:"map_hash"`
	SlotVersion      string    `json:"slot_version"`
	FixedVersion     string    `json:"fixed_version"`
	AlgorithmVersion string    `json:"algorithm_version"`
	CommitSeq        int64     `json:"commit_seq"`
	SavedAt          time.Time `json:"saved_at"`
	Valid            bool      `json:"valid"`
	StaleReason      string    `json:"stale_reason,omitempty"`
}
