package cachebench

import "encoding/json"

func LoadV1(data []byte) (*Project, error) {
	var d docV1
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, NewError(CodeSchemaField, "v1 JSON parse failed: %v", err)
	}
	if d.SchemaVersion != SchemaV1 {
		return nil, NewError(CodeSchemaVersion, "expected %s, got %q", SchemaV1, d.SchemaVersion)
	}
	tickMS := d.TickDurationMS
	if tickMS <= 0 {
		tickMS = 1
	}
	p := Project{
		SchemaVersion: SchemaV1, ProjectID: d.ProjectID, Seed: d.Seed, TickMS: 1,
		DurationMS: d.DurationTicks * tickMS, WindowMS: d.WindowTicks * tickMS,
		WeightSourceScale: ScalePermille, CapacitySourceScale: ScalePermille,
		RoutingPolicy: RoutingPolicy{Mode: d.RoutingMode, HashBucketScale: ScalePermyriad, ZeroWeightDeny: true},
		CriticalCases: d.CriticalCases,
	}
	for _, n := range d.FlatNodes {
		p.Nodes = append(p.Nodes, Node{
			ID: n.NodeID, Rack: n.RackName, Priority: n.Priority, DeclarationOrder: n.DeclarationOrder,
			WeightPermyriad: n.WeightPermille * 10, MaxInflight: n.MaxConnections,
			ServiceMS: n.ServiceTicks * tickMS, TimeoutMS: n.TimeoutTicks * tickMS,
			ConstraintID: n.ConstraintNo, CapacityLimitPPM: n.CapacityPermille * 1000,
			InflightThreshold: n.InflightThreshold, RecoveryInflightThreshold: n.RecoveryInflightThreshold,
			ErrorRateLimitPPM: n.ErrorRateLimitPermille * 1000, ErrorRateRecoveryPPM: n.ErrorRateRecoveryPermille * 1000,
			TriggerMS: n.TriggerTicks * tickMS, RecoveryMS: n.RecoveryTicks * tickMS,
			MinErrorSamples: n.MinErrorSamples,
		})
	}
	for _, r := range d.Requests {
		p.Requests = append(p.Requests, Request{ID: r.RequestID, Ordinal: r.Ordinal, AtMS: r.AtTick * tickMS, Candidates: r.Candidates, IsError: r.ForcedError})
	}
	for _, o := range d.Observations {
		p.Observations = append(p.Observations, Observation{
			WindowIndex: o.WindowIndex, AtMS: o.AtTick * tickMS, Requests: o.RequestCount, Errors: o.ErrorCount,
			TotalLatencyMS:  o.TotalLatencyTicks * tickMS,
			ErrorRateSource: o.ErrorCount * ScalePermille / maxInt64(o.RequestCount, 1),
		})
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return &p, nil
}

