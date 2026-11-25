package schema

import (
	"sort"
	"strings"

	"alertkernel/errs"
	"alertkernel/model"
)

type DocumentV3 struct {
	Schema         string                 `json:"schema"`
	Metadata       MetadataV3             `json:"metadata"`
	Schedule       ScheduleV3             `json:"schedule"`
	FixedPoint     FixedPointV3           `json:"fixed-point"`
	Performance    PerformanceV3          `json:"performance-budget"`
	MetricSeries   []SeriesV3             `json:"metric-series"`
	RuleTree       RuleGroupV3            `json:"rule-tree"`
	Receivers      []model.Receiver       `json:"receivers"`
	Routes         []model.Route          `json:"routes,omitempty"`
	Inhibitions    []model.InhibitionRule `json:"inhibition-rules,omitempty"`
	SilenceWindows []SilenceV3            `json:"silence-windows,omitempty"`
	Policy         *model.Policy          `json:"policy,omitempty"`
}

type MetadataV3 struct {
	ProjectID   string `json:"project-id"`
	RuleVersion string `json:"rule-version,omitempty"`
}

type ScheduleV3 struct {
	EpochOriginMS  int64 `json:"epoch-origin-ms"`
	EvalIntervalMS int64 `json:"eval-interval-ms"`
	LogicalSteps   int   `json:"logical-steps"`
}

type FixedPointV3 struct {
	Scale          int64  `json:"scale"`
	Bits           int    `json:"bits"`
	OverflowPolicy string `json:"overflow-policy"`
}

type PerformanceV3 struct {
	WorkerCount         int `json:"worker-count"`
	SeriesBufferPerRule int `json:"series-buffer-per-rule"`
}

type SeriesV3 struct {
	SeriesID   string            `json:"series-id"`
	MetricName string            `json:"metric-name"`
	TagSet     map[string]string `json:"tag-set"`
	Samples    []SampleV3        `json:"samples"`
}

type SampleV3 struct {
	AtEpochMS int64 `json:"at-epoch-ms"`
	ValueRaw  int64 `json:"integer-value"`
}

type RuleGroupV3 struct {
	GroupName string        `json:"group-name"`
	Rules     []RuleV3      `json:"rules,omitempty"`
	Groups    []RuleGroupV3 `json:"groups,omitempty"`
}

type PredicateV3 struct {
	MetricName   string          `json:"metric-name"`
	Selectors    []model.Matcher `json:"selectors,omitempty"`
	Comparison   string          `json:"comparison"`
	ThresholdRaw int64           `json:"threshold-raw"`
	AddRaw       *int64          `json:"add-raw,omitempty"`
	FreshnessMS  int64           `json:"freshness-ms,omitempty"`
}

type RuleV3 struct {
	RuleID          string            `json:"rule-id"`
	Priority        int               `json:"priority,omitempty"`
	When            PredicateV3       `json:"when"`
	ForMS           int64             `json:"for-ms"`
	KeepFiringForMS int64             `json:"keep-firing-for-ms"`
	RewriteTagSet   map[string]string `json:"rewrite-tag-set,omitempty"`
	GroupBy         []string          `json:"group-by,omitempty"`
	ReceiverName    string            `json:"receiver-name,omitempty"`
	NoDataPolicy    string            `json:"no-data-policy,omitempty"`
}

type SilenceV3 struct {
	SilenceID         string          `json:"silence-id"`
	Selectors         []model.Matcher `json:"selectors"`
	StartLogicalIndex int             `json:"start-logical-index"`
	EndLogicalIndex   int             `json:"end-logical-index"`
	Comment           string          `json:"comment,omitempty"`
}

