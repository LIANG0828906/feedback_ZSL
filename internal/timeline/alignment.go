package timeline

import (
	"sort"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/model"
)

type AlignedEvent struct {
	EventID           string   `json:"event_id"`
	Seq               int      `json:"seq"`
	Source            string   `json:"source"`
	ServiceID         string   `json:"service_id"`
	Kind              string   `json:"kind"`
	Fingerprint       string   `json:"fingerprint"`
	EffectiveMS       int64    `json:"effective_ms"`
	IngestLagMS       int64    `json:"ingest_lag_ms"`
	AliveMS           int64    `json:"alive_ms"`
	Kept              bool     `json:"kept"`
	KeptAs            string   `json:"kept_as,omitempty"`
	DuplicatesRemoved int      `json:"duplicates_removed,omitempty"`
	MergedRawCount    int      `json:"merged_raw_count,omitempty"`
	MergedIDs         []string `json:"merged_ids,omitempty"`
}

type Report struct {
	ToleranceMS int64          `json:"tolerance_ms"`
	All         []AlignedEvent `json:"all"`
	Kept        []AlignedEvent `json:"kept"`
	RemovedIDs  []string       `json:"removed_ids"`
}

// Align normalizes source ownership and applies first-logical-wins duplicate
// adjudication within Policy.DedupToleranceMS.
func Align(p *model.Project) (*Report, error) {
	if p.Policy.DedupToleranceMS < 0 {
		return nil, bizerr.New(bizerr.CodeInvalidInput, "policy.dedup_tolerance_ms", "dedup tolerance must not be negative").
			With("value", p.Policy.DedupToleranceMS)
	}
	events := p.SortedEvents()
	report := &Report{ToleranceMS: p.Policy.DedupToleranceMS}
	groups := map[string][]int{}

	for _, ev := range events {
		if ev.ServiceID == "" {
			return nil, bizerr.New(bizerr.CodeSchemaField, "events."+ev.ID+".service_id", "event service is empty")
		}
		if _, ok := p.Service(ev.ServiceID); !ok {
			return nil, bizerr.New(bizerr.CodeEditTarget, "events."+ev.ID+".service_id", "unknown service %q", ev.ServiceID)
		}
		fingerprint := ev.Fingerprint
		if fingerprint == "" {
			fingerprint = ev.Kind
		}
		key := ev.Source + "|" + ev.ServiceID + "|" + fingerprint
		effective := ev.OccurredMS
		item := AlignedEvent{
			EventID:     ev.ID,
			Seq:         ev.Seq,
			Source:      ev.Source,
			ServiceID:   ev.ServiceID,
			Kind:        ev.Kind,
			Fingerprint: key,
			EffectiveMS: effective,
			IngestLagMS: ev.IngestLagMS,
			AliveMS:     ev.AliveMS,
		}

		match := -1
		for _, idx := range groups[key] {
			candidate := report.Kept[idx]
			if absMS(candidate.EffectiveMS-effective) <= p.Policy.DedupToleranceMS {
				match = idx
				break
			}
		}

		if match < 0 {
			item.Kept = true
			item.MergedRawCount = 1
			idx := len(report.Kept)
			report.Kept = append(report.Kept, item)
			report.All = append(report.All, item)
			groups[key] = append(groups[key], idx)
			continue
		}

		item.Kept = false
		item.KeptAs = report.Kept[match].EventID
		report.RemovedIDs = append(report.RemovedIDs, ev.ID)
		report.All = append(report.All, item)

		survivor := &report.Kept[match]
		survivor.DuplicatesRemoved++
		survivor.MergedRawCount++
		survivor.MergedIDs = append(survivor.MergedIDs, ev.ID)
		for i := range report.All {
			if report.All[i].EventID == survivor.EventID {
				report.All[i] = *survivor
				break
			}
		}
	}

	sort.Slice(report.Kept, func(i, j int) bool {
		if report.Kept[i].Seq != report.Kept[j].Seq {
			return report.Kept[i].Seq < report.Kept[j].Seq
		}
		return report.Kept[i].EventID < report.Kept[j].EventID
	})
	sort.Strings(report.RemovedIDs)
	return report, nil
}

func absMS(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
