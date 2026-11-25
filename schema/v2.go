package schema

import (
	"sort"

	"alertkernel/errs"
	"alertkernel/model"
)

type DocumentV2 struct {
	SchemaVersion   string                 `json:"schemaVersion"`
	ProjectID       string                 `json:"projectId"`
	RuleVersion     string                 `json:"ruleVersion,omitempty"`
	TimeMode        string                 `json:"timeMode,omitempty"`
	TimeOriginMS    int64                  `json:"timeOriginMs"`
	StepMS          int64                  `json:"stepMs"`
	Steps           int                    `json:"logicalSteps"`
	Numeric         NumericConfigV2        `json:"numericConfig"`
	Budget          BudgetV2               `json:"evaluationBudget"`
	LabelTrees      []LabelTreeV2          `json:"labelTrees"`
	MetricSeries    []SeriesV2             `json:"metricSeries"`
	RuleGroups      []RuleGroupV2          `json:"ruleGroups"`
	Receivers       []model.Receiver       `json:"receivers"`
	Routes          []model.Route          `json:"routes,omitempty"`
	InhibitionRules []model.InhibitionRule `json:"inhibitionRules,omitempty"`
	SilenceWindows  []SilenceV2            `json:"silenceWindows,omitempty"`
	Policy          *model.Policy          `json:"policy,omitempty"`
}

type NumericConfigV2 struct {
	Scale          int64  `json:"scale"`
	Bits           int    `json:"bits"`
	OverflowPolicy string `json:"overflowPolicy"`
}

type BudgetV2 struct {
	WorkerThreads int `json:"workerThreads"`
	SeriesBuffer  int `json:"seriesBufferPerRule"`
}

type LabelTreeV2 struct {
	TreeID string      `json:"treeId"`
	Root   LabelNodeV2 `json:"root"`
}

