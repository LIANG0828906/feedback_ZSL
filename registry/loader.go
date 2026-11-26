package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

type Document struct {
	Project *Project
	Truth   *TruthReport
}

func LoadProjectFile(path string) (*Project, error) {
	document, err := LoadDocumentFile(path)
	if err != nil {
		return nil, err
	}
	return document.Project, nil
}

func LoadDocumentFile(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, WrapError(CodeSchemaLocation, "cannot read project file", ErrorContext{Path: path}, err)
	}
	return LoadDocument(data)
}

func LoadProject(data []byte) (*Project, error) {
	document, err := LoadDocument(data)
	if err != nil {
		return nil, err
	}
	return document.Project, nil
}

func LoadDocument(data []byte) (*Document, error) {
	top, err := objectMap(data, "$")
	if err != nil {
		return nil, err
	}

	version, err := schemaVersionFromTop(top)
	if err != nil {
		return nil, err
	}

	var project *Project
	var truth *TruthReport
	switch version {
	case SchemaV1:
		project, truth, err = loadV1(top)
	case SchemaV2:
		project, truth, err = loadV2(top)
	case SchemaV3:
		project, truth, err = loadV3(top)
	default:
		err = NewError(CodeSchemaUnknown, "unsupported schema version", ErrorContext{Path: "$.schema_version"})
	}
	if err != nil {
		return nil, err
	}
	if err := validateProject(project); err != nil {
		return nil, err
	}
	project.SchemaVersion = version
	if truth != nil {
		hash, hashErr := project.ModelHash()
		if hashErr != nil {
			return nil, hashErr
		}
		if truth.ProjectVersion != "" && truth.ProjectVersion != project.ProjectVersion {
			return nil, NewError(CodeSchemaField, "embedded truth project version does not match", ErrorContext{Path: "$.truth.project_version"})
		}
		if truth.ModelHash != "" && truth.ModelHash != hash {
			return nil, NewError(CodeSchemaField, "embedded truth model hash does not match", ErrorContext{Path: "$.truth.model_hash"})
		}
	}
	return &Document{Project: project, Truth: truth}, nil
}

func schemaVersionFromTop(top map[string]json.RawMessage) (SchemaVersion, error) {
	type candidate struct {
		key string
		ver SchemaVersion
	}
	candidates := []candidate{
		{key: "schema_version", ver: SchemaV1},
		{key: "schemaVersion", ver: SchemaV2},
		{key: "schema-version", ver: SchemaV3},
	}
	found := ""
	var version SchemaVersion
	for _, candidate := range candidates {
		raw, ok := top[candidate.key]
		if !ok {
			continue
		}
		if found != "" {
			return "", NewError(CodeSchemaField, "multiple schema version fields", ErrorContext{Path: "$." + candidate.key})
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", WrapError(CodeSchemaField, "schema version must be a string", ErrorContext{Path: "$." + candidate.key}, err)
		}
		found = candidate.key
		version = SchemaVersion(value)
		if version != candidate.ver {
			return "", NewError(CodeSchemaVersionMismatch, fmt.Sprintf("field %s does not match schema %s", candidate.key, value), ErrorContext{Path: "$." + candidate.key})
		}
	}
	if found == "" {
		return "", NewError(CodeSchemaUnknown, "schema version marker is missing", ErrorContext{Path: "$"})
	}
	if !version.Valid() {
		return "", NewError(CodeSchemaUnknown, "unsupported schema version", ErrorContext{Path: "$." + found})
	}
	return version, nil
}

type keyValidator func(path string, raw json.RawMessage) error

func validateKeys(m map[string]json.RawMessage, path string, allowed map[string]keyValidator) error {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		validator, ok := allowed[key]
		if !ok {
			return NewError(CodeSchemaField, "unknown field for schema version", ErrorContext{Path: path + "." + key, FieldName: key})
		}
		if validator != nil {
			if err := validator(path+"."+key, m[key]); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireKeys(m map[string]json.RawMessage, path string, keys ...string) error {
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			return NewError(CodeSchemaField, "required field is missing", ErrorContext{Path: path + "." + key, FieldName: key})
		}
	}
	return nil
}

func objectMap(data []byte, path string) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, WrapError(CodeSchemaLocation, "invalid JSON object", ErrorContext{Path: path}, err)
	}
	if value == nil {
		return nil, NewError(CodeSchemaLocation, "expected JSON object", ErrorContext{Path: path})
	}
	return value, nil
}

func objectValue(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return nil, WrapError(CodeSchemaField, "expected object", ErrorContext{Path: path}, err)
	}
	return value, nil
}

func arrayValue(raw json.RawMessage, path string) ([]json.RawMessage, error) {
	var value []json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, WrapError(CodeSchemaField, "expected array", ErrorContext{Path: path}, err)
	}
	return value, nil
}

func objectItems(raw json.RawMessage, path string, check func(map[string]json.RawMessage, string) error) error {
	items, err := arrayValue(raw, path)
	if err != nil {
		return err
	}
	for index, item := range items {
		itemPath := fmt.Sprintf("%s[%d]", path, index)
		object, err := objectValue(item, itemPath)
		if err != nil {
			return err
		}
		if err := check(object, itemPath); err != nil {
			return err
		}
	}
	return nil
}

