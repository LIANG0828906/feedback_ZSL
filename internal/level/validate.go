package level

import (
	"encoding/json"
	"fmt"
	"os"
)

var requiredProjectFields = []string{
	"project_id", "global_seed", "subseed_version", "map_width", "map_height",
	"grid_rows", "grid_cols", "global_resource_cap", "edge_open_pct",
	"spawn", "targets", "partitions",
	"preview_density", "precise_density", "max_budget_iters",
	"height_tolerance", "difficulty_tolerance", "resource_tolerance",
}

var requiredOpFields = []string{"seq", "timestamp", "kind", "partition", "value"}

var requiredPartitionFields = []string{
	"id", "row", "col", "budget_min", "budget_max",
	"obstacle_quota", "min_passable_pct", "resource_cap", "subseed_salt",
}

func checkMissing(data []byte, fields []string, prefix string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return schemaErr(prefix, "invalid JSON: "+err.Error())
	}
	for _, f := range fields {
		if _, ok := raw[f]; !ok {
			return schemaErr(prefix+"."+f, "required field missing")
		}
	}
	return nil
}

func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseProject(data)
}

func ParseProject(data []byte) (*Project, error) {
	if err := checkMissing(data, requiredProjectFields, "project"); err != nil {
		return nil, err
	}
	var raw struct {
		Partitions []json.RawMessage `json:"partitions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, schemaErr("project", "invalid JSON: "+err.Error())
	}
	for i, p := range raw.Partitions {
		if err := checkMissing(p, requiredPartitionFields, fmt.Sprintf("project.partitions[%d]", i)); err != nil {
			return nil, err
		}
	}
	var proj Project
	if err := json.Unmarshal(data, &proj); err != nil {
		return nil, schemaErr("project", "invalid JSON: "+err.Error())
	}
	if err := ValidateProject(&proj); err != nil {
		return nil, err
	}
	return &proj, nil
}

func LoadOps(path string) ([]EditOp, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseOps(data)
}

func ParseOps(data []byte) ([]EditOp, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, schemaErr("ops", "invalid JSON: "+err.Error())
	}
	for i, item := range raw {
		if err := checkMissing(item, requiredOpFields, fmt.Sprintf("ops[%d]", i)); err != nil {
			return nil, err
		}
	}
	var ops []EditOp
	if err := json.Unmarshal(data, &ops); err != nil {
		return nil, schemaErr("ops", "invalid JSON: "+err.Error())
	}
	return ops, nil
}

func ValidateProject(p *Project) error {
	if p.GridRows <= 0 {
		return paraErr("grid_rows", -1, "grid rows must be positive")
	}
	if p.GridCols <= 0 {
		return paraErr("grid_cols", -1, "grid cols must be positive")
	}
	if p.MapWidth <= 0 {
		return paraErr("map_width", -1, "map width must be positive")
	}
	if p.MapHeight <= 0 {
		return paraErr("map_height", -1, "map height must be positive")
	}
	if p.GlobalResourceCap < 0 {
		return paraErr("global_resource_cap", -1, "global resource cap must be non-negative")
	}
	if p.EdgeOpenPct < 0 || p.EdgeOpenPct > 100 {
		return paraErr("edge_open_pct", -1, "edge open pct must be in [0,100]")
	}
	if p.PreviewDensity <= 0 || p.PreciseDensity <= 0 {
		return paraErr("preview_density", -1, "densities must be positive")
	}
	if p.MaxBudgetIters <= 0 {
		return paraErr("max_budget_iters", -1, "max budget iters must be positive")
	}
	if p.Spawn.Row < 0 || p.Spawn.Row >= p.GridRows || p.Spawn.Col < 0 || p.Spawn.Col >= p.GridCols {
		return paraErr("spawn", -1, "spawn partition coordinate out of grid")
	}
	seenIDs := map[int]bool{}
	seenCells := map[[2]int]bool{}
	for i := range p.Partitions {
		pc := &p.Partitions[i]
		if err := validatePartition(p, pc); err != nil {
			return err
		}
		if seenIDs[pc.ID] {
			return paraErr("id", pc.ID, "duplicate partition id")
		}
		seenIDs[pc.ID] = true
		cell := [2]int{pc.Row, pc.Col}
		if seenCells[cell] {
			return paraErr("row", pc.ID, "duplicate partition grid cell")
		}
		seenCells[cell] = true
	}
	for _, t := range p.Targets {
		if p.PartitionByID(t.Partition) == nil {
			return paraErr("targets", t.Partition, "target references missing partition")
		}
	}
	return nil
}

func validatePartition(p *Project, pc *PartitionConstraint) error {
	if pc.Row < 0 || pc.Row >= p.GridRows || pc.Col < 0 || pc.Col >= p.GridCols {
		return paraErr("row", pc.ID, "partition grid coordinate out of range")
	}
	if pc.BudgetMin < 0 {
		return paraErr("budget_min", pc.ID, "budget min must be non-negative fixed-point")
	}
	if pc.BudgetMax < 0 || pc.BudgetMax < pc.BudgetMin {
		return paraErr("budget_max", pc.ID, "budget max must be non-negative and >= budget min")
	}
	if pc.ObstacleQuota < 0 {
		return paraErr("obstacle_quota", pc.ID, "obstacle quota must be non-negative")
	}
	if pc.MinPassablePct < 0 || pc.MinPassablePct > 100 {
		return paraErr("min_passable_pct", pc.ID, "min passable pct must be in [0,100]")
	}
	if pc.ResourceCap < 0 {
		return paraErr("resource_cap", pc.ID, "resource cap must be non-negative")
	}
	if pc.ResourceCap > p.GlobalResourceCap {
		return paraErr("resource_cap", pc.ID, "resource cap exceeds global resource cap")
	}
	return nil
}

// ValidateOps checks the loaded op sequence: contiguous seq numbers,
// strictly increasing logical timestamps, known kinds and existing
// target partitions.
func ValidateOps(p *Project, ops []EditOp) error {
	var lastTS int64
	for i, op := range ops {
		if op.Seq != i+1 {
			return histErr(op.Seq, "op sequence numbers must be contiguous starting at 1")
		}
		if i > 0 && op.Timestamp <= lastTS {
			return histErr(op.Seq, "op timestamp regression")
		}
		lastTS = op.Timestamp
		switch op.Kind {
		case OpSetBudget, OpAddObstacleQuota, OpRemoveObstacleQuota, OpSetResourceCap, OpSetSubSeedSalt:
		default:
			return schemaErr("kind", "unknown op kind")
		}
		if p.PartitionByID(op.Partition) == nil {
			return paraErr("partition", op.Partition, "op targets missing partition")
		}
	}
	return nil
}

// ApplyOpToConstraints mutates the constraint snapshot in place and
// validates the resulting parameter ranges.
func ApplyOpToConstraints(p *Project, op EditOp) error {
	pc := p.PartitionByID(op.Partition)
	if pc == nil {
		return paraErr("partition", op.Partition, "op targets missing partition")
	}
	switch op.Kind {
	case OpSetBudget:
		if op.Value < 0 {
			return paraErr("value", op.Partition, "budget min must be non-negative")
		}
		if op.Value2 < op.Value {
			return paraErr("value2", op.Partition, "budget max must be >= budget min")
		}
		pc.BudgetMin = op.Value
		pc.BudgetMax = op.Value2
	case OpAddObstacleQuota:
		pc.ObstacleQuota += int(op.Value)
		if pc.ObstacleQuota < 0 {
			return paraErr("value", op.Partition, "obstacle quota would become negative")
		}
	case OpRemoveObstacleQuota:
		pc.ObstacleQuota -= int(op.Value)
		if pc.ObstacleQuota < 0 {
			return paraErr("value", op.Partition, "obstacle quota would become negative")
		}
	case OpSetResourceCap:
		if op.Value < 0 || op.Value > int64(p.GlobalResourceCap) {
			return paraErr("value", op.Partition, "resource cap out of range [0, global cap]")
		}
		pc.ResourceCap = int(op.Value)
	case OpSetSubSeedSalt:
		pc.SubSeedSalt = uint64(op.Value)
	default:
		return schemaErr("kind", "unknown op kind")
	}
	return nil
}