type LabelNodeV2 struct {
	LabelSetID string            `json:"labelSetId,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Children   []LabelNodeV2     `json:"children,omitempty"`
}

type SeriesV2 struct {
	SeriesID   string     `json:"seriesId"`
	MetricName string     `json:"metricName"`
	LabelSetID string     `json:"labelSetId"`
	Samples    []SampleV2 `json:"samples"`
}

type SampleV2 struct {
	SampleIndex *int   `json:"sampleIndex,omitempty"`
	AtMS        *int64 `json:"atMs,omitempty"`
	ValueRaw    int64  `json:"valueRaw"`
}

type RuleGroupV2 struct {
	GroupName string   `json:"groupName"`
	Rules     []RuleV2 `json:"rules"`
}

type PredicateV2 struct {
	MetricName    string          `json:"metricName"`
	Matchers      []model.Matcher `json:"matchers,omitempty"`
	Operator      string          `json:"operator"`
	ThresholdRaw  int64           `json:"thresholdRaw"`
	AdjustmentRaw *int64          `json:"adjustmentRaw,omitempty"`
	FreshnessMS   int64           `json:"freshnessMs,omitempty"`
}

type RuleV2 struct {
	RuleID          string            `json:"ruleId"`
	Priority        int               `json:"priority,omitempty"`
	Condition       PredicateV2       `json:"condition"`
	ForMS           int64             `json:"forMs"`
	KeepFiringForMS int64             `json:"keepFiringForMs"`
	RewriteLabels   map[string]string `json:"rewriteLabels,omitempty"`
	GroupBy         []string          `json:"groupBy,omitempty"`
	ReceiverName    string            `json:"receiverName,omitempty"`
	NoDataPolicy    string            `json:"noDataPolicy,omitempty"`
}

type SilenceV2 struct {
	SilenceID  string          `json:"silenceId"`
	Selectors  []model.Matcher `json:"selectors"`
	StartIndex int             `json:"startIndex"`
	EndIndex   int             `json:"endIndex"`
	Comment    string          `json:"comment,omitempty"`
}

func fromV2(d *DocumentV2) (*model.Project, error) {
	if d.TimeMode != "" && d.TimeMode != "relative_sample_index" {
		return nil, errs.New(errs.ESchemaCombo, "$.timeMode", "v2 supports relative_sample_index, got %q", d.TimeMode)
	}
	if d.TimeOriginMS == 0 || d.StepMS <= 0 {
		return nil, errs.New(errs.ESchemaCombo, "$.time", "v2 requires timeOriginMs and positive stepMs")
	}
	labelSets := map[string]model.Labels{}
	for ti, tree := range d.LabelTrees {
		if err := expandLabelNode(tree.Root, model.Labels{}, labelSets); err != nil {
			return nil, errs.Wrap(errs.ESchemaCombo, path("labelTrees", ti), err, "invalid label tree: %v", err)
		}
	}

	p := &model.Project{
		ProjectID: d.ProjectID, RuleVersion: d.RuleVersion, SourceSchema: d.SchemaVersion,
		Clock:       model.Clock{BaseEpochMS: d.TimeOriginMS, StepMS: d.StepMS, Steps: d.Steps},
		Numeric:     model.NumericConfig{Scale: d.Numeric.Scale, Bits: d.Numeric.Bits, OverflowPolicy: d.Numeric.OverflowPolicy},
		Budget:      model.Budget{Workers: d.Budget.WorkerThreads, PerRuleBuffer: d.Budget.SeriesBuffer},
		Receivers:   d.Receivers,
		Routes:      d.Routes,
		Inhibitions: d.InhibitionRules,
	}
	if d.Policy != nil {
		p.Policy = *d.Policy
	}
	for i, s := range d.MetricSeries {
		labels, ok := labelSets[s.LabelSetID]
		if !ok {
			return nil, errs.New(errs.ESchemaCombo, path("metricSeries", i, "labelSetId"), "label set %q was not found in labelTrees", s.LabelSetID)
		}
		series := &model.Series{ID: s.SeriesID, Metric: s.MetricName, Labels: labels.Clone()}
		for j, sample := range s.Samples {
			if sample.SampleIndex == nil || sample.AtMS != nil {
				return nil, errs.New(errs.ESchemaCombo, path("metricSeries", i, "samples", j), "v2 samples require sampleIndex and must not use atMs")
			}
			idx := *sample.SampleIndex
			series.Samples = append(series.Samples, model.Sample{Index: idx, AtMS: p.Clock.TimeMS(idx), Raw: sample.ValueRaw})
		}
		p.Series = append(p.Series, series)
	}
	for gi, group := range d.RuleGroups {
		for i, r := range group.Rules {
			if r.ForMS < 0 || r.KeepFiringForMS < 0 {
				code := errs.EDurationNegative
				return nil, errs.New(code, path("ruleGroups", gi, "rules", i), "durations must be non-negative: forMs=%d keepFiringForMs=%d", r.ForMS, r.KeepFiringForMS)
			}
			p.Rules = append(p.Rules, &model.Rule{
				ID: r.RuleID, Group: group.GroupName, Priority: r.Priority,
				Predicate: model.Predicate{
					Metric: r.Condition.MetricName, Matchers: r.Condition.Matchers, Op: r.Condition.Operator,
					Threshold: r.Condition.ThresholdRaw, AddRaw: r.Condition.AdjustmentRaw,
					FreshnessMS: r.Condition.FreshnessMS,
				},
				ForMS: r.ForMS, KeepFiringMS: r.KeepFiringForMS,
				LabelRewrite: r.RewriteLabels, GroupBy: r.GroupBy, Receiver: r.ReceiverName, NoDataAction: r.NoDataPolicy,
			})
		}
	}
	for _, s := range d.SilenceWindows {
		p.Silences = append(p.Silences, model.SilenceWindow{
			ID: s.SilenceID, Matchers: s.Selectors, StartT: s.StartIndex, EndT: s.EndIndex, Comment: s.Comment,
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

func expandLabelNode(node LabelNodeV2, inherited model.Labels, out map[string]model.Labels) error {
	merged := inherited.Clone()
	for k, v := range node.Labels {
		merged[k] = v
	}
	if node.LabelSetID != "" {
		if _, exists := out[node.LabelSetID]; exists {
			return errs.New("E_LABEL_TREE_DUPLICATE", "labelSetId", "duplicate label set id %q", node.LabelSetID)
		}
		out[node.LabelSetID] = merged.Clone()
	}
	for _, child := range node.Children {
		if err := expandLabelNode(child, merged, out); err != nil {
			return err
		}
	}
	return nil
}

type mutableLabelNode struct {
	id       string
	labels   model.Labels
	children []*mutableLabelNode
}

func ToV2(p *model.Project) *DocumentV2 {
	doc := &DocumentV2{
		SchemaVersion: "v2", ProjectID: p.ProjectID, RuleVersion: p.RuleVersion,
		TimeMode: "relative_sample_index", TimeOriginMS: p.Clock.BaseEpochMS, StepMS: p.Clock.StepMS, Steps: p.Clock.Steps,
		Numeric:   NumericConfigV2{Scale: p.Numeric.Scale, Bits: p.Numeric.Bits, OverflowPolicy: p.Numeric.OverflowPolicy},
		Budget:    BudgetV2{WorkerThreads: p.Budget.Workers, SeriesBuffer: p.Budget.PerRuleBuffer},
		Receivers: p.Receivers, Routes: p.Routes, InhibitionRules: p.Inhibitions, Policy: &p.Policy,
	}
	root := &mutableLabelNode{}
	for _, s := range p.Series {
		keys := s.Labels.SortedKeys()
		cursor := root
		for _, k := range keys {
			v := s.Labels[k]
			var next *mutableLabelNode
			for _, ch := range cursor.children {
				if ch.labels[k] == v && len(ch.labels) == 1 {
					next = ch
					break
				}
			}
			if next == nil {
				next = &mutableLabelNode{labels: model.Labels{k: v}}
				cursor.children = append(cursor.children, next)
			}
			cursor = next
		}
		cursor.id = s.ID
	}
	doc.LabelTrees = []LabelTreeV2{{TreeID: "labels-" + p.ProjectID, Root: freezeLabelNode(root)}}
	for _, s := range p.Series {
		v2s := SeriesV2{SeriesID: s.ID, MetricName: s.Metric, LabelSetID: s.ID}
		for _, sm := range s.Samples {
			idx := sm.Index
			v2s.Samples = append(v2s.Samples, SampleV2{SampleIndex: &idx, ValueRaw: sm.Raw})
		}
		doc.MetricSeries = append(doc.MetricSeries, v2s)
	}
	groups := map[string][]RuleV2{}
	groupNames := []string{}
	for _, r := range p.Rules {
		name := r.Group
		if name == "" {
			name = "default"
		}
		if _, ok := groups[name]; !ok {
			groupNames = append(groupNames, name)
		}
		groups[name] = append(groups[name], RuleV2{
			RuleID: r.ID, Priority: r.Priority,
			Condition: PredicateV2{
				MetricName: r.Predicate.Metric, Matchers: r.Predicate.Matchers, Operator: r.Predicate.Op,
				ThresholdRaw: r.Predicate.Threshold, AdjustmentRaw: r.Predicate.AddRaw, FreshnessMS: r.Predicate.FreshnessMS,
			},
			ForMS: r.ForMS, KeepFiringForMS: r.KeepFiringMS, RewriteLabels: r.LabelRewrite,
			GroupBy: r.GroupBy, ReceiverName: r.Receiver, NoDataPolicy: r.NoDataAction,
		})
	}
	sort.Strings(groupNames)
	for _, name := range groupNames {
		doc.RuleGroups = append(doc.RuleGroups, RuleGroupV2{GroupName: name, Rules: groups[name]})
	}
	for _, s := range p.Silences {
		doc.SilenceWindows = append(doc.SilenceWindows, SilenceV2{
			SilenceID: s.ID, Selectors: s.Matchers, StartIndex: s.StartT, EndIndex: s.EndT, Comment: s.Comment,
		})
	}
	return doc
}

func freezeLabelNode(n *mutableLabelNode) LabelNodeV2 {
	out := LabelNodeV2{LabelSetID: n.id, Labels: n.labels}
	for _, ch := range n.children {
		out.Children = append(out.Children, freezeLabelNode(ch))
	}
	return out
}