func checkTruth(path string, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return WrapError(CodeSchemaField, "truth must be valid JSON", ErrorContext{Path: path}, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// v1: snake_case, flat instance rows, tick durations, absolute thresholds.

type rawV1 struct {
	SchemaVersion  string          `json:"schema_version"`
	ProjectVersion string          `json:"project_version"`
	Seed           int64           `json:"seed"`
	TickMS         int64           `json:"tick_ms"`
	Budgets        rawV1Budget     `json:"budgets"`
	Services       []rawV1Service  `json:"services"`
	InstanceRows   []rawV1Instance `json:"instance_rows"`
	Events         []rawV1Event    `json:"events"`
	Truth          json.RawMessage `json:"truth"`
}

type rawV1Budget struct {
	GlobalConcurrency int   `json:"global_concurrency"`
	NodeConcurrency   int   `json:"node_concurrency"`
	RackConcurrency   int   `json:"rack_concurrency"`
	InstanceBuffer    int   `json:"instance_buffer"`
	MaxDelayTicks     int64 `json:"max_delay_ticks"`
}

type rawV1Service struct {
	ID                     string `json:"service_id"`
	Name                   string `json:"name"`
	DefaultWeightScale     string `json:"default_weight_scale"`
	HalfOpenPolicy         string `json:"half_open_policy"`
	HalfOpenFactorPermille int64  `json:"half_open_factor_per_mille"`
	AdmitInitial           bool   `json:"admit_initial"`
}

type rawV1Instance struct {
	ID               string `json:"instance_id"`
	ServiceID        string `json:"service_id"`
	NodeID           string `json:"node_id"`
	RackID           string `json:"rack_id"`
	Address          string `json:"address"`
	Weight           int64  `json:"weight"`
	WeightScale      string `json:"weight_scale"`
	InitialHealth    string `json:"initial_health"`
	LeaseTTLTicks    int64  `json:"lease_ttl_ticks"`
	HeartbeatTicks   int64  `json:"heartbeat_every_ticks"`
	ProbeEveryTicks  int64  `json:"probe_every_ticks"`
	ProbeTimeoutTick int64  `json:"probe_timeout_ticks"`
	SuccessAbsolute  int    `json:"success_threshold_abs"`
	FailureAbsolute  int    `json:"failure_threshold_abs"`
}

type rawV1Event struct {
	AtTick      int64  `json:"at_tick"`
	Kind        string `json:"kind"`
	ServiceID   string `json:"service_id"`
	InstanceID  string `json:"instance_id"`
	ProbeID     string `json:"probe_id"`
	Success     *bool  `json:"success"`
	WeightScale string `json:"weight_scale"`
	QueryID     string `json:"query_id"`
	Limit       int    `json:"limit"`
}

func loadV1(top map[string]json.RawMessage) (*Project, *TruthReport, error) {
	allowed := map[string]keyValidator{
		"schema_version":  nil,
		"project_version": nil,
		"seed":            nil,
		"tick_ms":         nil,
		"budgets": func(path string, raw json.RawMessage) error {
			m, err := objectValue(raw, path)
			if err != nil {
				return err
			}
			return validateKeys(m, path, map[string]keyValidator{
				"global_concurrency": nil,
				"node_concurrency":   nil,
				"rack_concurrency":   nil,
				"instance_buffer":    nil,
				"max_delay_ticks":    nil,
			})
		},
		"services": func(path string, raw json.RawMessage) error {
			return objectItems(raw, path, func(m map[string]json.RawMessage, itemPath string) error {
				return validateKeys(m, itemPath, map[string]keyValidator{
					"service_id":                 nil,
					"name":                       nil,
					"default_weight_scale":       nil,
					"half_open_policy":           nil,
					"half_open_factor_per_mille": nil,
					"admit_initial":              nil,
				})
			})
		},
		"instance_rows": func(path string, raw json.RawMessage) error {
			return objectItems(raw, path, func(m map[string]json.RawMessage, itemPath string) error {
				return validateKeys(m, itemPath, map[string]keyValidator{
					"instance_id":           nil,
					"service_id":            nil,
					"node_id":               nil,
					"rack_id":               nil,
					"address":               nil,
					"weight":                nil,
					"weight_scale":          nil,
					"initial_health":        nil,
					"lease_ttl_ticks":       nil,
					"heartbeat_every_ticks": nil,
					"probe_every_ticks":     nil,
					"probe_timeout_ticks":   nil,
					"success_threshold_abs": nil,
					"failure_threshold_abs": nil,
				})
			})
		},
		"events": func(path string, raw json.RawMessage) error {
			return objectItems(raw, path, func(m map[string]json.RawMessage, itemPath string) error {
				return validateKeys(m, itemPath, map[string]keyValidator{
					"at_tick":      nil,
					"kind":         nil,
					"service_id":   nil,
					"instance_id":  nil,
					"probe_id":     nil,
					"success":      nil,
					"weight_scale": nil,
					"query_id":     nil,
					"limit":        nil,
				})
			})
		},
		"truth": checkTruth,
	}
	if err := validateKeys(top, "$", allowed); err != nil {
		return nil, nil, err
	}
	if err := requireKeys(top, "$", "schema_version", "project_version", "seed", "tick_ms", "budgets", "services", "instance_rows", "events"); err != nil {
		return nil, nil, err
	}
	var raw rawV1
	if err := json.Unmarshal(mustReencode(top), &raw); err != nil {
		return nil, nil, WrapError(CodeSchemaField, "cannot decode v1 project", ErrorContext{Path: "$"}, err)
	}
	if raw.TickMS <= 0 || raw.TickMS > MaxTickMS {
		return nil, nil, NewError(CodeInvalidDuration, "tick_ms is outside the supported range", ErrorContext{Path: "$.tick_ms"})
	}
	if raw.Budgets.MaxDelayTicks < 0 || raw.Budgets.MaxDelayTicks > MaxDurationMS/raw.TickMS {
		return nil, nil, NewError(CodeInvalidBudget, "max_delay_ticks is outside the supported range", ErrorContext{Path: "$.budgets.max_delay_ticks"})
	}
	project := &Project{
		SchemaVersion:  SchemaV1,
		ProjectVersion: raw.ProjectVersion,
		Seed:           raw.Seed,
		TickMS:         raw.TickMS,
		Budgets: Budgets{
			GlobalConcurrency: raw.Budgets.GlobalConcurrency,
			NodeConcurrency:   raw.Budgets.NodeConcurrency,
			RackConcurrency:   raw.Budgets.RackConcurrency,
			InstanceBuffer:    raw.Budgets.InstanceBuffer,
			MaxDelayMS:        raw.Budgets.MaxDelayTicks * raw.TickMS,
		},
	}
	for index, service := range raw.Services {
		project.Services = append(project.Services, Service{
			ID:                     service.ID,
			Name:                   service.Name,
			DefaultWeightScale:     WeightScale(service.DefaultWeightScale),
			HalfOpenPolicy:         HalfOpenPolicy(service.HalfOpenPolicy),
			HalfOpenFactorPermille: service.HalfOpenFactorPermille,
			AdmitInitial:           service.AdmitInitial,
			Order:                  index,
		})
	}
	for index, instance := range raw.InstanceRows {
		sourcePath := fmt.Sprintf("$.instance_rows[%d]", index)
		if instance.LeaseTTLTicks <= 0 || instance.LeaseTTLTicks > MaxDurationMS/raw.TickMS {
			return nil, nil, NewError(CodeInvalidDuration, "lease_ttl_ticks is outside the supported range", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: sourcePath + ".lease_ttl_ticks"})
		}
		if instance.HeartbeatTicks <= 0 || instance.HeartbeatTicks > MaxDurationMS/raw.TickMS {
			return nil, nil, NewError(CodeInvalidHeartbeat, "heartbeat_every_ticks is outside the supported range", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: sourcePath + ".heartbeat_every_ticks"})
		}
		if instance.ProbeEveryTicks <= 0 || instance.ProbeEveryTicks > MaxDurationMS/raw.TickMS {
			return nil, nil, NewError(CodeInvalidDuration, "probe_every_ticks is outside the supported range", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: sourcePath + ".probe_every_ticks"})
		}
		if instance.ProbeTimeoutTick <= 0 || instance.ProbeTimeoutTick > MaxDurationMS/raw.TickMS {
			return nil, nil, NewError(CodeInvalidDuration, "probe_timeout_ticks is outside the supported range", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: sourcePath + ".probe_timeout_ticks"})
		}
		project.Instances = append(project.Instances, Instance{
			ID:               instance.ID,
			ServiceID:        instance.ServiceID,
			NodeID:           instance.NodeID,
			RackID:           instance.RackID,
			Address:          instance.Address,
			Weight:           instance.Weight,
			WeightScale:      WeightScale(instance.WeightScale),
			InitialHealth:    HealthState(instance.InitialHealth),
			LeaseTTLMS:       instance.LeaseTTLTicks * raw.TickMS,
			HeartbeatMS:      instance.HeartbeatTicks * raw.TickMS,
			ProbePeriodMS:    instance.ProbeEveryTicks * raw.TickMS,
			ProbeTimeoutMS:   instance.ProbeTimeoutTick * raw.TickMS,
			SuccessThreshold: instance.SuccessAbsolute,
			FailureThreshold: instance.FailureAbsolute,
			Order:            index,
			SourcePath:       sourcePath,
		})
	}
	for index, event := range raw.Events {
		normalized, err := normalizeEvent(event.AtTick, event.Kind, event.ServiceID, event.InstanceID, event.ProbeID, event.Success, event.WeightScale, event.QueryID, event.Limit, raw.TickMS, fmt.Sprintf("$.events[%d]", index))
		if err != nil {
			return nil, nil, err
		}
		project.Events = append(project.Events, normalized)
	}
	truth, err := decodeTruth(raw.Truth, "$.truth")
	if err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

