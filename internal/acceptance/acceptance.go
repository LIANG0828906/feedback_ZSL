package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/checkpoint"
	"mirrorcast/internal/engine"
	"mirrorcast/internal/generator"
	"mirrorcast/internal/incremental"
	"mirrorcast/internal/session"
	"mirrorcast/internal/verify"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	OK     bool           `json:"ok"`
	Seed   int64          `json:"seed"`
	Verify *verify.Report `json:"verify,omitempty"`
	Checks []Check        `json:"checks"`
}

func Run(root string) (*Report, error) {
	if err := os.RemoveAll(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	suite, err := generator.GenerateSuite(generator.DefaultSeed)
	if err != nil {
		return nil, err
	}
	verification, err := verify.VerifySuite(suite)
	if err != nil {
		return nil, err
	}
	report := &Report{OK: verification.OK, Seed: suite.Seed, Verify: verification}

	canonical := findProject(suite, "canonical-budget-topology")
	if canonical == nil {
		report.add("canonical project exists", false, "", "canonical project missing")
		return report, nil
	}

	report.Check(checkpointRecovery(filepath.Join(root, "recovery"), canonical.Project, canonical.Truth))
	report.Check(checkpointOrdering(filepath.Join(root, "ordering"), canonical.Project))
	report.Check(truncatedCheckpoint(filepath.Join(root, "truncated"), canonical.Project))
	report.Check(versionMismatchCheckpoint(filepath.Join(root, "version-mismatch"), canonical.Project))
	report.Check(schedulingOrder(canonical.Project, canonical.Truth))
	report.Check(leavePreservesRegions(canonical.Truth))
	report.Check(canonicalDecisions(canonical.Truth))
	report.Check(incrementalScaleChroma(root, canonical.Project))
	report.Check(incrementalScaleEdit(root, canonical.Project))
	report.Check(incrementalBudgetEdit(root, canonical.Project))
	report.Check(incrementalTopologyDelete(root, canonical.Project))
	report.Check(incrementalTopologyAppend(root, canonical.Project))
	report.Check(invalidIncrementalScale(canonical.Project))

	for _, check := range report.Checks {
		if !check.OK {
			report.OK = false
		}
	}
	return report, nil
}

func findTickResult(truth session.TruthFile, tick int64) *session.TickResult {
	for i := range truth.Ticks {
		if truth.Ticks[i].Tick == tick {
			return &truth.Ticks[i]
		}
	}
	return nil
}

func findScreenResult(tick *session.TickResult, id string) *session.ScreenResult {
	if tick == nil {
		return nil
	}
	for i := range tick.Results {
		if tick.Results[i].ScreenID == id {
			return &tick.Results[i]
		}
	}
	return nil
}

func leavePreservesRegions(truth session.TruthFile) Check {
	before := findTickResult(truth, 5)
	after := findTickResult(truth, 6)
	left := findScreenResult(after, "s02")
	if left == nil || left.Decision != session.DecisionLeft {
		return Check{Name: "leave releases budget without moving peers", OK: false, Code: "LEAVE_DECISION_MISSING"}
	}
	for _, id := range []string{"s01", "s03"} {
		b := findScreenResult(before, id)
		a := findScreenResult(after, id)
		if b == nil || a == nil || !reflect.DeepEqual(b.Mapping, a.Mapping) || b.BudgetCostKbps != a.BudgetCostKbps {
			return Check{Name: "leave releases budget without moving peers", OK: false, Code: "PEER_REGION_CHANGED", Detail: id}
		}
	}
	return Check{Name: "leave releases budget without moving peers", OK: true}
}

func canonicalDecisions(truth session.TruthFile) Check {
	join := findScreenResult(findTickResult(truth, 3), "s03")
	if join == nil || join.Decision != session.DecisionDowngraded || join.Constraint != "C-BUDGET-TOTAL" {
		return Check{Name: "canonical downgrade and rejection constraints", OK: false, Code: "DOWNGRADE_CONSTRAINT_MISSING"}
	}
	rejected := findScreenResult(findTickResult(truth, 4), "s04")
	if rejected == nil || rejected.Decision != session.DecisionRejected || rejected.ErrorCode != bizerror.EAdmissionRejected || rejected.Constraint != "C-MIN-QUALITY" {
		return Check{Name: "canonical downgrade and rejection constraints", OK: false, Code: "REJECTION_CONSTRAINT_MISSING"}
	}
	budgetTick := findTickResult(truth, 8)
	expectedScales := map[string]int64{"s01": 6250, "s03": 2500, "s04": 5000}
	for id, scale := range expectedScales {
		result := findScreenResult(budgetTick, id)
		if result == nil || !result.Active || result.Decision != session.DecisionDowngraded || result.SettledScale != scale {
			return Check{Name: "canonical downgrade and rejection constraints", OK: false, Code: "DOWNGRADE_LADDER_MISMATCH", Detail: id}
		}
	}
	return Check{Name: "canonical downgrade and rejection constraints", OK: true, Detail: "C-BUDGET-TOTAL then C-MIN-QUALITY"}
}

func (r *Report) add(name string, ok bool, code, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Code: code, Detail: detail})
}

