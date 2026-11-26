package maskqc

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

type CheckResult struct {
	Name     string `json:"name"`
	Code     string `json:"code,omitempty"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
	ChunkID  int    `json:"chunk_id,omitempty"`
	StrokeID string `json:"stroke_id,omitempty"`
}

type SampleSummary struct {
	ID       string `json:"id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Chunks   int    `json:"chunks"`
	Strokes  int    `json:"strokes"`
	Groups   int    `json:"groups"`
	Domains  int    `json:"domains"`
	V1Loaded bool   `json:"v1_loaded"`
	V2Loaded bool   `json:"v2_loaded"`
	V3Loaded bool   `json:"v3_loaded"`
	V1Lossy  int    `json:"v1_lossy"`
}

type ThreePathSet struct {
	Live         *RunResult `json:"live"`
	Recovery     *RunResult `json:"recovery"`
	Full         *RunResult `json:"full"`
	LiveVsRec    *Mismatch  `json:"live_vs_recovery,omitempty"`
	LiveVsFull   *Mismatch  `json:"live_vs_full,omitempty"`
	OK           bool       `json:"ok"`
	CrashCommits int        `json:"crash_commits"`
}

type EditStepReport struct {
	Index            int              `json:"index"`
	Description      string           `json:"description"`
	Edits            []Edit           `json:"edits"`
	Plan             InvalidationPlan `json:"plan"`
	ReusedChunks     []int            `json:"reused_chunks"`
	RecomputedChunks []int            `json:"recomputed_chunks"`
	ReuseMatchesPlan bool             `json:"reuse_matches_plan"`
}

type WorkbenchReport struct {
	Seed             int64            `json:"seed"`
	Workdir          string           `json:"workdir"`
	SelectedSampleID string           `json:"selected_sample_id"`
	Samples          []SampleSummary  `json:"samples"`
	Checks           []CheckResult    `json:"checks"`
	ThreePath        ThreePathSet     `json:"three_path"`
	EditSteps        []EditStepReport `json:"edit_steps"`
	EditedThreePath  ThreePathSet     `json:"edited_three_path"`
	DependencyGraph  DependencyGraph  `json:"dependency_graph"`
	QueueBudgetDemo  []QueueEvent     `json:"queue_budget_demo"`
	AcceptanceOK     bool             `json:"acceptance_ok"`
}

func WriteJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func CloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Strokes = append([]Stroke(nil), p.Strokes...)
	cp.Groups = append([]StrokeGroup(nil), p.Groups...)
	cp.Trace = append([]TraceEntry(nil), p.Trace...)
	cp.Lossy = append([]LossyItem(nil), p.Lossy...)
	cp.Candidate = p.Candidate.Clone()
	cp.Icon = nil // Edit/invalidation tests do not need to duplicate RGBA bytes.
	if p.Truth != nil {
		cp.Truth = &Truth{Mask: p.Truth.Mask.Clone(), Domains: append([]DomainTruth(nil), p.Truth.Domains...)}
	}
	return &cp
}

func intSet(ids []int) map[int]bool {
	m := map[int]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func sameIntSet(a []int, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[int]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
		if m[x] < 0 {
			return false
		}
	}
	return true
}