// ---------------------------------------------------------------------------
// v2: camelCase, nested instances, millisecond durations, remaining thresholds.

type rawV2 struct {
	SchemaVersion  string          `json:"schemaVersion"`
	ProjectVersion string          `json:"projectVersion"`
	Seed           int64           `json:"seed"`
	TickMS         int64           `json:"tickMs"`
	Budgets        rawV2Budget     `json:"budgets"`
	Services       []rawV2Service  `json:"services"`
	Events         []rawV2Event    `json:"events"`
	Truth          json.RawMessage `json:"truth"`
}

type rawV2Budget struct {
	GlobalConcurrency int   `json:"globalConcurrency"`
	NodeConcurrency   int   `json:"nodeConcurrency"`
	RackConcurrency   int   `json:"rackConcurrency"`
	InstanceBuffer    int   `json:"instanceBuffer"`
	MaxDelayMS        int64 `json:"maxDelayMs"`
}

type rawV2Service struct {
	ID                     string          `json:"id"`
	Name                   string          `json:"name"`
	WeightScale            string          `json:"weightScale"`
	HalfOpenPolicy         string          `json:"halfOpenPolicy"`
	HalfOpenFactorPermille int64           `json:"halfOpenFactorPermille"`
	AdmitInitial           bool            `json:"admitInitial"`
	Instances              []rawV2Instance `json:"instances"`
}

type rawV2Instance struct {
	ID               string `json:"id"`
	NodeID           string `json:"nodeId"`
	RackID           string `json:"rackId"`
	Address          string `json:"address"`
	Weight           int64  `json:"weight"`
	WeightScale      string `json:"weightScale"`
	InitialHealth    string `json:"initialHealth"`
	LeaseTTLMS       int64  `json:"leaseTtlMs"`
	HeartbeatMS      int64  `json:"heartbeatEveryMs"`
	ProbePeriodMS    int64  `json:"probeEveryMs"`
	ProbeTimeoutMS   int64  `json:"probeTimeoutMs"`
	SuccessRemaining *int   `json:"successThresholdRemaining"`
	FailureRemaining *int   `json:"failureThresholdRemaining"`
	LeaseWatermarkMS *int64 `json:"leaseWatermarkMs"`
}

type rawV2Event struct {
	AtTick      int64  `json:"atTick"`
	Kind        string `json:"kind"`
	ServiceID   string `json:"serviceId"`
	InstanceID  string `json:"instanceId"`
	ProbeID     string `json:"probeId"`
	Success     *bool  `json:"success"`
	WeightScale string `json:"weightScale"`
	QueryID     string `json:"queryId"`
	Limit       int    `json:"limit"`
}

