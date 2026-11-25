package orchestrator

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// VerifyTruth compares the selected per-tick witness from the deterministic
// generator against a run. Generator truth is intentionally sparse at event
// boundaries, so every listed field is checked item by item in canonical order.
func VerifyTruth(result *RunResult, truth Truth) error {
	if result == nil {
		return fmt.Errorf("nil run result")
	}
	byTick := make(map[int]TickResult, len(result.Ticks))
	for _, tick := range result.Ticks {
		byTick[int(tick.Tick)] = tick
	}
	ticks := make([]int, 0, len(truth.Ticks))
	for tick := range truth.Ticks {
		ticks = append(ticks, tick)
	}
	sort.Ints(ticks)
	for _, tickNumber := range ticks {
		expected := truth.Ticks[tickNumber]
		actual, ok := byTick[tickNumber]
		if !ok {
			return fmt.Errorf("missing tick %d", tickNumber)
		}
		if !equalStrings(actual.FaultyNodes, expected.Faulty) {
			return fmt.Errorf("tick %d faulty = %v, want %v", tickNumber, actual.FaultyNodes, expected.Faulty)
		}
		if !equalStrings(actual.FrozenWorkloads, expected.Frozen) {
			return fmt.Errorf("tick %d frozen = %v, want %v", tickNumber, actual.FrozenWorkloads, expected.Frozen)
		}
		waiting := make([]string, 0, len(actual.WaitingQueue))
		for _, wait := range actual.WaitingQueue {
			waiting = append(waiting, wait.WorkloadID)
		}
		if !equalStrings(waiting, expected.Waiting) {
			return fmt.Errorf("tick %d waiting = %v, want %v", tickNumber, waiting, expected.Waiting)
		}
		unfrozen := make([]string, 0, len(actual.Unfrozen))
		for _, item := range actual.Unfrozen {
			unfrozen = append(unfrozen, item.WorkloadID)
		}
		sort.Strings(unfrozen)
		if !equalStrings(unfrozen, expected.Unfrozen) {
			return fmt.Errorf("tick %d unfrozen = %v, want %v", tickNumber, unfrozen, expected.Unfrozen)
		}
		recovered := make([]string, 0)
		blocked := make(map[string][]string)
		for _, decision := range actual.Recoveries {
			switch decision.Status {
			case "recovered":
				recovered = append(recovered, decision.NodeID)
			case "blocked":
				blocked[decision.NodeID] = decision.Chain
			}
		}
		sort.Strings(recovered)
		if !equalStrings(recovered, expected.Recovered) {
			return fmt.Errorf("tick %d recovered = %v, want %v", tickNumber, recovered, expected.Recovered)
		}
		for node, chain := range expected.Blocked {
			if !reflect.DeepEqual(blocked[node], chain) {
				return fmt.Errorf("tick %d blocked chain for %s = %v, want %v", tickNumber, node, blocked[node], chain)
			}
		}
		for domainID, expectedNodes := range expected.Domains {
			found := false
			for _, domain := range actual.Domains {
				if domain.ID != domainID {
					continue
				}
				found = true
				if !reflect.DeepEqual(domain.Nodes, expectedNodes) {
					return fmt.Errorf("tick %d domain %d nodes = %v, want %v", tickNumber, domainID, domain.Nodes, expectedNodes)
				}
			}
			if !found {
				return fmt.Errorf("tick %d missing domain %d", tickNumber, domainID)
			}
		}
		for _, domainID := range expected.ForcedDomains {
			found := false
			for _, domain := range actual.Domains {
				if domain.ID == domainID && domain.Forced {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("tick %d domain %d was not forced", tickNumber, domainID)
			}
		}
	}
	return nil
}

// SemanticRunsEqual compares all deterministic semantic outputs while
// ignoring two observability-only fields: backpressure signals (which reflect
// the configured worker budget) and invalidation provenance (which exists only
// for changed cache intervals).
func SemanticRunsEqual(a, b *RunResult) bool {
	left, err := canonicalRun(a)
	if err != nil {
		return false
	}
	right, err := canonicalRun(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func canonicalRun(result *RunResult) (*RunResult, error) {
	if result == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var clone RunResult
	if err := json.Unmarshal(encoded, &clone); err != nil {
		return nil, err
	}
	for index := range clone.Ticks {
		clone.Ticks[index].Backpressure = nil
		clone.Ticks[index].InvalidatedBy = nil
	}
	return &clone, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