func RunThreePath(p *Project, baseDir string, opts SchedulerOptions, crashCommits int) (*ThreePathSet, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	if crashCommits <= 0 {
		crashCommits = 3
	}
	set := &ThreePathSet{CrashCommits: crashCommits}

	liveStore, err := OpenStore(filepath.Join(baseDir, "live"))
	if err != nil {
		return nil, err
	}
	live, err := RunPipeline(PipelineConfig{Label: "live-incremental", Project: p, Store: liveStore, Scheduler: opts})
	_ = liveStore.Close()
	if err != nil {
		return nil, err
	}
	set.Live = live

	recDir := filepath.Join(baseDir, "recovery")
	crashStore, err := OpenStore(recDir)
	if err != nil {
		return nil, err
	}
	_, crashErr := RunPipeline(PipelineConfig{
		Label: "recovery-crash-prefix", Project: p, Store: crashStore, Scheduler: opts,
		CrashAfterComponentCommits: crashCommits,
	})
	_ = crashStore.Close()
	if !errors.Is(crashErr, ErrSimulatedCrash) {
		return nil, fmt.Errorf("expected simulated crash, got %w", crashErr)
	}
	recStore, err := OpenStore(recDir)
	if err != nil {
		return nil, err
	}
	recovery, err := RunPipeline(PipelineConfig{
		Label: "checkpoint-recovery", Project: p, Store: recStore, Scheduler: opts,
		ReusePersistent: true,
	})
	_ = recStore.Close()
	if err != nil {
		return nil, err
	}
	set.Recovery = recovery

	fullStore, err := OpenStore(filepath.Join(baseDir, "full"))
	if err != nil {
		return nil, err
	}
	full, err := RunPipeline(PipelineConfig{Label: "one-shot-full", Project: p, Store: fullStore, Scheduler: opts})
	_ = fullStore.Close()
	if err != nil {
		return nil, err
	}
	set.Full = full

	set.LiveVsRec = CompareResults(live, recovery)
	set.LiveVsFull = CompareResults(live, full)
	set.OK = set.LiveVsRec == nil && set.LiveVsFull == nil
	return set, nil
}

func RunEditSequence(p *Project, dir string, opts SchedulerOptions) ([]EditStepReport, *Project, error) {
	store, err := OpenStore(dir)
	if err != nil {
		return nil, nil, err
	}
	base := CloneProject(p)
	initial, err := RunPipeline(PipelineConfig{Label: "edit-initial", Project: base, Store: store, Scheduler: opts})
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	if initial.Fatal != nil {
		_ = store.Close()
		return nil, nil, initial.Fatal
	}
	if err := store.Close(); err != nil {
		return nil, nil, err
	}

	current := base
	steps := []struct {
		desc   string
		edit   Edit
		mutate func(*Project)
	}{
		{
			desc: "extend s1 end coordinate by 3 pixels",
			edit: Edit{Type: "update_end", StrokeID: "s1"},
			mutate: func(next *Project) {
				for i := range next.Strokes {
					if next.Strokes[i].ID == "s1" {
						next.Strokes[i].End.X += 3
					}
				}
			},
		},
		{
			desc: "change s2 hardness from 65 to 35",
			edit: Edit{Type: "update_hardness", StrokeID: "s2"},
			mutate: func(next *Project) {
				for i := range next.Strokes {
					if next.Strokes[i].ID == "s2" {
						next.Strokes[i].HardnessPct = 35
					}
				}
			},
		},
		{
			desc: "insert s4 short diagonal stroke",
			edit: Edit{Type: "insert", StrokeID: "s4"},
			mutate: func(next *Project) {
				next.Strokes = append(next.Strokes, Stroke{
					ID: "s4", Seq: 4, Priority: 40, Group: "accent",
					Start: Point{X: next.Width * 3 / 4, Y: next.Height - 8},
					End:   Point{X: next.Width*3/4 + 5, Y: next.Height - 4},
					Width: 4, HardnessPct: 90, WeightMyriad: 8500,
					DirectionClass: DirDiagonal,
				})
				next.Groups = append(next.Groups, StrokeGroup{ID: "inserted", Kind: "edit_group", StrokeIDs: []string{"s4"}})
			},
		},
	}

	reports := []EditStepReport{}
	for i, step := range steps {
		next := CloneProject(current)
		step.mutate(next)
		step.edit.NewStroke = nil
		plan := PlanEditInvalidation(current, next, NewChunkGrid(next.Width, next.Height, next.TileWidth, next.TileHeight), []Edit{step.edit})
		stepStore, err := OpenStore(dir)
		if err != nil {
			return reports, next, err
		}
		run, err := RunPipeline(PipelineConfig{
			Label: fmt.Sprintf("edit-%d", i+1), Project: next, Store: stepStore, Scheduler: opts,
			ReusePersistent: true, InvalidChunks: intSet(plan.InvalidChunks),
		})
		closeErr := stepStore.Close()
		if err != nil {
			return reports, next, err
		}
		if closeErr != nil {
			return reports, next, closeErr
		}
		reports = append(reports, EditStepReport{
			Index: i + 1, Description: step.desc, Edits: []Edit{step.edit}, Plan: plan,
			ReusedChunks: run.ReusedChunks, RecomputedChunks: run.RecomputedChunks,
			ReuseMatchesPlan: sameIntSet(run.ReusedChunks, plan.ReusableChunks) &&
				sameIntSet(run.RecomputedChunks, plan.InvalidChunks),
		})
		current = next
	}
	return reports, current, nil
}

