package samplegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"serviceregistry/registry"
)

const (
	DefaultSeed = int64(42)
	TickMS      = int64(100)
)

type Bundle struct {
	Project *registry.Project
	Truth   registry.TruthReport
	Files   map[registry.SchemaVersion][]byte
}

func BuildProject(seed int64) *registry.Project {
	project := &registry.Project{
		SchemaVersion:  registry.SchemaV1,
		ProjectVersion: fmt.Sprintf("regproj-seed-%d", seed),
		Seed:           seed,
		TickMS:         TickMS,
		Budgets: registry.Budgets{
			GlobalConcurrency: 4,
			NodeConcurrency:   2,
			RackConcurrency:   2,
			InstanceBuffer:    2,
			MaxDelayMS:        300,
		},
		Services: []registry.Service{
			{
				ID:                     "svc-api",
				Name:                   "public-api",
				DefaultWeightScale:     registry.ScalePerMille,
				HalfOpenPolicy:         registry.HalfOpenDemote,
				HalfOpenFactorPermille: 500,
				AdmitInitial:           true,
				Order:                  0,
			},
			{
				ID:                     "svc-batch",
				Name:                   "batch-worker",
				DefaultWeightScale:     registry.ScalePer10K,
				HalfOpenPolicy:         registry.HalfOpenExclude,
				HalfOpenFactorPermille: 500,
				AdmitInitial:           false,
				Order:                  1,
			},
		},
		Instances: []registry.Instance{
			{
				ID:               "api-a",
				ServiceID:        "svc-api",
				NodeID:           "node-1",
				RackID:           "rack-1",
				Address:          "10.20.0.11:8080",
				Weight:           400,
				WeightScale:      registry.ScalePerMille,
				InitialHealth:    registry.HealthHealthy,
				LeaseTTLMS:       1500,
				HeartbeatMS:      500,
				ProbePeriodMS:    300,
				ProbeTimeoutMS:   200,
				SuccessThreshold: 2,
				FailureThreshold: 2,
				Order:            0,
			},
			{
				ID:               "api-b",
				ServiceID:        "svc-api",
				NodeID:           "node-1",
				RackID:           "rack-2",
				Address:          "10.20.0.12:8080",
				Weight:           300,
				WeightScale:      registry.ScalePerMille,
				InitialHealth:    registry.HealthInitial,
				LeaseTTLMS:       1200,
				HeartbeatMS:      500,
				ProbePeriodMS:    400,
				ProbeTimeoutMS:   300,
				SuccessThreshold: 2,
				FailureThreshold: 2,
				Order:            1,
			},
			{
				ID:               "api-c",
				ServiceID:        "svc-api",
				NodeID:           "node-2",
				RackID:           "rack-1",
				Address:          "10.20.0.13:8080",
				Weight:           800,
				WeightScale:      registry.ScalePerMille,
				InitialHealth:    registry.HealthHealthy,
				LeaseTTLMS:       800,
				HeartbeatMS:      400,
				ProbePeriodMS:    300,
				ProbeTimeoutMS:   200,
				SuccessThreshold: 1,
				FailureThreshold: 1,
				Order:            2,
			},
			{
				ID:               "batch-a",
				ServiceID:        "svc-batch",
				NodeID:           "node-2",
				RackID:           "rack-2",
				Address:          "10.20.0.21:9090",
				Weight:           6000,
				WeightScale:      registry.ScalePer10K,
				InitialHealth:    registry.HealthHealthy,
				LeaseTTLMS:       2000,
				HeartbeatMS:      500,
				ProbePeriodMS:    500,
				ProbeTimeoutMS:   300,
				SuccessThreshold: 2,
				FailureThreshold: 2,
				Order:            3,
			},
			{
				ID:               "batch-b",
				ServiceID:        "svc-batch",
				NodeID:           "node-3",
				RackID:           "rack-3",
				Address:          "10.20.0.22:9090",
				Weight:           4000,
				WeightScale:      registry.ScalePer10K,
				InitialHealth:    registry.HealthHealthy,
				LeaseTTLMS:       2500,
				HeartbeatMS:      500,
				ProbePeriodMS:    400,
				ProbeTimeoutMS:   300,
				SuccessThreshold: 2,
				FailureThreshold: 2,
				Order:            4,
			},
		},
	}
	project.Events = buildEvents()
	for index := range project.Events {
		event := &project.Events[index]
		if event.InstanceID == "" || event.ServiceID != "" {
			continue
		}
		if instance, ok := project.InstanceByID(event.InstanceID); ok {
			event.ServiceID = instance.ServiceID
		}
	}
	return project
}

