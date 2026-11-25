package sample

import (
	"sre-timeline/internal/intervals"
	"sre-timeline/internal/model"
	"sre-timeline/internal/scoring"
	"sre-timeline/internal/timeline"
	"sre-timeline/internal/topology"
)

// BuildTruth computes the checked-in deterministic oracle values using the
// same pure domain kernels but without cache/incremental/concurrency machinery.
func BuildTruth(p *model.Project) (*model.ExpectedTruth, error) {
	topo, err := topology.New(p.Services, p.Edges)
	if err != nil {
		return nil, err
	}
	if err := topo.ValidateAcyclic(); err != nil {
		return nil, err
	}
	aligned, err := timeline.Align(p)
	if err != nil {
		return nil, err
	}
	results, err := intervals.Derive(p, aligned)
	if err != nil {
		return nil, err
	}
	targets := scoring.NumberTargets(results)
	keptEvents := keptModelEvents(p, aligned)
	scoreReport, err := scoring.Score(p, topo, targets, keptEvents)
	if err != nil {
		return nil, err
	}

	truth := &model.ExpectedTruth{}
	for _, item := range aligned.All {
		keptID := item.EventID
		if !item.Kept {
			keptID = item.KeptAs
		}
		truth.AlignedEvents = append(truth.AlignedEvents, model.TruthEvent{
			ID:             item.EventID,
			KeptID:         keptID,
			EffectiveMS:    item.EffectiveMS,
			ServiceID:      item.ServiceID,
			DedupedCount:   item.DuplicatesRemoved,
			MergedRawCount: item.MergedRawCount,
			MergedIDs:      append([]string{}, item.MergedIDs...),
		})
	}
	for _, target := range targets {
		iv := target.Data
		truth.Intervals = append(truth.Intervals, model.TruthInterval{
			Ordinal:     target.Ordinal,
			ServiceID:   iv.ServiceID,
			StartMS:     iv.StartMS,
			EndMS:       iv.EndMS,
			EvidenceIDs: append([]string{}, iv.EvidenceIDs...),
			ThresholdBP: iv.ThresholdBP,
			Metric:      iv.Metric,
		})
	}
	for _, candidate := range scoreReport.Candidates {
		truth.Candidates = append(truth.Candidates, model.TruthCandidate{
			EventID:  candidate.EventID,
			ScoreRaw: candidate.TotalRaw,
			Rank:     candidate.Rank,
		})
	}
	p.Truth = truth
	return truth, nil
}

func keptModelEvents(p *model.Project, report *timeline.Report) []model.Event {
	byID := map[string]model.Event{}
	for _, ev := range p.Events {
		byID[ev.ID] = ev
	}
	out := make([]model.Event, 0, len(report.Kept))
	for _, alignedEvent := range report.Kept {
		if ev, ok := byID[alignedEvent.EventID]; ok {
			out = append(out, ev)
		}
	}
	return out
}
