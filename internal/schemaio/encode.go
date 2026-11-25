package schemaio

import (
	"encoding/json"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/fixedpoint"
	"sre-timeline/internal/model"
)

func Encode(p *model.Project, version string) ([]byte, error) {
	var doc any
	var err error
	switch version {
	case model.SchemaV1:
		doc = toV1(p)
	case model.SchemaV2:
		doc, err = toV2(p)
	case model.SchemaV3:
		doc = toV3(p)
	default:
		return nil, bizerr.New(bizerr.CodeSchemaVersion, "version", "unsupported schema version %q", version)
	}
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, bizerr.Wrap(bizerr.CodeSchemaField, "$", err, "encode schema failed")
	}
	return data, nil
}

func toV1(p *model.Project) V1Document {
	doc := V1Document{
		SchemaVersion: model.SchemaV1, ProjectID: p.ID, Revision: p.Revision, Seed: p.Seed,
		Window: V1Window{StartEpochMS: p.Window.StartMS, EndEpochMS: p.Window.EndMS, TickIntervalMS: p.Window.TickMS},
		Policy: V1Policy{
			DedupWindowSeconds:       p.Policy.DedupToleranceMS / 1000,
			MergeGapSeconds:          p.Policy.MergeGapMS / 1000,
			LookbackSeconds:          p.Policy.LookbackMS / 1000,
			MaxPropagationGeneration: p.Policy.PropagationGen,
			ScoreScale:               10000, ScoreToleranceRaw: p.Policy.ScoreTolerance,
		},
	}
	for _, svc := range p.Services {
		doc.Services = append(doc.Services, V1Service{
			ServiceID: svc.ID, DisplayName: svc.Name,
			ErrorUnhealthyThresholdBP:      svc.ErrorThresholdBP,
			SaturationUnhealthyThresholdBP: svc.SaturationThresholdBP,
			SteadyErrorBaselineBP:          svc.BaselineErrorBP,
			SteadySaturationBaselineBP:     svc.BaselineSaturationBP,
		})
	}
	for _, edge := range p.EdgesSorted() {
		doc.Dependencies = append(doc.Dependencies, V1Edge{UpstreamCallerID: edge.Caller, DownstreamDependencyID: edge.Callee})
	}
	for _, ev := range p.SortedEvents() {
		doc.Events = append(doc.Events, V1Event{
			EventID: ev.ID, LogicalSequence: ev.Seq, SourceKind: ev.Source, ServiceID: ev.ServiceID,
			EventKind: ev.Kind, DedupFingerprint: ev.Fingerprint, OccurredAtEpochMS: ev.OccurredMS,
			IngestLagSeconds: ev.IngestLagMS / 1000, AliveTTLSeconds: ev.AliveMS / 1000,
			ChangeType: ev.ChangeType, ChangeWeightPermyriad: ev.TypeWeightBP,
			ErrorRateBP: ev.ErrorRateBP, SaturationBP: ev.SaturationBP, RecoveryFor: ev.RecoveryFor,
		})
	}
	if p.Truth != nil {
		truth := &V1Truth{}
		for _, ev := range p.Truth.AlignedEvents {
			truth.AlignedEvents = append(truth.AlignedEvents, V1TruthEvent{
				EventID: ev.ID, KeptID: ev.KeptID, EffectiveMS: ev.EffectiveMS, ServiceID: ev.ServiceID,
				DedupedCount: ev.DedupedCount, MergedRaw: ev.MergedRawCount, MergedIDs: ev.MergedIDs,
			})
		}
		for _, iv := range p.Truth.Intervals {
			truth.Intervals = append(truth.Intervals, V1TruthInterval{
				Ordinal: iv.Ordinal, ServiceID: iv.ServiceID, StartMS: iv.StartMS, EndMS: iv.EndMS,
				EvidenceIDs: append([]string{}, iv.EvidenceIDs...), ThresholdBP: iv.ThresholdBP, Metric: iv.Metric,
			})
		}
		for _, c := range p.Truth.Candidates {
			truth.Candidates = append(truth.Candidates, V1TruthCandidate{
				EventID: c.EventID, Rank: c.Rank,
				ScorePermyriad: (c.ScoreRaw*10000 + fixedpoint.Scale/2) / fixedpoint.Scale,
			})
		}
		doc.Truth = truth
	}
	return doc
}

