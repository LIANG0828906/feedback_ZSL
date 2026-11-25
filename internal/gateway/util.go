package gateway

import "strconv"

func itoa(i int) string { return strconv.Itoa(i) }

func int64Set(values []int64) map[int64]bool {
	out := make(map[int64]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func sortedInt64Set(values map[int64]bool) []int64 {
	out := make([]int64, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
