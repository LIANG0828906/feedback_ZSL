package schemaio

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/fixedpoint"
	"sre-timeline/internal/model"
	"sre-timeline/internal/topology"
)

func DetectVersion(data []byte) (string, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(data, &head); err != nil {
		return "", bizerr.Wrap(bizerr.CodeSchemaField, "$", err, "invalid JSON document")
	}
	if raw, ok := head["schema_version"]; ok && jsonString(raw) == model.SchemaV1 {
		return model.SchemaV1, nil
	}
	if raw, ok := head["schemaVersion"]; ok && jsonString(raw) == model.SchemaV2 {
		return model.SchemaV2, nil
	}
	if raw, ok := head["schema"]; ok && jsonString(raw) == model.SchemaV3 {
		return model.SchemaV3, nil
	}
	return "", bizerr.New(bizerr.CodeSchemaVersion, "$", "unrecognized schema/version field combination").
		With("keys", mapKeys(head))
}

func jsonString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func Load(data []byte) (*model.Project, error) {
	version, err := DetectVersion(data)
	if err != nil {
		return nil, err
	}
	switch version {
	case model.SchemaV1:
		var doc V1Document
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return fromV1(&doc)
	case model.SchemaV2:
		var doc V2Document
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return fromV2(&doc)
	case model.SchemaV3:
		var doc V3Document
		if err := decodeStrict(data, &doc); err != nil {
			return nil, err
		}
		return fromV3(&doc)
	default:
		return nil, bizerr.New(bizerr.CodeSchemaVersion, "$", "unsupported schema version %q", version)
	}
}

var unknownFieldRE = regexp.MustCompile(`unknown field "([^"]+)"`)

func decodeStrict(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return bizerr.Wrap(bizerr.CodeSchemaField, typeErr.Field, err, "field has invalid type")
		}
		match := unknownFieldRE.FindStringSubmatch(err.Error())
		if len(match) == 2 {
			return bizerr.Wrap(bizerr.CodeSchemaUnknownField, "$."+match[1], err, "field is not declared by this schema version").
				With("field", match[1])
		}
		return bizerr.Wrap(bizerr.CodeSchemaField, "$", err, "schema decode failed")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return bizerr.New(bizerr.CodeSchemaField, "$", "document contains unexpected trailing JSON value")
		}
		return bizerr.Wrap(bizerr.CodeSchemaField, "$", err, "schema decode failed after document")
	}
	return nil
}

func fromV1(doc *V1Document) (*model.Project, error) {
	if doc.Policy.ScoreScale != 10000 {
		return nil, bizerr.New(bizerr.CodeScaleOutOfRange, "policy.score_scale", "v1 score scale must be 10000 (permyriad)").With("value", doc.Policy.ScoreScale)
	}
	p := &model.Project{
		ID:       doc.ProjectID,
		Revision: doc.Revision,
		Seed:     doc.Seed,
		Schema:   model.SchemaV1,
		Window: model.Window{
			StartMS: doc.Window.StartEpochMS,
			EndMS:   doc.Window.EndEpochMS,
			TickMS:  doc.Window.TickIntervalMS,
		},
		Policy: model.Policy{
			DedupToleranceMS: doc.Policy.DedupWindowSeconds * 1000,
			MergeGapMS:       doc.Policy.MergeGapSeconds * 1000,
			LookbackMS:       doc.Policy.LookbackSeconds * 1000,
			PropagationGen:   doc.Policy.MaxPropagationGeneration,
			FixedScale:       fixedpoint.Scale,
			FixedWidthBits:   fixedpoint.WidthBits,
			ScoreTolerance:   doc.Policy.ScoreToleranceRaw,
		},
	}
	for _, svc := range doc.Services {
		p.Services = append(p.Services, model.Service{
			ID: svc.ServiceID, Name: svc.DisplayName,
			ErrorThresholdBP: svc.ErrorUnhealthyThresholdBP, SaturationThresholdBP: svc.SaturationUnhealthyThresholdBP,
			BaselineErrorBP: svc.SteadyErrorBaselineBP, BaselineSaturationBP: svc.SteadySaturationBaselineBP,
		})
	}
	for _, edge := range doc.Dependencies {
		p.Edges = append(p.Edges, model.Edge{Caller: edge.UpstreamCallerID, Callee: edge.DownstreamDependencyID})
	}
	for _, ev := range doc.Events {
		p.Events = append(p.Events, model.Event{
			ID: ev.EventID, Seq: ev.LogicalSequence, Source: ev.SourceKind, ServiceID: ev.ServiceID,
			Kind: ev.EventKind, Fingerprint: ev.DedupFingerprint, OccurredMS: ev.OccurredAtEpochMS,
			IngestLagMS: ev.IngestLagSeconds * 1000, AliveMS: ev.AliveTTLSeconds * 1000,
			ChangeType: ev.ChangeType, TypeWeightBP: ev.ChangeWeightPermyriad,
			ErrorRateBP: ev.ErrorRateBP, SaturationBP: ev.SaturationBP, RecoveryFor: ev.RecoveryFor,
		})
	}
	if doc.Truth != nil {
		p.Truth = &model.ExpectedTruth{}
		for _, ev := range doc.Truth.AlignedEvents {
			p.Truth.AlignedEvents = append(p.Truth.AlignedEvents, model.TruthEvent{
				ID: ev.EventID, KeptID: ev.KeptID, EffectiveMS: ev.EffectiveMS, ServiceID: ev.ServiceID,
				DedupedCount: ev.DedupedCount, MergedRawCount: ev.MergedRaw, MergedIDs: ev.MergedIDs,
			})
		}
		for _, iv := range doc.Truth.Intervals {
			p.Truth.Intervals = append(p.Truth.Intervals, model.TruthInterval{
				Ordinal: iv.Ordinal, ServiceID: iv.ServiceID, StartMS: iv.StartMS, EndMS: iv.EndMS,
				EvidenceIDs: iv.EvidenceIDs, ThresholdBP: iv.ThresholdBP, Metric: iv.Metric,
			})
		}
		for _, c := range doc.Truth.Candidates {
			p.Truth.Candidates = append(p.Truth.Candidates, model.TruthCandidate{
				EventID: c.EventID, Rank: c.Rank, ScoreRaw: c.ScorePermyriad * fixedpoint.Scale / 10000,
			})
		}
	}
	return finalizeLoad(p)
}

