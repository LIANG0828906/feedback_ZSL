package diagnostic

import (
	"context"
	"time"

	"presentkernel/internal/checkpoint"
	"presentkernel/internal/domain"
	"presentkernel/internal/engine"
	"presentkernel/internal/generator"
	"presentkernel/internal/runtime"
	"presentkernel/internal/scheduler"
	"presentkernel/internal/schema"
)

type CrossVersionReport struct {
	Version      string        `json:"version"`
	OK           bool          `json:"ok"`
	TraceFields  int           `json:"trace_fields"`
	DefaultFills int           `json:"default_fills"`
	ExportLossy  int           `json:"export_lossy"`
	MatchesTruth bool          `json:"matches_truth"`
	LoadError    *domain.Error `json:"load_error,omitempty"`
	Mismatch     *domain.Error `json:"mismatch,omitempty"`
}

type FaultReport struct {
	Code         string        `json:"code"`
	Name         string        `json:"name"`
	Injection    string        `json:"injection"`
	ExpectedCode string        `json:"expected_code"`
	Error        *domain.Error `json:"error,omitempty"`
	OK           bool          `json:"ok"`
}

type State struct {
	ProjectID        string                      `json:"project_id"`
	Seed             int64                       `json:"seed"`
	Versions         []string                    `json:"versions"`
	CrossVersion     []CrossVersionReport        `json:"cross_version"`
	Verification     *runtime.VerificationReport `json:"verification"`
	Graph            *engine.DependencyGraph     `json:"graph"`
	Faults           []FaultReport               `json:"faults"`
	Isolation        *runtime.Snapshot           `json:"isolation"`
	QueueBudgetDemo  *scheduler.Report           `json:"queue_budget_demo"`
	WorkerBufferDemo *scheduler.Report           `json:"worker_buffer_demo"`
	CheckpointStatus *checkpoint.Status          `json:"checkpoint_status"`
	GeneratedSample  *generator.SampleSet        `json:"-"`
}

func BuildState(dbPath string) (*State, error) {
	set, err := generator.GenerateSampleSet()
	if err != nil {
		return nil, err
	}
	st := &State{ProjectID: set.Canonical.ProjectID, Seed: set.Seed, GeneratedSample: set}
	versions := []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3}
	st.Versions = versions

	truthSnap := snapshotFromResults(set.TruthManifest.Partitions)
	for _, v := range versions {
		data := set.V1
		if v == domain.SchemaV2 {
			data = set.V2
		} else if v == domain.SchemaV3 {
			data = set.V3
		}
		report := CrossVersionReport{Version: v}
		loaded, lerr := schema.LoadProject(data)
		if lerr != nil {
			if de, ok := domain.AsError(lerr); ok {
				report.LoadError = de
			}
			st.CrossVersion = append(st.CrossVersion, report)
			continue
		}
		report.TraceFields = len(loaded.Trace.Fields)
		report.DefaultFills = len(loaded.Trace.Fills)
		if er := set.ExportReports[v]; er != nil {
			report.ExportLossy = len(er.Lossy)
		}
		results, berr := engine.BuildAll(loaded.Project, engine.Options{FPS: loaded.Project.TargetFPS})
		if berr != nil {
			if de, ok := domain.AsError(berr); ok {
				report.Mismatch = de
			}
			st.CrossVersion = append(st.CrossVersion, report)
			continue
		}
		loadedSnap := snapshotFromResults(results)
		mismatch := runtime.CompareSnapshots("truth", truthSnap, "loaded-"+v, loadedSnap)
		report.Mismatch = mismatch
		report.MatchesTruth = mismatch == nil
		report.OK = mismatch == nil
		st.CrossVersion = append(st.CrossVersion, report)
	}

	edits := []engine.Edit{
		{Kind: "char_weight", DialogueID: "D0", PartitionIndex: 0, CharIndex: 2, Value: 1400},
		{Kind: "segment_duration", DialogueID: "D1", PartitionIndex: 1, Value: 48},
		{Kind: "target_fps", Value: 30},
	}
	verification, err := runtime.VerifyEditSequence(set.Canonical, edits, dbPath, scheduler.Config{MaxParallel: 3, PerWorkerBuffer: 2, QueueBudget: 4})
	if err != nil {
		return nil, err
	}
	st.Verification = verification
	st.Graph = engine.BuildGraph(set.Canonical, set.Canonical.TargetFPS)
	st.Faults = buildFaultReports(set.Canonical)

	isolationProject := runtime.CloneProject(set.Canonical)
	isolation, err := runtime.RunParallel(context.Background(), isolationProject, "fault-isolation", runtime.RunConfig{
		Scheduler: scheduler.Config{MaxParallel: 2, PerWorkerBuffer: 1, QueueBudget: 3},
		Faults:    map[string]string{"D0-P0": engine.FaultStagePanic},
	})
	if err != nil {
		return nil, err
	}
	st.Isolation = isolation
	st.QueueBudgetDemo = buildQueueBudgetDemo()
	st.WorkerBufferDemo = buildWorkerBufferDemo()

	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	status, err := store.Status(verification.RunID)
	_ = store.Close()
	if err != nil {
		return nil, err
	}
	st.CheckpointStatus = status
	return st, nil
}

