package engine

import (
	"fmt"

	"example.com/columnar/domain"
)

// CompareResults compares version-independent outcomes. Operation kind and
// checksum-check details are intentionally excluded because v1 expresses a
// logical patch as a whole-batch replacement.
func CompareResults(a, b *domain.RunResult) error {
	if a == nil || b == nil {
		return fmt.Errorf("nil run result")
	}
	if err := compareBatchLists(a.InitialBatches, b.InitialBatches); err != nil {
		return fmt.Errorf("initial batches: %w", err)
	}
	if len(a.Operations) != len(b.Operations) {
		return fmt.Errorf("operation result count %d != %d", len(a.Operations), len(b.Operations))
	}
	for i := range a.Operations {
		if err := compareBatchLists(a.Operations[i].Batches, b.Operations[i].Batches); err != nil {
			return fmt.Errorf("op %d batches: %w", i+1, err)
		}
		if fmt.Sprint(a.Operations[i].Changed) != fmt.Sprint(b.Operations[i].Changed) {
			return fmt.Errorf("op %d changed set differs", i+1)
		}
		if err := compareGraphs(a.Operations[i].Graph, b.Operations[i].Graph); err != nil {
			return fmt.Errorf("op %d graph: %w", i+1, err)
		}
		if !a.Operations[i].Check.Match || !b.Operations[i].Check.Match {
			return fmt.Errorf("op %d checksum check failed", i+1)
		}
	}
	if len(a.Scans) != len(b.Scans) {
		return fmt.Errorf("scan result count %d != %d", len(a.Scans), len(b.Scans))
	}
	for i := range a.Scans {
		x, y := a.Scans[i], b.Scans[i]
		if x.ScanID != y.ScanID || x.AfterOp != y.AfterOp ||
			fmt.Sprint(x.ReadBatches) != fmt.Sprint(y.ReadBatches) ||
			x.DecodedRows != y.DecodedRows ||
			fmt.Sprint(x.RebuiltBatches) != fmt.Sprint(y.RebuiltBatches) ||
			fmt.Sprint(x.ReusedBatches) != fmt.Sprint(y.ReusedBatches) ||
			fmt.Sprint(x.InvalidSources) != fmt.Sprint(y.InvalidSources) ||
			x.ColdCache != y.ColdCache || x.CacheHit != y.CacheHit {
			return fmt.Errorf("scan %s/%d differs", x.ScanID, x.AfterOp)
		}
	}
	if err := compareBatchLists(a.FinalBatches, b.FinalBatches); err != nil {
		return fmt.Errorf("final batches: %w", err)
	}
	if err := compareGraphs(a.Graph, b.Graph); err != nil {
		return fmt.Errorf("final graph: %w", err)
	}
	return nil
}

func VerifyVersionEquivalence(results []*domain.RunResult) error {
	for i := 1; i < len(results); i++ {
		if err := CompareResults(results[0], results[i]); err != nil {
			return &domain.Error{
				Code: domain.CodeCrossVersion, Context: fmt.Sprintf("versions.%s.%s", results[0].Version, results[i].Version),
				Message: err.Error(),
			}
		}
	}
	return nil
}

func compareBatchLists(a, b []domain.Batch) error {
	if len(a) != len(b) {
		return fmt.Errorf("batch count %d != %d", len(a), len(b))
	}
	for i := range a {
		x, y := a[i], b[i]
		if fmt.Sprint(x) != fmt.Sprint(y) {
			return fmt.Errorf("batch %d: %+v != %+v", a[i].ID, a[i], b[i])
		}
	}
	return nil
}

func compareGraphs(a, b []domain.DependencyNode) error {
	if len(a) != len(b) {
		return fmt.Errorf("node count %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i].BatchID != b[i].BatchID || a[i].Family != b[i].Family ||
			a[i].Status != b[i].Status || a[i].SourceID != b[i].SourceID ||
			fmt.Sprint(a[i].Dependencies) != fmt.Sprint(b[i].Dependencies) ||
			fmt.Sprint(a[i].Dependents) != fmt.Sprint(b[i].Dependents) {
			return fmt.Errorf("node %d differs", a[i].BatchID)
		}
	}
	return nil
}