func buildEvents() []registry.Event {
	boolPtr := func(value bool) *bool { return &value }
	event := func(tick int64, kind registry.EventKind, instanceID, serviceID string) registry.Event {
		return registry.Event{
			AtMS:       tick * TickMS,
			Kind:       kind,
			InstanceID: instanceID,
			ServiceID:  serviceID,
		}
	}
	probe := func(tick int64, instanceID string, success bool) registry.Event {
		item := event(tick, registry.EventProbeResult, instanceID, "")
		item.Success = boolPtr(success)
		return item
	}
	query := func(tick int64, serviceID, queryID string, scale registry.WeightScale) registry.Event {
		item := registry.Event{
			AtMS:        tick * TickMS,
			Kind:        registry.EventQuery,
			ServiceID:   serviceID,
			WeightScale: scale,
			QueryID:     queryID,
		}
		return item
	}

	events := []registry.Event{
		event(0, registry.EventRegister, "api-a", ""),
		event(0, registry.EventRegister, "api-b", ""),
		event(0, registry.EventRegister, "api-c", ""),
		event(0, registry.EventRegister, "batch-a", ""),
		event(0, registry.EventRegister, "batch-b", ""),

		probe(1, "api-a", true),
		probe(1, "api-b", true),
		probe(1, "api-c", true),
		probe(1, "batch-a", false),
		probe(2, "batch-b", true),
		query(2, "svc-api", "q-2-api-10k", registry.ScalePer10K),

		probe(4, "api-a", false),
		probe(4, "api-c", false),
		event(4, registry.EventHeartbeat, "api-a", ""),
		event(4, registry.EventHeartbeat, "api-b", ""),

		probe(5, "api-b", false),
		probe(5, "batch-b", false),
		query(6, "svc-api", "q-6-api", registry.ScalePerMille),

		probe(7, "api-a", true),
		probe(7, "api-c", true),

		event(9, registry.EventHeartbeat, "api-a", ""),
		event(9, registry.EventHeartbeat, "api-b", ""),
		event(9, registry.EventRegister, "api-c", ""),
		query(9, "svc-api", "q-9-api", registry.ScalePer10K),

		probe(10, "api-a", true),
		probe(10, "api-c", true),
		query(11, "svc-batch", "q-11-batch", registry.ScalePerMille),

		event(12, registry.EventHeartbeat, "api-a", ""),
		event(12, registry.EventHeartbeat, "api-b", ""),
		event(12, registry.EventDeregister, "batch-a", ""),
		query(12, "svc-batch", "q-12-batch", registry.ScalePer10K),

		probe(13, "api-a", true),
		probe(13, "api-c", true),

		event(14, registry.EventHeartbeat, "api-a", ""),
		event(14, registry.EventHeartbeat, "api-b", ""),
		query(15, "svc-api", "q-15-api", registry.ScalePerMille),
		query(15, "svc-batch", "q-15-batch", registry.ScalePerMille),

		probe(16, "api-a", true),
		probe(16, "api-c", true),
		query(18, "svc-api", "q-18-api", registry.ScalePer10K),

		event(19, registry.EventRegister, "batch-a", ""),
		query(20, "svc-api", "q-20-api", registry.ScalePerMille),
		query(20, "svc-batch", "q-20-batch", registry.ScalePer10K),
	}
	return events
}

func BuildBundle(seed int64) (*Bundle, error) {
	project := BuildProject(seed)
	run, err := registry.RunProject(project, registry.RunOptions{})
	if err != nil {
		return nil, err
	}
	bundle := &Bundle{
		Project: project,
		Truth:   run.Truth(),
		Files:   make(map[registry.SchemaVersion][]byte),
	}
	for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
		data, err := MarshalVersion(project, schema, &bundle.Truth)
		if err != nil {
			return nil, err
		}
		bundle.Files[schema] = data
	}
	return bundle, nil
}

