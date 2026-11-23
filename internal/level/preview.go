package level

import (
	"encoding/json"
	"os"
	"sort"
	"time"
)

const previewBasis = "coarse density, affected+neighbor partitions only, same sub-seed derivation and budget logic as precise path"

// PreviewOp produces the interactive coarse preview for an edit op: it
// lowers the subdivision density and only processes the edited partition
// plus its adjacency influence region, reusing cached results elsewhere.
// It must return within the interactive budget (100 ms); the measured
// latency, density, partition set and basis are recorded in the report.
func (s *Session) PreviewOp(op EditOp) (*PreviewReport, error) {
	start := time.Now()
	if s.cur.PartitionByID(op.Partition) == nil {
		return nil, paraErr("partition", op.Partition, "op targets missing partition")
	}
	newCur := s.cur.Clone()
	if err := ApplyOpToConstraints(newCur, op); err != nil {
		return nil, err
	}
	affected, _ := affectedPartitions(s.cur, op, true)
	fresh, err := s.genSet(affected, newCur, s.Proj.PreviewDensity)
	if err != nil {
		return nil, err
	}
	mixedBase := CloneParts(s.base)
	for id, r := range fresh {
		mixedBase[id] = r
	}
	mixed := CloneParts(mixedBase)
	conn, err := EnsureConnectivity(newCur, mixed)
	if err != nil {
		return nil, err
	}
	budgets := make(map[int]int64, len(mixed))
	for id, r := range mixed {
		budgets[id] = r.Difficulty
	}
	return &PreviewReport{
		OpSeq:               op.Seq,
		Density:             s.Proj.PreviewDensity,
		Partitions:          affected,
		Basis:               previewBasis,
		ElapsedMs:           float64(time.Since(start).Nanoseconds()) / 1e6,
		Connected:           conn.Connected,
		BudgetByPartition:   budgets,
		GlobalResourceTotal: GlobalResourceTotal(mixed),
		constraints:         newCur,
	}, nil
}

// FinalizePreview runs the precise regeneration of all partitions at the
// previewed constraint state and compares the last preview against it on
// the three declared dimensions: connectivity verdict, difficulty budget
// occupancy and global resource total. Any divergence beyond the declared
// tolerance returns E_DIVERGE with the measured deviations, the first
// diverging partition and the item name; the report is exported for
// review when reportPath is non-empty.
func (s *Session) FinalizePreview(preview *PreviewReport, reportPath string) (*DivergeReport, error) {
	constraints := preview.constraints
	ids := make([]int, 0, len(constraints.Partitions))
	for _, pc := range constraints.Partitions {
		ids = append(ids, pc.ID)
	}
	sort.Ints(ids)
	base, err := s.genSet(ids, constraints, s.Proj.PreciseDensity)
	if err != nil {
		return nil, err
	}
	parts := CloneParts(base)
	conn, err := EnsureConnectivity(constraints, parts)
	if err != nil {
		return nil, err
	}

	report := &DivergeReport{FirstPartition: -1}
	if conn.Connected != preview.Connected {
		report.ConnectivityDiff = true
		report.Item = "connectivity"
	}
	var maxBudgetDiff int64
	firstBudgetPart := -1
	for _, id := range SortedIDs(parts) {
		diff := parts[id].Difficulty - preview.BudgetByPartition[id]
		if diff < 0 {
			diff = -diff
		}
		if diff > maxBudgetDiff {
			maxBudgetDiff = diff
		}
		if diff > s.Proj.DifficultyTolerance && firstBudgetPart < 0 {
			firstBudgetPart = id
		}
	}
	report.BudgetDiff = maxBudgetDiff
	total := GlobalResourceTotal(parts)
	report.ResourceDiff = total - preview.GlobalResourceTotal
	if report.ResourceDiff < 0 {
		report.ResourceDiff = -report.ResourceDiff
	}

	diverged := false
	if report.ConnectivityDiff {
		diverged = true
	} else if maxBudgetDiff > s.Proj.DifficultyTolerance {
		diverged = true
		report.Item = "difficulty_budget"
		report.FirstPartition = firstBudgetPart
	} else if report.ResourceDiff > s.Proj.ResourceTolerance {
		diverged = true
		report.Item = "global_resource_total"
	}
	if report.Item == "" {
		report.Item = "none"
	}
	if reportPath != "" {
		data, merr := json.MarshalIndent(report, "", "  ")
		if merr == nil {
			if werr := os.WriteFile(reportPath, data, 0o644); werr != nil {
				return report, werr
			}
		}
	}
	if diverged {
		return report, divergeErr(report)
	}
	return report, nil
}