func fromV3(d *DocumentV3) (*model.Project, error) {
	if d.Schedule.EpochOriginMS == 0 || d.Schedule.EvalIntervalMS <= 0 {
		return nil, errs.New(errs.ESchemaCombo, "$.schedule", "v3 requires epoch-origin-ms and positive eval-interval-ms")
	}
	p := &model.Project{
		ProjectID: d.Metadata.ProjectID, RuleVersion: d.Metadata.RuleVersion, SourceSchema: d.Schema,
		Clock:     model.Clock{BaseEpochMS: d.Schedule.EpochOriginMS, StepMS: d.Schedule.EvalIntervalMS, Steps: d.Schedule.LogicalSteps},
		Numeric:   model.NumericConfig{Scale: d.FixedPoint.Scale, Bits: d.FixedPoint.Bits, OverflowPolicy: d.FixedPoint.OverflowPolicy},
		Budget:    model.Budget{Workers: d.Performance.WorkerCount, PerRuleBuffer: d.Performance.SeriesBufferPerRule},
		Receivers: d.Receivers, Routes: d.Routes, Inhibitions: d.Inhibitions,
	}
	if d.Policy != nil {
		p.Policy = *d.Policy
	}
	for i, s := range d.MetricSeries {
		series := &model.Series{ID: s.SeriesID, Metric: s.MetricName, Labels: model.Labels(s.TagSet)}
		for j, sm := range s.Samples {
			idx := p.Clock.IndexAtOrBefore(sm.AtEpochMS)
			if idx < 0 {
				return nil, errs.New(errs.ESchemaCombo, path("metric-series", i, "samples", j), "epoch ms %d precedes origin", sm.AtEpochMS)
			}
			series.Samples = append(series.Samples, model.Sample{Index: idx, AtMS: sm.AtEpochMS, Raw: sm.ValueRaw})
		}
		p.Series = append(p.Series, series)
	}
	if err := flattenRuleGroupV3(d.RuleTree, "", p); err != nil {
		return nil, err
	}
	for _, s := range d.SilenceWindows {
		p.Silences = append(p.Silences, model.SilenceWindow{
			ID: s.SilenceID, Matchers: s.Selectors, StartT: s.StartLogicalIndex, EndT: s.EndLogicalIndex, Comment: s.Comment,
		})
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

func flattenRuleGroupV3(group RuleGroupV3, parent string, p *model.Project) error {
	name := group.GroupName
	if parent == "" && name == "root" {
		name = ""
	} else if parent != "" {
		name = parent + "/" + group.GroupName
	}
	for i, r := range group.Rules {
		if r.ForMS < 0 || r.KeepFiringForMS < 0 {
			return errs.New(errs.EDurationNegative, path("rule-tree", group.GroupName, i), "durations must be non-negative")
		}
		p.Rules = append(p.Rules, &model.Rule{
			ID: r.RuleID, Group: name, Priority: r.Priority,
			Predicate: model.Predicate{
				Metric: r.When.MetricName, Matchers: r.When.Selectors, Op: r.When.Comparison,
				Threshold: r.When.ThresholdRaw, AddRaw: r.When.AddRaw, FreshnessMS: r.When.FreshnessMS,
			},
			ForMS: r.ForMS, KeepFiringMS: r.KeepFiringForMS, LabelRewrite: r.RewriteTagSet,
			GroupBy: r.GroupBy, Receiver: r.ReceiverName, NoDataAction: r.NoDataPolicy,
		})
	}
	for _, child := range group.Groups {
		if err := flattenRuleGroupV3(child, name, p); err != nil {
			return err
		}
	}
	return nil
}

func ToV3(p *model.Project) *DocumentV3 {
	doc := &DocumentV3{
		Schema:      "alert-kernel/v3",
		Metadata:    MetadataV3{ProjectID: p.ProjectID, RuleVersion: p.RuleVersion},
		Schedule:    ScheduleV3{EpochOriginMS: p.Clock.BaseEpochMS, EvalIntervalMS: p.Clock.StepMS, LogicalSteps: p.Clock.Steps},
		FixedPoint:  FixedPointV3{Scale: p.Numeric.Scale, Bits: p.Numeric.Bits, OverflowPolicy: p.Numeric.OverflowPolicy},
		Performance: PerformanceV3{WorkerCount: p.Budget.Workers, SeriesBufferPerRule: p.Budget.PerRuleBuffer},
		Receivers:   p.Receivers, Routes: p.Routes, Inhibitions: p.Inhibitions, Policy: &p.Policy,
	}
	for _, s := range p.Series {
		v3s := SeriesV3{SeriesID: s.ID, MetricName: s.Metric, TagSet: s.Labels}
		for _, sm := range s.Samples {
			v3s.Samples = append(v3s.Samples, SampleV3{AtEpochMS: sm.AtMS, ValueRaw: sm.Raw})
		}
		doc.MetricSeries = append(doc.MetricSeries, v3s)
	}
	doc.RuleTree = freezeRuleTreeV3(buildMutableRuleTreeV3(p.Rules))
	for _, s := range p.Silences {
		doc.SilenceWindows = append(doc.SilenceWindows, SilenceV3{
			SilenceID: s.ID, Selectors: s.Matchers, StartLogicalIndex: s.StartT, EndLogicalIndex: s.EndT, Comment: s.Comment,
		})
	}
	return doc
}

type mutableRuleGroupV3 struct {
	name   string
	rules  []RuleV3
	groups []*mutableRuleGroupV3
}

func buildMutableRuleTreeV3(rules []*model.Rule) *mutableRuleGroupV3 {
	root := &mutableRuleGroupV3{name: "root"}
	byPath := map[string]*mutableRuleGroupV3{"": root}
	for _, r := range rules {
		parts := strings.Split(r.Group, "/")
		if r.Group == "" {
			parts = []string{"default"}
		}
		parentPath := ""
		parent := root
		for _, part := range parts {
			pathKey := parentPath + "/" + part
			group, ok := byPath[pathKey]
			if !ok {
				group = &mutableRuleGroupV3{name: part}
				parent.groups = append(parent.groups, group)
				byPath[pathKey] = group
			}
			parent = group
			parentPath = pathKey
		}
		parent.rules = append(parent.rules, RuleV3{
			RuleID: r.ID, Priority: r.Priority,
			When: PredicateV3{
				MetricName: r.Predicate.Metric, Selectors: r.Predicate.Matchers, Comparison: r.Predicate.Op,
				ThresholdRaw: r.Predicate.Threshold, AddRaw: r.Predicate.AddRaw, FreshnessMS: r.Predicate.FreshnessMS,
			},
			ForMS: r.ForMS, KeepFiringForMS: r.KeepFiringMS, RewriteTagSet: r.LabelRewrite,
			GroupBy: r.GroupBy, ReceiverName: r.Receiver, NoDataPolicy: r.NoDataAction,
		})
	}
	sortMutableRuleTreeV3(root)
	return root
}

func sortMutableRuleTreeV3(g *mutableRuleGroupV3) {
	sort.SliceStable(g.rules, func(i, j int) bool { return g.rules[i].RuleID < g.rules[j].RuleID })
	sort.SliceStable(g.groups, func(i, j int) bool { return g.groups[i].name < g.groups[j].name })
	for _, child := range g.groups {
		sortMutableRuleTreeV3(child)
	}
}

func freezeRuleTreeV3(g *mutableRuleGroupV3) RuleGroupV3 {
	out := RuleGroupV3{GroupName: g.name, Rules: g.rules}
	for _, child := range g.groups {
		out.Groups = append(out.Groups, freezeRuleTreeV3(child))
	}
	return out
}
