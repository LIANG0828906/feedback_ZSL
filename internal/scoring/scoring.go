package scoring

import (
	"fmt"
	"sort"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/fixedpoint"
	"sre-timeline/internal/intervals"
	"sre-timeline/internal/model"
	"sre-timeline/internal/topology"
)

type Target struct {
	Ordinal int                `json:"ordinal"`
	ID      string             `json:"id"`
	Result  string             `json:"service_id"`
	Data    intervals.Interval `json:"data"`
}

type Contribution struct {
	EventID            string `json:"event_id"`
	EventSeq           int    `json:"event_seq"`
	IntervalOrdinal    int    `json:"interval_ordinal"`
	IntervalID         string `json:"interval_id"`
	IntervalKey        string `json:"interval_key"`
	ServiceID          string `json:"service_id"`
	TimingRaw          int64  `json:"timing_raw"`
	ReachRaw           int64  `json:"reach_raw"`
	TypeRaw            int64  `json:"type_raw"`
	ScoreRaw           int64  `json:"score_raw"`
	TopologyGeneration int    `json:"topology_generation"`
	Participated       bool   `json:"participated"`
	Saturated          bool   `json:"saturated,omitempty"`
}

type Candidate struct {
	Rank              int      `json:"rank"`
	EventID           string   `json:"event_id"`
	EventSeq          int      `json:"event_seq"`
	Source            string   `json:"source"`
	ServiceID         string   `json:"service_id"`
	TotalRaw          int64    `json:"total_raw"`
	Saturated         bool     `json:"saturated,omitempty"`
	IntervalOrdinals  []int    `json:"interval_ordinals"`
	IntervalIDs       []string `json:"interval_ids"`
	ContributionCount int      `json:"contribution_count"`
}

type Report struct {
	Targets       []Target       `json:"targets"`
	Contributions []Contribution `json:"contributions"`
	Candidates    []Candidate    `json:"candidates"`
}

type EventTotal struct {
	EventID      string
	Value        fixedpoint.Value
	Ordinal      int
	IntervalID   string
	Participated bool
}

func NumberTargets(results map[string]*intervals.ServiceResult) []Target {
	var raw []intervals.Interval
	for _, result := range results {
		raw = append(raw, result.Intervals...)
	}
	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].StartMS != raw[j].StartMS {
			return raw[i].StartMS < raw[j].StartMS
		}
		if raw[i].EndMS != raw[j].EndMS {
			return raw[i].EndMS < raw[j].EndMS
		}
		return raw[i].ServiceID < raw[j].ServiceID
	})
	targets := make([]Target, 0, len(raw))
	for i, iv := range raw {
		id := fmt.Sprintf("I-%04d", i+1)
		iv.Ordinal = i + 1
		iv.ID = id
		targets = append(targets, Target{Ordinal: i + 1, ID: id, Result: iv.ServiceID, Data: iv})
		if results[iv.ServiceID] != nil {
			for j := range results[iv.ServiceID].Intervals {
				if results[iv.ServiceID].Intervals[j].Key == iv.Key {
					results[iv.ServiceID].Intervals[j].Ordinal = i + 1
					results[iv.ServiceID].Intervals[j].ID = id
				}
			}
		}
	}
	return targets
}

func Score(p *model.Project, topo *topology.Topology, targets []Target, keptEvents []model.Event) (*Report, error) {
	allContribs := make([]Contribution, 0)
	for _, target := range targets {
		contribs, _, err := ScoreTarget(p, topo, target, keptEvents)
		if err != nil {
			return nil, err
		}
		allContribs = append(allContribs, contribs...)
	}
	return Aggregate(targets, allContribs, keptEvents), nil
}

func Aggregate(targets []Target, contributions []Contribution, keptEvents []model.Event) *Report {
	report := &Report{Targets: targets, Contributions: contributions}
	totals := map[string]fixedpoint.Value{}
	intervalOrdinals := map[string][]int{}
	intervalIDs := map[string][]string{}
	contribCount := map[string]int{}
	for _, c := range contributions {
		if !c.Participated {
			continue
		}
		totals[c.EventID] = fixedpoint.AddSaturating(totals[c.EventID], fixedpoint.Value{Raw: c.ScoreRaw, Saturated: c.Saturated})
		intervalOrdinals[c.EventID] = appendUniqueInt(intervalOrdinals[c.EventID], c.IntervalOrdinal)
		intervalIDs[c.EventID] = appendUniqueString(intervalIDs[c.EventID], c.IntervalID)
		contribCount[c.EventID]++
	}

	for _, ev := range keptEvents {
		total := totals[ev.ID]
		report.Candidates = append(report.Candidates, Candidate{
			EventID:           ev.ID,
			EventSeq:          ev.Seq,
			Source:            ev.Source,
			ServiceID:         ev.ServiceID,
			TotalRaw:          total.Raw,
			Saturated:         total.Saturated,
			IntervalOrdinals:  intervalOrdinals[ev.ID],
			IntervalIDs:       intervalIDs[ev.ID],
			ContributionCount: contribCount[ev.ID],
		})
	}
	sort.SliceStable(report.Candidates, func(i, j int) bool {
		a, b := report.Candidates[i], report.Candidates[j]
		if a.TotalRaw != b.TotalRaw {
			return a.TotalRaw > b.TotalRaw
		}
		if a.EventSeq != b.EventSeq {
			return a.EventSeq < b.EventSeq
		}
		return a.EventID < b.EventID
	})
	for i := range report.Candidates {
		report.Candidates[i].Rank = i + 1
	}
	sort.Slice(report.Contributions, func(i, j int) bool {
		if report.Contributions[i].IntervalOrdinal != report.Contributions[j].IntervalOrdinal {
			return report.Contributions[i].IntervalOrdinal < report.Contributions[j].IntervalOrdinal
		}
		return report.Contributions[i].EventSeq < report.Contributions[j].EventSeq
	})
	return report
}

