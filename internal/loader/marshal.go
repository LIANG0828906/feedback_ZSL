package loader

import (
	"encoding/json"
	"fmt"
	"os"

	"grayrelease/internal/model"
)

func MarshalProject(p *model.Project, version string) ([]byte, error) {
	var value any
	switch version {
	case model.SchemaV1:
		value = toV1(p)
	case model.SchemaV2:
		value = toV2(p)
	case model.SchemaV3:
		value = toV3(p)
	default:
		return nil, model.NewError(CodeSchemaUnsupported, "cannot marshal unsupported schema").With("schema", version)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, model.NewError("E_JSON_MARSHAL", "marshal project failed").With("cause", err.Error())
	}
	return data, nil
}

func toV1(p *model.Project) v1Project {
	out := v1Project{
		APIVersion: model.SchemaV1, ProjectRevision: p.EngineeringVersion, TrafficName: p.Name,
		TickDurationMS: p.TickIntervalMS,
	}
	for _, svc := range p.Services {
		out.Upstreams = append(out.Upstreams, v1Service{
			Name: svc.Name, Main: svc.Baseline, Gray: svc.Canary, Downstream: svc.Dependencies,
		})
	}
	for _, rule := range p.Rules {
		permille := rule.WeightBPS / 10
		order := rule.Order
		out.GrayRules = append(out.GrayRules, v1Rule{
			RuleID: rule.ID, RuleNo: rule.RuleNo, Service: rule.Service, Priority: rule.Priority,
			DeclarationOrder: &order, MatchLabels: rule.Match.Labels, WeightPermille: &permille,
		})
	}
	for _, mirror := range p.Mirrors {
		permille := mirror.RatioBPS / 10
		out.MirrorRules = append(out.MirrorRules, v1Mirror{
			Service: mirror.Service, RuleID: mirror.RuleID, MirrorPermille: &permille,
		})
	}
	return out
}

func toV2(p *model.Project) v2Project {
	out := v2Project{
		APIVersion: model.SchemaV2, EngineeringVersion: p.EngineeringVersion, Name: p.Name,
		TickIntervalMs: p.TickIntervalMS,
	}
	for _, svc := range p.Services {
		out.Services = append(out.Services, v2Service{
			Name: svc.Name, Baseline: svc.Baseline, Canary: svc.Canary, Dependencies: svc.Dependencies,
		})
	}
	groups := map[string]*v2RuleGroup{}
	for _, rule := range p.Rules {
		group := groups[rule.Service]
		if group == nil {
			out.RuleTree = append(out.RuleTree, v2RuleGroup{Service: rule.Service})
			group = &out.RuleTree[len(out.RuleTree)-1]
			groups[rule.Service] = group
		}
		weight := rule.WeightBPS
		order := rule.Order
		group.Rules = append(group.Rules, v2Rule{
			ID: rule.ID, RuleNo: rule.RuleNo, Priority: rule.Priority, DeclarationOrder: &order,
			Match: v2Match{Labels: rule.Match.Labels}, WeightBps: &weight,
		})
	}
	for _, mirror := range p.Mirrors {
		ratio := mirror.RatioBPS
		policy := v2MirrorPolicy{Service: mirror.Service, RuleID: mirror.RuleID, CopyRatioBps: &ratio}
		if mirror.Sample != nil && mirror.Sample.Present {
			modulus := mirror.Sample.Modulus
			policy.SampleModulus = &modulus
			policy.SampleRemainder = mirror.Sample.Remainder
		}
		out.MirrorPolicies = append(out.MirrorPolicies, policy)
	}
	return out
}

func toV3(p *model.Project) v3Project {
	out := v3Project{APIVersion: model.SchemaV3, Kind: "TrafficEngineering"}
	out.Metadata.Name = p.Name
	out.Metadata.Revision = p.EngineeringVersion
	out.Spec.Release.Revision = p.EngineeringVersion
	out.Spec.Timing.WindowMS = p.TickIntervalMS
	for _, svc := range p.Services {
		out.Spec.Topology.Nodes = append(out.Spec.Topology.Nodes, v3Node{
			Name: svc.Name, Baseline: svc.Baseline, Canary: svc.Canary, Calls: svc.Dependencies,
		})
	}
	groups := map[string]*v3RuleGroup{}
	for _, rule := range p.Rules {
		group := groups[rule.Service]
		if group == nil {
			out.Spec.RuleGroups = append(out.Spec.RuleGroups, v3RuleGroup{ServiceName: rule.Service})
			group = &out.Spec.RuleGroups[len(out.Spec.RuleGroups)-1]
			groups[rule.Service] = group
		}
		bps := rule.WeightBPS
		order := rule.Order
		v3rule := v3Rule{
			RuleID: rule.ID, RuleNumber: rule.RuleNo, Precedence: rule.Priority,
		}
		v3rule.Sequence = &order
		v3rule.When.Tags = rule.Match.Labels
		v3rule.Weight.BasisPoints = &bps
		group.Rules = append(group.Rules, v3rule)
	}
	for _, mirror := range p.Mirrors {
		bps := mirror.RatioBPS
		policy := v3Mirror{TargetService: mirror.Service, RuleName: mirror.RuleID}
		policy.Copy.BasisPoints = &bps
		if mirror.Sample != nil && mirror.Sample.Present {
			policy.Sampling = &struct {
				Modulus   uint64 `json:"modulus"`
				Remainder uint64 `json:"remainder"`
			}{Modulus: mirror.Sample.Modulus, Remainder: mirror.Sample.Remainder}
		}
		out.Spec.Mirroring.Policies = append(out.Spec.Mirroring.Policies, policy)
	}
	return out
}

