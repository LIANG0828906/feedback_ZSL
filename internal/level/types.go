package level

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// FixedScale is the fixed-point scale for difficulty values (milli-units).
const FixedScale int64 = 1000

// RefDensity is the fixed reference grid (per partition side) on which
// obstacles, resources and passability live. It is independent of the
// render/preview density so that preview and precise paths agree on
// difficulty, resource totals and connectivity verdicts.
const RefDensity = 16

// ResourceDifficulty is the fixed-point difficulty contributed per resource.
const ResourceDifficulty int64 = 50

type Point struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

type Target struct {
	ID        string `json:"id"`
	Partition int    `json:"partition"`
}

type PartitionConstraint struct {
	ID             int    `json:"id"`
	Row            int    `json:"row"`
	Col            int    `json:"col"`
	BudgetMin      int64  `json:"budget_min"`
	BudgetMax      int64  `json:"budget_max"`
	ObstacleQuota  int    `json:"obstacle_quota"`
	MinPassablePct int    `json:"min_passable_pct"`
	ResourceCap    int    `json:"resource_cap"`
	SubSeedSalt    uint64 `json:"subseed_salt"`
}

type Project struct {
	ProjectID           string                `json:"project_id"`
	GlobalSeed          uint64                `json:"global_seed"`
	SubSeedVersion      uint32                `json:"subseed_version"`
	MapWidth            int                   `json:"map_width"`
	MapHeight           int                   `json:"map_height"`
	GridRows            int                   `json:"grid_rows"`
	GridCols            int                   `json:"grid_cols"`
	GlobalResourceCap   int                   `json:"global_resource_cap"`
	EdgeOpenPct         int                   `json:"edge_open_pct"`
	Spawn               Point                 `json:"spawn"`
	Targets             []Target              `json:"targets"`
	Partitions          []PartitionConstraint `json:"partitions"`
	PreviewDensity      int                   `json:"preview_density"`
	PreciseDensity      int                   `json:"precise_density"`
	MaxBudgetIters      int                   `json:"max_budget_iters"`
	HeightTolerance     int32                 `json:"height_tolerance"`
	DifficultyTolerance int64                 `json:"difficulty_tolerance"`
	ResourceTolerance   int                   `json:"resource_tolerance"`
}

type OpKind string

const (
	OpSetBudget           OpKind = "set_budget"
	OpAddObstacleQuota    OpKind = "add_obstacle_quota"
	OpRemoveObstacleQuota OpKind = "remove_obstacle_quota"
	OpSetResourceCap      OpKind = "set_resource_cap"
	OpSetSubSeedSalt      OpKind = "set_subseed_salt"
)

type EditOp struct {
	Seq       int    `json:"seq"`
	Timestamp int64  `json:"timestamp"`
	Kind      OpKind `json:"kind"`
	Partition int    `json:"partition"`
	Value     int64  `json:"value"`
	Value2    int64  `json:"value2,omitempty"`
}

type Obstacle struct {
	ID   string `json:"id"`
	Cell int    `json:"cell"`
	Cost int64  `json:"cost"`
}

type Resource struct {
	ID       string `json:"id"`
	Cell     int    `json:"cell"`
	Category int    `json:"category"`
}

type TradeoffRecord struct {
	Partition       int    `json:"partition"`
	CandidateID     string `json:"candidate_id"`
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	DifficultyAfter int64  `json:"difficulty_after"`
}

type RepairRecord struct {
	Edge             [2]int   `json:"edge"`
	RemovedObstacles []string `json:"removed_obstacles,omitempty"`
	AddedObstacles   []string `json:"added_obstacles,omitempty"`
}

type PartitionResult struct {
	PartitionID int              `json:"partition_id"`
	Density     int              `json:"density"`
	Heights     []int32          `json:"heights"`
	Obstacles   []Obstacle       `json:"obstacles"`
	Resources   []Resource       `json:"resources"`
	Difficulty  int64            `json:"difficulty"`
	PassablePct int              `json:"passable_pct"`
	Edges       [4]bool          `json:"edges"`
	Tradeoffs   []TradeoffRecord `json:"tradeoffs,omitempty"`
	Failed      bool             `json:"failed,omitempty"`
	Err         string           `json:"err,omitempty"`
	Reserve     []Obstacle       `json:"-"`
}