func ScoreTarget(p *model.Project, topo *topology.Topology, target Target, keptEvents []model.Event) ([]Contribution, []EventTotal, error) {
	iv := target.Data
	distances := topo.Distances(iv.ServiceID)
	contribs := make([]Contribution, 0, len(keptEvents))
	totals := make([]EventTotal, 0, len(keptEvents))
	for _, ev := range keptEvents {
		if ev.TypeWeightBP < 0 || ev.TypeWeightBP > 10000 {
			return nil, nil, bizerr.New(bizerr.CodeScaleOutOfRange, "events."+ev.ID+".type_weight_bp", "weight must be in [0,10000] basis points").
				With("event", ev.ID).With("value", ev.TypeWeightBP)
		}
		timing, timingErr := timingFactor(ev.OccurredMS, iv.StartMS, iv.EndMS, p.Policy.LookbackMS)
		if timingErr != nil {
			return nil, nil, timingErr
		}
		gen, reachable := distances[ev.ServiceID]
		reach := fixedpoint.Value{}
		if ev.ServiceID == iv.ServiceID {
			gen = 0
			reachable = true
		}
		if reachable {
			reach = reachFactor(gen)
		}
		typeFactor := fixedpoint.FromBP(ev.TypeWeightBP)
		score, err := fixedpoint.Mul(timing, reach)
		if err != nil {
			return nil, nil, err
		}
		score, err = fixedpoint.Mul(score, typeFactor)
		if err != nil {
			return nil, nil, err
		}
		c := Contribution{
			EventID:            ev.ID,
			EventSeq:           ev.Seq,
			IntervalOrdinal:    target.Ordinal,
			IntervalID:         target.ID,
			IntervalKey:        iv.Key,
			ServiceID:          iv.ServiceID,
			TimingRaw:          timing.Raw,
			ReachRaw:           reach.Raw,
			TypeRaw:            typeFactor.Raw,
			ScoreRaw:           score.Raw,
			TopologyGeneration: gen,
			Participated:       score.Raw != 0,
			Saturated:          score.Saturated,
		}
		contribs = append(contribs, c)
		totals = append(totals, EventTotal{
			EventID: ev.ID, Value: score, Ordinal: target.Ordinal, IntervalID: target.ID,
			Participated: c.Participated,
		})
	}
	return contribs, totals, nil
}

func timingFactor(eventMS, startMS, endMS, lookbackMS int64) (fixedpoint.Value, error) {
	switch {
	case eventMS > endMS:
		return fixedpoint.Value{}, nil
	case eventMS == endMS:
		return fixedpoint.Value{}, nil
	case eventMS >= startMS && eventMS < endMS:
		return fixedpoint.Value{Raw: fixedpoint.Scale}, nil
	case eventMS <= startMS:
		age := startMS - eventMS
		if lookbackMS <= 0 || age > lookbackMS {
			return fixedpoint.Value{}, nil
		}
		if age == 0 {
			return fixedpoint.Value{Raw: fixedpoint.Scale}, nil
		}
		return fixedpoint.FromRatio(lookbackMS-age, lookbackMS)
	default:
		return fixedpoint.Value{}, nil
	}
}

func reachFactor(generation int) fixedpoint.Value {
	raw := fixedpoint.Scale
	for i := 0; i < generation; i++ {
		raw /= 2
		if raw == 0 {
			break
		}
	}
	return fixedpoint.Value{Raw: raw}
}

func appendUniqueInt(in []int, v int) []int {
	for _, existing := range in {
		if existing == v {
			return in
		}
	}
	return append(in, v)
}

func appendUniqueString(in []string, v string) []string {
	for _, existing := range in {
		if existing == v {
			return in
		}
	}
	return append(in, v)
}