type requestsEnvelope struct {
	APIVersion string            `json:"apiVersion"`
	APISnake   string            `json:"api_version"`
	Schema     string            `json:"schema"`
	Requests   []json.RawMessage `json:"requests"`
	Items      []json.RawMessage `json:"items"`
}

func LoadRequestsFile(path, schemaVersion string, tickIntervalMS int64) ([]model.Request, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return LoadRequests(data, schemaVersion, tickIntervalMS)
}

func LoadRequests(data []byte, schemaVersion string, tickIntervalMS int64) ([]model.Request, error) {
	var envelope requestsEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, model.NewError("E_JSON_PARSE", "request stream parse failed").With("cause", err.Error())
	}
	rawItems := envelope.Requests
	if len(rawItems) == 0 {
		rawItems = envelope.Items
	}
	if tickIntervalMS <= 0 {
		tickIntervalMS = 100
	}
	version := firstNonEmpty(schemaVersion, envelope.APIVersion, envelope.APISnake, envelope.Schema)
	requests := make([]model.Request, 0, len(rawItems))
	for i, raw := range rawItems {
		req, err := loadOneRequest(raw, version, tickIntervalMS, i)
		if err != nil {
			return nil, err
		}
		req.Sequence = i
		requests = append(requests, req)
	}
	return requests, nil
}