func LoadV2(data []byte) (*Project, error) {
	var d docV2
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, NewError(CodeSchemaField, "v2 JSON parse failed: %v", err)
	}
	if d.SchemaVersion != SchemaV2 {
		return nil, NewError(CodeSchemaVersion, "expected %s, got %q", SchemaV2, d.SchemaVersion)
	}
	p := Project{
		SchemaVersion: SchemaV2, ProjectID: d.ProjectID, Seed: d.Seed, TickMS: 1,
		DurationMS: d.DurationMS, WindowMS: d.WindowMS,
		WeightSourceScale: ScalePermyriad, CapacitySourceScale: ScalePermyriad,
		RoutingPolicy: RoutingPolicy{Mode: d.RoutingMode, HashBucketScale: ScalePermyriad, ZeroWeightDeny: true},
		CriticalCases: d.CriticalCases,
	}
	for _, rack := range d.Topology.Racks {
		for _, n := range rack.Nodes {
			p.Nodes = append(p.Nodes, Node{
				ID: n.NodeID, Rack: rack.RackID, Priority: n.Priority, DeclarationOrder: n.DeclarationOrder,
				WeightPermyriad: n.WeightPermyriad, MaxInflight: n.MaxConnections,
				ServiceMS: n.ServiceMS, TimeoutMS: n.TimeoutMS, ConstraintID: n.ConstraintID,
				CapacityLimitPPM: n.CapacityPermyriad * 100, InflightThreshold: n.InflightThreshold,
				RecoveryInflightThreshold: n.RecoveryInflightThreshold, ErrorRateLimitPPM: n.ErrorRateLimitPPM,
				ErrorRateRecoveryPPM: n.ErrorRateRecoveryPPM, TriggerMS: n.TriggerMS, RecoveryMS: n.RecoveryMS,
				MinErrorSamples: n.MinErrorSamples,
			})
		}
	}
	for _, r := range d.Requests {
		p.Requests = append(p.Requests, Request{ID: r.RequestID, Ordinal: r.Ordinal, AtMS: r.AtMS, Candidates: r.Candidates, IsError: r.ForcedError})
	}
	for _, o := range d.Observations {
		at := int64(o.WindowIndex) * o.WindowMS
		if o.WindowMS == 0 {
			at = int64(o.WindowIndex) * p.WindowMS
		}
		ob := Observation{WindowIndex: o.WindowIndex, AtMS: at, Requests: o.RequestCount, Errors: o.ErrorCount, TotalLatencyMS: o.TotalLatencyMS}
		if o.Overloaded != nil {
			ob.OverloadMarkerPresent = true
			ob.Overloaded = *o.Overloaded
		}
		p.Observations = append(p.Observations, ob)
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return &p, nil
}

func LoadV3(data []byte) (*Project, error) {
	var d docV3
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, NewError(CodeSchemaField, "v3 JSON parse failed: %v", err)
	}
	if d.SchemaVersion != SchemaV3 {
		return nil, NewError(CodeSchemaVersion, "expected %s, got %q", SchemaV3, d.SchemaVersion)
	}
	p := Project{
		SchemaVersion: SchemaV3, ProjectID: d.ProjectID, Seed: d.Seed, TickMS: 1,
		DurationMS: d.DurationQ3Sec, WindowMS: d.WindowQ3Sec,
		WeightSourceScale: ScalePermyriad, CapacitySourceScale: ScalePermyriad,
		RoutingPolicy: RoutingPolicy{Mode: d.RoutingMode, HashBucketScale: ScalePermyriad, ZeroWeightDeny: true},
		CriticalCases: d.CriticalCases,
	}
	for _, rack := range d.Topology.RackTree {
		for _, n := range rack.Nodes {
			p.Nodes = append(p.Nodes, Node{
				ID: n.NodeID, Rack: rack.RackID, Priority: n.Priority, DeclarationOrder: n.DeclarationOrder,
				WeightPermyriad: n.WeightPermyriad, MaxInflight: n.MaxConnections,
				ServiceMS: n.ServiceQ3Seconds, TimeoutMS: n.TimeoutMS, ConstraintID: n.ConstraintID,
				CapacityLimitPPM: n.CapacityPermyriad * 100, InflightThreshold: n.InflightThreshold,
				RecoveryInflightThreshold: n.RecoveryInflightThreshold, ErrorRateLimitPPM: n.ErrorRateLimitPPM,
				ErrorRateRecoveryPPM: n.ErrorRateRecoveryPPM, TriggerMS: n.TriggerMS, RecoveryMS: n.RecoveryMS,
				MinErrorSamples: n.MinErrorSamples,
			})
		}
	}
	for _, r := range d.Requests {
		p.Requests = append(p.Requests, Request{ID: r.RequestID, Ordinal: r.Ordinal, AtMS: r.AtQ3Seconds, Candidates: r.Candidates, IsError: r.ForcedError})
	}
	for _, o := range d.Observations {
		ob := Observation{WindowIndex: o.WindowIndex, AtMS: o.AtQ3Seconds, Requests: o.RequestCount, Errors: o.ErrorCount, TotalLatencyMS: o.TotalLatencyMS}
		if o.OverloadFlag != nil {
			ob.OverloadMarkerPresent = true
			ob.Overloaded = *o.OverloadFlag
		}
		p.Observations = append(p.Observations, ob)
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return &p, nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