func QueueBudgetBlockingDemo() []QueueEvent {
	sched := NewScheduler(SchedulerOptions{Parallelism: 1, PixelBufferCap: 4096, QueueBudget: 1})
	started := make(chan struct{})
	release := make(chan struct{})
	tasks := []Task{
		{Index: 0, ChunkID: 900, PixelCount: 16, Run: func() error { close(started); <-release; return nil }},
		{Index: 1, ChunkID: 901, PixelCount: 16, Run: func() error { <-release; return nil }},
		{Index: 2, ChunkID: 902, PixelCount: 16, Run: func() error { <-release; return nil }},
	}
	done := make(chan []error, 1)
	go func() { done <- sched.Run(tasks) }()
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-done
	return sched.Events()
}

func BuildWorkbenchReport(workdir string) (*WorkbenchReport, error) {
	for _, name := range []string{"three-path", "edited-three-path", "edit-checkpoint", "state-check"} {
		if err := os.RemoveAll(filepath.Join(workdir, name)); err != nil {
			return nil, err
		}
	}
	set, err := GenerateSamples(DefaultSeed)
	if err != nil {
		return nil, err
	}
	selected := set.Samples[len(set.Samples)-1].Project
	opts := SchedulerOptions{Parallelism: 2, PixelBufferCap: 4096, QueueBudget: 1}
	report := &WorkbenchReport{
		Seed: DefaultSeed, Workdir: workdir, SelectedSampleID: selected.ID,
		QueueBudgetDemo: QueueBudgetBlockingDemo(),
	}

	for _, sample := range set.Samples {
		g := NewChunkGrid(sample.Project.Width, sample.Project.Height, sample.Project.TileWidth, sample.Project.TileHeight)
		summary := SampleSummary{
			ID: sample.Project.ID, Width: sample.Project.Width, Height: sample.Project.Height,
			Chunks: g.Count, Strokes: len(sample.Project.Strokes), Groups: len(sample.Project.Groups),
			Domains:  len(sample.Project.Truth.Domains),
			V1Loaded: sample.Loaded[SchemaV1] != nil, V2Loaded: sample.Loaded[SchemaV2] != nil, V3Loaded: sample.Loaded[SchemaV3] != nil,
		}
		if res, err := ExportProject(sample.Project, SchemaV1); err == nil {
			summary.V1Lossy = len(res.Lossy)
		}
		report.Samples = append(report.Samples, summary)
	}

	report.Checks = append(report.Checks, schemaChecks(set)...)
	report.Checks = append(report.Checks, conversionChecks()...)
	report.Checks = append(report.Checks, stateCheck(filepath.Join(workdir, "state-check")))
	report.Checks = append(report.Checks, injectionChecks(selected, opts)...)
	report.Checks = append(report.Checks, topologyChecks(selected)...)

	three, err := RunThreePath(selected, filepath.Join(workdir, "three-path"), opts, 3)
	if err != nil {
		return nil, err
	}
	report.ThreePath = *three

	editDir := filepath.Join(workdir, "edit-checkpoint")
	steps, edited, err := RunEditSequence(selected, editDir, opts)
	if err != nil {
		return nil, err
	}
	report.EditSteps = steps
	editedThree, err := RunThreePath(edited, filepath.Join(workdir, "edited-three-path"), opts, 4)
	if err != nil {
		return nil, err
	}
	report.EditedThreePath = *editedThree
	g := NewChunkGrid(selected.Width, selected.Height, selected.TileWidth, selected.TileHeight)
	report.DependencyGraph = BuildDependencyGraph(selected, g)

	report.AcceptanceOK = report.ThreePath.OK && report.EditedThreePath.OK
	for _, step := range report.EditSteps {
		if !step.ReuseMatchesPlan {
			report.AcceptanceOK = false
		}
	}
	for _, check := range report.Checks {
		if !check.OK {
			report.AcceptanceOK = false
		}
	}
	return report, nil
}