func loadV2(top map[string]json.RawMessage) (*Project, *TruthReport, error) {
	allowed := map[string]keyValidator{
		"schemaVersion":  nil,
		"projectVersion": nil,
		"seed":           nil,
		"tickMs":         nil,
		"budgets": func(path string, raw json.RawMessage) error {
			m, err := objectValue(raw, path)
			if err != nil {
				return err
			}
			return validateKeys(m, path, map[string]keyValidator{
				"globalConcurrency": nil,
				"nodeConcurrency":   nil,
				"rackConcurrency":   nil,
				"instanceBuffer":    nil,
				"maxDelayMs":        nil,
			})
		},
		"services": func(path string, raw json.RawMessage) error {
			return objectItems(raw, path, func(m map[string]json.RawMessage, itemPath string) error {
				if err := validateKeys(m, itemPath, map[string]keyValidator{
					"id":                     nil,
					"name":                   nil,
					"weightScale":            nil,
					"halfOpenPolicy":         nil,
					"halfOpenFactorPermille": nil,
					"admitInitial":           nil,
					"instances": func(path string, raw json.RawMessage) error {
						return objectItems(raw, path, func(im map[string]json.RawMessage, ip string) error {
							return validateKeys(im, ip, map[string]keyValidator{
								"id":                        nil,
								"nodeId":                    nil,
								"rackId":                    nil,
								"address":                   nil,
								"weight":                    nil,
								"weightScale":               nil,
								"initialHealth":             nil,
								"leaseTtlMs":                nil,
								"heartbeatEveryMs":          nil,
								"probeEveryMs":              nil,
								"probeTimeoutMs":            nil,
								"successThresholdRemaining": nil,
								"failureThresholdRemaining": nil,
								"leaseWatermarkMs":          nil,
							})
						})
					},
				}); err != nil {
					return err
				}
				return nil
			})
		},
		"events": func(path string, raw json.RawMessage) error {
			return objectItems(raw, path, func(m map[string]json.RawMessage, itemPath string) error {
				return validateKeys(m, itemPath, map[string]keyValidator{
					"atTick":      nil,
					"kind":        nil,
					"serviceId":   nil,
					"instanceId":  nil,
					"probeId":     nil,
					"success":     nil,
					"weightScale": nil,
					"queryId":     nil,
					"limit":       nil,
				})
			})
		},
		"truth": checkTruth,
	}
	if err := validateKeys(top, "$", allowed); err != nil {
		return nil, nil, err
	}
	if err := requireKeys(top, "$", "schemaVersion", "projectVersion", "seed", "tickMs", "budgets", "services", "events"); err != nil {
		return nil, nil, err
	}
	var raw rawV2
	if err := json.Unmarshal(mustReencode(top), &raw); err != nil {
		return nil, nil, WrapError(CodeSchemaField, "cannot decode v2 project", ErrorContext{Path: "$"}, err)
	}
	if raw.TickMS <= 0 || raw.TickMS > MaxTickMS {
		return nil, nil, NewError(CodeInvalidDuration, "tickMs is outside the supported range", ErrorContext{Path: "$.tickMs"})
	}
	project := &Project{
		SchemaVersion:  SchemaV2,
		ProjectVersion: raw.ProjectVersion,
		Seed:           raw.Seed,
		TickMS:         raw.TickMS,
		Budgets: Budgets{
			GlobalConcurrency: raw.Budgets.GlobalConcurrency,
			NodeConcurrency:   raw.Budgets.NodeConcurrency,
			RackConcurrency:   raw.Budgets.RackConcurrency,
			InstanceBuffer:    raw.Budgets.InstanceBuffer,
			MaxDelayMS:        raw.Budgets.MaxDelayMS,
		},
	}
	order := 0
	for serviceIndex, service := range raw.Services {
		project.Services = append(project.Services, Service{
			ID:                     service.ID,
			Name:                   service.Name,
			DefaultWeightScale:     WeightScale(service.WeightScale),
			HalfOpenPolicy:         HalfOpenPolicy(service.HalfOpenPolicy),
			HalfOpenFactorPermille: service.HalfOpenFactorPermille,
			AdmitInitial:           service.AdmitInitial,
			Order:                  serviceIndex,
		})
		for instanceIndex, instance := range service.Instances {
			sourcePath := fmt.Sprintf("$.services[%d].instances[%d]", serviceIndex, instanceIndex)
			scale := WeightScale(instance.WeightScale)
			if scale == "" {
				scale = WeightScale(service.WeightScale)
			}
			success, err := remainingToAbsolute(instance.SuccessRemaining, service.ID, instance.ID, sourcePath+".successThresholdRemaining")
			if err != nil {
				return nil, nil, err
			}
			failure, err := remainingToAbsolute(instance.FailureRemaining, service.ID, instance.ID, sourcePath+".failureThresholdRemaining")
			if err != nil {
				return nil, nil, err
			}
			project.Instances = append(project.Instances, Instance{
				ID:               instance.ID,
				ServiceID:        service.ID,
				NodeID:           instance.NodeID,
				RackID:           instance.RackID,
				Address:          instance.Address,
				Weight:           instance.Weight,
				WeightScale:      scale,
				InitialHealth:    HealthState(instance.InitialHealth),
				LeaseTTLMS:       instance.LeaseTTLMS,
				HeartbeatMS:      instance.HeartbeatMS,
				ProbePeriodMS:    instance.ProbePeriodMS,
				ProbeTimeoutMS:   instance.ProbeTimeoutMS,
				SuccessThreshold: success,
				FailureThreshold: failure,
				LeaseWatermarkMS: instance.LeaseWatermarkMS,
				Order:            order,
				SourcePath:       sourcePath,
			})
			if instance.LeaseWatermarkMS != nil && *instance.LeaseWatermarkMS != instance.LeaseTTLMS {
				return nil, nil, NewError(CodeLeaseWatermarkConflict, "lease watermark does not equal normalized lease TTL", ErrorContext{ServiceID: service.ID, InstanceID: instance.ID, Path: sourcePath + ".leaseWatermarkMs"})
			}
			order++
		}
	}
	for index, event := range raw.Events {
		normalized, err := normalizeEvent(event.AtTick, event.Kind, event.ServiceID, event.InstanceID, event.ProbeID, event.Success, event.WeightScale, event.QueryID, event.Limit, raw.TickMS, fmt.Sprintf("$.events[%d]", index))
		if err != nil {
			return nil, nil, err
		}
		project.Events = append(project.Events, normalized)
	}
	truth, err := decodeTruth(raw.Truth, "$.truth")
	if err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

// ---------------------------------------------------------------------------
// v3: kebab-case, nested instances, composite seconds+millis, optional
// watermark, remaining thresholds. Generic legacy fields intentionally fail
// with E_THRESHOLD_AMBIGUOUS or E_LEASE_UNIT_AMBIGUOUS when no witness exists.

type rawV3 struct {
	SchemaVersion  string          `json:"schema-version"`
	ProjectVersion string          `json:"project-version"`
	Seed           int64           `json:"seed"`
	TickMS         int64           `json:"tick-ms"`
	Budgets        rawV3Budget     `json:"budgets"`
	Services       []rawV3Service  `json:"services"`
	Events         []rawV3Event    `json:"events"`
	Truth          json.RawMessage `json:"truth"`
}

type rawV3Budget struct {
	GlobalConcurrency int   `json:"global-concurrency"`
	NodeConcurrency   int   `json:"node-concurrency"`
	RackConcurrency   int   `json:"rack-concurrency"`
	InstanceBuffer    int   `json:"instance-buffer"`
	MaxDelayMS        int64 `json:"max-delay-ms"`
}

type rawV3Service struct {
	ID                     string          `json:"id"`
	Name                   string          `json:"name"`
	DefaultWeightScale     string          `json:"default-weight-scale"`
	HalfOpenPolicy         string          `json:"half-open-policy"`
	HalfOpenFactorPermille int64           `json:"half-open-factor-per-mille"`
	AdmitInitial           bool            `json:"admit-initial"`
	Instances              []rawV3Instance `json:"instances"`
}

type rawV3Instance struct {
	ID                  string `json:"id"`
	NodeID              string `json:"node-id"`
	RackID              string `json:"rack-id"`
	Address             string `json:"address"`
	Weight              int64  `json:"weight"`
	WeightScale         string `json:"weight-scale"`
	InitialHealth       string `json:"initial-health"`
	LeaseTTLSeconds     *int64 `json:"lease-ttl-seconds"`
	LeaseTTLMillis      *int64 `json:"lease-ttl-millis"`
	HeartbeatSeconds    *int64 `json:"heartbeat-every-seconds"`
	HeartbeatMillis     *int64 `json:"heartbeat-every-millis"`
	ProbeSeconds        *int64 `json:"probe-every-seconds"`
	ProbeMillis         *int64 `json:"probe-every-millis"`
	ProbeTimeoutSeconds *int64 `json:"probe-timeout-seconds"`
	ProbeTimeoutMillis  *int64 `json:"probe-timeout-millis"`
	SuccessRemaining    *int   `json:"success-threshold-remaining"`
	FailureRemaining    *int   `json:"failure-threshold-remaining"`
	SuccessGeneric      *int   `json:"success-threshold"`
	FailureGeneric      *int   `json:"failure-threshold"`
	ThresholdMode       string `json:"threshold-mode"`
	LeaseTTLGeneric     *int64 `json:"lease-ttl"`
	LeaseWatermarkMS    *int64 `json:"lease-watermark-ms"`
}

type rawV3Event struct {
	AtTick      int64  `json:"at-tick"`
	Kind        string `json:"kind"`
	ServiceID   string `json:"service-id"`
	InstanceID  string `json:"instance-id"`
	ProbeID     string `json:"probe-id"`
	Success     *bool  `json:"success"`
	WeightScale string `json:"weight-scale"`
	QueryID     string `json:"query-id"`
	Limit       int    `json:"limit"`
}

func loadV3(top map[string]json.RawMessage) (*Project, *TruthReport, error) {
	allowed := map[string]keyValidator{
		"schema-version":  nil,
		"project-version": nil,
		"seed":            nil,
		"tick-ms":         nil,
		"budgets": func(path string, raw json.RawMessage) error {
			m, err := objectValue(raw, path)
			if err != nil {
				return err
			}
			return validateKeys(m, path, map[string]keyValidator{
				"global-concurrency": nil,
				"node-concurrency":   nil,
				"rack-concurrency":   nil,
				"instance-buffer":    nil,
				"max-delay-ms":       nil,
			})
		},
		"services": func(path string, raw json.RawMessage) error {
			return objectItems(raw, path, func(m map[string]json.RawMessage, itemPath string) error {
				return validateKeys(m, itemPath, map[string]keyValidator{
					"id":                         nil,
					"name":                       nil,
					"default-weight-scale":       nil,
					"half-open-policy":           nil,
					"half-open-factor-per-mille": nil,
					"admit-initial":              nil,
					"instances": func(path string, raw json.RawMessage) error {
						return objectItems(raw, path, func(im map[string]json.RawMessage, ip string) error {
							return validateKeys(im, ip, map[string]keyValidator{
								"id":                          nil,
								"node-id":                     nil,
								"rack-id":                     nil,
								"address":                     nil,
								"weight":                      nil,
								"weight-scale":                nil,
								"initial-health":              nil,
								"lease-ttl-seconds":           nil,
								"lease-ttl-millis":            nil,
								"heartbeat-every-seconds":     nil,
								"heartbeat-every-millis":      nil,
								"probe-every-seconds":         nil,
								"probe-every-millis":          nil,
								"probe-timeout-seconds":       nil,
								"probe-timeout-millis":        nil,
								"success-threshold-remaining": nil,
								"failure-threshold-remaining": nil,
								"success-threshold":           nil,
								"failure-threshold":           nil,
								"threshold-mode":              nil,
								"lease-ttl":                   nil,
								"lease-watermark-ms":          nil,
							})
						})
					},
				})
			})
		},
		"events": func(path string, raw json.RawMessage) error {
			return objectItems(raw, path, func(m map[string]json.RawMessage, itemPath string) error {
				return validateKeys(m, itemPath, map[string]keyValidator{
					"at-tick":      nil,
					"kind":         nil,
					"service-id":   nil,
					"instance-id":  nil,
					"probe-id":     nil,
					"success":      nil,
					"weight-scale": nil,
					"query-id":     nil,
					"limit":        nil,
				})
			})
		},
		"truth": checkTruth,
	}
	if err := validateKeys(top, "$", allowed); err != nil {
		return nil, nil, err
	}
	if err := requireKeys(top, "$", "schema-version", "project-version", "seed", "tick-ms", "budgets", "services", "events"); err != nil {
		return nil, nil, err
	}
	var raw rawV3
	if err := json.Unmarshal(mustReencode(top), &raw); err != nil {
		return nil, nil, WrapError(CodeSchemaField, "cannot decode v3 project", ErrorContext{Path: "$"}, err)
	}
	if raw.TickMS <= 0 || raw.TickMS > MaxTickMS {
		return nil, nil, NewError(CodeInvalidDuration, "tick-ms is outside the supported range", ErrorContext{Path: "$.tick-ms"})
	}
	project := &Project{
		SchemaVersion:  SchemaV3,
		ProjectVersion: raw.ProjectVersion,
		Seed:           raw.Seed,
		TickMS:         raw.TickMS,
		Budgets: Budgets{
			GlobalConcurrency: raw.Budgets.GlobalConcurrency,
			NodeConcurrency:   raw.Budgets.NodeConcurrency,
			RackConcurrency:   raw.Budgets.RackConcurrency,
			InstanceBuffer:    raw.Budgets.InstanceBuffer,
			MaxDelayMS:        raw.Budgets.MaxDelayMS,
		},
	}
	order := 0
	for serviceIndex, service := range raw.Services {
		project.Services = append(project.Services, Service{
			ID:                     service.ID,
			Name:                   service.Name,
			DefaultWeightScale:     WeightScale(service.DefaultWeightScale),
			HalfOpenPolicy:         HalfOpenPolicy(service.HalfOpenPolicy),
			HalfOpenFactorPermille: service.HalfOpenFactorPermille,
			AdmitInitial:           service.AdmitInitial,
			Order:                  serviceIndex,
		})
		for instanceIndex, instance := range service.Instances {
			sourcePath := fmt.Sprintf("$.services[%d].instances[%d]", serviceIndex, instanceIndex)
			scale := WeightScale(instance.WeightScale)
			if scale == "" {
				scale = WeightScale(service.DefaultWeightScale)
			}
			leaseTTL, err := v3LeaseTTL(instance, raw.TickMS, service.ID, instance.ID, sourcePath)
			if err != nil {
				return nil, nil, err
			}
			heartbeat, err := compositeDuration(instance.HeartbeatSeconds, instance.HeartbeatMillis, service.ID, instance.ID, sourcePath+".heartbeat-every-seconds", sourcePath+".heartbeat-every-millis")
			if err != nil {
				return nil, nil, err
			}
			probePeriod, err := compositeDuration(instance.ProbeSeconds, instance.ProbeMillis, service.ID, instance.ID, sourcePath+".probe-every-seconds", sourcePath+".probe-every-millis")
			if err != nil {
				return nil, nil, err
			}
			probeTimeout, err := compositeDuration(instance.ProbeTimeoutSeconds, instance.ProbeTimeoutMillis, service.ID, instance.ID, sourcePath+".probe-timeout-seconds", sourcePath+".probe-timeout-millis")
			if err != nil {
				return nil, nil, err
			}
			success, err := v3Threshold(instance.SuccessRemaining, instance.SuccessGeneric, instance.ThresholdMode, service.ID, instance.ID, sourcePath, true)
			if err != nil {
				return nil, nil, err
			}
			failure, err := v3Threshold(instance.FailureRemaining, instance.FailureGeneric, instance.ThresholdMode, service.ID, instance.ID, sourcePath, false)
			if err != nil {
				return nil, nil, err
			}
			if instance.LeaseWatermarkMS != nil && *instance.LeaseWatermarkMS != leaseTTL {
				return nil, nil, NewError(CodeLeaseWatermarkConflict, "lease watermark does not equal normalized lease TTL", ErrorContext{ServiceID: service.ID, InstanceID: instance.ID, Path: sourcePath + ".lease-watermark-ms"})
			}
			project.Instances = append(project.Instances, Instance{
				ID:               instance.ID,
				ServiceID:        service.ID,
				NodeID:           instance.NodeID,
				RackID:           instance.RackID,
				Address:          instance.Address,
				Weight:           instance.Weight,
				WeightScale:      scale,
				InitialHealth:    HealthState(instance.InitialHealth),
				LeaseTTLMS:       leaseTTL,
				HeartbeatMS:      heartbeat,
				ProbePeriodMS:    probePeriod,
				ProbeTimeoutMS:   probeTimeout,
				SuccessThreshold: success,
				FailureThreshold: failure,
				LeaseWatermarkMS: instance.LeaseWatermarkMS,
				Order:            order,
				SourcePath:       sourcePath,
			})
			order++
		}
	}
	for index, event := range raw.Events {
		normalized, err := normalizeEvent(event.AtTick, event.Kind, event.ServiceID, event.InstanceID, event.ProbeID, event.Success, event.WeightScale, event.QueryID, event.Limit, raw.TickMS, fmt.Sprintf("$.events[%d]", index))
		if err != nil {
			return nil, nil, err
		}
		project.Events = append(project.Events, normalized)
	}
	truth, err := decodeTruth(raw.Truth, "$.truth")
	if err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

func v3LeaseTTL(instance rawV3Instance, tickMS int64, serviceID, instanceID, sourcePath string) (int64, error) {
	explicitSeconds := instance.LeaseTTLSeconds != nil || instance.LeaseTTLMillis != nil
	if instance.LeaseTTLGeneric != nil {
		if explicitSeconds {
			return 0, NewError(CodeLeaseUnitAmbiguous, "lease-ttl conflicts with explicit lease duration fields", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: sourcePath + ".lease-ttl"})
		}
		if instance.LeaseWatermarkMS == nil {
			return 0, NewError(CodeLeaseUnitAmbiguous, "legacy lease-ttl has no watermark to disambiguate ticks from milliseconds", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: sourcePath + ".lease-ttl"})
		}
		asMilliseconds := *instance.LeaseTTLGeneric
		asTicks := *instance.LeaseTTLGeneric * tickMS
		matchesMS := asMilliseconds == *instance.LeaseWatermarkMS
		matchesTicks := asTicks == *instance.LeaseWatermarkMS
		switch {
		case matchesMS == matchesTicks:
			return 0, NewError(CodeLeaseWatermarkConflict, "lease watermark cannot disambiguate legacy lease-ttl", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: sourcePath + ".lease-watermark-ms"})
		case matchesMS:
			return asMilliseconds, nil
		default:
			return asTicks, nil
		}
	}
	return compositeDuration(instance.LeaseTTLSeconds, instance.LeaseTTLMillis, serviceID, instanceID, sourcePath+".lease-ttl-seconds", sourcePath+".lease-ttl-millis")
}

