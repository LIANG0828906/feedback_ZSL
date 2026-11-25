package schema

import (
	"sort"
	"strconv"

	"maintenanceorchestrator/internal/domain"
)

func itoaSchema(i int) string { return strconv.Itoa(i) }

func sortWaves(waves []domain.Wave) {
	sort.SliceStable(waves, func(i, j int) bool { return waves[i].Seq < waves[j].Seq })
	for i := range waves {
		sort.Strings(waves[i].BatchIDs)
	}
}

func wavesFromNested(groups []struct {
	Seq      int
	BatchIDs []string
}) []domain.Wave {
	out := make([]domain.Wave, 0, len(groups))
	for _, g := range groups {
		out = append(out, domain.Wave{Seq: g.Seq, BatchIDs: append([]string(nil), g.BatchIDs...)})
	}
	sortWaves(out)
	return out
}