func toV2(p *model.Project) (V2Document, error) {
	tick := p.Window.TickMS
	if tick <= 0 {
		tick = 5000
	}
	if (p.Window.EndMS-p.Window.StartMS)%tick != 0 {
		return V2Document{}, bizerr.New(bizerr.CodeScaleOutOfRange, "window.tickIntervalMs", "window is not divisible by tick")
	}
	doc := V2Document{
		SchemaVersion: model.SchemaV2, ProjectID: p.ID, Revision: p.Revision, Seed: p.Seed,
		Window: V2Window{OriginEpochMS: p.Window.StartMS, StartTick: 0, EndTick: (p.Window.EndMS - p.Window.StartMS) / tick, TickIntervalMS: tick},
		Policy: V2Policy{
			DedupToleranceMS: p.Policy.DedupToleranceMS, MergeGapMS: p.Policy.MergeGapMS,
			LookbackMS: p.Policy.LookbackMS, MaxPropagationGeneration: p.Policy.PropagationGen,
			ScoreScale: 1000, ScoreToleranceRaw: p.Policy.ScoreTolerance,
		},
	}
	for _, svc := range p.Services {
		doc.Services = append(doc.Services, V2Service{
			ServiceID: svc.ID, DisplayName: svc.Name,
			ErrorThresholdPermille:      svc.ErrorThresholdBP / 10,
			SaturationThresholdPermille: svc.SaturationThresholdBP / 10,
			BaselineErrorPermille:       svc.BaselineErrorBP / 10,
			BaselineSaturationPermille:  svc.BaselineSaturationBP / 10,
		})
	}
	tree, err := buildV2Tree(p)
	if err != nil {
		return V2Document{}, err
	}
	doc.DependencyTree = tree
	for _, ev := range p.SortedEvents() {
		if (ev.OccurredMS-p.Window.StartMS)%tick != 0 {
			return V2Document{}, bizerr.New(bizerr.CodeScaleOutOfRange, "events."+ev.ID+".atTick", "event is not aligned to declared tick")
		}
		v2ev := V2Event{
			EventID: ev.ID, LogicalSequence: ev.Seq, SourceKind: ev.Source, ServiceID: ev.ServiceID,
			EventKind: ev.Kind, DedupFingerprint: ev.Fingerprint,
			AtTick:      (ev.OccurredMS - p.Window.StartMS) / tick,
			IngestLagMS: ev.IngestLagMS, AliveTTLMS: ev.AliveMS, ChangeType: ev.ChangeType,
			ChangeWeightPermille: ev.TypeWeightBP / 10, RecoveryFor: ev.RecoveryFor,
		}
		if ev.ErrorRateBP != nil {
			v := *ev.ErrorRateBP / 10
			v2ev.ErrorRatePermille = &v
		}
		if ev.SaturationBP != nil {
			v := *ev.SaturationBP / 10
			v2ev.SaturationPermille = &v
		}
		doc.Events = append(doc.Events, v2ev)
	}
	return doc, nil
}

func buildV2Tree(p *model.Project) ([]V2Node, error) {
	children := map[string][]string{}
	incoming := map[string]bool{}
	for _, edge := range p.EdgesSorted() {
		children[edge.Caller] = append(children[edge.Caller], edge.Callee)
		incoming[edge.Callee] = true
	}
	var roots []string
	for _, svc := range p.ServiceIDs() {
		if !incoming[svc] {
			roots = append(roots, svc)
		}
	}
	var build func(id string, seen map[string]bool) V2Node
	build = func(id string, pathSeen map[string]bool) V2Node {
		node := V2Node{ServiceID: id}
		if pathSeen[id] {
			return node
		}
		nextSeen := make(map[string]bool, len(pathSeen)+1)
		for k, v := range pathSeen {
			nextSeen[k] = v
		}
		nextSeen[id] = true
		for _, child := range children[id] {
			node.Calls = append(node.Calls, build(child, nextSeen))
		}
		return node
	}
	out := make([]V2Node, 0, len(roots))
	for _, root := range roots {
		out = append(out, build(root, map[string]bool{}))
	}
	return out, nil
}

func toV3(p *model.Project) V3Document {
	doc := V3Document{
		Schema: model.SchemaV3, ProjectCode: p.ID, RevisionID: p.Revision, Seed: p.Seed,
		TimeWindow: V3Window{OriginEpochMS: p.Window.StartMS, StartRelMS: 0, EndRelMS: p.Window.EndMS - p.Window.StartMS},
		AnalysisConfig: V3Config{
			DedupToleranceMS: p.Policy.DedupToleranceMS, MergeGapMS: p.Policy.MergeGapMS,
			LookbackMS: p.Policy.LookbackMS, PropagationGenerations: p.Policy.PropagationGen,
			ScoreScale: 10000, ScoreToleranceRaw: p.Policy.ScoreTolerance,
		},
	}
	for _, svc := range p.Services {
		doc.ManagedServices = append(doc.ManagedServices, V3Service{
			ID: svc.ID, Label: svc.Name, ErrorThresholdPermyriad: svc.ErrorThresholdBP,
			SaturationThresholdPermyriad: svc.SaturationThresholdBP,
			BaselineErrorPermyriad:       svc.BaselineErrorBP, BaselineSaturationPermyriad: svc.BaselineSaturationBP,
		})
	}
	for _, edge := range p.EdgesSorted() {
		doc.TopologyEdges = append(doc.TopologyEdges, V3Edge{Caller: edge.Caller, Dependency: edge.Callee})
	}
	for _, ev := range p.SortedEvents() {
		doc.TimelineEvents = append(doc.TimelineEvents, V3Event{
			ID: ev.ID, Seq: ev.Seq, Source: ev.Source, Service: ev.ServiceID, Kind: ev.Kind,
			Fingerprint: ev.Fingerprint, AtRelMS: ev.OccurredMS - p.Window.StartMS,
			LagMS: ev.IngestLagMS, TTLSeconds: ev.AliveMS / 1000, ChangeType: ev.ChangeType,
			WeightPermyriad: ev.TypeWeightBP, ErrorRatePermyriad: ev.ErrorRateBP,
			SaturationPermyriad: ev.SaturationBP, RecoveryFor: ev.RecoveryFor,
		})
	}
	return doc
}