func compositeDuration(seconds, millis *int64, serviceID, instanceID, secondsPath, millisPath string) (int64, error) {
	var total int64
	if seconds != nil {
		if *seconds < 0 || *seconds > MaxDurationMS/1000 {
			return 0, NewError(CodeInvalidDuration, "seconds component is outside the supported range", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: secondsPath})
		}
		total += *seconds * 1000
	}
	if millis != nil {
		if *millis < 0 || *millis >= 1000 {
			return 0, NewError(CodeInvalidDuration, "millis component must be in [0,999]", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: millisPath})
		}
		total += *millis
	}
	if total <= 0 || total > MaxDurationMS {
		return 0, NewError(CodeInvalidDuration, "duration must be positive and within seven days", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: secondsPath})
	}
	return total, nil
}

func remainingToAbsolute(remaining *int, serviceID, instanceID, path string) (int, error) {
	if remaining == nil {
		return 0, NewError(CodeThresholdAmbiguous, "threshold semantics cannot be determined", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: path})
	}
	if *remaining < 0 || *remaining > 1_000_000 {
		return 0, NewError(CodeInvalidThreshold, "remaining threshold is outside the supported range", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: path})
	}
	return *remaining + 1, nil
}

func v3Threshold(remaining *int, generic *int, mode, serviceID, instanceID, sourcePath string, success bool) (int, error) {
	label := "failure-threshold"
	remainingPath := sourcePath + ".failure-threshold-remaining"
	if success {
		label = "success-threshold"
		remainingPath = sourcePath + ".success-threshold-remaining"
	}
	genericPath := sourcePath + "." + label
	if remaining != nil && generic != nil {
		return 0, NewError(CodeThresholdAmbiguous, "both remaining and generic threshold fields are present", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: genericPath})
	}
	if remaining != nil {
		return remainingToAbsolute(remaining, serviceID, instanceID, remainingPath)
	}
	if generic == nil {
		return 0, NewError(CodeThresholdAmbiguous, "threshold semantics cannot be determined", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: remainingPath})
	}
	switch mode {
	case "absolute":
		if *generic <= 0 || *generic > 1_000_001 {
			return 0, NewError(CodeInvalidThreshold, "absolute threshold is outside the supported range", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: genericPath})
		}
		return *generic, nil
	case "remaining":
		return remainingToAbsolute(generic, serviceID, instanceID, genericPath)
	default:
		return 0, NewError(CodeThresholdAmbiguous, "generic threshold has no unambiguous threshold-mode", ErrorContext{ServiceID: serviceID, InstanceID: instanceID, Path: sourcePath + ".threshold-mode"})
	}
}

