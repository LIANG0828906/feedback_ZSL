package sample

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"

	"alertkernel/engine"
	"alertkernel/model"
	"alertkernel/schema"
)

const Seed = int64(20261126)

type Bundle struct {
	Seed          int64             `json:"seed"`
	Project       *model.Project    `json:"project"`
	Events        model.EventBundle `json:"events"`
	Docs          map[string][]byte `json:"docs"`
	CanonicalJSON []byte            `json:"canonical_json"`
	Truth         *engine.RunReport `json:"truth"`
}

func Generate() (*Bundle, error) {
	rng := rand.New(rand.NewSource(Seed))
	projectID := fmt.Sprintf("kernel-%08x", rng.Int63())
	base := schema.DefaultBaseEpoch
	step := int64(15000)
	steps := 10

	p := &model.Project{
		ProjectID: projectID,
		Clock:     model.Clock{BaseEpochMS: base, StepMS: step, Steps: steps},
		Numeric:   model.NumericConfig{Scale: 100, Bits: 64, OverflowPolicy: "error"},
		Budget:    model.Budget{Workers: 4, PerRuleBuffer: 16},
		Receivers: []model.Receiver{{Name: "ops", Kind: "webhook"}, {Name: "db", Kind: "webhook"}, {Name: "fallback", Kind: "log"}},
		Routes: []model.Route{
			{ID: "route-db", Order: 10, Receiver: "db", Matchers: []model.Matcher{{Label: "service", Op: "=", Value: "db"}}, BasisLabel: "database alerts"},
			{ID: "route-ops", Order: 20, Receiver: "ops", Matchers: []model.Matcher{{Label: "env", Op: "=", Value: "prod"}}, BasisLabel: "production alerts"},
			{ID: "route-default", Order: 100, Receiver: "fallback"},
		},
		Policy: model.Policy{ReasonPriority: []string{"suppressed", "silenced"}, NoDataAction: "reset_to_inactive"},
		Inhibitions: []model.InhibitionRule{{
			ID: "INH-DB", Priority: 100,
			SourceMatchers: []model.Matcher{{Label: "alertname", Op: "=", Value: "SourceActive"}},
			TargetMatchers: []model.Matcher{{Label: "alertname", Op: "=", Value: "TargetHigh"}},
			EqualLabels:    []string{"env"},
		}},
		Silences: []model.SilenceWindow{{
			ID: "SIL-DB-EVENING", Matchers: []model.Matcher{{Label: "service", Op: "=", Value: "db"}},
			StartT: 7, EndT: 8, Comment: "deterministic maintenance window",
		}},
	}

	p.Series = []*model.Series{
		makeSeries("cpu-h1", "cpu_usage", model.Labels{"env": "prod", "region": "cn", "service": "api", "host": "h1"},
			[]int64{4000, 8500, 9000, 9100, 7000, 6800, 6500, 6200, 6000, 5800}, base, step),
		makeSeries("cpu-h2", "cpu_usage", model.Labels{"env": "prod", "region": "cn", "service": "api", "host": "h2"},
			[]int64{4100, 8300, 8800, 9200, 6900, 6700, 6400, 6100, 5900, 5700}, base, step),
		{
			ID: "cpu-h3-gap", Metric: "cpu_usage",
			Labels:  model.Labels{"env": "prod", "region": "cn", "service": "api", "host": "h3"},
			Samples: []model.Sample{{Index: 0, AtMS: base, Raw: 4200}},
		},
		makeSeries("disk-db1", "disk_used", model.Labels{"env": "prod", "service": "db", "host": "db1", "tier": "database"},
			[]int64{5000, 5500, 9500, 9600, 7000, 6900, 6800, 6700, 6600, 6500}, base, step),
		makeSeries("source-chaos", "source_signal", model.Labels{"env": "prod", "component": "chaos"},
			[]int64{0, 0, 0, 0, 0, 1, 1, 1, 0, 0}, base, step),
		makeSeries("target-db0", "target_load", model.Labels{"env": "prod", "service": "db", "instance": "db-0", "tier": "database"},
			[]int64{4000, 4100, 4200, 4300, 9500, 9600, 9700, 9800, 9900, 4400}, base, step),
	}

	p.Rules = []*model.Rule{
		{
			ID: "R_CPU", Group: "compute", Priority: 20,
			Predicate: model.Predicate{Metric: "cpu_usage", Matchers: []model.Matcher{{Label: "env", Op: "=", Value: "prod"}}, Op: ">", Threshold: 8000},
			ForMS:     30000, KeepFiringMS: 30000, Receiver: "ops", GroupBy: []string{"alertname", "env"},
			LabelRewrite: map[string]string{"alertname": "CPUHigh", "env": "${env}", "host": "${host}", "severity": "critical"},
		},
		{
			ID: "R_DISK", Group: "storage", Priority: 10,
			Predicate: model.Predicate{Metric: "disk_used", Op: ">", Threshold: 9000},
			ForMS:     0, KeepFiringMS: 0, Receiver: "db", GroupBy: []string{"alertname", "env", "service", "host"},
			LabelRewrite: map[string]string{"alertname": "DiskHigh", "env": "${env}", "service": "${service}", "host": "${host}", "tier": "${tier}"},
		},
		{
			ID: "R_SOURCE", Group: "suppression", Priority: 40,
			Predicate: model.Predicate{Metric: "source_signal", Op: ">", Threshold: 0},
			ForMS:     0, KeepFiringMS: 0, Receiver: "ops", GroupBy: []string{"alertname", "env"},
			LabelRewrite: map[string]string{"alertname": "SourceActive", "env": "${env}"},
		},
		{
			ID: "R_TARGET", Group: "suppression", Priority: 30,
			Predicate: model.Predicate{Metric: "target_load", Op: ">", Threshold: 9000},
			ForMS:     0, KeepFiringMS: 0, Receiver: "db", GroupBy: []string{"alertname", "env", "service", "instance"},
			LabelRewrite: map[string]string{"alertname": "TargetHigh", "env": "${env}", "service": "${service}", "instance": "${instance}", "tier": "${tier}"},
		},
	}

	if err := schema.Canonicalize(p); err != nil {
		return nil, err
	}
	events := model.EventBundle{SuppressionWindows: []model.SuppressionWindow{{
		ID: "SEV-INH-DB-05", InhibitionID: "INH-DB", StartT: 5, EndT: 7, SourceRuleID: "R_SOURCE",
		Comment: "deterministic source-firing suppression interval",
	}}}

	truth, err := engine.RunAll(context.Background(), p, events)
	if err != nil {
		return nil, err
	}
	canonicalJSON, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	docs := map[string][]byte{}
	if docs["v1"], err = json.MarshalIndent(schema.ToV1(p), "", "  "); err != nil {
		return nil, err
	}
	if docs["v2"], err = json.MarshalIndent(schema.ToV2(p), "", "  "); err != nil {
		return nil, err
	}
	if docs["v3"], err = json.MarshalIndent(schema.ToV3(p), "", "  "); err != nil {
		return nil, err
	}
	return &Bundle{Seed: Seed, Project: p, Events: events, Docs: docs, CanonicalJSON: canonicalJSON, Truth: truth}, nil
}

func makeSeries(id, metric string, labels model.Labels, values []int64, base, stepMS int64) *model.Series {
	s := &model.Series{ID: id, Metric: metric, Labels: labels}
	for i, raw := range values {
		s.Samples = append(s.Samples, model.Sample{Index: i, AtMS: base + int64(i)*stepMS, Raw: raw})
	}
	return s
}
