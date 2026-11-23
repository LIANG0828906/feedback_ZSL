package level

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SampleSeed is the fixed seed of the local deterministic sample
// generator. Regenerating the sample always produces identical files.
const SampleSeed uint64 = 0x5EED0042

// BasicProject builds the initial constraint project for the standard
// sample: a 4x4 partition grid over a 256x256 map.
func BasicProject() *Project {
	rng := NewRNG(SampleSeed)
	proj := &Project{
		ProjectID:           "sample-basic",
		GlobalSeed:          SampleSeed,
		SubSeedVersion:      1,
		MapWidth:            256,
		MapHeight:           256,
		GridRows:            4,
		GridCols:            4,
		GlobalResourceCap:   8,
		EdgeOpenPct:         70,
		Spawn:               Point{Row: 0, Col: 0},
		Targets:             []Target{{ID: "t1", Partition: 5}, {ID: "t2", Partition: 10}, {ID: "t3", Partition: 15}},
		PreviewDensity:      8,
		PreciseDensity:      32,
		MaxBudgetIters:      64,
		HeightTolerance:     2,
		DifficultyTolerance: 0,
		ResourceTolerance:   0,
	}
	for id := 0; id < 16; id++ {
		proj.Partitions = append(proj.Partitions, PartitionConstraint{
			ID:             id,
			Row:            id / 4,
			Col:            id % 4,
			BudgetMin:      500,
			BudgetMax:      int64(2200 + rng.Intn(20)*200),
			ObstacleQuota:  4 + rng.Intn(5),
			MinPassablePct: 60,
			ResourceCap:    2 + rng.Intn(5),
			SubSeedSalt:    rng.Next(),
		})
	}
	return proj
}

// BasicOps is the deterministic edit op sequence with logical timestamps.
func BasicOps() []EditOp {
	return []EditOp{
		{Seq: 1, Timestamp: 100, Kind: OpSetBudget, Partition: 3, Value: 800, Value2: 3000},
		{Seq: 2, Timestamp: 200, Kind: OpAddObstacleQuota, Partition: 5, Value: 2},
		{Seq: 3, Timestamp: 300, Kind: OpSetResourceCap, Partition: 5, Value: 4},
		{Seq: 4, Timestamp: 400, Kind: OpSetSubSeedSalt, Partition: 7, Value: 0xABCD},
		{Seq: 5, Timestamp: 500, Kind: OpRemoveObstacleQuota, Partition: 3, Value: 1},
		{Seq: 6, Timestamp: 600, Kind: OpSetBudget, Partition: 10, Value: 0, Value2: 5000},
		{Seq: 7, Timestamp: 700, Kind: OpSetResourceCap, Partition: 15, Value: 6},
		{Seq: 8, Timestamp: 800, Kind: OpAddObstacleQuota, Partition: 12, Value: 3},
		{Seq: 9, Timestamp: 900, Kind: OpSetSubSeedSalt, Partition: 0, Value: 0x1234},
		{Seq: 10, Timestamp: 1000, Kind: OpSetBudget, Partition: 8, Value: 1000, Value2: 4500},
		{Seq: 11, Timestamp: 1100, Kind: OpRemoveObstacleQuota, Partition: 12, Value: 1},
		{Seq: 12, Timestamp: 1200, Kind: OpSetResourceCap, Partition: 2, Value: 3},
	}
}

// BudgetScenarioProject forces the budget tradeoff path: partition 0 is
// heavily over budget and partition 1 can never reach its budget minimum
// within the iteration limit (E_BUDGET).
func BudgetScenarioProject() *Project {
	proj := BasicProject()
	proj.ProjectID = "scenario-budget"
	proj.Partitions[0].ObstacleQuota = 8
	proj.Partitions[0].BudgetMin = 0
	proj.Partitions[0].BudgetMax = 300
	proj.Partitions[1].ObstacleQuota = 1
	proj.Partitions[1].ResourceCap = 0
	proj.Partitions[1].BudgetMin = 20000
	proj.Partitions[1].BudgetMax = 30000
	return proj
}

// ConnScenarioProject closes every boundary edge so the connectivity
// repair path must carve boundary bands to reconnect the targets.
func ConnScenarioProject() *Project {
	proj := BasicProject()
	proj.ProjectID = "scenario-conn"
	proj.EdgeOpenPct = 0
	return proj
}

// ConnFailProject makes partition 5 impassable (obstacle coverage far
// above its passable floor), so no boundary repair can reconnect t1 and
// the run must fail with E_CONN.
func ConnFailProject() *Project {
	proj := ConnScenarioProject()
	proj.ProjectID = "scenario-conn-fail"
	pc := &proj.Partitions[5]
	pc.ObstacleQuota = 200
	pc.MinPassablePct = 95
	pc.BudgetMin = 0
	pc.BudgetMax = 200000
	return proj
}

// HistRegressionOps contains a logical timestamp regression (E_HIST).
func HistRegressionOps() []EditOp {
	return []EditOp{
		{Seq: 1, Timestamp: 100, Kind: OpSetResourceCap, Partition: 1, Value: 2},
		{Seq: 2, Timestamp: 90, Kind: OpSetResourceCap, Partition: 2, Value: 2},
	}
}