// ---------------------------------------------------------------------------

func normalizeEvent(atTick int64, kind, serviceID, instanceID, probeID string, success *bool, weightScale, queryID string, limit int, tickMS int64, path string) (Event, error) {
	if atTick < 0 {
		return Event{}, NewError(CodeInvalidEvent, "event tick must not be negative", ErrorContext{Tick: &atTick, Path: path + ".at_tick"})
	}
	if atTick > MaxDurationMS/tickMS {
		return Event{}, NewError(CodeInvalidEvent, "event tick is too large", ErrorContext{Tick: &atTick, Path: path + ".at_tick"})
	}
	eventKind := EventKind(kind)
	if !eventKind.Valid() {
		return Event{}, NewError(CodeInvalidEvent, "unknown event kind", ErrorContext{InstanceID: instanceID, ServiceID: serviceID, Tick: &atTick, Path: path + ".kind"})
	}
	event := Event{
		AtMS:        atTick * tickMS,
		Kind:        eventKind,
		ServiceID:   serviceID,
		InstanceID:  instanceID,
		ProbeID:     probeID,
		Success:     success,
		WeightScale: WeightScale(weightScale),
		QueryID:     queryID,
		Limit:       limit,
		SourcePath:  path,
	}
	switch eventKind {
	case EventRegister, EventDeregister, EventHeartbeat:
		if instanceID == "" {
			return Event{}, NewError(CodeInvalidEvent, "instance_id is required", ErrorContext{ServiceID: serviceID, Tick: &atTick, Path: path + ".instance_id"})
		}
	case EventProbeResult:
		if instanceID == "" || success == nil {
			return Event{}, NewError(CodeInvalidEvent, "probe result requires instance_id and success", ErrorContext{InstanceID: instanceID, ServiceID: serviceID, Tick: &atTick, Path: path})
		}
	case EventQuery:
		if serviceID == "" {
			return Event{}, NewError(CodeInvalidEvent, "query requires service_id", ErrorContext{Tick: &atTick, Path: path + ".service_id"})
		}
		if limit < 0 {
			return Event{}, NewError(CodeInvalidEvent, "query limit must not be negative", ErrorContext{ServiceID: serviceID, Tick: &atTick, Path: path + ".limit"})
		}
		if event.WeightScale != "" && event.WeightScale.Denominator() == 0 {
			return Event{}, NewError(CodeInvalidWeightScale, "unknown query weight scale", ErrorContext{ServiceID: serviceID, Tick: &atTick, Path: path + ".weight_scale"})
		}
	}
	return event, nil
}