func schemaChecks(set *SampleSet) []CheckResult {
	checks := []CheckResult{}
	_, err := LoadProject([]byte(`{"schema":"mask-qc/v99","id":"bad"}`))
	checks = append(checks, expectBiz("unknown schema version returns E_SCHEMA", CodeSchema, err))
	_, err = LoadProject([]byte(`{"schema":"` + SchemaV3 + `"}`))
	checks = append(checks, expectBiz("missing required id returns E_SCHEMA with path", CodeSchema, err))
	allRoundtrip := true
	detail := "v1/v2/v3 generated docs loaded"
	for _, s := range set.Samples {
		if s.Loaded[SchemaV1] == nil || s.Loaded[SchemaV2] == nil || s.Loaded[SchemaV3] == nil {
			allRoundtrip = false
		}
	}
	checks = append(checks, CheckResult{Name: "three schema versions load", OK: allRoundtrip, Detail: detail})
	res, err := ExportProject(set.Samples[0].Project, SchemaV1)
	lossyOK := err == nil && len(res.Lossy) > 0
	checks = append(checks, CheckResult{Name: "v1 downgrade reports E_LOSSY items", Code: CodeLossy, OK: lossyOK, Detail: fmt.Sprintf("%d lossy items", len(res.Lossy))})
	return checks
}

func expectBiz(name, code string, err error) CheckResult {
	be, ok := AsBizError(err)
	okCode := ok && be.Code == code
	detail := "ok"
	if err != nil {
		detail = err.Error()
	}
	return CheckResult{Name: name, Code: code, OK: okCode, Detail: detail, ChunkID: func() int {
		if be != nil {
			return be.ChunkID
		}
		return -1
	}(), StrokeID: func() string {
		if be != nil {
			return be.StrokeID
		}
		return ""
	}()}
}

func conversionChecks() []CheckResult {
	checks := []CheckResult{}
	w, rem, tr := MyriadToPermille(1234)
	restored, err := RestoreMyriad(w, rem)
	ok := err == nil && w == 123 && rem == 4 && restored == 1234 && tr.Remainder == 4
	checks = append(checks, CheckResult{Name: "weight integer remainder roundtrip", OK: ok, Detail: fmt.Sprintf("1234 -> %d permille rem %d -> %d", w, rem, restored)})

	st, en, _, err := AbsoluteToOffsets(Point{X: 53, Y: 41}, Point{X: 80, Y: 59}, 7, "conv", DefaultCoordinateRule)
	var backS, backE Point
	if err == nil {
		backS, backE, _, err = OffsetsToAbsolute(st, en, 7, "conv", DefaultCoordinateRule)
	}
	ok = err == nil && backS == Point{X: 53, Y: 41} && backE == Point{X: 80, Y: 59}
	checks = append(checks, CheckResult{Name: "sequence offset coordinate roundtrip", OK: ok, Detail: fmt.Sprintf("offsets=%+v,%+v", st, en)})

	_, err = CheckedMul64(math.MaxInt64, 2, "seq*step", "overflow")
	checks = append(checks, expectBiz("integer overflow returns E_OVF with operands", CodeOVF, err))

	bad := Stroke{ID: "bad-param", HardnessPct: 121, WeightMyriad: 5000, Width: 3}
	checks = append(checks, expectBiz("hardness out of range returns E_PARA", CodePara, ValidateStrokeParameters(bad)))
	return checks
}

func stateCheck(dir string) CheckResult {
	store, err := OpenStore(dir)
	if err != nil {
		return CheckResult{Name: "illegal stage transition returns E_STATE", Code: CodeState, OK: false, Detail: err.Error()}
	}
	defer store.Close()
	_, err = store.TransitionStage(4242, PhaseCoverage, StageCommitted, "illegal jump")
	return expectBiz("illegal stage transition returns E_STATE", CodeState, err)
}

