package workbench

import (
	"encoding/json"
	"sort"
)

func jsonUnmarshalStrict(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

func allocateProportional(keys []string, weights map[string]float64, caps map[string]int, total int) map[string]int {
	out := make(map[string]int, len(keys))
	if total <= 0 {
		return out
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	remaining := total
	sumWeights := 0.0
	for _, key := range sorted {
		capValue := caps[key]
		if capValue <= 0 {
			continue
		}
		w := weights[key]
		if w <= 0 {
			w = 1
		}
		sumWeights += w
	}
	if sumWeights <= 0 {
		return out
	}
	type fraction struct {
		key   string
		value float64
	}
	fractions := make([]fraction, 0, len(sorted))
	for _, key := range sorted {
		capValue := caps[key]
		if capValue <= 0 {
			continue
		}
		w := weights[key]
		if w <= 0 {
			w = 1
		}
		exact := float64(total) * w / sumWeights
		n := int(exact)
		if n > capValue {
			n = capValue
		}
		if n < 0 {
			n = 0
		}
		out[key] = n
		remaining -= n
		fractions = append(fractions, fraction{key: key, value: exact - float64(n)})
	}
	sort.Slice(fractions, func(i, j int) bool {
		if fractions[i].value != fractions[j].value {
			return fractions[i].value > fractions[j].value
		}
		return fractions[i].key < fractions[j].key
	})
	for remaining > 0 {
		progress := false
		for _, f := range fractions {
			if remaining == 0 {
				break
			}
			if out[f.key] >= caps[f.key] {
				continue
			}
			out[f.key]++
			remaining--
			progress = true
		}
		if !progress {
			break
		}
	}
	return out
}

func sumIntMap(values map[string]int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func normalizeMix(mix map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(mix))
	total := 0.0
	for _, key := range sortedMapKeys(mix) {
		value := mix[key]
		if value > 0 {
			total += value
		}
	}
	if total <= 0 {
		return out
	}
	for _, key := range sortedMapKeys(mix) {
		value := mix[key]
		if value > 0 {
			out[key] = value / total
		} else {
			out[key] = 0
		}
	}
	return out
}

func quantizeMix(mix map[string]float64, step float64) (map[string]float64, float64) {
	keys := sortedMapKeys(mix)
	normalized := normalizeMix(mix)
	out := make(map[string]float64, len(keys))
	if len(keys) == 0 || step <= 0 {
		return out, 0
	}
	targetUnits := int(mathRound(1 / step))
	if targetUnits <= 0 {
		targetUnits = 1
	}
	type remainder struct {
		key   string
		value float64
	}
	rems := make([]remainder, 0, len(keys))
	totalUnits := 0
	for _, key := range keys {
		raw := normalized[key] / step
		units := int(raw)
		if units < 0 {
			units = 0
		}
		out[key] = float64(units) * step
		totalUnits += units
		rems = append(rems, remainder{key: key, value: raw - float64(units)})
	}
	sort.Slice(rems, func(i, j int) bool {
		if rems[i].value != rems[j].value {
			return rems[i].value > rems[j].value
		}
		return rems[i].key < rems[j].key
	})
	for totalUnits < targetUnits {
		progress := false
		for _, rem := range rems {
			if totalUnits >= targetUnits {
				break
			}
			out[rem.key] += step
			totalUnits++
			progress = true
		}
		if !progress {
			break
		}
	}
	for totalUnits > targetUnits {
		progress := false
		for i := len(rems) - 1; i >= 0; i-- {
			key := rems[i].key
			if out[key] < step-1e-12 {
				continue
			}
			out[key] -= step
			totalUnits--
			progress = true
			if totalUnits == targetUnits {
				break
			}
		}
		if !progress {
			break
		}
	}
	residual := 0.0
	for _, key := range keys {
		residual += mathAbs(normalized[key] - out[key])
	}
	return out, residual
}

func mathRound(value float64) float64 {
	if value >= 0 {
		return float64(int(value + 0.5))
	}
	return float64(int(value - 0.5))
}

func mathAbs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func quantizeValue(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return mathRound(value/step) * step
}