func (r *Report) Check(check Check) {
	r.Checks = append(r.Checks, check)
}

func findProject(suite *generator.Suite, id string) *generator.GeneratedProject {
	for i := range suite.Projects {
		if suite.Projects[i].Project.ProjectID == id {
			return &suite.Projects[i]
		}
	}
	return nil
}

func checkpointRecovery(dir string, project session.Project, truth session.TruthFile) Check {
	eng, err := engine.New(project)
	if err != nil {
		return fail("checkpoint recovery", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.AdvanceOne(); err != nil {
			return fail("checkpoint recovery", err)
		}
	}
	store, err := checkpoint.NewStore(dir, project.ProjectID)
	if err != nil {
		return fail("checkpoint recovery", err)
	}
	if err := store.SaveState(eng.State(), nil); err != nil {
		return fail("checkpoint recovery", err)
	}

	cp, err := store.Load()
	if err != nil {
		return fail("checkpoint recovery", err)
	}
	var sum int64
	seen := map[string]bool{}
	for _, alloc := range cp.State.Allocations {
		if seen[alloc.ScreenID] {
			return Check{Name: "checkpoint recovery", OK: false, Code: "DUPLICATE_BUDGET", Detail: alloc.ScreenID}
		}
		seen[alloc.ScreenID] = true
		sum += alloc.BudgetCostKbps
	}
	if sum != cp.State.UsedBudget {
		return Check{Name: "checkpoint recovery", OK: false, Code: "BUDGET_SUM_MISMATCH"}
	}

	recovered, err := engine.New(project)
	if err != nil {
		return fail("checkpoint recovery", err)
	}
	if err := recovered.RestoreState(cp.State); err != nil {
		return fail("checkpoint recovery", err)
	}
	ticks, err := recovered.RunAll()
	if err != nil {
		return fail("checkpoint recovery", err)
	}
	if !reflect.DeepEqual(ticks, truth.Ticks[3:]) {
		return Check{Name: "checkpoint recovery", OK: false, Code: "TRUTH_MISMATCH", Detail: "recovered suffix differs from deterministic truth"}
	}

	journal, err := checkpoint.LoadJournal(dir)
	if err != nil {
		return fail("step journal resume", err)
	}
	steps := []string{"prepare_source", "map_planes", "send_regions", "settle_budget"}
	for _, step := range steps[:2] {
		if err := journal.MarkCompleted(4, "tick:4:s04", step); err != nil {
			return fail("step journal resume", err)
		}
	}
	reloaded, err := checkpoint.LoadJournal(dir)
	if err != nil {
		return fail("step journal resume", err)
	}
	if !reloaded.IsCompleted("tick:4:s04", "map_planes") || reloaded.IsCompleted("tick:4:s04", "send_regions") {
		return Check{Name: "step journal resume", OK: false, Code: "JOURNAL_RESUME_MISMATCH"}
	}
	for _, step := range steps[2:] {
		if err := reloaded.MarkCompleted(4, "tick:4:s04", step); err != nil {
			return fail("step journal resume", err)
		}
	}
	if got := len(reloaded.CompletedSteps("tick:4:s04")); got != len(steps) {
		return Check{Name: "step journal resume", OK: false, Code: "JOURNAL_STEP_COUNT", Detail: "expected 4 unique steps"}
	}
	return Check{Name: "checkpoint recovery and step resume", OK: true, Detail: "tick 3 checkpoint; ticks 4-8 match truth; journal skips completed steps"}
}

func checkpointOrdering(dir string, project session.Project) Check {
	eng, err := engine.New(project)
	if err != nil {
		return fail("checkpoint write ordering", err)
	}
	if _, err := eng.AdvanceOne(); err != nil {
		return fail("checkpoint write ordering", err)
	}
	state1 := eng.State()
	if _, err := eng.AdvanceOne(); err != nil {
		return fail("checkpoint write ordering", err)
	}
	store, err := checkpoint.NewStore(dir, project.ProjectID)
	if err != nil {
		return fail("checkpoint write ordering", err)
	}
	if err := store.SaveState(state1, nil); err != nil {
		return fail("checkpoint write ordering", err)
	}

	state2 := eng.State()
	global4, err := store.PrepareScreenStates(state2, nil)
	if err != nil {
		return fail("checkpoint write ordering", err)
	}
	beforeCommit, err := store.Load()
	if err != nil {
		return fail("checkpoint write ordering", err)
	}
	if beforeCommit.State.CommittedTick != state1.CommittedTick {
		return Check{Name: "checkpoint write ordering", OK: false, Code: "GLOBAL_MARKER_CHANGED_EARLY"}
	}
	if err := store.CommitGlobal(global4); err != nil {
		return fail("checkpoint write ordering", err)
	}
	afterCommit, err := store.Load()
	if err != nil {
		return fail("checkpoint write ordering", err)
	}
	if afterCommit.State.CommittedTick != state2.CommittedTick {
		return Check{Name: "checkpoint write ordering", OK: false, Code: "GLOBAL_MARKER_NOT_PUBLISHED"}
	}
	return Check{Name: "screen state precedes global tick marker", OK: true}
}

func truncatedCheckpoint(dir string, project session.Project) Check {
	eng, _ := engine.New(project)
	if _, err := eng.AdvanceOne(); err != nil {
		return fail("truncated checkpoint code", err)
	}
	store, err := checkpoint.NewStore(dir, project.ProjectID)
	if err != nil {
		return fail("truncated checkpoint code", err)
	}
	if err := store.SaveState(eng.State(), nil); err != nil {
		return fail("truncated checkpoint code", err)
	}
	globalPath := filepath.Join(dir, "checkpoint.json")
	info, err := os.Stat(globalPath)
	if err != nil {
		return fail("truncated checkpoint code", err)
	}
	if err := os.Truncate(globalPath, info.Size()/2); err != nil {
		return fail("truncated checkpoint code", err)
	}
	_, err = store.Load()
	return expectCode("truncated checkpoint code", err, bizerror.ECheckpointTruncated)
}

func versionMismatchCheckpoint(dir string, project session.Project) Check {
	eng, _ := engine.New(project)
	if _, err := eng.AdvanceOne(); err != nil {
		return fail("version mismatch checkpoint code", err)
	}
	store, err := checkpoint.NewStore(dir, project.ProjectID)
	if err != nil {
		return fail("version mismatch checkpoint code", err)
	}
	if err := store.SaveState(eng.State(), nil); err != nil {
		return fail("version mismatch checkpoint code", err)
	}
	globalPath := filepath.Join(dir, "checkpoint.json")
	data, err := os.ReadFile(globalPath)
	if err != nil {
		return fail("version mismatch checkpoint code", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return fail("version mismatch checkpoint code", err)
	}
	raw["format_version"] = 999
	bad, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fail("version mismatch checkpoint code", err)
	}
	if err := os.WriteFile(globalPath, bad, 0o644); err != nil {
		return fail("version mismatch checkpoint code", err)
	}
	_, loadErr := store.Load()
	check := expectCode("version mismatch checkpoint code", loadErr, bizerror.ECheckpointVersion)
	if !check.OK {
		return check
	}
	fresh, err := engine.New(project)
	if err != nil {
		return fail("version mismatch full recompute", err)
	}
	ticks, err := fresh.RunAll()
	if err != nil || len(ticks) != int(projectMaxTick(project)) {
		return Check{Name: "version mismatch forces full recompute", OK: false, Detail: "fresh replay did not complete"}
	}
	return Check{Name: "version mismatch forces full recompute", OK: true, Code: bizerror.ECheckpointVersion}
}

func schedulingOrder(project session.Project, truth session.TruthFile) Check {
	reversed := project
	reversed.Screens = append([]session.Screen(nil), project.Screens...)
	for i, j := 0, len(reversed.Screens)-1; i < j; i, j = i+1, j-1 {
		reversed.Screens[i], reversed.Screens[j] = reversed.Screens[j], reversed.Screens[i]
	}
	reversed.Events = append([]session.Event(nil), project.Events...)
	for i, j := 0, len(reversed.Events)-1; i < j; i, j = i+1, j-1 {
		reversed.Events[i], reversed.Events[j] = reversed.Events[j], reversed.Events[i]
	}
	eng, err := engine.New(reversed)
	if err != nil {
		return fail("scheduling order independence", err)
	}
	ticks, err := eng.RunAll()
	if err != nil {
		return fail("scheduling order independence", err)
	}
	if !reflect.DeepEqual(ticks, truth.Ticks) {
		return Check{Name: "scheduling order independence", OK: false, Code: "ORDER_DEPENDENT_RESULT"}
	}
	return Check{Name: "canonical sorting makes settlement order-independent", OK: true}
}

func incrementalScaleChroma(root string, project session.Project) Check {
	eval, err := incremental.NewEvaluator(project)
	if err != nil {
		return fail("incremental chroma propagation", err)
	}
	baseline, err := eval.Baseline()
	if err != nil {
		return fail("incremental chroma propagation", err)
	}
	report, err := eval.ApplyEdit(incremental.Edit{
		Kind: incremental.EditChroma, AtTick: 6, ScreenID: "s03", Chroma: session.Chroma444,
	})
	if err != nil {
		return fail("incremental chroma propagation", err)
	}
	if len(report.ReusedTicks) != 5 || report.DirtyFromTick != 6 {
		return Check{Name: "incremental chroma propagation", OK: false, Code: "INVALID_DIRTY_RANGE", Detail: "expected ticks 1-5 reused and tick 6 dirty"}
	}
	if !reflect.DeepEqual(report.Results[:5], baseline.Results[:5]) {
		return Check{Name: "incremental chroma propagation", OK: false, Code: "UNAFFECTED_RANGE_CHANGED"}
	}
	var sawGeometryMiss bool
	var sawSource bool
	for _, p := range report.Provenance {
		if p.ScreenID == "s03" && p.Tick == 6 && p.Status == "geometry_recomputed" {
			sawGeometryMiss = true
		}
		for _, source := range p.Sources {
			if source == "edit:chroma:s03:chroma=444" {
				sawSource = true
			}
		}
	}
	if !sawGeometryMiss || !sawSource {
		return Check{Name: "incremental chroma propagation", OK: false, Code: "MISSING_INVALIDATION_PROVENANCE"}
	}
	if report.CacheHits == 0 {
		return Check{Name: "incremental chroma propagation", OK: false, Code: "CACHE_NOT_REUSED"}
	}
	return Check{Name: "chroma edit invalidates only dependent suffix", OK: true, Detail: report.EditSource}
}

func incrementalTopologyDelete(root string, project session.Project) Check {
	eval, err := incremental.NewEvaluator(project)
	if err != nil {
		return fail("topology event deletion", err)
	}
	if _, err := eval.Baseline(); err != nil {
		return fail("topology event deletion", err)
	}
	report, err := eval.ApplyEdit(incremental.Edit{Kind: incremental.EditDeleteEvent, EventID: "e05"})
	if err != nil {
		return fail("topology event deletion", err)
	}
	if report.DirtyFromTick != 5 || len(report.ReusedTicks) != 4 {
		return Check{Name: "topology event deletion", OK: false, Code: "INVALID_DELETE_DIRTY_RANGE"}
	}
	var tick5 *session.TickResult
	for i := range report.Results {
		if report.Results[i].Tick == 5 {
			tick5 = &report.Results[i]
		}
	}
	if tick5 == nil || tick5.TopologyVersion != 1 {
		return Check{Name: "topology event deletion", OK: false, Code: "DELETED_TOPOLOGY_STILL_APPLIED"}
	}
	return Check{Name: "delete topology event recomputes event suffix only", OK: true}
}

func incrementalScaleEdit(root string, project session.Project) Check {
	eval, err := incremental.NewEvaluator(project)
	if err != nil {
		return fail("incremental scale propagation", err)
	}
	baseline, err := eval.Baseline()
	if err != nil {
		return fail("incremental scale propagation", err)
	}
	report, err := eval.ApplyEdit(incremental.Edit{
		Kind: incremental.EditScale, AtTick: 2, ScreenID: "s02", Scale: 5000,
	})
	if err != nil {
		return fail("incremental scale propagation", err)
	}
	if report.DirtyFromTick != 2 || len(report.ReusedTicks) != 1 {
		return Check{Name: "incremental scale propagation", OK: false, Code: "INVALID_SCALE_DIRTY_RANGE"}
	}
	if !reflect.DeepEqual(report.Results[:1], baseline.Results[:1]) {
		return Check{Name: "incremental scale propagation", OK: false, Code: "SCALE_UNAFFECTED_RANGE_CHANGED"}
	}
	var sawScaleMiss bool
	for _, p := range report.Provenance {
		if p.ScreenID == "s02" && p.Tick == 2 && p.Status == "geometry_recomputed" {
			sawScaleMiss = true
		}
	}
	if !sawScaleMiss {
		return Check{Name: "incremental scale propagation", OK: false, Code: "SCALE_PROVENANCE_MISSING"}
	}
	return Check{Name: "scale edit invalidates dependent suffix", OK: true}
}

func incrementalBudgetEdit(root string, project session.Project) Check {
	eval, err := incremental.NewEvaluator(project)
	if err != nil {
		return fail("incremental budget propagation", err)
	}
	baseline, err := eval.Baseline()
	if err != nil {
		return fail("incremental budget propagation", err)
	}
	report, err := eval.ApplyEdit(incremental.Edit{Kind: incremental.EditBudget, AtTick: 6, Budget: 50000})
	if err != nil {
		return fail("incremental budget propagation", err)
	}
	if report.DirtyFromTick != 6 || len(report.ReusedTicks) != 5 {
		return Check{Name: "incremental budget propagation", OK: false, Code: "INVALID_BUDGET_DIRTY_RANGE"}
	}
	if !reflect.DeepEqual(report.Results[:5], baseline.Results[:5]) {
		return Check{Name: "incremental budget propagation", OK: false, Code: "BUDGET_UNAFFECTED_RANGE_CHANGED"}
	}
	if report.CacheHits == 0 {
		return Check{Name: "incremental budget propagation", OK: false, Code: "BUDGET_GEOMETRY_CACHE_NOT_USED"}
	}
	return Check{Name: "budget edit recomputes settlement suffix", OK: true}
}

func incrementalTopologyAppend(root string, project session.Project) Check {
	eval, err := incremental.NewEvaluator(project)
	if err != nil {
		return fail("topology event append", err)
	}
	if _, err := eval.Baseline(); err != nil {
		return fail("topology event append", err)
	}
	topology := &session.Topology{Version: 3, Hosts: []session.TopologyHost{
		{ID: "h1", ScreenIDs: []string{"s01", "s02", "s03", "s04"}},
	}}
	event := &session.Event{ID: "e09-topology-v3", Tick: 9, Type: session.EventTopology, Topology: topology}
	report, err := eval.ApplyEdit(incremental.Edit{Kind: incremental.EditAddEvent, Event: event})
	if err != nil {
		return fail("topology event append", err)
	}
	if report.DirtyFromTick != 9 || len(report.RecomputedTicks) != 1 {
		return Check{Name: "topology event append", OK: false, Code: "INVALID_APPEND_DIRTY_RANGE"}
	}
	if len(report.Results) != 9 || report.Results[8].TopologyVersion != 3 {
		return Check{Name: "topology event append", OK: false, Code: "APPENDED_TOPOLOGY_MISSING"}
	}
	return Check{Name: "append topology event extends recomputation interval", OK: true}
}

func invalidIncrementalScale(project session.Project) Check {
	eval, err := incremental.NewEvaluator(project)
	if err != nil {
		return fail("invalid incremental scale code", err)
	}
	if _, err := eval.Baseline(); err != nil {
		return fail("invalid incremental scale code", err)
	}
	_, err = eval.ApplyEdit(incremental.Edit{Kind: incremental.EditScale, AtTick: 6, ScreenID: "s03", Scale: 0})
	return expectCode("invalid incremental scale code", err, bizerror.EScaleZero)
}

func expectCode(name string, err error, code string) Check {
	actual := bizerror.CodeOf(err)
	if actual != code {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		return Check{Name: name, OK: false, Code: actual, Detail: detail}
	}
	return Check{Name: name, OK: true, Code: code}
}

func fail(name string, err error) Check {
	return Check{Name: name, OK: false, Code: bizerror.CodeOf(err), Detail: err.Error()}
}

func projectMaxTick(project session.Project) int64 {
	var max int64
	for _, event := range project.Events {
		if event.Tick > max {
			max = event.Tick
		}
	}
	return max
}