func validateProject(project *Project) error {
	if project.ProjectVersion == "" {
		return NewError(CodeSchemaField, "project_version must not be empty", ErrorContext{Path: "$.project_version"})
	}
	if err := validateBudgets(project.Budgets); err != nil {
		return err
	}
	if len(project.Services) == 0 {
		return NewError(CodeSchemaField, "at least one service is required", ErrorContext{Path: "$.services"})
	}
	for index := range project.Services {
		project.Services[index].Order = index
	}
	services := make(map[string]Service, len(project.Services))
	for index, service := range project.Services {
		path := fmt.Sprintf("$.services[%d]", index)
		if service.ID == "" || service.Name == "" {
			return NewError(CodeSchemaField, "service id and name must not be empty", ErrorContext{ServiceID: service.ID, Path: path})
		}
		if _, exists := services[service.ID]; exists {
			return NewError(CodeDuplicateService, "duplicate service id", ErrorContext{ServiceID: service.ID, Path: path + ".id"})
		}
		services[service.ID] = service
		if service.DefaultWeightScale.Denominator() == 0 {
			return NewError(CodeInvalidWeightScale, "unknown default weight scale", ErrorContext{ServiceID: service.ID, Path: path})
		}
		switch service.HalfOpenPolicy {
		case HalfOpenExclude:
		case HalfOpenDemote:
			if service.HalfOpenFactorPermille <= 0 || service.HalfOpenFactorPermille > 1000 {
				return NewError(CodeInvalidWeight, "half-open factor must be in (0,1000]", ErrorContext{ServiceID: service.ID, Path: path})
			}
		default:
			return NewError(CodeInvalidHealth, "unknown half-open policy", ErrorContext{ServiceID: service.ID, Path: path})
		}
	}
	if len(project.Instances) == 0 {
		return NewError(CodeSchemaField, "at least one instance is required", ErrorContext{Path: "$.instances"})
	}
	seenInstances := make(map[string]struct{}, len(project.Instances))
	for index := range project.Instances {
		instance := &project.Instances[index]
		path := instance.SourcePath
		if path == "" {
			path = fmt.Sprintf("$.instances[%d]", index)
		}
		if instance.ID == "" {
			return NewError(CodeSchemaField, "instance id must not be empty", ErrorContext{ServiceID: instance.ServiceID, Path: path + ".id"})
		}
		if _, exists := seenInstances[instance.ID]; exists {
			return NewError(CodeDuplicateInstance, "duplicate instance id", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path + ".id"})
		}
		seenInstances[instance.ID] = struct{}{}
		service, exists := services[instance.ServiceID]
		if !exists {
			return NewError(CodeUnknownService, "instance references an unknown service", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.WeightScale == "" {
			instance.WeightScale = service.DefaultWeightScale
		}
		if instance.WeightScale.Denominator() == 0 {
			return NewError(CodeInvalidWeightScale, "unknown instance weight scale", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.Weight < 0 || instance.Weight > instance.WeightScale.Denominator() {
			return NewError(CodeInvalidWeight, "weight is outside scale domain", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if !instance.InitialHealth.Valid() {
			return NewError(CodeInvalidHealth, "unknown initial health state", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.LeaseTTLMS <= 0 || instance.LeaseTTLMS > MaxDurationMS {
			return NewError(CodeInvalidDuration, "lease TTL must be positive and within seven days", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.HeartbeatMS <= 0 || instance.HeartbeatMS > MaxDurationMS || instance.HeartbeatMS > instance.LeaseTTLMS {
			return NewError(CodeInvalidHeartbeat, "heartbeat must be positive, within seven days, and not exceed lease TTL", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.ProbePeriodMS <= 0 || instance.ProbePeriodMS > MaxDurationMS {
			return NewError(CodeInvalidDuration, "probe period must be positive and within seven days", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.ProbeTimeoutMS <= 0 || instance.ProbeTimeoutMS > MaxDurationMS {
			return NewError(CodeInvalidDuration, "probe timeout must be positive and within seven days", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.SuccessThreshold <= 0 || instance.SuccessThreshold > 1_000_001 {
			return NewError(CodeInvalidThreshold, "success threshold must be positive", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		if instance.FailureThreshold <= 0 || instance.FailureThreshold > 1_000_001 {
			return NewError(CodeInvalidThreshold, "failure threshold must be positive", ErrorContext{ServiceID: instance.ServiceID, InstanceID: instance.ID, Path: path})
		}
		// The watermark is a load-time unit/integrity witness only. It has no
		// runtime semantics, so it is normalized away to keep all versions'
		// canonical models byte-identical when the field is absent in one schema.
		instance.LeaseWatermarkMS = nil
	}
	// Canonical instance order is service declaration order, preserving the
	// declaration order inside each service. All schema versions flatten to the
	// same sequence.
	sort.SliceStable(project.Instances, func(i, j int) bool {
		leftService := services[project.Instances[i].ServiceID]
		rightService := services[project.Instances[j].ServiceID]
		if leftService.Order != rightService.Order {
			return leftService.Order < rightService.Order
		}
		return project.Instances[i].Order < project.Instances[j].Order
	})
	for index := range project.Instances {
		project.Instances[index].Order = index
	}
	for index, override := range project.Overrides {
		path := fmt.Sprintf("$.overrides[%d]", index)
		if override.FromTick < 0 {
			return NewError(CodeInvalidEvent, "override tick must not be negative", ErrorContext{InstanceID: override.InstanceID, Tick: &override.FromTick, Path: path})
		}
		base, exists := project.InstanceByID(override.InstanceID)
		if !exists {
			return NewError(CodeUnknownInstance, "override references an unknown instance", ErrorContext{InstanceID: override.InstanceID, Path: path})
		}
		effective := EffectiveInstance(project, base, override.FromTick)
		if override.LeaseTTLMS != nil && (*override.LeaseTTLMS <= 0 || *override.LeaseTTLMS > MaxDurationMS) {
			return NewError(CodeInvalidDuration, "override lease TTL is outside the supported range", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
		if override.HeartbeatMS != nil && (*override.HeartbeatMS <= 0 || *override.HeartbeatMS > MaxDurationMS || *override.HeartbeatMS > effective.LeaseTTLMS) {
			return NewError(CodeInvalidHeartbeat, "override heartbeat is outside the supported range", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
		if override.ProbePeriodMS != nil && (*override.ProbePeriodMS <= 0 || *override.ProbePeriodMS > MaxDurationMS) {
			return NewError(CodeInvalidDuration, "override probe period is outside the supported range", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
		if override.ProbeTimeoutMS != nil && (*override.ProbeTimeoutMS <= 0 || *override.ProbeTimeoutMS > MaxDurationMS) {
			return NewError(CodeInvalidDuration, "override probe timeout is outside the supported range", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
		if override.SuccessThreshold != nil && (*override.SuccessThreshold <= 0 || *override.SuccessThreshold > 1_000_001) {
			return NewError(CodeInvalidThreshold, "override success threshold is invalid", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
		if override.FailureThreshold != nil && (*override.FailureThreshold <= 0 || *override.FailureThreshold > 1_000_001) {
			return NewError(CodeInvalidThreshold, "override failure threshold is invalid", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
		if effective.WeightScale.Denominator() == 0 {
			return NewError(CodeInvalidWeightScale, "override weight scale is invalid", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
		if effective.Weight < 0 || effective.Weight > effective.WeightScale.Denominator() {
			return NewError(CodeInvalidWeight, "override weight is outside scale domain", ErrorContext{InstanceID: base.ID, ServiceID: base.ServiceID, Path: path})
		}
	}
	for index, service := range project.Services {
		service.Order = index
		project.Services[index] = service
	}
	lastMS := int64(-1)
	for index := range project.Events {
		event := &project.Events[index]
		path := event.SourcePath
		if path == "" {
			path = fmt.Sprintf("$.events[%d]", index)
		}
		if event.AtMS < 0 || event.AtMS%project.TickMS != 0 {
			return NewError(CodeInvalidEvent, "event time must be a nonnegative logical tick", ErrorContext{InstanceID: event.InstanceID, ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path})
		}
		if !event.Kind.Valid() {
			return NewError(CodeInvalidEvent, "unknown event kind", ErrorContext{InstanceID: event.InstanceID, ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path + ".kind"})
		}
		switch event.Kind {
		case EventRegister, EventDeregister, EventHeartbeat:
			if event.InstanceID == "" {
				return NewError(CodeInvalidEvent, "instance event requires an instance", ErrorContext{AtMS: &event.AtMS, Path: path + ".instance_id"})
			}
		case EventProbeResult:
			if event.InstanceID == "" || event.Success == nil {
				return NewError(CodeInvalidEvent, "probe result requires an instance and success flag", ErrorContext{InstanceID: event.InstanceID, AtMS: &event.AtMS, Path: path})
			}
		case EventQuery:
			if event.ServiceID == "" {
				return NewError(CodeInvalidEvent, "query requires a service", ErrorContext{AtMS: &event.AtMS, Path: path + ".service_id"})
			}
			if event.Limit < 0 {
				return NewError(CodeInvalidEvent, "query limit must not be negative", ErrorContext{ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path + ".limit"})
			}
			if event.WeightScale != "" && event.WeightScale.Denominator() == 0 {
				return NewError(CodeInvalidWeightScale, "unknown query display scale", ErrorContext{ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path + ".weight_scale"})
			}
		}
		if event.AtMS < lastMS {
			return NewError(CodeEventOrder, "events must be in nondecreasing logical order", ErrorContext{InstanceID: event.InstanceID, ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path})
		}
		lastMS = event.AtMS
		if event.InstanceID != "" {
			if _, exists := seenInstances[event.InstanceID]; !exists {
				return NewError(CodeUnknownInstance, "event references an unknown instance", ErrorContext{InstanceID: event.InstanceID, ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path})
			}
			if instance, ok := project.InstanceByID(event.InstanceID); ok {
				if event.ServiceID == "" {
					event.ServiceID = instance.ServiceID
				} else if event.ServiceID != instance.ServiceID {
					return NewError(CodeInvalidEvent, "event service does not match instance service", ErrorContext{InstanceID: event.InstanceID, ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path})
				}
			}
		}
		if event.ServiceID != "" {
			if _, exists := services[event.ServiceID]; !exists {
				return NewError(CodeUnknownService, "event references an unknown service", ErrorContext{InstanceID: event.InstanceID, ServiceID: event.ServiceID, AtMS: &event.AtMS, Path: path})
			}
		}
	}
	return nil
}

func validateBudgets(budgets Budgets) error {
	if budgets.GlobalConcurrency <= 0 || budgets.GlobalConcurrency > 4096 {
		return NewError(CodeInvalidBudget, "global concurrency must be in [1,4096]", ErrorContext{Path: "$.budgets"})
	}
	if budgets.NodeConcurrency <= 0 || budgets.NodeConcurrency > 4096 {
		return NewError(CodeInvalidBudget, "node concurrency must be in [1,4096]", ErrorContext{Path: "$.budgets"})
	}
	if budgets.RackConcurrency <= 0 || budgets.RackConcurrency > 4096 {
		return NewError(CodeInvalidBudget, "rack concurrency must be in [1,4096]", ErrorContext{Path: "$.budgets"})
	}
	if budgets.InstanceBuffer <= 0 || budgets.InstanceBuffer > 4096 {
		return NewError(CodeInvalidBudget, "instance buffer must be in [1,4096]", ErrorContext{Path: "$.budgets"})
	}
	if budgets.MaxDelayMS < 0 || budgets.MaxDelayMS > MaxDurationMS {
		return NewError(CodeInvalidBudget, "max delay must be in [0,seven days]", ErrorContext{Path: "$.budgets"})
	}
	return nil
}

func decodeTruth(raw json.RawMessage, path string) (*TruthReport, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var truth TruthReport
	if err := json.Unmarshal(raw, &truth); err != nil {
		return nil, WrapError(CodeSchemaField, "cannot decode embedded truth", ErrorContext{Path: path}, err)
	}
	return &truth, nil
}

func mustReencode(top map[string]json.RawMessage) []byte {
	data, err := json.Marshal(top)
	if err != nil {
		panic(fmt.Sprintf("re-encode JSON: %v", err))
	}
	return data
}