// PartitionTruth is the ground-truth manifest entry for one partition.
type PartitionTruth struct {
	Partition     int     `json:"partition"`
	ObstacleCount int     `json:"obstacle_count"`
	ResourceCount int     `json:"resource_count"`
	Difficulty    int64   `json:"difficulty"`
	HeightSamples []int32 `json:"height_samples"`
}

type Truth struct {
	ProjectID    string           `json:"project_id"`
	Density      int              `json:"density"`
	SamplePoints [][2]int         `json:"sample_points"`
	Partitions   []PartitionTruth `json:"partitions"`
}

func truthSamplePoints(density int) [][2]int {
	last := density - 1
	mid := density / 2
	return [][2]int{{0, 0}, {0, last}, {last, 0}, {last, last}, {mid, mid}}
}

// BuildTruth generates the analytic ground-truth manifest for a project
// using the precise density and pure per-partition generation (no
// connectivity repair).
func BuildTruth(proj *Project) (*Truth, error) {
	t := &Truth{
		ProjectID:    proj.ProjectID,
		Density:      proj.PreciseDensity,
		SamplePoints: truthSamplePoints(proj.PreciseDensity),
	}
	for i := range proj.Partitions {
		pc := &proj.Partitions[i]
		res, err := GeneratePartition(proj, pc, proj.PreciseDensity)
		if err != nil {
			return nil, err
		}
		entry := PartitionTruth{
			Partition:     pc.ID,
			ObstacleCount: len(res.Obstacles),
			ResourceCount: len(res.Resources),
			Difficulty:    res.Difficulty,
		}
		for _, sp := range t.SamplePoints {
			entry.HeightSamples = append(entry.HeightSamples, res.Heights[sp[0]*proj.PreciseDensity+sp[1]])
		}
		t.Partitions = append(t.Partitions, entry)
	}
	return t, nil
}

// CheckTruth regenerates every partition and verifies the truth manifest:
// obstacle and resource counts must match exactly and height samples must
// lie within the declared tolerance.
func CheckTruth(proj *Project, truth *Truth) error {
	for _, entry := range truth.Partitions {
		pc := proj.PartitionByID(entry.Partition)
		if pc == nil {
			return paraErr("truth", entry.Partition, "truth references missing partition")
		}
		res, err := GeneratePartition(proj, pc, proj.PreciseDensity)
		if err != nil {
			return err
		}
		if len(res.Obstacles) != entry.ObstacleCount {
			return fmt.Errorf("truth mismatch partition %d: obstacle count %d != %d", entry.Partition, len(res.Obstacles), entry.ObstacleCount)
		}
		if len(res.Resources) != entry.ResourceCount {
			return fmt.Errorf("truth mismatch partition %d: resource count %d != %d", entry.Partition, len(res.Resources), entry.ResourceCount)
		}
		points := truthSamplePoints(proj.PreciseDensity)
		for i, sp := range points {
			got := res.Heights[sp[0]*proj.PreciseDensity+sp[1]]
			want := entry.HeightSamples[i]
			diff := got - want
			if diff < 0 {
				diff = -diff
			}
			if diff > proj.HeightTolerance {
				return fmt.Errorf("truth mismatch partition %d: height sample %d diff %d exceeds tolerance %d", entry.Partition, i, diff, proj.HeightTolerance)
			}
		}
	}
	return nil
}

type SampleMeta struct {
	Command    string `json:"command"`
	Scenario   string `json:"scenario"`
	Seed       uint64 `json:"seed"`
	GridRows   int    `json:"grid_rows"`
	GridCols   int    `json:"grid_cols"`
	Partitions int    `json:"partitions"`
	OpCount    int    `json:"op_count"`
}

// WriteSample deterministically generates a sample scenario on disk:
// project.json, ops.json, truth.json and meta.json.
func WriteSample(dir, scenario string) error {
	var proj *Project
	var ops []EditOp
	switch scenario {
	case "basic":
		proj, ops = BasicProject(), BasicOps()
	case "budget":
		proj, ops = BudgetScenarioProject(), nil
	case "conn":
		proj, ops = ConnScenarioProject(), nil
	case "conn-fail":
		proj, ops = ConnFailProject(), nil
	case "hist":
		proj, ops = BasicProject(), HistRegressionOps()
	default:
		return fmt.Errorf("unknown scenario %q", scenario)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	write := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}
	if err := write("project.json", proj); err != nil {
		return err
	}
	if ops == nil {
		ops = []EditOp{}
	}
	if err := write("ops.json", ops); err != nil {
		return err
	}
	if scenario == "basic" || scenario == "conn" {
		truth, err := BuildTruth(proj)
		if err != nil {
			return err
		}
		if err := write("truth.json", truth); err != nil {
			return err
		}
	}
	meta := SampleMeta{
		Command:    fmt.Sprintf("go run ./cmd/levelsession sample -scenario %s -out DIR", scenario),
		Scenario:   scenario,
		Seed:       SampleSeed,
		GridRows:   proj.GridRows,
		GridCols:   proj.GridCols,
		Partitions: len(proj.Partitions),
		OpCount:    len(ops),
	}
	return write("meta.json", meta)
}
