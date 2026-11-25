package colortrace

import "sort"

// ComputeCacheKey mixes region content, level index, full parameters, algorithm
// version and all upstream cache keys.
func ComputeCacheKey(inputHash string, stage StageConfig, upstreamKeys []string) string {
	keys := append([]string{}, upstreamKeys...)
	sort.Strings(keys)
	return HashCanonical("chunk-cache-v1", map[string]any{
		"input_hash":    inputHash,
		"stage_index":   stage.Index,
		"stage_config":  stage,
		"algo_version":  AlgorithmVersion,
		"upstream_keys": keys,
	})
}

// InputRegionHash hashes deterministic base-aligned amplitudes in a region.
func InputRegionHash(stage int, plane string, r Region, amplitudes []int64, width int) string {
	values := make([]int64, 0, r.Samples())
	for y := r.MinY; y < r.MaxY; y++ {
		for x := r.MinX; x < r.MaxX; x++ {
			values = append(values, amplitudes[y*width+x])
		}
	}
	return HashCanonical("input-region-v1", stage, plane, r, values)
}
