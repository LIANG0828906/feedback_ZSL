package schema

import (
	"alertkernel/errs"
	"alertkernel/model"
)

type DocumentV1 struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     string `json:"project_id"`
	RuleVersion   string `json:"rule_version,omitempty"`
	BaseEpochMS   int64  `json:"base_epoch_ms"`
	StepSeconds   int64  `json:"step_seconds"`
	Steps         int    `json:"steps"`
	Scale         int64  `json:"scale,omitempty"`
	Bits          int    `json:"bits,omitempty"`
	Overflow      string `json:"overflow_policy,omitempty"`
	Workers       int    `json:"workers,omitempty"`
	Buffer        int    `json:"per_rule_buffer,omitempty"`

	Series      []SeriesV1             `json:"metric_series"`
	Rules       []RuleV1               `json:"rules"`
	Receivers   []model.Receiver       `json:"receivers"`
	Routes      []model.Route          `json:"routes,omitempty"`
	Inhibitions []model.InhibitionRule `json:"inhibition_rules,omitempty"`
	Policy      *model.Policy          `json:"policy,omitempty"`
}

type SeriesV1 struct {
	ID      string            `json:"id"`
	Metric  string            `json:"metric"`
	Labels  map[string]string `json:"labels"`
	Samples []SampleV1        `json:"samples"`
}

type SampleV1 struct {
	AtMS        *int64 `json:"at_ms,omitempty"`
	SampleIndex *int   `json:"sample_index,omitempty"`
	ValueRaw    int64  `json:"value_raw"`
}

type PredicateV1 struct {
	Metric           string          `json:"metric"`
	Matchers         []model.Matcher `json:"matchers,omitempty"`
	Op               string          `json:"op"`
	ThresholdRaw     int64           `json:"threshold_raw"`
	AddRaw           *int64          `json:"add_raw,omitempty"`
	FreshnessSeconds int64           `json:"freshness_seconds,omitempty"`
}

type RuleV1 struct {
	ID                   string            `json:"id"`
	Group                string            `json:"rule_group,omitempty"`
	Priority             int               `json:"priority,omitempty"`
	Predicate            PredicateV1       `json:"predicate"`
	ForSeconds           int64             `json:"for_seconds"`
	KeepFiringForSeconds int64             `json:"keep_firing_for_seconds"`
	LabelRewrite         map[string]string `json:"label_rewrite,omitempty"`
	GroupBy              []string          `json:"group_by,omitempty"`
	Receiver             string            `json:"receiver,omitempty"`
	NoDataAction         string            `json:"no_data_action,omitempty"`
}