func loadOneRequest(raw json.RawMessage, version string, intervalMS int64, index int) (model.Request, error) {
	switch version {
	case model.SchemaV1:
		var row struct {
			Tick        int               `json:"tick"`
			AtTick      int               `json:"at_tick"`
			RequestID   string            `json:"request_id"`
			Service     string            `json:"service"`
			StableKey   string            `json:"stable_key"`
			Key         string            `json:"key"`
			Tags        map[string]string `json:"tags"`
			Labels      map[string]string `json:"labels"`
			ShadowFault string            `json:"shadow_fault"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return model.Request{}, parseReqErr(err, index)
		}
		tick := firstNonZeroInt(row.AtTick, row.Tick)
		if row.AtTick != 0 && row.Tick != 0 && row.AtTick != row.Tick {
			return model.Request{}, model.NewError(CodeFieldAmbiguous, "at_tick and tick disagree").
				With("index", index).With("at_tick", row.AtTick).With("tick", row.Tick)
		}
		key := firstNonEmpty(row.StableKey, row.Key)
		return model.Request{Tick: tick, RequestID: row.RequestID, Service: row.Service, Key: key,
			Labels: firstNonNilMap(row.Labels, row.Tags), ShadowFault: row.ShadowFault}, validateRequest(tick, row.RequestID, row.Service, key, index)
	case model.SchemaV2:
		var row struct {
			Tick        int               `json:"tick"`
			TimestampMS int64             `json:"timestampMs"`
			RequestID   string            `json:"requestId"`
			Service     string            `json:"service"`
			StableKey   string            `json:"stableKey"`
			Key         string            `json:"key"`
			Labels      map[string]string `json:"labels"`
			ShadowFault string            `json:"shadowFault"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return model.Request{}, parseReqErr(err, index)
		}
		tick := row.Tick
		if row.TimestampMS > 0 {
			tick = int(row.TimestampMS / intervalMS)
		}
		if row.Tick != 0 && row.TimestampMS > 0 && int(row.TimestampMS/intervalMS) != row.Tick {
			return model.Request{}, model.NewError(CodeFieldAmbiguous, "tick and timestampMs normalize to different logical moments").
				With("index", index).With("tick", row.Tick).With("timestamp_ms", row.TimestampMS).With("tick_interval_ms", intervalMS)
		}
		key := firstNonEmpty(row.StableKey, row.Key)
		return model.Request{Tick: tick, RequestID: row.RequestID, Service: row.Service, Key: key,
			Labels: row.Labels, ShadowFault: row.ShadowFault}, validateRequest(tick, row.RequestID, row.Service, key, index)
	case model.SchemaV3:
		var row struct {
			Tick       int `json:"tick"`
			OccurredAt struct {
				TimeMS int64 `json:"timeMs"`
			} `json:"occurredAt"`
			RequestID string            `json:"requestID"`
			Service   string            `json:"serviceName"`
			Key       string            `json:"stableKey"`
			Tags      map[string]string `json:"tags"`
			Fault     string            `json:"shadowFault"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return model.Request{}, parseReqErr(err, index)
		}
		tick := row.Tick
		if row.OccurredAt.TimeMS > 0 {
			tick = int(row.OccurredAt.TimeMS / intervalMS)
		}
		if row.Tick != 0 && row.OccurredAt.TimeMS > 0 && int(row.OccurredAt.TimeMS/intervalMS) != row.Tick {
			return model.Request{}, model.NewError(CodeFieldAmbiguous, "tick and occurredAt.timeMs normalize to different logical moments").
				With("index", index).With("tick", row.Tick).With("time_ms", row.OccurredAt.TimeMS).With("tick_interval_ms", intervalMS)
		}
		return model.Request{Tick: tick, RequestID: row.RequestID, Service: row.Service, Key: row.Key,
			Labels: row.Tags, ShadowFault: row.Fault}, validateRequest(tick, row.RequestID, row.Service, row.Key, index)
	default:
		return model.Request{}, model.NewError(CodeSchemaUnsupported, "unsupported request schema").With("schema", version)
	}
}

func MarshalRequests(requests []model.Request, version string, intervalMS int64) ([]byte, error) {
	if intervalMS <= 0 {
		intervalMS = 100
	}
	type envelope struct {
		APIVersion string `json:"apiVersion,omitempty"`
		APISnake   string `json:"api_version,omitempty"`
		Items      any    `json:"items,omitempty"`
		Requests   any    `json:"requests,omitempty"`
	}
	var env envelope
	switch version {
	case model.SchemaV1:
		env = envelope{APISnake: version, Requests: toV1Requests(requests)}
	case model.SchemaV2:
		env = envelope{APIVersion: version, Requests: toV2Requests(requests, intervalMS)}
	case model.SchemaV3:
		env = envelope{APIVersion: version, Items: toV3Requests(requests, intervalMS)}
	default:
		return nil, model.NewError(CodeSchemaUnsupported, "cannot marshal requests").With("schema", version)
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, model.NewError("E_JSON_MARSHAL", "marshal requests failed").With("cause", err.Error())
	}
	return data, nil
}

func toV1Requests(requests []model.Request) any {
	type row struct {
		AtTick      int               `json:"at_tick"`
		RequestID   string            `json:"request_id"`
		Service     string            `json:"service"`
		StableKey   string            `json:"stable_key"`
		Tags        map[string]string `json:"tags,omitempty"`
		ShadowFault string            `json:"shadow_fault,omitempty"`
	}
	rows := make([]row, 0, len(requests))
	for _, req := range requests {
		rows = append(rows, row{AtTick: req.Tick, RequestID: req.RequestID, Service: req.Service,
			StableKey: req.Key, Tags: req.Labels, ShadowFault: req.ShadowFault})
	}
	return rows
}

func toV2Requests(requests []model.Request, intervalMS int64) any {
	type row struct {
		TimestampMS int64             `json:"timestampMs"`
		RequestID   string            `json:"requestId"`
		Service     string            `json:"service"`
		StableKey   string            `json:"stableKey"`
		Labels      map[string]string `json:"labels,omitempty"`
		ShadowFault string            `json:"shadowFault,omitempty"`
	}
	rows := make([]row, 0, len(requests))
	for _, req := range requests {
		rows = append(rows, row{TimestampMS: int64(req.Tick) * intervalMS, RequestID: req.RequestID,
			Service: req.Service, StableKey: req.Key, Labels: req.Labels, ShadowFault: req.ShadowFault})
	}
	return rows
}

func toV3Requests(requests []model.Request, intervalMS int64) any {
	type occurred struct {
		TimeMS int64 `json:"timeMs"`
	}
	type row struct {
		OccurredAt  occurred          `json:"occurredAt"`
		RequestID   string            `json:"requestID"`
		ServiceName string            `json:"serviceName"`
		StableKey   string            `json:"stableKey"`
		Tags        map[string]string `json:"tags,omitempty"`
		ShadowFault string            `json:"shadowFault,omitempty"`
	}
	rows := make([]row, 0, len(requests))
	for _, req := range requests {
		rows = append(rows, row{OccurredAt: occurred{TimeMS: int64(req.Tick) * intervalMS},
			RequestID: req.RequestID, ServiceName: req.Service, StableKey: req.Key,
			Tags: req.Labels, ShadowFault: req.ShadowFault})
	}
	return rows
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func parseReqErr(err error, index int) error {
	return model.NewError("E_JSON_PARSE", "request JSON parse failed").With("index", index).With("cause", err.Error())
}

func validateRequest(tick int, id, service, key string, index int) error {
	if tick < 0 {
		return model.NewError(CodeTimeInvalid, "request tick must be non-negative").With("index", index).With("tick", tick)
	}
	if id == "" {
		return model.NewError(CodeFieldMissing, "request id is required").With("index", index)
	}
	if service == "" {
		return model.NewError(CodeServiceMissing, "request service is required").With("request_id", id)
	}
	if key == "" {
		return model.NewError(CodeFieldMissing, "stable key is required").With("request_id", id)
	}
	return nil
}

func firstNonZeroInt(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func firstNonNilMap(maps ...map[string]string) map[string]string {
	for _, value := range maps {
		if value != nil {
			return value
		}
	}
	return nil
}

var _ = fmt.Sprintf