func injectionChecks(p *Project, opts SchedulerOptions) []CheckResult {
	checks := []CheckResult{}
	r, err := RunPipeline(PipelineConfig{Label: "size-injection", Project: p, Scheduler: opts, Injection: Injection{CandidateSizeMismatch: true}})
	checks = append(checks, resultCheck("candidate size mismatch returns E_SIZE and location", CodeSize, r, err))

	r, err = RunPipeline(PipelineConfig{Label: "range-injection", Project: p, Scheduler: opts, Injection: Injection{OutOfRangeStrokeID: "s1"}})
	checks = append(checks, resultCheck("coordinate out of range returns E_RANGE and location", CodeRange, r, err))

	r, err = RunPipeline(PipelineConfig{Label: "block-injection", Project: p, Scheduler: SchedulerOptions{Parallelism: 1, PixelBufferCap: 4096, QueueBudget: 1}, Injection: Injection{FailEnabled: true, FailChunkID: 1, FailStrokeID: "s1"}})
	checks = append(checks, resultCheck("injected chunk failure returns E_BLOCK and location", CodeBlock, r, err))
	return checks
}

func resultCheck(name, code string, r *RunResult, err error) CheckResult {
	check := expectBiz(name, code, err)
	if r != nil {
		if be, ok := AsBizError(err); ok {
			check.ChunkID = be.ChunkID
			check.StrokeID = be.StrokeID
			check.Detail = be.Error()
		}
	}
	if code == CodeBlock {
		check.OK = check.OK && check.ChunkID == 1 && check.StrokeID == "s1"
	}
	if code == CodeRange {
		check.OK = check.OK && check.StrokeID == "s1" && check.ChunkID >= 0
	}
	if code == CodeSize {
		check.OK = check.OK && check.ChunkID >= 0
	}
	return check
}

func topologyChecks(p *Project) []CheckResult {
	checks := []CheckResult{}
	badTruth := *p.Truth
	badDomains := append([]DomainTruth(nil), p.Truth.Domains...)
	if len(badDomains) > 0 {
		badDomains[0].Holes += 99
	}
	badTruth.Domains = badDomains
	_, err := VerifyTopology(p.Truth.Mask, &badTruth, p.Strokes, DefaultTolerance())
	checks = append(checks, expectBiz("topology deviation returns E_TOP with domain and both values", CodeTop, err))

	r, runErr := RunPipeline(PipelineConfig{Label: "topology-sample", Project: p, Scheduler: DefaultSchedulerOptions()})
	if runErr != nil {
		checks = append(checks, CheckResult{Name: "sample pipeline executes topology verification", OK: false, Detail: runErr.Error()})
	} else {
		detected := r.TopologyError != nil && IsCode(r.TopologyError, CodeTop)
		checks = append(checks, CheckResult{
			Name: "seeded candidate defects are diagnosed as E_TOP", Code: CodeTop,
			OK: detected, Detail: topologyReason(r.TopologyError),
		})
	}
	return checks
}

func WritePathReports(workdir string, report *WorkbenchReport) error {
	if report == nil {
		return NewError(CodeSchema, "nil report")
	}
	if err := WriteJSONFile(filepath.Join(workdir, "path-live.json"), report.ThreePath.Live); err != nil {
		return err
	}
	if err := WriteJSONFile(filepath.Join(workdir, "path-recovery.json"), report.ThreePath.Recovery); err != nil {
		return err
	}
	if err := WriteJSONFile(filepath.Join(workdir, "path-full.json"), report.ThreePath.Full); err != nil {
		return err
	}
	return WriteJSONFile(filepath.Join(workdir, "diagnostics.json"), report)
}

func WriteGeneratedSchemaDocs(workdir string) error {
	set, err := GenerateSamples(DefaultSeed)
	if err != nil {
		return err
	}
	short := map[string]string{SchemaV1: "v1", SchemaV2: "v2", SchemaV3: "v3"}
	for _, sample := range set.Samples {
		for version, data := range sample.Docs {
			path := filepath.Join(workdir, "schema-docs", sample.Project.ID+"."+short[version]+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}
