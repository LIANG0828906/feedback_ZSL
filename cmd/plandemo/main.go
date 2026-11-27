package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"planopt"
)

type demoReport struct {
	Seed                 int64                     `json:"seed"`
	SchemaEquivalence    string                    `json:"schema_equivalence"`
	Plans                []planopt.OptimizedPlan   `json:"plans"`
	Sensitivity          planopt.SensitivityBundle `json:"sensitivity"`
	IncrementalSteps     []planopt.IncrementalStep `json:"incremental_steps"`
	ParallelOutcomes     int                       `json:"parallel_outcomes"`
	CheckpointStates     int                       `json:"checkpoint_states"`
	InjectionCodes       map[string]string         `json:"injection_codes"`
	VerificationFailures []string                  `json:"verification_failures,omitempty"`
}

func main() {
	seed := flag.Int64("seed", 42, "deterministic generator seed")
	out := flag.String("out", "artifacts/demo/generated", "directory for generated schema documents and report")
	flag.Parse()

	report, failures := run(*seed, *out)
	if err := writeReport(*out, report); err != nil {
		fmt.Fprintf(os.Stderr, "write report failed: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("planopt demo seed=%d out=%s\n", *seed, *out)
	for _, p := range report.Plans {
		fmt.Printf("plan %d: join=%v cost=%d paths=%v\n", p.PlanSeq, p.JoinOrder, p.CostMilli, p.SelectedPath)
	}
	for _, s := range report.Sensitivity.Reports {
		if s.Changed {
			fmt.Printf("sensitivity plan %d changed paths=%v->%v triggers=%v costDelta=%d\n", s.PlanSeq, s.BeforePaths, s.AfterPaths, s.TriggerPredicateIDs, s.CostDeltaMilli)
		}
	}
	for _, code := range report.InjectionCodes {
		fmt.Printf("injection code: %s\n", code)
	}
	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "verification failures:")
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "- %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("all demo verification checks passed")
}

func run(seed int64, out string) (demoReport, []string) {
	var failures []string
	addFailure := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf(format, args...))
	}

	generated := planopt.Generate(seed)
	if err := os.MkdirAll(out, 0o755); err != nil {
		failures = append(failures, fmt.Sprintf("mkdir output: %v", err))
	}
	if truthData, err := json.MarshalIndent(generated.Truth, "", "  "); err == nil {
		if err := os.WriteFile(filepath.Join(out, "truth.json"), truthData, 0o644); err != nil {
			failures = append(failures, fmt.Sprintf("write truth: %v", err))
		}
	} else {
		failures = append(failures, fmt.Sprintf("marshal truth: %v", err))
	}
	docs := map[string][]byte{}
	marshalers := map[string]func(planopt.Project) ([]byte, error){
		"engineering-kernel-v1.json": planopt.MarshalV1,
		"engineering-kernel-v2.json": planopt.MarshalV2,
		"engineering-kernel-v3.json": planopt.MarshalV3,
	}
	loaded := map[string]planopt.Project{}
	for name, marshal := range marshalers {
		data, err := marshal(generated.Project)
		if err != nil {
			addFailure("marshal %s: %v", name, err)
			continue
		}
		docs[name] = data
		if err := os.WriteFile(filepath.Join(out, name), data, 0o644); err != nil {
			addFailure("write %s: %v", name, err)
		}
		project, err := planopt.LoadEngineeringJSON(data)
		if err != nil {
			addFailure("load %s: %v", name, err)
			continue
		}
		loaded[name] = project
		if err := planopt.EquivalentProject(generated.Project, project); err != nil {
			addFailure("equivalence %s: %v", name, err)
		}
	}

	baseBindings := planopt.DefaultBindings(generated.Project)
	base, err := planopt.OptimizeProject(generated.Project, baseBindings)
	if err != nil {
		addFailure("optimize canonical: %v", err)
	}
	for name, project := range loaded {
		other, err := planopt.OptimizeProject(project, planopt.DefaultBindings(project))
		if err != nil {
			addFailure("optimize %s: %v", name, err)
			continue
		}
		if !optimizationEqual(base, other) {
			addFailure("schema %s optimization did not converge", name)
		}
	}

	verifyTruth(generated.Truth, base, addFailure)
	verifyPruning(base, addFailure)

	globalScenario := map[int]int64{7: 1000}
	relativeScenario := map[int]map[int]int64{6: {1: 1000}}
	if err := planopt.GlobalRelativeBindingEquivalence(generated.Project, globalScenario, relativeScenario); err != nil {
		addFailure("global/relative binding equivalence: %v", err)
	}
	sensitivity, err := planopt.CompareBindings(generated.Project, planopt.BindingSet{}, planopt.BindingSet{Global: globalScenario})
	if err != nil {
		addFailure("sensitivity: %v", err)
	}

	incrementalSteps := verifyIncremental(generated.Project, baseBindings, addFailure)

	scheduler := planopt.NewScheduler(4, 64)
	outcomes := scheduler.RunAll(context.Background(), generated.Project, baseBindings)
	if len(outcomes) != len(generated.Project.Plans) {
		addFailure("parallel outcome count=%d", len(outcomes))
	}
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			addFailure("parallel plan %d: %s", outcome.PlanSeq, outcome.Err.Error())
		}
	}
	bufferScheduler := planopt.NewScheduler(2, 1)
	if _, err := bufferScheduler.TryProcessPlan(generated.Project, generated.Project.Plans[0], baseBindings); codeOf(err) != planopt.CodeBackpressureBuffer {
		addFailure("buffer backpressure code=%v", err)
	}
	workerScheduler := planopt.NewScheduler(1, 64)
	release, err := workerScheduler.AcquireWorker()
	if err != nil {
		addFailure("acquire worker: %v", err)
	} else {
		defer release()
		if _, err := workerScheduler.TryProcessPlan(generated.Project, generated.Project.Plans[0], baseBindings); codeOf(err) != planopt.CodeBackpressureWorkers {
			addFailure("worker backpressure code=%v", err)
		}
	}

	checkpointStates := verifyCheckpoint(out, base, addFailure)
	injectionCodes := verifyInjections(docs["engineering-kernel-v1.json"], out, addFailure)

	return demoReport{
		Seed: generated.Project.Seed, SchemaEquivalence: "v1=v2=v3 canonical node/ref/parameter model",
		Plans: base, Sensitivity: sensitivity, IncrementalSteps: incrementalSteps,
		ParallelOutcomes: len(outcomes), CheckpointStates: checkpointStates,
		InjectionCodes: injectionCodes, VerificationFailures: failures,
	}, failures
}

