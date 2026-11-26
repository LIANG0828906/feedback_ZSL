package slikernel

import (
	"sort"
)

// ComputeTruth is a deliberately direct, cache-free implementation used to
// validate the production engine. It scans source observations for each
// evaluation window and applies the documented fixed-point formulas.
func ComputeTruth(project Project) (Report, error) {
	if err := ValidateProject(CloneProject(project)); err != nil {
		return Report{}, err
	}
	report := Report{
		ProjectID: project.ProjectID, RuleVersion: ProjectRuleVersion(project),
		CommittedObservationSeq: project.LastObservationSeq(),
	}
	for _, service := range project.Services {
		for _, sli := range service.SLIs {
			indices, err := evaluationIndices(project, sli)
			if err != nil {
				return Report{}, err
			}
			states := make([]ThresholdState, len(sli.Thresholds))
			for index, threshold := range sli.Thresholds {
				states[index] = ThresholdState{
					ID: threshold.ID, BurnPPM: threshold.BurnPPM,
					ConsecutiveRequired: threshold.ConsecutiveWindows,
				}
			}
			for _, evalIndex := range indices {
				first := evalIndex - int64(sli.RollingWindows) + 1
				start := project.TimeOriginMs + first*sli.WindowMs
				end := project.TimeOriginMs + (evalIndex+1)*sli.WindowMs
				result := truthWindow(project, service.Name, sli, evalIndex, start, end)
				for index := range states {
					state := &states[index]
					state.Matched = result.Budget.ConsumedPPM >= state.BurnPPM
					if state.Matched {
						state.ConsecutiveObserved++
					} else {
						state.ConsecutiveObserved = 0
					}
					state.Fired = state.ConsecutiveObserved >= state.ConsecutiveRequired
					if state.Matched {
						result.Alert.MatchedThresholds = append(result.Alert.MatchedThresholds, state.ID)
					}
					if state.Fired {
						result.Alert.FiredThresholds = append(result.Alert.FiredThresholds, state.ID)
						result.Alert.Alert = true
						result.Alert.PrimaryThreshold = state.ID
					}
					result.Alert.Thresholds = append(result.Alert.Thresholds, *state)
				}
				result.Alert.GatePass = !result.Alert.Alert
				report.Results = append(report.Results, result)
			}
		}
	}
	return report, nil
}

func truthWindow(project Project, serviceName string, sli SLI, evalIndex, start, end int64) WindowResult {
	result := WindowResult{
		Service: serviceName, SLI: sli.ID, Route: sli.Route, Kind: sli.Kind, EvalIndex: evalIndex,
		StartMs: start, EndMs: end,
		AggregationRevision: SLIAggregationRevision(serviceName, sli),
		NoData:              true,
	}
	type groupWork struct {
		aggregation GroupAgg
		latencies   []int64
	}
	groups := make(map[int]*groupWork)
	var latencies []int64
	var observedThroughput int64
	for _, observation := range project.Observations {
		if observation.Service != serviceName || !routeMatches(sli.Route, observation.Route) ||
			!statusMatches(sli.StatusCodes, observation.StatusCode) {
			continue
		}
		if observation.TimestampMs < start || observation.TimestampMs >= end {
			continue
		}
		result.NoData = false
		group := groups[observation.StatusCode]
		if group == nil {
			group = &groupWork{aggregation: GroupAgg{StatusCode: observation.StatusCode}}
			groups[observation.StatusCode] = group
		}
		switch sli.Kind {
		case KindAvailability:
			result.Total += observation.Events
			result.Failures += observation.Errors
			group.aggregation.Events += observation.Events
			group.aggregation.Errors += observation.Errors
		case KindLatency:
			for _, sample := range observation.LatencySamplesUs {
				latencies = append(latencies, sample)
				group.latencies = append(group.latencies, sample)
				group.aggregation.LatencySamples++
				if sample > sli.LatencyThresholdUs {
					result.Failures++
					group.aggregation.LatencyFailures++
				}
			}
			result.Total = int64(len(latencies))
		case KindThroughput:
			observedThroughput += observation.ThroughputEvents
			group.aggregation.ThroughputEvents += observation.ThroughputEvents
		}
	}
	if sli.Kind == KindThroughput {
		expected := sli.ExpectedEventsPerWindow * int64(sli.RollingWindows)
		result.Failures = expected - observedThroughput
		if result.Failures < 0 {
			result.Failures = 0
		}
		result.Total = expected
	}
	if sli.Kind == KindLatency {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		result.QuantileUs = truthQuantile(latencies, sli.QuantilePPM)
	}
	if result.Total <= 0 {
		result.SLIValuePPM = PPM
	} else if result.Failures >= result.Total {
		result.SLIValuePPM = 0
	} else {
		result.SLIValuePPM = ((result.Total-result.Failures)*PPM + result.Total/2) / result.Total
	}
	if sli.Kind == KindThroughput && result.SLIValuePPM > PPM {
		result.SLIValuePPM = PPM
	}
	result.Budget = truthBudget(sli, result.Total, result.Failures)
	for _, group := range groups {
		if len(group.latencies) > 0 {
			sort.Slice(group.latencies, func(i, j int) bool { return group.latencies[i] < group.latencies[j] })
			group.aggregation.QuantileUs = truthQuantile(group.latencies, sli.QuantilePPM)
		}
		result.Groups = append(result.Groups, group.aggregation)
	}
	sort.Slice(result.Groups, func(i, j int) bool {
		return result.Groups[i].StatusCode < result.Groups[j].StatusCode
	})
	return result
}

func truthQuantile(sorted []int64, quantilePPM int64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := (int64(len(sorted))*quantilePPM + PPM - 1) / PPM
	if rank < 1 {
		rank = 1
	}
	if rank > int64(len(sorted)) {
		rank = int64(len(sorted))
	}
	return sorted[rank-1]
}

func truthBudget(sli SLI, total, failures int64) BudgetResult {
	allowed := total * (PPM - sli.TargetPPM) / PPM
	result := BudgetResult{
		TargetPPM: sli.TargetPPM, Total: total, Failures: failures, AllowedErrors: allowed,
	}
	if allowed > 0 {
		result.ConsumedPPM = (failures*PPM + allowed/2) / allowed
	} else if failures > 0 {
		result.ConsumedPPM = PPM * 2
	}
	if allowed-failures > 0 {
		result.RemainingErrors = allowed - failures
	} else {
		result.DeficitErrors = failures - allowed
	}
	if allowed == 0 {
		result.Exhausted = failures > 0
	} else {
		result.Exhausted = failures >= allowed
	}
	return result
}
