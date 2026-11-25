package schemaio

import (
	"fmt"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/model"
)

func ValidateCanonical(p *model.Project) error {
	if p.ID == "" {
		return bizerr.New(bizerr.CodeSchemaField, "project_id", "project id is empty")
	}
	if p.Revision == "" {
		return bizerr.New(bizerr.CodeSchemaField, "revision", "project revision is empty")
	}
	if p.Window.StartMS > p.Window.EndMS {
		return bizerr.New(bizerr.CodeTimeOrder, "window", "window start is after end").
			With("start_ms", p.Window.StartMS).With("end_ms", p.Window.EndMS)
	}
	if p.Policy.DedupToleranceMS < 0 || p.Policy.MergeGapMS < 0 || p.Policy.LookbackMS < 0 {
		return bizerr.New(bizerr.CodeInvalidInput, "policy", "millisecond policies must be non-negative").
			With("dedup_tolerance_ms", p.Policy.DedupToleranceMS).
			With("merge_gap_ms", p.Policy.MergeGapMS).
			With("lookback_ms", p.Policy.LookbackMS)
	}
	if p.Policy.PropagationGen < 0 {
		return bizerr.New(bizerr.CodeInvalidInput, "policy.propagation_generation", "generation limit must be non-negative")
	}
	known := map[string]bool{}
	for i, svc := range p.Services {
		path := fmt.Sprintf("services[%d]", i)
		if svc.ID == "" {
			return bizerr.New(bizerr.CodeSchemaField, path+".id", "service id is empty")
		}
		if known[svc.ID] {
			return bizerr.New(bizerr.CodeSchemaField, path+".id", "duplicate service id").With("service", svc.ID)
		}
		known[svc.ID] = true
		if svc.ErrorThresholdBP < 0 {
			return bizerr.New(bizerr.CodeThresholdNegative, path+".error_threshold", "error threshold must not be negative").
				With("service", svc.ID).With("value", svc.ErrorThresholdBP)
		}
		if svc.SaturationThresholdBP < 0 {
			return bizerr.New(bizerr.CodeThresholdNegative, path+".saturation_threshold", "saturation threshold must not be negative").
				With("service", svc.ID).With("value", svc.SaturationThresholdBP)
		}
	}
	seqSeen := map[int]string{}
	idSeen := map[string]bool{}
	for i, ev := range p.Events {
		path := fmt.Sprintf("events[%d]", i)
		if ev.ID == "" {
			return bizerr.New(bizerr.CodeSchemaField, path+".id", "event id is empty")
		}
		if idSeen[ev.ID] {
			return bizerr.New(bizerr.CodeSchemaField, path+".id", "duplicate event id").With("event", ev.ID)
		}
		idSeen[ev.ID] = true
		if ev.Seq <= 0 {
			return bizerr.New(bizerr.CodeSchemaField, path+".seq", "logical sequence must be positive").With("event", ev.ID).With("seq", ev.Seq)
		}
		if prior, ok := seqSeen[ev.Seq]; ok {
			return bizerr.New(bizerr.CodeSchemaField, path+".seq", "duplicate logical sequence").
				With("event", ev.ID).With("seq", ev.Seq).With("prior_event", prior)
		}
		seqSeen[ev.Seq] = ev.ID
		if ev.ServiceID == "" || !known[ev.ServiceID] {
			return bizerr.New(bizerr.CodeSchemaField, path+".service_id", "event references unknown service").
				With("event", ev.ID).With("service", ev.ServiceID)
		}
		if ev.OccurredMS < p.Window.StartMS || ev.OccurredMS > p.Window.EndMS {
			return bizerr.New(bizerr.CodeTimeOrder, path+".occurred_ms", "event time lies outside the declared window").
				With("event", ev.ID).With("occurred_ms", ev.OccurredMS).
				With("window_start_ms", p.Window.StartMS).With("window_end_ms", p.Window.EndMS)
		}
		if ev.TypeWeightBP < 0 || ev.TypeWeightBP > 10000 {
			return bizerr.New(bizerr.CodeScaleOutOfRange, path+".type_weight", "weight must be in [0,10000] basis points").
				With("event", ev.ID).With("value", ev.TypeWeightBP)
		}
	}
	return nil
}

// CheckMonotonicLogicalStream enforces non-decreasing occurrence time in
// logical submission order. It is used by explicit time-reversal injection.
func CheckMonotonicLogicalStream(p *model.Project) error {
	events := p.SortedEvents()
	for i := 1; i < len(events); i++ {
		if events[i].OccurredMS < events[i-1].OccurredMS {
			return bizerr.New(bizerr.CodeTimeOrder, "events."+events[i].ID+".occurred_ms", "event time is earlier than preceding logical event").
				With("event", events[i].ID).
				With("previous_event", events[i-1].ID).
				With("occurred_ms", events[i].OccurredMS).
				With("previous_ms", events[i-1].OccurredMS)
		}
	}
	return nil
}
