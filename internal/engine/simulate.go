package engine

import (
	"sort"

	"grayrelease/internal/model"
)

func Simulate(project *model.Project, requests []model.Request, config Config) (*SimulationResult, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	runtime, err := NewRuntime(project, config, nil)
	if err != nil {
		return nil, err
	}

	maxTick := 0
	for _, req := range requests {
		if req.Tick > maxTick {
			maxTick = req.Tick
		}
	}
	drain := config.ShadowTimeoutTicks
	if config.ShadowLatencyTicks > drain {
		drain = config.ShadowLatencyTicks
	}
	if drain <= 0 {
		drain = 2
	}
	endTick := maxTick + drain

	result := &SimulationResult{Config: config}
	allPointers := make([]*Decision, 0, len(requests))
	for tick := 0; tick <= endTick; tick++ {
		requestsAtTick := make([]model.Request, 0)
		for _, req := range requests {
			if req.Tick == tick {
				requestsAtTick = append(requestsAtTick, req)
			}
		}
		report, decisions, err := runtime.ProcessTick(tick, requestsAtTick)
		if err != nil {
			return nil, err
		}
		result.Reports = append(result.Reports, report)
		allPointers = append(allPointers, decisions...)
	}

	sort.SliceStable(allPointers, func(i, j int) bool {
		if allPointers[i].Tick != allPointers[j].Tick {
			return allPointers[i].Tick < allPointers[j].Tick
		}
		return allPointers[i].RequestID < allPointers[j].RequestID
	})
	result.Decisions = make([]Decision, 0, len(allPointers))
	for _, pointer := range allPointers {
		result.Decisions = append(result.Decisions, *pointer)
	}
	return result, nil
}

func DecisionMap(result *SimulationResult) map[string]Decision {
	out := map[string]Decision{}
	if result == nil {
		return out
	}
	for _, decision := range result.Decisions {
		out[decision.RequestID] = decision
	}
	return out
}

func SortedKeys(keys []string) []string {
	out := append([]string{}, keys...)
	sort.Strings(out)
	return out
}

var _ = model.SchemaV1
