package cachebench

const sampleV1TickMS int64 = 5

func EncodeSchemaDocs(p Project) (map[string][]byte, error) {
	v1, err := EncodeV1(p)
	if err != nil {
		return nil, err
	}
	v2, err := EncodeV2(p)
	if err != nil {
		return nil, err
	}
	v3, err := EncodeV3(p)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{SchemaV1: v1, SchemaV2: v2, SchemaV3: v3}, nil
}

func EncodeV1(p Project) ([]byte, error) {
	d := docV1{
		SchemaVersion: SchemaV1, ProjectID: p.ProjectID, Seed: p.Seed,
		TickDurationMS: sampleV1TickMS, DurationTicks: p.DurationMS / sampleV1TickMS,
		WindowTicks: p.WindowMS / sampleV1TickMS, WeightScale: "permille", CapacityScale: "permille",
		RoutingMode: p.RoutingPolicy.Mode, CriticalCases: p.CriticalCases,
	}
	for _, n := range p.Nodes {
		d.FlatNodes = append(d.FlatNodes, nodeV1{
			NodeID: n.ID, RackName: n.Rack, Priority: n.Priority, DeclarationOrder: n.DeclarationOrder,
			WeightPermille: n.WeightPermyriad / 10, MaxConnections: n.MaxInflight,
			ServiceTicks: n.ServiceMS / sampleV1TickMS, TimeoutTicks: n.TimeoutMS / sampleV1TickMS,
			ConstraintNo: n.ConstraintID, CapacityPermille: n.CapacityLimitPPM / 1000,
			InflightThreshold: n.InflightThreshold, RecoveryInflightThreshold: n.RecoveryInflightThreshold,
			ErrorRateLimitPermille:    n.ErrorRateLimitPPM / 1000,
			ErrorRateRecoveryPermille: n.ErrorRateRecoveryPPM / 1000,
			TriggerTicks:              n.TriggerMS / sampleV1TickMS, RecoveryTicks: n.RecoveryMS / sampleV1TickMS,
			MinErrorSamples: n.MinErrorSamples,
		})
	}
	for _, r := range p.Requests {
		d.Requests = append(d.Requests, requestV1{RequestID: r.ID, Ordinal: r.Ordinal, AtTick: r.AtMS / sampleV1TickMS, Candidates: r.Candidates, ForcedError: r.IsError})
	}
	for _, o := range p.Observations {
		d.Observations = append(d.Observations, obsV1{WindowIndex: o.WindowIndex, AtTick: o.AtMS / sampleV1TickMS, RequestCount: o.Requests, ErrorCount: o.Errors, TotalLatencyTicks: o.TotalLatencyMS / sampleV1TickMS})
	}
	return marshalDoc(d)
}

func EncodeV2(p Project) ([]byte, error) {
	d := docV2{
		SchemaVersion: SchemaV2, ProjectID: p.ProjectID, Seed: p.Seed,
		DurationMS: p.DurationMS, WindowMS: p.WindowMS,
		WeightScale: "permyriad", CapacityScale: "permyriad", RoutingMode: p.RoutingPolicy.Mode,
		CriticalCases: p.CriticalCases,
	}
	rackIndex := map[string]int{}
	for _, n := range p.Nodes {
		idx, ok := rackIndex[n.Rack]
		if !ok {
			idx = len(d.Topology.Racks)
			rackIndex[n.Rack] = idx
			d.Topology.Racks = append(d.Topology.Racks, rackV2{RackID: n.Rack})
		}
		d.Topology.Racks[idx].Nodes = append(d.Topology.Racks[idx].Nodes, nodeV2{
			NodeID: n.ID, Priority: n.Priority, DeclarationOrder: n.DeclarationOrder,
			WeightPermyriad: n.WeightPermyriad, MaxConnections: n.MaxInflight,
			ServiceMS: n.ServiceMS, TimeoutMS: n.TimeoutMS, ConstraintID: n.ConstraintID,
			CapacityPermyriad: n.CapacityLimitPPM / 100, InflightThreshold: n.InflightThreshold,
			RecoveryInflightThreshold: n.RecoveryInflightThreshold, ErrorRateLimitPPM: n.ErrorRateLimitPPM,
			ErrorRateRecoveryPPM: n.ErrorRateRecoveryPPM, TriggerMS: n.TriggerMS, RecoveryMS: n.RecoveryMS,
			MinErrorSamples: n.MinErrorSamples,
		})
	}
	for _, r := range p.Requests {
		d.Requests = append(d.Requests, requestV2{RequestID: r.ID, Ordinal: r.Ordinal, AtMS: r.AtMS, Candidates: r.Candidates, ForcedError: r.IsError})
	}
	for _, o := range p.Observations {
		ob := obsV2{WindowIndex: o.WindowIndex, WindowMS: p.WindowMS, RequestCount: o.Requests, ErrorCount: o.Errors, TotalLatencyMS: o.TotalLatencyMS}
		if o.OverloadMarkerPresent {
			v := o.Overloaded
			ob.Overloaded = &v
		}
		d.Observations = append(d.Observations, ob)
	}
	return marshalDoc(d)
}

func EncodeV3(p Project) ([]byte, error) {
	d := docV3{
		SchemaVersion: SchemaV3, ProjectID: p.ProjectID, Seed: p.Seed,
		DurationQ3Sec: p.DurationMS, WindowQ3Sec: p.WindowMS,
		WeightScale: "permyriad", CapacityScale: "permyriad", RoutingMode: p.RoutingPolicy.Mode,
		CriticalCases: p.CriticalCases,
	}
	rackIndex := map[string]int{}
	for _, n := range p.Nodes {
		idx, ok := rackIndex[n.Rack]
		if !ok {
			idx = len(d.Topology.RackTree)
			rackIndex[n.Rack] = idx
			d.Topology.RackTree = append(d.Topology.RackTree, rackV3{RackID: n.Rack})
		}
		d.Topology.RackTree[idx].Nodes = append(d.Topology.RackTree[idx].Nodes, nodeV3{
			NodeID: n.ID, Priority: n.Priority, DeclarationOrder: n.DeclarationOrder,
			WeightPermyriad: n.WeightPermyriad, MaxConnections: n.MaxInflight,
			ServiceQ3Seconds: n.ServiceMS, TimeoutMS: n.TimeoutMS, ConstraintID: n.ConstraintID,
			CapacityPermyriad: n.CapacityLimitPPM / 100, InflightThreshold: n.InflightThreshold,
			RecoveryInflightThreshold: n.RecoveryInflightThreshold, ErrorRateLimitPPM: n.ErrorRateLimitPPM,
			ErrorRateRecoveryPPM: n.ErrorRateRecoveryPPM, TriggerMS: n.TriggerMS, RecoveryMS: n.RecoveryMS,
			MinErrorSamples: n.MinErrorSamples,
		})
	}
	for _, r := range p.Requests {
		d.Requests = append(d.Requests, requestV3{RequestID: r.ID, Ordinal: r.Ordinal, AtQ3Seconds: r.AtMS, Candidates: r.Candidates, ForcedError: r.IsError})
	}
	for _, o := range p.Observations {
		ob := obsV3{WindowIndex: o.WindowIndex, AtQ3Seconds: o.AtMS, RequestCount: o.Requests, ErrorCount: o.Errors, TotalLatencyMS: o.TotalLatencyMS}
		if o.OverloadMarkerPresent {
			v := o.Overloaded
			ob.OverloadFlag = &v
		}
		d.Observations = append(d.Observations, ob)
	}
	return marshalDoc(d)
}
