package verify

import (
	"fmt"
	"reflect"

	"github.com/google/go-cmp/cmp"

	"samplingkernel/internal/engine"
	"samplingkernel/internal/loader"
	"samplingkernel/internal/model"
	"samplingkernel/internal/sample"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type Report struct {
	Bundle *sample.Bundle
	Checks []Check
}

func Run() (*Report, error) {
	bundle, err := sample.Generate()
	if err != nil {
		return nil, err
	}
	report := &Report{Bundle: bundle}
	add := func(name string, ok bool, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, OK: ok, Detail: detail})
	}

	canonical := engine.RunFull(bundle.Project)
	add("generator_truth_matches_kernel", engine.ReportsSemanticEqual(canonical, bundle.Truth),
		fmt.Sprintf("head=%d tail=%d budget=%d merge=%d", len(canonical.Head), len(canonical.Tail), len(canonical.Budget), len(canonical.Merge)))

	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data := bundle.Docs[version]
		loaded, err := loader.Load(data)
		if err != nil {
			add("load_"+version, false, err.Error())
			continue
		}
		normalized := *loaded
		normalized.SchemaVersion = ""
		projectEqual := reflect.DeepEqual(&normalized, bundle.Project)
		add("project_equivalence_"+version, projectEqual, cmp.Diff(bundle.Project, &normalized))

		loadedReport := engine.RunFull(loaded)
		semanticEqual := engine.ReportsSemanticEqual(loadedReport, bundle.Truth)
		add("observation_equivalence_"+version, semanticEqual, fmt.Sprintf("events=%d", len(loaded.Events)))
	}

	cache := engine.NewCache(bundle.Project)
	edits := []engine.Edit{
		{Type: "edge_rate", EdgeID: "e-checkout", RatePermyriad: 0},
		{Type: "edge_rate", EdgeID: "e-checkout", RatePermyriad: 10000},
		{Type: "edge_rate", EdgeID: "e-checkout", RatePermyriad: 4200},
		{Type: "allocation_weight", ServiceID: "pay", WeightPermyriad: 1000},
		{Type: "tail_condition", RuleID: "tr-latency", Condition: &model.TailCondition{Kind: "latency_ms", Op: "gt", Value: "200"}},
	}
	for i, edit := range edits {
		incremental, err := cache.Apply(edit)
		if err != nil {
			add(fmt.Sprintf("incremental_%d_%s", i, edit.Type), false, err.Error())
			continue
		}
		full := engine.RunFull(incremental.Project)
		equal := engine.ReportsSemanticEqual(incremental.Report, full)
		eventsEqual := reflect.DeepEqual(incremental.Report.Events, full.Events)
		detail := fmt.Sprintf("closure_nodes=%d reused=%d event_views_equal=%t ranges=%v",
			len(incremental.Closure.Nodes), len(incremental.Closure.Reused), eventsEqual, incremental.Closure.EventRanges)
		if !eventsEqual {
			detail += "\nevent diff: " + cmp.Diff(full.Events, incremental.Report.Events)
		}
		add(fmt.Sprintf("incremental_%d_%s", i, edit.Type), equal && eventsEqual, detail)
	}

	return report, nil
}

func (r *Report) OK() bool {
	for _, check := range r.Checks {
		if !check.OK {
			return false
		}
	}
	return true
}