type failureAdder func(string, ...any)

func verifyTruth(truth planopt.Truth, plans []planopt.OptimizedPlan, addFailure failureAdder) {
	bySeq := map[int]planopt.OptimizedPlan{}
	for _, p := range plans {
		bySeq[p.PlanSeq] = p
	}
	for _, pt := range truth.Plans {
		opt := bySeq[pt.PlanSeq]
		if len(opt.Normalized.Operators) != len(pt.Operators) {
			addFailure("truth plan %d op count %d != %d", pt.PlanSeq, len(opt.Normalized.Operators), len(pt.Operators))
			continue
		}
		for i, opTruth := range pt.Operators {
			op := opt.Normalized.Operators[i]
			if op.Key != opTruth.Key || op.Ordinal != opTruth.Ordinal {
				addFailure("truth plan %d op %d mismatch %s/%s", pt.PlanSeq, i, op.Key, opTruth.Key)
			}
			if !reflect.DeepEqual(op.OutputColumns, opTruth.OutputColumns) {
				addFailure("truth plan %d op %d columns mismatch", pt.PlanSeq, i)
			}
		}
		if !reflect.DeepEqual(opt.Normalized.RetainedColumns, pt.RetainedColumns) {
			addFailure("truth plan %d retained columns mismatch", pt.PlanSeq)
		}
		for _, es := range opt.Estimates {
			ptPredicate := pt.Optimization.Predicates[es.PredicateID]
			declared := ptPredicate.SelectivityPPM
			if abs(es.SelectivityPPM-declared) > planopt.SelTolerancePPM {
				addFailure("truth plan %d predicate %s selectivity %d vs %d", pt.PlanSeq, es.PredicateID, es.SelectivityPPM, declared)
			}
		}
		if !reflect.DeepEqual(opt.JoinOrder, pt.Optimization.JoinOrder) || opt.CostMilli != pt.Optimization.CostMilli {
			addFailure("truth plan %d optimization mismatch", pt.PlanSeq)
		}
	}
}

func verifyPruning(plans []planopt.OptimizedPlan, addFailure failureAdder) {
	for _, p := range plans {
		pruned := map[string]bool{}
		for _, c := range p.Normalized.PrunedColumns {
			pruned[c] = true
		}
		for _, op := range p.Normalized.Operators {
			for _, c := range op.OutputColumns {
				if pruned[c] {
					addFailure("plan %d pruned column %s appears in op %s", p.PlanSeq, c, op.Key)
				}
			}
		}
	}
}

func verifyIncremental(project planopt.Project, bindings planopt.BindingSet, addFailure failureAdder) []planopt.IncrementalStep {
	engine, err := planopt.NewIncrementalEngine(project, bindings)
	if err != nil {
		addFailure("incremental engine: %v", err)
		return nil
	}
	customers, _ := project.Table("customers")
	segmentCol, _ := customers.Column("segment")
	segmentCol.NullPermyriad = 200
	p6, _ := project.Plan(6)
	var gt planopt.Predicate
	for _, p := range p6.Predicates {
		if p.ID == "p6_pred_order_gt" {
			gt = p
		}
	}
	gt.Op = "GTE"
	edits := []planopt.Edit{
		{Kind: planopt.EditTableStats, Table: "customers", Column: &segmentCol},
		{Kind: planopt.EditPredicate, PlanSeq: 6, PredicateID: gt.ID, Predicate: &gt},
		{Kind: planopt.EditOperatorParam, PlanSeq: 6, NodeID: "p6_limit", ParamKey: "limit_rows", ParamValue: 37},
	}
	allSteps := []planopt.IncrementalStep{}
	for _, edit := range edits {
		report, err := engine.ApplyEdits(edit)
		if err != nil {
			addFailure("incremental edit %s: %v", edit.Kind, err)
			continue
		}
		allSteps = append(allSteps, report.Steps...)
		full, err := planopt.OptimizeProject(engine.ProjectSnapshot(), bindings)
		if err != nil {
			addFailure("full reopt after %s: %v", edit.Kind, err)
			continue
		}
		if !reflect.DeepEqual(engine.Results(), full) {
			addFailure("incremental result after %s differs from full reoptimization", edit.Kind)
		}
		if len(report.Steps[0].PropagationClosure) == 0 {
			addFailure("incremental edit %s has empty propagation closure", edit.Kind)
		}
	}
	return allSteps
}