func fromV2(doc *V2Document) (*model.Project, error) {
	if doc.Window.TickIntervalMS <= 0 {
		return nil, bizerr.New(bizerr.CodeScaleOutOfRange, "window.tickIntervalMs", "tick interval must be positive").With("value", doc.Window.TickIntervalMS)
	}
	if doc.Policy.ScoreScale != 1000 {
		return nil, bizerr.New(bizerr.CodeScaleOutOfRange, "policy.scoreScale", "v2 score scale must be 1000 (permille)").With("value", doc.Policy.ScoreScale)
	}
	start := doc.Window.OriginEpochMS + doc.Window.StartTick*doc.Window.TickIntervalMS
	end := doc.Window.OriginEpochMS + doc.Window.EndTick*doc.Window.TickIntervalMS
	p := &model.Project{
		ID: doc.ProjectID, Revision: doc.Revision, Seed: doc.Seed, Schema: model.SchemaV2,
		Window: model.Window{StartMS: start, EndMS: end, TickMS: doc.Window.TickIntervalMS},
		Policy: model.Policy{
			DedupToleranceMS: doc.Policy.DedupToleranceMS, MergeGapMS: doc.Policy.MergeGapMS,
			LookbackMS: doc.Policy.LookbackMS, PropagationGen: doc.Policy.MaxPropagationGeneration,
			FixedScale: fixedpoint.Scale, FixedWidthBits: fixedpoint.WidthBits, ScoreTolerance: doc.Policy.ScoreToleranceRaw,
		},
	}
	for _, svc := range doc.Services {
		p.Services = append(p.Services, model.Service{
			ID: svc.ServiceID, Name: svc.DisplayName,
			ErrorThresholdBP:      svc.ErrorThresholdPermille * 10,
			SaturationThresholdBP: svc.SaturationThresholdPermille * 10,
			BaselineErrorBP:       svc.BaselineErrorPermille * 10,
			BaselineSaturationBP:  svc.BaselineSaturationPermille * 10,
		})
	}
	var walkNodes func(nodes []V2Node, parent string, path string) error
	walkNodes = func(nodes []V2Node, parent string, path string) error {
		for i, node := range nodes {
			nodePath := path + "[" + itoa(i) + "]"
			if node.ServiceID == "" {
				return bizerr.New(bizerr.CodeSchemaField, nodePath+".serviceId", "tree node service id is empty")
			}
			if parent != "" {
				p.Edges = append(p.Edges, model.Edge{Caller: parent, Callee: node.ServiceID})
			}
			if err := walkNodes(node.Calls, node.ServiceID, nodePath+".calls"); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walkNodes(doc.DependencyTree, "", "dependencyTree"); err != nil {
		return nil, err
	}
	tickSpan := doc.Window.EndTick - doc.Window.StartTick
	for _, ev := range doc.Events {
		if ev.AtTick < 0 || ev.AtTick > tickSpan {
			return nil, bizerr.New(bizerr.CodeScaleOutOfRange, "events."+ev.EventID+".atTick", "event tick is outside declared window ticks").
				With("event", ev.EventID).With("tick", ev.AtTick).With("span", tickSpan)
		}
		var errBP, satBP *int
		if ev.ErrorRatePermille != nil {
			v := *ev.ErrorRatePermille * 10
			errBP = &v
		}
		if ev.SaturationPermille != nil {
			v := *ev.SaturationPermille * 10
			satBP = &v
		}
		p.Events = append(p.Events, model.Event{
			ID: ev.EventID, Seq: ev.LogicalSequence, Source: ev.SourceKind, ServiceID: ev.ServiceID,
			Kind: ev.EventKind, Fingerprint: ev.DedupFingerprint,
			OccurredMS:  start + ev.AtTick*doc.Window.TickIntervalMS,
			IngestLagMS: ev.IngestLagMS, AliveMS: ev.AliveTTLMS,
			ChangeType: ev.ChangeType, TypeWeightBP: ev.ChangeWeightPermille * 10,
			ErrorRateBP: errBP, SaturationBP: satBP, RecoveryFor: ev.RecoveryFor,
		})
	}
	return finalizeLoad(p)
}

func fromV3(doc *V3Document) (*model.Project, error) {
	if doc.AnalysisConfig.ScoreScale != 10000 {
		return nil, bizerr.New(bizerr.CodeScaleOutOfRange, "analysis_config.score_scale", "v3 score scale must be 10000 (permyriad)").With("value", doc.AnalysisConfig.ScoreScale)
	}
	start := doc.TimeWindow.OriginEpochMS + doc.TimeWindow.StartRelMS
	end := doc.TimeWindow.OriginEpochMS + doc.TimeWindow.EndRelMS
	p := &model.Project{
		ID: doc.ProjectCode, Revision: doc.RevisionID, Seed: doc.Seed, Schema: model.SchemaV3,
		Window: model.Window{StartMS: start, EndMS: end},
		Policy: model.Policy{
			DedupToleranceMS: doc.AnalysisConfig.DedupToleranceMS, MergeGapMS: doc.AnalysisConfig.MergeGapMS,
			LookbackMS: doc.AnalysisConfig.LookbackMS, PropagationGen: doc.AnalysisConfig.PropagationGenerations,
			FixedScale: fixedpoint.Scale, FixedWidthBits: fixedpoint.WidthBits, ScoreTolerance: doc.AnalysisConfig.ScoreToleranceRaw,
		},
	}
	for _, svc := range doc.ManagedServices {
		p.Services = append(p.Services, model.Service{
			ID: svc.ID, Name: svc.Label, ErrorThresholdBP: svc.ErrorThresholdPermyriad,
			SaturationThresholdBP: svc.SaturationThresholdPermyriad, BaselineErrorBP: svc.BaselineErrorPermyriad,
			BaselineSaturationBP: svc.BaselineSaturationPermyriad,
		})
	}
	for _, edge := range doc.TopologyEdges {
		p.Edges = append(p.Edges, model.Edge{Caller: edge.Caller, Callee: edge.Dependency})
	}
	span := doc.TimeWindow.EndRelMS - doc.TimeWindow.StartRelMS
	for _, ev := range doc.TimelineEvents {
		if ev.AtRelMS < 0 || ev.AtRelMS > span {
			return nil, bizerr.New(bizerr.CodeTimeOrder, "timeline_events."+ev.ID+".at_rel_ms", "relative timestamp is outside window").
				With("event", ev.ID).With("at_rel_ms", ev.AtRelMS).With("span", span)
		}
		p.Events = append(p.Events, model.Event{
			ID: ev.ID, Seq: ev.Seq, Source: ev.Source, ServiceID: ev.Service, Kind: ev.Kind,
			Fingerprint: ev.Fingerprint, OccurredMS: start + ev.AtRelMS, IngestLagMS: ev.LagMS,
			AliveMS: ev.TTLSeconds * 1000, ChangeType: ev.ChangeType, TypeWeightBP: ev.WeightPermyriad,
			ErrorRateBP: ev.ErrorRatePermyriad, SaturationBP: ev.SaturationPermyriad, RecoveryFor: ev.RecoveryFor,
		})
	}
	return finalizeLoad(p)
}

func finalizeLoad(p *model.Project) (*model.Project, error) {
	if err := ValidateCanonical(p); err != nil {
		return nil, err
	}
	topo, err := topology.New(p.Services, p.Edges)
	if err != nil {
		return nil, err
	}
	if err := topo.ValidateAcyclic(); err != nil {
		return nil, err
	}
	return p, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