type ConnectivityResult struct {
	Connected          bool           `json:"connected"`
	Reachable          []int          `json:"reachable"`
	UnreachableTargets []string       `json:"unreachable_targets,omitempty"`
	Repairs            []RepairRecord `json:"repairs,omitempty"`
}

type ManifestEntry struct {
	Partition int    `json:"partition"`
	Reason    string `json:"reason"`
	Reused    bool   `json:"reused"`
	Hash      string `json:"hash"`
}

type Manifest struct {
	OpSeq               int             `json:"op_seq"`
	Entries             []ManifestEntry `json:"entries"`
	GlobalResourceTotal int             `json:"global_resource_total"`
	Connected           bool            `json:"connected"`
}

// Commit is one node of the session history: a constraint snapshot plus
// the full partition state after applying the op with this sequence
// number (seq 0 is the initial state before any op).
type Commit struct {
	OpSeq          int                      `json:"op_seq"`
	Fingerprint    string                   `json:"fingerprint"`
	SubSeedVersion uint32                   `json:"subseed_version"`
	Constraints    *Project                 `json:"constraints"`
	Base           map[int]*PartitionResult `json:"base"`
	Partitions     map[int]*PartitionResult `json:"partitions"`
	Connectivity   *ConnectivityResult      `json:"connectivity"`
	Manifest       *Manifest                `json:"manifest"`
	Timestamp      int64                    `json:"timestamp"`
}

type PreviewReport struct {
	OpSeq               int           `json:"op_seq"`
	Density             int           `json:"density"`
	Partitions          []int         `json:"partitions"`
	Basis               string        `json:"basis"`
	ElapsedMs           float64       `json:"elapsed_ms"`
	Connected           bool          `json:"connected"`
	BudgetByPartition   map[int]int64 `json:"budget_by_partition"`
	GlobalResourceTotal int           `json:"global_resource_total"`
	constraints         *Project
}

type DivergeReport struct {
	ConnectivityDiff bool   `json:"connectivity_diff"`
	BudgetDiff       int64  `json:"budget_diff"`
	ResourceDiff     int    `json:"resource_diff"`
	FirstPartition   int    `json:"first_partition"`
	Item             string `json:"item"`
}

func (p *Project) PartitionByID(id int) *PartitionConstraint {
	for i := range p.Partitions {
		if p.Partitions[i].ID == id {
			return &p.Partitions[i]
		}
	}
	return nil
}

func (p *Project) Clone() *Project {
	data, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var out Project
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return &out
}

// Fingerprint is the content hash of the canonical constraint snapshot.
func Fingerprint(p *Project) string {
	data, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Canonical returns the deterministic byte form used for byte-identical
// cross-path comparison.
func (r *PartitionResult) Canonical() []byte {
	data, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return data
}

func (r *PartitionResult) Hash() string {
	sum := sha256.Sum256(r.Canonical())
	return hex.EncodeToString(sum[:8])
}

func (r *PartitionResult) Clone() *PartitionResult {
	out := *r
	out.Heights = append([]int32(nil), r.Heights...)
	out.Obstacles = append([]Obstacle(nil), r.Obstacles...)
	out.Resources = append([]Resource(nil), r.Resources...)
	out.Tradeoffs = append([]TradeoffRecord(nil), r.Tradeoffs...)
	out.Reserve = append([]Obstacle(nil), r.Reserve...)
	return &out
}

func (c *ConnectivityResult) Clone() *ConnectivityResult {
	out := *c
	out.Reachable = append([]int(nil), c.Reachable...)
	out.UnreachableTargets = append([]string(nil), c.UnreachableTargets...)
	out.Repairs = append([]RepairRecord(nil), c.Repairs...)
	return &out
}

func (c *ConnectivityResult) Canonical() []byte {
	data, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return data
}

func CloneParts(src map[int]*PartitionResult) map[int]*PartitionResult {
	out := make(map[int]*PartitionResult, len(src))
	for id, r := range src {
		out[id] = r.Clone()
	}
	return out
}

func SortedIDs(parts map[int]*PartitionResult) []int {
	ids := make([]int, 0, len(parts))
	for id := range parts {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func GlobalResourceTotal(parts map[int]*PartitionResult) int {
	total := 0
	for _, r := range parts {
		total += len(r.Resources)
	}
	return total
}