func verifyCheckpoint(out string, plans []planopt.OptimizedPlan, addFailure failureAdder) int {
	dir := filepath.Join(out, "checkpoint")
	store := planopt.NewFileCheckpoint(dir)
	engine, err := planopt.NewIncrementalEngine(planopt.Generate(42).Project, planopt.DefaultBindings(planopt.Generate(42).Project))
	if err != nil {
		addFailure("checkpoint cache: %v", err)
	}
	cache := engine.CacheSnapshot()
	for _, p := range plans {
		if err := store.CommitPlan(p, cache); err != nil {
			addFailure("checkpoint commit plan %d: %v", p.PlanSeq, err)
		}
	}
	recovered, err := store.Recover()
	if err != nil {
		addFailure("checkpoint recover: %v", err)
	}

	orphanDir := filepath.Join(out, "checkpoint-orphan")
	orphan := planopt.NewFileCheckpoint(orphanDir)
	if err := orphan.CommitPlan(plans[0], cache); err != nil {
		addFailure("orphan commit: %v", err)
	}
	if err := orphan.WritePlanState(plans[1]); err != nil {
		addFailure("orphan write state: %v", err)
	}
	orphanRecovered, err := orphan.Recover()
	if err != nil {
		addFailure("orphan recover: %v", err)
	}
	if len(orphanRecovered.States) != 1 || orphanRecovered.Manifest.CommittedSeq != 1 {
		addFailure("orphan state was visible: states=%d seq=%d", len(orphanRecovered.States), orphanRecovered.Manifest.CommittedSeq)
	}

	truncDir := filepath.Join(out, "checkpoint-truncated")
	trunc := planopt.NewFileCheckpoint(truncDir)
	if err := trunc.InjectTruncatedManifest(); err != nil {
		addFailure("inject truncation: %v", err)
	}
	if _, err := trunc.Recover(); codeOf(err) != planopt.CodeCheckpointTruncated {
		addFailure("truncated checkpoint code=%v", err)
	}

	versionDir := filepath.Join(out, "checkpoint-version")
	versioned := planopt.NewFileCheckpoint(versionDir)
	if err := versioned.InjectVersionMismatchSnapshot(); err != nil {
		addFailure("inject version: %v", err)
	}
	if _, err := versioned.Recover(); codeOf(err) != planopt.CodeCheckpointVersionMismatch {
		addFailure("version mismatch code=%v", err)
	}
	return len(recovered.States)
}

func verifyInjections(v1 []byte, out string, addFailure failureAdder) map[string]string {
	cases := map[string][]byte{
		planopt.CodeMalformedNodeTree:     planopt.InjectMalformedNodeTreeV1(v1),
		planopt.CodePlaceholderOutOfRange: planopt.InjectPlaceholderOutOfRangeV1(v1),
		planopt.CodeScaleOutOfRange:       planopt.InjectScaleOutOfRangeV1(v1),
		planopt.CodeFieldCombination:      planopt.InjectUnknownFieldCombinationV1(v1),
	}
	codes := map[string]string{}
	for expected, data := range cases {
		_, err := planopt.LoadEngineeringJSON(data)
		actual := codeOf(err)
		codes[expected] = actual
		if actual != expected {
			addFailure("injection expected %s got %v", expected, err)
		}
	}
	return codes
}

func writeReport(out string, report demoReport) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "demo-report.json"), data, 0o644)
}

func codeOf(err error) string {
	var pe *planopt.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	if err == nil {
		return planopt.CodeOK
	}
	return "E_UNCLASSIFIED"
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func optimizationEqual(a, b []planopt.OptimizedPlan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].PlanSeq != b[i].PlanSeq || a[i].CostMilli != b[i].CostMilli || !reflect.DeepEqual(a[i].JoinOrder, b[i].JoinOrder) || !reflect.DeepEqual(a[i].SelectedPath, b[i].SelectedPath) {
			return false
		}
		if !reflect.DeepEqual(normalizedKeys(a[i].Normalized), normalizedKeys(b[i].Normalized)) {
			return false
		}
	}
	return true
}

func normalizedKeys(n planopt.NormalizedPlan) []string {
	out := make([]string, 0, len(n.Operators))
	for _, op := range n.Operators {
		out = append(out, op.Key)
	}
	return out
}
