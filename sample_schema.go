package hckernel

import "encoding/json"

func MarshalV1(p *Project) ([]byte, error) {
	targets := make([]map[string]any, 0, len(p.Instances))
	for _, in := range p.Instances {
		targets = append(targets, map[string]any{
			"id": in.ID, "node": in.NodeID, "rack": in.RackID, "protocol": in.Protocol, "address": in.Address,
			"interval_ticks": in.IntervalMs / 50, "timeout_ticks": in.TimeoutMs / 50, "backoff_ticks": in.BackoffMs / 50,
			"retries": in.RetryLimit, "success_threshold": in.SuccessThreshold, "failure_threshold": in.FailureThreshold,
			"initial_state": string(in.InitialState), "weight_permille": in.WeightPPM / 1000,
			"active_probe": in.ActiveProbe,
		})
	}
	racks := make([]map[string]any, 0, len(p.Racks))
	for _, r := range p.Racks {
		racks = append(racks, map[string]any{"rack_id": r.ID, "slots": r.Concurrency})
	}
	doc := map[string]any{
		"schema": SchemaV1, "project_name": p.ProjectID, "project_version": p.ProjectVersion, "seed": p.Seed,
		"rounds": p.Rounds, "tick_ms": 50, "round_ticks": p.StepMs / 50, "max_defer_ticks": p.MaxDeferMs / 50,
		"budget":          map[string]any{"global_slots": p.GlobalConcurrency, "rack_slots": racks},
		"worker_budget":   p.Workers,
		"buffer_capacity": p.BufferCapacity,
		"slot_priority":   string(p.SlotPriority),
		"count_policy": map[string]any{
			"success_threshold_action": string(p.CountPolicy.SuccessReachedAction),
			"failure_threshold_action": string(p.CountPolicy.FailureReachedAction),
			"half_open_success_limit":  p.CountPolicy.HalfOpenSuccessLimit,
			"half_open_failure_limit":  p.CountPolicy.HalfOpenFailureLimit,
			"half_open_delay_ticks":    p.CountPolicy.HalfOpenDelayMs / 50,
		},
		"targets": targets,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func MarshalV2(p *Project) ([]byte, error) {
	instances := make([]map[string]any, 0, len(p.Instances))
	for _, in := range p.Instances {
		instances = append(instances, map[string]any{
			"instanceId": in.ID, "nodeName": in.NodeID, "rackId": in.RackID, "probeType": in.Protocol, "address": in.Address,
			"intervalMs": in.IntervalMs, "timeoutMs": in.TimeoutMs, "backoffMs": in.BackoffMs, "retryLimit": in.RetryLimit,
			"remainingSuccesses": in.SuccessThreshold, "remainingFailures": in.FailureThreshold,
			"stateAtStart": v2State(in.InitialState), "weightPerTenThousand": in.WeightPPM / 100,
			"activeProbe": in.ActiveProbe,
		})
	}
	rackPermits := map[string]int{}
	for _, r := range p.Racks {
		rackPermits[r.ID] = r.Concurrency
	}
	doc := map[string]any{
		"schemaVersion": SchemaV2,
		"metadata":      map[string]any{"projectId": p.ProjectID, "projectVersion": p.ProjectVersion, "seed": p.Seed},
		"rounds":        p.Rounds,
		"timing":        map[string]any{"stepMs": p.StepMs, "maxDeferMs": p.MaxDeferMs},
		"concurrency":   map[string]any{"globalPermits": p.GlobalConcurrency, "rackPermits": rackPermits},
		"workerCount":   p.Workers, "probeBuffer": p.BufferCapacity, "slotPriority": string(p.SlotPriority),
		"countPolicy": map[string]any{
			"onSuccessThreshold":   string(p.CountPolicy.SuccessReachedAction),
			"onFailureThreshold":   string(p.CountPolicy.FailureReachedAction),
			"halfOpenSuccessLimit": p.CountPolicy.HalfOpenSuccessLimit,
			"halfOpenFailureLimit": p.CountPolicy.HalfOpenFailureLimit,
			"halfOpenDelayMs":      p.CountPolicy.HalfOpenDelayMs,
		},
		"instances": instances,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func v2State(s HealthState) string {
	switch s {
	case StateHealthy:
		return "HEALTHY"
	case StateUnhealthy:
		return "UNHEALTHY"
	case StateHalfOpen:
		return "HALF_OPEN"
	default:
		return "INITIAL"
	}
}

func MarshalV3(p *Project) ([]byte, error) {
	topology := make([]map[string]any, 0, len(p.Racks))
	for _, rack := range p.Racks {
		nodes := map[string][]map[string]any{}
		nodeOrder := []string{}
		for _, in := range p.Instances {
			if in.RackID != rack.ID {
				continue
			}
			if _, ok := nodes[in.NodeID]; !ok {
				nodeOrder = append(nodeOrder, in.NodeID)
			}
			nodes[in.NodeID] = append(nodes[in.NodeID], map[string]any{
				"backend_id": in.ID, "protocol": in.Protocol, "address": in.Address,
				"probe_interval_seconds": float64(in.IntervalMs) / 1000,
				"probe_timeout_ms":       in.TimeoutMs,
				"backoff_seconds":        float64(in.BackoffMs) / 1000,
				"max_retries":            in.RetryLimit,
				"success_limit":          in.SuccessThreshold,
				"failure_limit":          in.FailureThreshold,
				"boot_state":             string(in.InitialState),
				"weight_ppm":             in.WeightPPM,
			})
		}
		nodeDocs := make([]map[string]any, 0, len(nodeOrder))
		for _, nodeID := range nodeOrder {
			nodeDocs = append(nodeDocs, map[string]any{"node": nodeID, "backends": nodes[nodeID]})
		}
		topology = append(topology, map[string]any{"rack": rack.ID, "az": rack.AvailabilityZone, "nodes": nodeDocs})
	}
	policies := make([]map[string]any, 0, len(p.Racks))
	for _, r := range p.Racks {
		policies = append(policies, map[string]any{"name": r.ID, "permits": r.Concurrency})
	}
	doc := map[string]any{
		"schema_version": SchemaV3,
		"project":        map[string]any{"id": p.ProjectID, "version": p.ProjectVersion, "seed": p.Seed, "rounds": p.Rounds},
		"logic_clock":    map[string]any{"round_seconds": float64(p.StepMs) / 1000, "max_defer_seconds": float64(p.MaxDeferMs) / 1000},
		"budget_policy":  map[string]any{"total_permits": p.GlobalConcurrency, "rack_policies": policies},
		"worker_policy": map[string]any{
			"threads": p.Workers, "probe_buffer": p.BufferCapacity, "slot_priority": string(p.SlotPriority),
		},
		"counting_rules": map[string]any{
			"on_success_limit":       string(p.CountPolicy.SuccessReachedAction),
			"on_failure_limit":       string(p.CountPolicy.FailureReachedAction),
			"half_open_ok":           p.CountPolicy.HalfOpenSuccessLimit,
			"half_open_bad":          p.CountPolicy.HalfOpenFailureLimit,
			"half_open_wait_seconds": float64(p.CountPolicy.HalfOpenDelayMs) / 1000,
		},
		"topology": topology,
	}
	return json.MarshalIndent(doc, "", "  ")
}
