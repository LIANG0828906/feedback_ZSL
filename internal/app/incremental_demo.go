package app

import (
	"fmt"

	"maintenanceorchestrator/internal/generator"
	"maintenanceorchestrator/internal/incremental"
)

func IncrementalDemo(target string) ([]incremental.Record, error) {
	p := generator.Generate(generator.DefaultSeed)
	oldAnalysis, err := Analyze(p)
	if err != nil {
		return nil, err
	}
	eng := incremental.NewEngine()
	deps := map[string][]string{
		"feasibility:10": {"window:global", "node:t-1", "node:t-2", "node:t-3", "node:t-4", "node:t-over"},
		"wave:2":         {"window:global", "node:t-3", "lease:loan-fast-t3"},
		"lease:fast":     {"lease:loan-fast-t3", "wave:2", "node:t-3"},
		"gate:m-fast":    {"lease:loan-fast-t3", "wave:2", "instance:i-fast-2"},
		"unrelated":      {"node:unrelated"},
	}
	oldValues := map[string]any{
		"feasibility:10": oldAnalysis.Feasibility[0],
		"wave:2":         oldAnalysis.Plan,
		"lease:fast":     oldAnalysis.LeaseTimeline,
		"gate:m-fast":    oldAnalysis.Gate,
		"unrelated":      "cached",
	}
	for key, value := range oldValues {
		if _, err := eng.GetOrCompute(key, deps[key], func() (any, error) { return value, nil }); err != nil {
			return nil, err
		}
	}

	var change incremental.Change
	patched := p
	switch target {
	case "", "lease":
		patched, change, err = incremental.PatchLeaseExpiry(p, "loan-fast-t3", 16)
	case "node":
		patched, change, err = incremental.PatchNodeWatermark(p, "t-3", 7000)
	case "window":
		patched, change, err = incremental.PatchWindow(p, "global", 10, 21)
	default:
		return nil, fmt.Errorf("unknown incremental target %q", target)
	}
	if err != nil {
		return nil, err
	}
	newAnalysis, err := Analyze(patched)
	if err != nil {
		return nil, err
	}
	newValues := map[string]any{
		"feasibility:10": newAnalysis.Feasibility[0],
		"wave:2":         newAnalysis.Plan,
		"lease:fast":     newAnalysis.LeaseTimeline,
		"gate:m-fast":    newAnalysis.Gate,
		"unrelated":      "cached",
	}

	eng.ResetRecords()
	eng.Invalidate(change)
	keys := []string{"feasibility:10", "wave:2", "lease:fast", "gate:m-fast", "unrelated"}
	for _, key := range keys {
		key := key
		if _, err := eng.GetOrCompute(key, deps[key], func() (any, error) { return newValues[key], nil }); err != nil {
			return nil, err
		}
	}
	return eng.Records(), nil
}