func snapshotFromResults(results []*domain.PartitionResult) *runtime.Snapshot {
	s := &runtime.Snapshot{Results: results}
	for _, r := range results {
		s.Sequence = append(s.Sequence, r.PartitionID)
	}
	return s
}

func buildFaultReports(base *domain.Project) []FaultReport {
	reports := []FaultReport{}
	run := func(name, injection, expected string, mutate func(p *domain.Project), fault string) {
		p := runtime.CloneProject(base)
		if mutate != nil {
			mutate(p)
		}
		_, err := engine.BuildPartition(p, 0, 0, engine.Options{FPS: p.TargetFPS, Fault: fault})
		de, ok := domain.AsError(err)
		reports = append(reports, FaultReport{
			Name: name, Injection: injection, ExpectedCode: expected, Error: de,
			OK: ok && de.Code == expected,
		})
	}
	run("phoneme order inversion", engine.FaultOrder, domain.CodeOrder, nil, engine.FaultOrder)
	run("fps/sample-rate contradiction", engine.FaultRate, domain.CodeRate, nil, engine.FaultRate)
	run("lip category out of table", engine.FaultLipCategory, domain.CodeLip, nil, engine.FaultLipCategory)
	run("transition tolerance exceeded", engine.FaultLipTolerance, domain.CodeLip, nil, engine.FaultLipTolerance)
	run("partition internal panic", engine.FaultStagePanic, domain.CodeStage, nil, engine.FaultStagePanic)
	run("unreadable compressed interval", "duration_samples=4000", domain.CodeRead, func(p *domain.Project) {
		p.Dialogues[0].Segments[0].DurationFrames = 2
		p.Dialogues[0].Segments[0].DurationSamples = 4000
	}, "")
	return reports
}

func buildQueueBudgetDemo() *scheduler.Report {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	go func() {
		<-started
		time.Sleep(40 * time.Millisecond)
		close(release)
	}()
	jobs := []scheduler.Job{
		budgetJob(0, "QUEUE-0", func() { started <- struct{}{}; <-release }),
		budgetJob(1, "QUEUE-1", func() {}),
		budgetJob(2, "QUEUE-2", func() {}),
	}
	return scheduler.RunJobs(context.Background(), jobs, scheduler.Config{MaxParallel: 1, PerWorkerBuffer: 1, QueueBudget: 1})
}

func buildWorkerBufferDemo() *scheduler.Report {
	release := make(chan struct{})
	go func() {
		time.Sleep(40 * time.Millisecond)
		close(release)
	}()
	jobs := []scheduler.Job{
		budgetJob(0, "BUFFER-0", func() { <-release }),
		budgetJob(1, "BUFFER-1", func() {}),
		budgetJob(2, "BUFFER-2", func() {}),
	}
	return scheduler.RunJobs(context.Background(), jobs, scheduler.Config{MaxParallel: 1, PerWorkerBuffer: 1, QueueBudget: 3})
}

func budgetJob(index int, id string, fn func()) scheduler.Job {
	return scheduler.Job{
		Index: index, Ref: domain.PartitionRef{DialogueID: "BUDGET", PartitionID: id},
		Run: func(ctx context.Context) (*domain.PartitionResult, error) {
			fn()
			return &domain.PartitionResult{DialogueID: "BUDGET", PartitionID: id, PartitionIndex: index}, nil
		},
	}
}