func WriteBundle(dir string, seed int64) (*Bundle, error) {
	bundle, err := BuildBundle(seed)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for schema, data := range bundle.Files {
		path := filepath.Join(dir, string(schema)+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	return bundle, nil
}

func MarshalVersion(project *registry.Project, schema registry.SchemaVersion, truth *registry.TruthReport) ([]byte, error) {
	var truthValue any
	if truth != nil {
		truthValue = truth
	}
	var document map[string]any
	switch schema {
	case registry.SchemaV1:
		document = encodeV1(project, truthValue)
	case registry.SchemaV2:
		document = encodeV2(project, truthValue)
	case registry.SchemaV3:
		document = encodeV3(project, truthValue)
	default:
		return nil, fmt.Errorf("unsupported schema %s", schema)
	}
	return json.MarshalIndent(document, "", "  ")
}

func encodeV1(project *registry.Project, truth any) map[string]any {
	document := map[string]any{
		"schema_version":  string(registry.SchemaV1),
		"project_version": project.ProjectVersion,
		"seed":            project.Seed,
		"tick_ms":         project.TickMS,
		"budgets": map[string]any{
			"global_concurrency": project.Budgets.GlobalConcurrency,
			"node_concurrency":   project.Budgets.NodeConcurrency,
			"rack_concurrency":   project.Budgets.RackConcurrency,
			"instance_buffer":    project.Budgets.InstanceBuffer,
			"max_delay_ticks":    project.Budgets.MaxDelayMS / project.TickMS,
		},
		"services":      []any{},
		"instance_rows": []any{},
		"events":        []any{},
	}
	services := make([]any, 0, len(project.Services))
	for _, service := range project.Services {
		services = append(services, map[string]any{
			"service_id":                 service.ID,
			"name":                       service.Name,
			"default_weight_scale":       string(service.DefaultWeightScale),
			"half_open_policy":           string(service.HalfOpenPolicy),
			"half_open_factor_per_mille": service.HalfOpenFactorPermille,
			"admit_initial":              service.AdmitInitial,
		})
	}
	document["services"] = services
	rows := make([]any, 0, len(project.Instances))
	for _, instance := range project.Instances {
		rows = append(rows, map[string]any{
			"instance_id":           instance.ID,
			"service_id":            instance.ServiceID,
			"node_id":               instance.NodeID,
			"rack_id":               instance.RackID,
			"address":               instance.Address,
			"weight":                instance.Weight,
			"weight_scale":          string(instance.WeightScale),
			"initial_health":        string(instance.InitialHealth),
			"lease_ttl_ticks":       instance.LeaseTTLMS / project.TickMS,
			"heartbeat_every_ticks": instance.HeartbeatMS / project.TickMS,
			"probe_every_ticks":     instance.ProbePeriodMS / project.TickMS,
			"probe_timeout_ticks":   instance.ProbeTimeoutMS / project.TickMS,
			"success_threshold_abs": instance.SuccessThreshold,
			"failure_threshold_abs": instance.FailureThreshold,
		})
	}
	document["instance_rows"] = rows
	document["events"] = encodeEventsV1(project)
	if truth != nil {
		document["truth"] = truth
	}
	return document
}

func encodeV2(project *registry.Project, truth any) map[string]any {
	document := map[string]any{
		"schemaVersion":  string(registry.SchemaV2),
		"projectVersion": project.ProjectVersion,
		"seed":           project.Seed,
		"tickMs":         project.TickMS,
		"budgets": map[string]any{
			"globalConcurrency": project.Budgets.GlobalConcurrency,
			"nodeConcurrency":   project.Budgets.NodeConcurrency,
			"rackConcurrency":   project.Budgets.RackConcurrency,
			"instanceBuffer":    project.Budgets.InstanceBuffer,
			"maxDelayMs":        project.Budgets.MaxDelayMS,
		},
		"services": []any{},
		"events":   encodeEventsV2(project),
	}
	services := make([]any, 0, len(project.Services))
	for _, service := range project.Services {
		instances := make([]any, 0)
		for _, instance := range project.Instances {
			if instance.ServiceID != service.ID {
				continue
			}
			instances = append(instances, map[string]any{
				"id":                        instance.ID,
				"nodeId":                    instance.NodeID,
				"rackId":                    instance.RackID,
				"address":                   instance.Address,
				"weight":                    instance.Weight,
				"weightScale":               string(instance.WeightScale),
				"initialHealth":             string(instance.InitialHealth),
				"leaseTtlMs":                instance.LeaseTTLMS,
				"heartbeatEveryMs":          instance.HeartbeatMS,
				"probeEveryMs":              instance.ProbePeriodMS,
				"probeTimeoutMs":            instance.ProbeTimeoutMS,
				"successThresholdRemaining": instance.SuccessThreshold - 1,
				"failureThresholdRemaining": instance.FailureThreshold - 1,
			})
		}
		services = append(services, map[string]any{
			"id":                     service.ID,
			"name":                   service.Name,
			"weightScale":            string(service.DefaultWeightScale),
			"halfOpenPolicy":         string(service.HalfOpenPolicy),
			"halfOpenFactorPermille": service.HalfOpenFactorPermille,
			"admitInitial":           service.AdmitInitial,
			"instances":              instances,
		})
	}
	document["services"] = services
	if truth != nil {
		document["truth"] = truth
	}
	return document
}

func encodeV3(project *registry.Project, truth any) map[string]any {
	document := map[string]any{
		"schema-version":  string(registry.SchemaV3),
		"project-version": project.ProjectVersion,
		"seed":            project.Seed,
		"tick-ms":         project.TickMS,
		"budgets": map[string]any{
			"global-concurrency": project.Budgets.GlobalConcurrency,
			"node-concurrency":   project.Budgets.NodeConcurrency,
			"rack-concurrency":   project.Budgets.RackConcurrency,
			"instance-buffer":    project.Budgets.InstanceBuffer,
			"max-delay-ms":       project.Budgets.MaxDelayMS,
		},
		"services": []any{},
		"events":   encodeEventsV3(project),
	}
	services := make([]any, 0, len(project.Services))
	for _, service := range project.Services {
		instances := make([]any, 0)
		for _, instance := range project.Instances {
			if instance.ServiceID != service.ID {
				continue
			}
			leaseSeconds, leaseMillis := splitMS(instance.LeaseTTLMS)
			heartbeatSeconds, heartbeatMillis := splitMS(instance.HeartbeatMS)
			probeSeconds, probeMillis := splitMS(instance.ProbePeriodMS)
			timeoutSeconds, timeoutMillis := splitMS(instance.ProbeTimeoutMS)
			watermark := instance.LeaseTTLMS
			instances = append(instances, map[string]any{
				"id":                          instance.ID,
				"node-id":                     instance.NodeID,
				"rack-id":                     instance.RackID,
				"address":                     instance.Address,
				"weight":                      instance.Weight,
				"weight-scale":                string(instance.WeightScale),
				"initial-health":              string(instance.InitialHealth),
				"lease-ttl-seconds":           leaseSeconds,
				"lease-ttl-millis":            leaseMillis,
				"heartbeat-every-seconds":     heartbeatSeconds,
				"heartbeat-every-millis":      heartbeatMillis,
				"probe-every-seconds":         probeSeconds,
				"probe-every-millis":          probeMillis,
				"probe-timeout-seconds":       timeoutSeconds,
				"probe-timeout-millis":        timeoutMillis,
				"success-threshold-remaining": instance.SuccessThreshold - 1,
				"failure-threshold-remaining": instance.FailureThreshold - 1,
				"lease-watermark-ms":          watermark,
			})
		}
		services = append(services, map[string]any{
			"id":                         service.ID,
			"name":                       service.Name,
			"default-weight-scale":       string(service.DefaultWeightScale),
			"half-open-policy":           string(service.HalfOpenPolicy),
			"half-open-factor-per-mille": service.HalfOpenFactorPermille,
			"admit-initial":              service.AdmitInitial,
			"instances":                  instances,
		})
	}
	document["services"] = services
	if truth != nil {
		document["truth"] = truth
	}
	return document
}

func splitMS(value int64) (int64, int64) {
	return value / 1000, value % 1000
}

func encodeEventsV1(project *registry.Project) []any {
	events := make([]any, 0, len(project.Events))
	for _, event := range project.Events {
		item := map[string]any{
			"at_tick": event.AtMS / project.TickMS,
			"kind":    string(event.Kind),
		}
		addOptionalEventFields(item, event, "instance_id", "service_id", "probe_id", "weight_scale", "query_id", "limit")
		events = append(events, item)
	}
	return events
}

func encodeEventsV2(project *registry.Project) []any {
	events := make([]any, 0, len(project.Events))
	for _, event := range project.Events {
		item := map[string]any{
			"atTick": event.AtMS / project.TickMS,
			"kind":   string(event.Kind),
		}
		addOptionalEventFields(item, event, "instanceId", "serviceId", "probeId", "weightScale", "queryId", "limit")
		events = append(events, item)
	}
	return events
}

func encodeEventsV3(project *registry.Project) []any {
	events := make([]any, 0, len(project.Events))
	for _, event := range project.Events {
		item := map[string]any{
			"at-tick": event.AtMS / project.TickMS,
			"kind":    string(event.Kind),
		}
		addOptionalEventFields(item, event, "instance-id", "service-id", "probe-id", "weight-scale", "query-id", "limit")
		events = append(events, item)
	}
	return events
}

func addOptionalEventFields(item map[string]any, event registry.Event, instanceKey, serviceKey, probeKey, scaleKey, queryKey, limitKey string) {
	if event.InstanceID != "" {
		item[instanceKey] = event.InstanceID
	}
	if event.ServiceID != "" {
		item[serviceKey] = event.ServiceID
	}
	if event.ProbeID != "" {
		item[probeKey] = event.ProbeID
	}
	if event.Success != nil {
		item["success"] = *event.Success
	}
	if event.WeightScale != "" {
		item[scaleKey] = string(event.WeightScale)
	}
	if event.QueryID != "" {
		item[queryKey] = event.QueryID
	}
	if event.Limit != 0 {
		item[limitKey] = event.Limit
	}
}
