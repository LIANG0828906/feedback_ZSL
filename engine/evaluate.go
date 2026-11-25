package engine

import (
	"sort"
	"time"

	"alertkernel/fixed"
	"alertkernel/model"
	"alertkernel/schema"
)

func evaluateRule(p *model.Project, rule *model.Rule, t int) ([]Observation, RuleScheduling, error) {
	started := time.Now()
	report := RuleScheduling{RuleID: rule.ID}
	obsByKey := map[string]*Observation{}
	buffer := make([]*model.Series, 0, p.Budget.PerRuleBuffer)
	metricSeen := false

	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		report.BufferChunks++
		for _, series := range buffer {
			report.SeriesCount++
			obs, err := evaluateSeries(p, rule, series, t)
			if err != nil {
				return err
			}
			existing, ok := obsByKey[obs.InstanceKey]
			if !ok {
				copyObs := obs
				obsByKey[obs.InstanceKey] = &copyObs
				continue
			}
			existing.Matched = existing.Matched || obs.Matched
			existing.HasData = existing.HasData || obs.HasData
			existing.Overflow = existing.Overflow || obs.Overflow
			existing.Saturated = existing.Saturated || obs.Saturated
			if existing.ValueRaw == nil && obs.ValueRaw != nil {
				v := *obs.ValueRaw
				existing.ValueRaw = &v
			}
			existing.Sources = append(existing.Sources, obs.Sources...)
			if existing.NoDataReason == "" {
				existing.NoDataReason = obs.NoDataReason
			}
		}
		buffer = buffer[:0]
		return nil
	}

	for _, series := range p.Series {
		if series.Metric != rule.Predicate.Metric {
			continue
		}
		metricSeen = true
		if !matchAll(rule.Predicate.Matchers, series.Labels) {
			continue
		}
		if len(buffer) == cap(buffer) {
			report.Backpressure++
			if err := flush(); err != nil {
				return nil, report, err
			}
		}
		buffer = append(buffer, series)
	}
	if err := flush(); err != nil {
		return nil, report, err
	}
	if !metricSeen {
		obs := Observation{
			RuleID: rule.ID, Order: rule.Order, InstanceKey: "__missing_metric__",
			Labels: model.Labels{}, HasData: false, Matched: false, NoDataReason: "missing_metric",
		}
		return []Observation{obs}, report, nil
	}

	out := make([]Observation, 0, len(obsByKey))
	for _, obs := range obsByKey {
		sort.SliceStable(obs.Sources, func(i, j int) bool { return obs.Sources[i].SeriesID < obs.Sources[j].SeriesID })
		out = append(out, *obs)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].InstanceKey < out[j].InstanceKey })
	report.DurationNS = time.Since(started).Nanoseconds()
	return out, report, nil
}

func evaluateSeries(p *model.Project, rule *model.Rule, series *model.Series, t int) (Observation, error) {
	rewritten := schema.RewriteLabels(rule, series.Labels)
	obs := Observation{
		RuleID: rule.ID, Order: rule.Order, InstanceKey: rewritten.CanonicalKey(), Labels: rewritten,
	}
	evalMS := p.Clock.TimeMS(t)
	var sample *model.Sample
	for i := len(series.Samples) - 1; i >= 0; i-- {
		if series.Samples[i].Index <= t {
			sample = &series.Samples[i]
			break
		}
	}
	if sample == nil {
		obs.NoDataReason = "no_sample_before_eval"
		return obs, nil
	}
	if evalMS-sample.AtMS > rule.Predicate.FreshnessMS {
		obs.NoDataReason = "stale_sample_gap"
		return obs, nil
	}
	if err := fixed.CheckRaw(sample.Raw, p.Numeric.Bits, rule.ID+".predicate.raw"); err != nil {
		return obs, err
	}
	value := fixed.Value{Raw: sample.Raw, Scale: p.Numeric.Scale, Bits: p.Numeric.Bits}
	usedRaw := sample.Raw
	if rule.Predicate.AddRaw != nil {
		adjustment := fixed.Value{Raw: *rule.Predicate.AddRaw, Scale: p.Numeric.Scale, Bits: p.Numeric.Bits}
		result, overflow, err := fixed.Add(value, adjustment, p.Numeric.OverflowPolicy, "rules."+rule.ID+".predicate.add_raw")
		if err != nil {
			return obs, err
		}
		usedRaw = result.Raw
		obs.Overflow = overflow
		obs.Saturated = overflow && p.Numeric.OverflowPolicy == fixed.OverflowSaturate
	}
	matched, err := fixed.Compare(usedRaw, rule.Predicate.Op, rule.Predicate.Threshold)
	if err != nil {
		return obs, err
	}
	obs.HasData = true
	obs.Matched = matched
	obs.ValueRaw = &usedRaw
	obs.Sources = []SourceRef{{
		SeriesID: series.ID, Metric: series.Metric, Labels: series.Labels.Clone(),
		SampleIndex: sample.Index, SampleAtMS: sample.AtMS, Raw: sample.Raw,
	}}
	return obs, nil
}

func matchAll(matchers []model.Matcher, labels model.Labels) bool {
	for _, matcher := range matchers {
		if !matcher.Matches(labels) {
			return false
		}
	}
	return true
}