func fromV1(d *DocumentV1) (*model.Project, error) {
	if d.BaseEpochMS == 0 || d.StepSeconds <= 0 {
		return nil, errs.New(errs.ESchemaCombo, "$.time", "v1 requires epoch base_ms and positive step_seconds")
	}
	p := &model.Project{
		ProjectID: d.ProjectID, RuleVersion: d.RuleVersion, SourceSchema: d.SchemaVersion,
		Clock:     model.Clock{BaseEpochMS: d.BaseEpochMS, StepMS: d.StepSeconds * 1000, Steps: d.Steps},
		Numeric:   model.NumericConfig{Scale: d.Scale, Bits: d.Bits, OverflowPolicy: d.Overflow},
		Budget:    model.Budget{Workers: d.Workers, PerRuleBuffer: d.Buffer},
		Receivers: d.Receivers, Routes: d.Routes, Inhibitions: d.Inhibitions,
	}
	if d.Policy != nil {
		p.Policy = *d.Policy
	}
	for i, s := range d.Series {
		series := &model.Series{ID: s.ID, Metric: s.Metric, Labels: model.Labels(s.Labels)}
		for j, sample := range s.Samples {
			if sample.AtMS == nil || sample.SampleIndex != nil {
				return nil, errs.New(errs.ESchemaCombo, path("metric_series", i, "samples", j), "v1 samples require at_ms and must not use sample_index")
			}
			idx := p.Clock.IndexAtOrBefore(*sample.AtMS)
			if idx < 0 {
				return nil, errs.New(errs.ESchemaCombo, path("metric_series", i, "samples", j, "at_ms"), "timestamp %d precedes epoch base", *sample.AtMS)
			}
			series.Samples = append(series.Samples, model.Sample{Index: idx, AtMS: *sample.AtMS, Raw: sample.ValueRaw})
		}
		p.Series = append(p.Series, series)
	}
	for i, r := range d.Rules {
		if r.ForSeconds < 0 {
			return nil, errs.New(errs.EDurationNegative, path("rules", i, "for_seconds"), "negative duration %d seconds", r.ForSeconds)
		}
		if r.KeepFiringForSeconds < 0 {
			return nil, errs.New(errs.EDurationNegative, path("rules", i, "keep_firing_for_seconds"), "negative duration %d seconds", r.KeepFiringForSeconds)
		}
		rule := &model.Rule{
			ID: r.ID, Group: r.Group, Priority: r.Priority,
			Predicate: model.Predicate{
				Metric: r.Predicate.Metric, Matchers: r.Predicate.Matchers, Op: r.Predicate.Op,
				Threshold: r.Predicate.ThresholdRaw, AddRaw: r.Predicate.AddRaw,
				FreshnessMS: r.Predicate.FreshnessSeconds * 1000,
			},
			ForMS: r.ForSeconds * 1000, KeepFiringMS: r.KeepFiringForSeconds * 1000,
			LabelRewrite: r.LabelRewrite, GroupBy: r.GroupBy, Receiver: r.Receiver, NoDataAction: r.NoDataAction,
		}
		p.Rules = append(p.Rules, rule)
	}
	if p.Budget.Workers == 0 {
		p.Budget.Workers = DefaultWorkers
	}
	if p.Budget.PerRuleBuffer == 0 {
		p.Budget.PerRuleBuffer = DefaultBuffer
	}
	if err := Canonicalize(p); err != nil {
		return nil, err
	}
	return p, nil
}

func ToV1(p *model.Project) *DocumentV1 {
	doc := &DocumentV1{
		SchemaVersion: "v1", ProjectID: p.ProjectID, RuleVersion: p.RuleVersion,
		BaseEpochMS: p.Clock.BaseEpochMS, StepSeconds: p.Clock.StepMS / 1000, Steps: p.Clock.Steps,
		Scale: p.Numeric.Scale, Bits: p.Numeric.Bits, Overflow: p.Numeric.OverflowPolicy,
		Workers: p.Budget.Workers, Buffer: p.Budget.PerRuleBuffer,
		Receivers: p.Receivers, Routes: p.Routes, Inhibitions: p.Inhibitions, Policy: &p.Policy,
	}
	for _, s := range p.Series {
		v1s := SeriesV1{ID: s.ID, Metric: s.Metric, Labels: s.Labels, Samples: make([]SampleV1, 0, len(s.Samples))}
		for _, sm := range s.Samples {
			at := sm.AtMS
			v1s.Samples = append(v1s.Samples, SampleV1{AtMS: &at, ValueRaw: sm.Raw})
		}
		doc.Series = append(doc.Series, v1s)
	}
	for _, r := range p.Rules {
		doc.Rules = append(doc.Rules, RuleV1{
			ID: r.ID, Group: r.Group, Priority: r.Priority,
			Predicate: PredicateV1{
				Metric: r.Predicate.Metric, Matchers: r.Predicate.Matchers, Op: r.Predicate.Op,
				ThresholdRaw: r.Predicate.Threshold, AddRaw: r.Predicate.AddRaw,
				FreshnessSeconds: r.Predicate.FreshnessMS / 1000,
			},
			ForSeconds: r.ForMS / 1000, KeepFiringForSeconds: r.KeepFiringMS / 1000,
			LabelRewrite: r.LabelRewrite, GroupBy: r.GroupBy, Receiver: r.Receiver, NoDataAction: r.NoDataAction,
		})
	}
	return doc
}
