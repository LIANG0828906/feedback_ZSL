package hckernel

import (
	"fmt"
	"sort"
)

const (
	ConstraintGlobalConcurrency  = "C-GLOBAL-CONCURRENCY"
	ConstraintGlobalBudgetZero   = "C-GLOBAL-BUDGET-ZERO"
	ConstraintRackConcurrencyFmt = "C-RACK-%s-CONCURRENCY"
	ConstraintRackBudgetZeroFmt  = "C-RACK-%s-BUDGET-ZERO"
)

func RackConcurrencyConstraint(rackID string) string {
	return fmt.Sprintf(ConstraintRackConcurrencyFmt, rackID)
}

func RackBudgetZeroConstraint(rackID string) string {
	return fmt.Sprintf(ConstraintRackBudgetZeroFmt, rackID)
}

// AllocateWeightedSlots distributes integer slots by Hamilton/largest
// remainder. Multiplying every weight by 10 (permille -> per-ten-thousand) or
// 1000 (permille -> PPM) does not change the result.
func AllocateWeightedSlots(weights map[string]int64, slots int) (map[string]int, error) {
	if slots < 0 {
		return nil, newError(ErrBudgetZero, "slot count cannot be negative")
	}
	out := make(map[string]int, len(weights))
	if slots == 0 {
		for id := range weights {
			out[id] = 0
		}
		return out, nil
	}
	var total int64
	ids := make([]string, 0, len(weights))
	for id, w := range weights {
		if w <= 0 {
			return nil, newError(ErrWeightInvalid, "weight must be positive", withInstance(id))
		}
		ids = append(ids, id)
		total += w
		out[id] = 0
	}
	if total <= 0 {
		return nil, newError(ErrWeightInvalid, "total weight must be positive")
	}

	type remainder struct {
		id    string
		rem   int64
		index int
	}
	remainders := make([]remainder, 0, len(ids))
	allocated := 0
	for i, id := range ids {
		w := weights[id]
		product := int64(slots) * w
		quota := product / total
		out[id] = int(quota)
		allocated += out[id]
		remainders = append(remainders, remainder{id: id, rem: product % total, index: i})
	}
	left := slots - allocated
	sort.SliceStable(remainders, func(i, j int) bool {
		if remainders[i].rem != remainders[j].rem {
			return remainders[i].rem > remainders[j].rem
		}
		return remainders[i].id < remainders[j].id
	})
	for i := 0; i < left && i < len(remainders); i++ {
		out[remainders[i].id]++
	}
	return out, nil
}

// RankByWeight returns the deterministic weight order used for fresh probes.
func RankByWeight(ids []string, weight func(string) int64) []string {
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool {
		wi, wj := weight(out[i]), weight(out[j])
		if wi != wj {
			return wi > wj
		}
		return out[i] < out[j]
	})
	return out
}
