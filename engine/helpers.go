package engine

import "economycore/fixed"

func addInt64(a, b int64) (int64, error) {
	return fixed.Add(a, b)
}
