package acceptance

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"performanceexpressions/workspace/checkpoint"
	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
	"performanceexpressions/workspace/generator"
	"performanceexpressions/workspace/session"
	"performanceexpressions/workspace/workbench"
)

type Options struct {
	SampleDir string
	DBPath    string
	Seed      int64
	Params    domain.Parameters
}

type Check struct {
	Name         string   `json:"name"`
	OK           bool     `json:"ok"`
	ExpectedCode string   `json:"expected_code,omitempty"`
	ActualCode   string   `json:"actual_code,omitempty"`
	SegmentID    string   `json:"segment_id,omitempty"`
	ChannelID    string   `json:"channel_id,omitempty"`
	LayerID      string   `json:"layer_id,omitempty"`
	Channels     []string `json:"channels,omitempty"`
	Message      string   `json:"message,omitempty"`
}

type ProjectReport struct {
	File             string           `json:"file"`
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Hash             string           `json:"hash"`
	Valid            bool             `json:"valid"`
	Segments         int              `json:"segments"`
	Samples          int              `json:"samples"`
	Decisions        int              `json:"decisions"`
	Tolerance        float64          `json:"tolerance"`
	PassedPoints     int              `json:"passed_points"`
	FailedPoints     int              `json:"failed_points"`
	SourceMismatches int              `json:"source_mismatches"`
	SourceMatchRate  float64          `json:"source_match_rate"`
	MaxAbsError      float64          `json:"max_abs_error"`
	Monotonic        bool             `json:"tolerance_monotonic"`
	ToleranceSets    []ToleranceLevel `json:"tolerance_sets"`
}

type ToleranceLevel struct {
	Tolerance float64 `json:"tolerance"`
	Passing   int     `json:"passing_points"`
}

type RecoveryCheck struct {
	Name            string   `json:"name"`
	OK              bool     `json:"ok"`
	Invalidated     bool     `json:"invalidated"`
	ReplayedBatches int      `json:"replayed_batches"`
	LoadedBatches   int      `json:"loaded_batches"`
	CheckpointCount int      `json:"checkpoint_count"`
	Reasons         []string `json:"reasons,omitempty"`
	ResultEqual     bool     `json:"result_equal"`
	FirstDifference string   `json:"first_difference,omitempty"`
}

type PoolCheck struct {
	OK          bool  `json:"ok"`
	Submitted   int64 `json:"submitted"`
	Completed   int64 `json:"completed"`
	Blocked     int64 `json:"blocked_submits"`
	MaxInFlight int64 `json:"max_in_flight"`
	MaxWorkers  int   `json:"max_workers"`
	BufferSize  int   `json:"per_worker_buffer"`
}

type Report struct {
	GeneratedAt  time.Time                 `json:"generated_at"`
	Seed         int64                     `json:"seed"`
	SampleDir    string                    `json:"sample_dir"`
	DBPath       string                    `json:"db_path"`
	Parameters   domain.Parameters         `json:"parameters"`
	Projects     []ProjectReport           `json:"projects"`
	Faults       []Check                   `json:"faults"`
	Duplicate    Check                     `json:"duplicate_commit"`
	Isolation    Check                     `json:"uncommitted_isolation"`
	ThreePaths   workbench.ThreePathReport `json:"three_paths"`
	Recovery     RecoveryCheck             `json:"recovery"`
	Invalidation RecoveryCheck             `json:"invalidation_replay"`
	Concurrency  PoolCheck                 `json:"concurrency"`
	OK           bool                      `json:"ok"`
	Notes        []string                  `json:"notes,omitempty"`
}

func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.Seed == 0 {
		opts.Seed = generator.DefaultSeed
	}
	if opts.Params.Interpolation == "" {
		opts.Params = domain.DefaultParameters()
	}
	if opts.SampleDir == "" {
		opts.SampleDir = "samples"
	}
	if opts.DBPath == "" {
		opts.DBPath = "artifacts/expression-checkpoints.db"
	}
	report := Report{
		GeneratedAt: time.Now().UTC(),
		Seed:        opts.Seed,
		SampleDir:   opts.SampleDir,
		DBPath:      opts.DBPath,
		Parameters:  opts.Params,
	}
	index, err := generator.WriteSamples(opts.SampleDir, opts.Seed)
	if err != nil {
		return report, err
	}
	projects := generator.GenerateProjects(opts.Seed)
	for i, project := range projects {
		file := index.Projects[i].File
		projectReport, err := inspectProject(project, file)
		report.Projects = append(report.Projects, projectReport)
		if err != nil {
			report.OK = false
			return report, err
		}
	}
	base := projects[0]
	report.Faults = inspectFaults(base, opts.Params)
	report.Duplicate = inspectDuplicate(base, opts.Params)
	report.Isolation = inspectIsolation(base, opts.Params)
	trace, err := acceptanceTrace(base)
	if err != nil {
		return report, err
	}
	threePaths, err := workbench.RunThreePaths(ctx, base, opts.Params, trace)
	report.ThreePaths = threePaths
	if err != nil {
		report.OK = false
		return report, err
	}
	recovery, err := inspectRecovery(ctx, opts, base, trace, threePaths)
	report.Recovery = recovery
	if err != nil {
		report.OK = false
		return report, err
	}
	invalidation, err := inspectInvalidation(ctx, opts, base, trace, threePaths)
	report.Invalidation = invalidation
	if err != nil {
		report.OK = false
		return report, err
	}
	report.Concurrency = inspectPool(ctx)
	report.OK = allChecksOK(report)
	return report, nil
}

func inspectProject(project domain.Project, file string) (ProjectReport, error) {
	hash, err := domain.HashJSON(project)
	if err != nil {
		return ProjectReport{}, err
	}
	r := ProjectReport{File: file, ID: project.ID, Name: project.Name, Hash: hash, Segments: len(project.Segments)}
	if err := engine.ValidateProject(project, engine.ValidateOptions{RequireTruth: true}); err != nil {
		return r, err
	}
	r.Valid = true
	result, err := engine.EvaluateProject(project, domain.DefaultParameters())
	if err != nil {
		return r, err
	}
	for _, seg := range result.Segments {
		r.Samples += len(seg.Samples)
		for _, sample := range seg.Samples {
			r.Decisions += len(sample.Decisions)
		}
	}
	tolerance := 1e-9
	precision, _ := engine.ValidatePrecision(project, result, tolerance)
	r.Tolerance = tolerance
	r.PassedPoints = precision.PassedPoints
	r.FailedPoints = precision.FailedPoints
	r.SourceMismatches = precision.SourceMismatches
	r.SourceMatchRate = precision.SourceMatchRate
	for _, point := range precision.Points {
		if point.AbsError > r.MaxAbsError {
			r.MaxAbsError = point.AbsError
		}
	}
	levels := []float64{1e-6, 1e-9, 1e-12}
	var previous map[engine.PointKey]struct{}
	r.Monotonic = true
	for _, level := range levels {
		levelReport, _ := engine.ValidatePrecision(project, result, level)
		set := engine.PassingPointSet(levelReport)
		r.ToleranceSets = append(r.ToleranceSets, ToleranceLevel{Tolerance: level, Passing: len(set)})
		if previous != nil && !engine.SetIsSubset(set, previous) {
			r.Monotonic = false
		}
		previous = set
	}
	return r, nil
}

func inspectFaults(project domain.Project, params domain.Parameters) []Check {
	kinds := []session.FaultKind{
		session.FaultPriorityCycle,
		session.FaultPosition,
		session.FaultBatchConflict,
		session.FaultCommit,
	}
	expected := map[session.FaultKind]string{
		session.FaultPriorityCycle: domain.CodeCycle,
		session.FaultPosition:      domain.CodePosition,
		session.FaultBatchConflict: domain.CodeConflict,
		session.FaultCommit:        domain.CodeCommit,
	}
	var checks []Check
	for _, kind := range kinds {
		sess, err := session.New(project, params)
		if err != nil {
			checks = append(checks, Check{Name: string(kind), OK: false, ExpectedCode: expected[kind], Message: err.Error()})
			continue
		}
		healthyEdit := healthyEditOp(project, "healthy-"+string(kind))
		if healthyEdit != nil {
			_ = sess.ApplyEdit(*healthyEdit)
			_, _ = sess.CommitBatch(context.Background(), "healthy-"+string(kind), []string{healthyEdit.SegmentID})
		}
		if err := sess.InjectFault(session.Fault{Kind: kind, SegmentID: project.Segments[0].ID}); err != nil {
			checks = append(checks, Check{Name: string(kind), OK: false, ExpectedCode: expected[kind], Message: err.Error()})
			continue
		}
		before := domain.HashJSONMust(sess.CommittedProject())
		_, commitErr := sess.CommitSegment(context.Background(), "fault-"+string(kind), project.Segments[0].ID)
		after := domain.HashJSONMust(sess.CommittedProject())
		appErr, _ := domain.AsAppError(commitErr)
		check := Check{Name: string(kind), ExpectedCode: expected[kind], ActualCode: domain.CodeOf(commitErr), OK: commitErr != nil && domain.CodeOf(commitErr) == expected[kind] && before == after}
		if appErr != nil {
			check.SegmentID = appErr.SegmentID
			check.ChannelID = appErr.ChannelID
			check.LayerID = appErr.LayerID
			check.Channels = append([]string(nil), appErr.Channels...)
			check.Message = appErr.Message
		}
		states := sess.States()
		var failedOne bool
		for _, state := range states {
			if state.SegmentID == project.Segments[0].ID && state.Failed {
				failedOne = true
			}
			if state.SegmentID != project.Segments[0].ID && state.LastError != nil {
				if !strings.HasPrefix(string(kind), "healthy-") {
					check.OK = false
				}
			}
		}
		check.OK = check.OK && failedOne
		if err := sess.RetrySegment(project.Segments[0].ID); err == nil {
			if _, retryErr := sess.CommitSegment(context.Background(), "retry-"+string(kind), project.Segments[0].ID); retryErr != nil {
				check.OK = false
				check.Message += "; retry failed: " + retryErr.Error()
			}
		} else {
			check.OK = false
			check.Message += "; retry: " + err.Error()
		}
		checks = append(checks, check)
	}
	return checks
}

func inspectDuplicate(project domain.Project, params domain.Parameters) Check {
	sess, err := session.New(project, params)
	if err != nil {
		return Check{Name: "duplicate batch", OK: false, ExpectedCode: domain.CodeDuplicate, Message: err.Error()}
	}
	first, err := sess.CommitSegment(context.Background(), "duplicate-batch", project.Segments[0].ID)
	if err != nil {
		return Check{Name: "duplicate batch", OK: false, ExpectedCode: domain.CodeDuplicate, Message: first.BatchID + ": " + err.Error()}
	}
	before := domain.HashJSONMust(sess.CommittedProject())
	_, secondErr := sess.CommitSegment(context.Background(), "duplicate-batch", project.Segments[0].ID)
	after := domain.HashJSONMust(sess.CommittedProject())
	return Check{
		Name:         "duplicate batch",
		OK:           domain.CodeOf(secondErr) == domain.CodeDuplicate && before == after,
		ExpectedCode: domain.CodeDuplicate,
		ActualCode:   domain.CodeOf(secondErr),
		SegmentID:    project.Segments[0].ID,
		Message:      "duplicate submission must preserve committed model",
	}
}

func inspectIsolation(project domain.Project, params domain.Parameters) Check {
	sess, err := session.New(project, params)
	if err != nil {
		return Check{Name: "uncommitted isolation", OK: false, Message: err.Error()}
	}
	seg := project.Segments[0]
	ch, _, ok := seg.ChannelByID("velocity.main")
	if !ok || len(ch.Layers) == 0 || len(ch.Layers[0].Points) < 2 {
		return Check{Name: "uncommitted isolation", OK: false, Message: "sample lacks velocity.main/base point"}
	}
	beat := ch.Layers[0].Points[0].Beat
	beforePlayback, err := sess.PlaybackAtTick(seg.ID, int(beat*float64(project.SamplesPerBeat)))
	if err != nil {
		return Check{Name: "uncommitted isolation", OK: false, Message: err.Error()}
	}
	beforeExport := domain.HashJSONMust(sess.CommittedProject())
	op := domain.EditOp{
		Kind: domain.EditMovePoint, SegmentID: seg.ID, ChannelID: "velocity.main", LayerID: ch.Layers[0].ID,
		PointIndex: 0, Beat: beat, Value: ch.Layers[0].Points[0].Value + 0.125, BatchID: "isolation-uncommitted",
	}
	if err := sess.ApplyEdit(op); err != nil {
		return Check{Name: "uncommitted isolation", OK: false, Message: err.Error()}
	}
	afterPlayback, err := sess.PlaybackAtTick(seg.ID, int(beat*float64(project.SamplesPerBeat)))
	if err != nil {
		return Check{Name: "uncommitted isolation", OK: false, Message: err.Error()}
	}
	afterExport := domain.HashJSONMust(sess.CommittedProject())
	diff, diffErr := sess.DiffWorkingAgainstCommitted()
	playbackSame := fmt.Sprintf("%v", beforePlayback.Values) == fmt.Sprintf("%v", afterPlayback.Values)
	return Check{
		Name:      "uncommitted isolation",
		OK:        beforeExport == afterExport && playbackSame && domain.CodeOf(diffErr) == domain.CodeDiff && diff.Differences > 0,
		SegmentID: seg.ID,
		Message:   "export hash and committed playback must not change before commit; working diff detected",
	}
}

func acceptanceTrace(project domain.Project) (workbench.Trace, error) {
	if len(project.Segments) < 3 {
		return workbench.Trace{}, domain.NewError(domain.CodeSchema, "acceptance requires at least three segments")
	}
	seg0 := project.Segments[0]
	ch0, _, ok := seg0.ChannelByID("velocity.main")
	if !ok || len(ch0.Layers) == 0 || len(ch0.Layers[0].Points) < 2 {
		return workbench.Trace{}, domain.NewError(domain.CodeSchema, "sample lacks velocity.main/base point")
	}
	basePoint := ch0.Layers[0].Points[1]
	op1 := domain.EditOp{
		Kind: domain.EditMovePoint, SegmentID: seg0.ID, ChannelID: ch0.ID, LayerID: ch0.Layers[0].ID,
		PointIndex: 1, Beat: basePoint.Beat, Value: round4(basePoint.Value + 0.045),
		BatchID: "accept-batch-01",
	}

	seg1 := project.Segments[1]
	op2 := domain.EditOp{
		Kind: domain.EditAddLayer, SegmentID: seg1.ID, ChannelID: "velocity.main", BatchID: "accept-batch-02",
		Layer: &domain.Layer{
			ID: "user.shimmer",
			Points: []domain.ControlPoint{
				{Beat: seg1.StartBeat, Value: 0.02},
				{Beat: round4(seg1.StartBeat + (seg1.EndBeat-seg1.StartBeat)*0.33), Value: 0.07},
				{Beat: round4(seg1.StartBeat + (seg1.EndBeat-seg1.StartBeat)*0.67), Value: 0.03},
				{Beat: seg1.EndBeat, Value: 0.01},
			},
		},
	}

	seg2 := project.Segments[2]
	rules := []domain.PriorityRule{
		{Higher: "velocity.accent", Lower: "velocity.main", Basis: "accent-track-overrides-main"},
		{Higher: "vibrato.main", Lower: "vibrato.delay", Basis: "main-vibrato-overrides-delay"},
		{Higher: "velocity.accent", Lower: "vibrato.main", Basis: "user-priority-adjustment"},
		{Higher: "pitchbend.main", Lower: "vibrato.main", Basis: "expression-balance-rank"},
		{Higher: "sustain.main", Lower: "pitchbend.main", Basis: "expression-balance-rank"},
	}
	op3 := domain.EditOp{
		Kind: domain.EditSetPriority, SegmentID: seg2.ID, Priorities: rules, BatchID: "accept-batch-03",
	}
	return workbench.Trace{Name: "move-point/add-layer/priority-adjust", Operations: []domain.EditOp{op1, op2, op3}}, nil
}

func inspectRecovery(ctx context.Context, opts Options, base domain.Project, trace workbench.Trace, three workbench.ThreePathReport) (RecoveryCheck, error) {
	if err := resetDB(opts.DBPath); err != nil {
		return RecoveryCheck{}, err
	}
	store, err := checkpoint.Open(opts.DBPath)
	if err != nil {
		return RecoveryCheck{}, err
	}
	if err := store.EnsureProject(ctx, base); err != nil {
		_ = store.Close()
		return RecoveryCheck{}, err
	}
	sess, err := session.New(base, opts.Params, session.Options{Sink: store})
	if err != nil {
		_ = store.Close()
		return RecoveryCheck{}, err
	}
	if _, err := workbench.ApplyTraceToSession(ctx, sess, trace.Operations); err != nil {
		_ = store.Close()
		return RecoveryCheck{}, err
	}
	expected := sess.CommittedResult()
	if err := store.Close(); err != nil {
		return RecoveryCheck{}, err
	}
	reopened, err := checkpoint.Open(opts.DBPath)
	if err != nil {
		return RecoveryCheck{}, err
	}
	defer reopened.Close()
	resumed, recovery, err := workbench.Recover(ctx, reopened, base, opts.Params)
	if err != nil {
		return RecoveryCheck{}, err
	}
	count, err := reopened.Count(ctx, base.ID)
	if err != nil {
		return RecoveryCheck{}, err
	}
	check := RecoveryCheck{
		Name:            "valid checkpoint resume",
		Invalidated:     recovery.Invalidated,
		LoadedBatches:   recovery.LoadedBatches,
		ReplayedBatches: recovery.ReplayedBatches,
		CheckpointCount: count,
		Reasons:         recovery.Reasons,
	}
	cmp, cmpErr := engine.CompareResultSets(expected, resumed.CommittedResult())
	check.ResultEqual = cmp.Equal
	if cmpErr != nil && cmp.First != nil {
		check.FirstDifference = fmt.Sprintf("%s tick=%d field=%s left=%v right=%v", cmp.First.SegmentID, cmp.First.Tick, cmp.First.Field, cmp.First.Left, cmp.First.Right)
	}
	check.OK = !recovery.Invalidated && recovery.LoadedBatches == len(traceOperationsBatches(trace.Operations)) && check.ResultEqual && count == len(traceOperationsBatches(trace.Operations))
	return check, nil
}

func inspectInvalidation(ctx context.Context, opts Options, base domain.Project, trace workbench.Trace, three workbench.ThreePathReport) (RecoveryCheck, error) {
	dbPath := opts.DBPath + ".invalidation"
	if err := resetDB(dbPath); err != nil {
		return RecoveryCheck{}, err
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return RecoveryCheck{}, err
	}
	if err := store.EnsureProject(ctx, base); err != nil {
		_ = store.Close()
		return RecoveryCheck{}, err
	}
	sess, err := session.New(base, opts.Params, session.Options{Sink: store})
	if err != nil {
		_ = store.Close()
		return RecoveryCheck{}, err
	}
	if _, err := workbench.ApplyTraceToSession(ctx, sess, trace.Operations); err != nil {
		_ = store.Close()
		return RecoveryCheck{}, err
	}
	expected := sess.CommittedResult()
	if err := store.Close(); err != nil {
		return RecoveryCheck{}, err
	}
	if err := tamperSQLite(ctx, dbPath, "UPDATE checkpoints SET param_version='tampered-version'"); err != nil {
		return RecoveryCheck{}, err
	}
	reopened, err := checkpoint.Open(dbPath)
	if err != nil {
		return RecoveryCheck{}, err
	}
	defer reopened.Close()
	resumed, recovery, err := workbench.Recover(ctx, reopened, base, opts.Params)
	if err != nil {
		return RecoveryCheck{}, err
	}
	count, err := reopened.Count(ctx, base.ID)
	if err != nil {
		return RecoveryCheck{}, err
	}
	check := RecoveryCheck{
		Name:            "version mismatch invalidate and replay",
		Invalidated:     recovery.Invalidated,
		LoadedBatches:   recovery.LoadedBatches,
		ReplayedBatches: recovery.ReplayedBatches,
		CheckpointCount: count,
		Reasons:         recovery.Reasons,
	}
	cmp, cmpErr := engine.CompareResultSets(expected, resumed.CommittedResult())
	check.ResultEqual = cmp.Equal
	if cmpErr != nil && cmp.First != nil {
		check.FirstDifference = fmt.Sprintf("%s tick=%d field=%s left=%v right=%v", cmp.First.SegmentID, cmp.First.Tick, cmp.First.Field, cmp.First.Left, cmp.First.Right)
	}
	check.OK = recovery.Invalidated && len(recovery.Reasons) > 0 && recovery.ReplayedBatches == len(traceOperationsBatches(trace.Operations)) && check.ResultEqual
	return check, nil
}

func inspectPool(ctx context.Context) PoolCheck {
	const maxWorkers = 3
	const buffer = 1
	const jobs = 9
	pool := session.NewPool(ctx, maxWorkers, buffer)
	var executed atomic.Int64
	for i := 0; i < jobs; i++ {
		if err := pool.Submit(ctx, func(context.Context) error {
			executed.Add(1)
			time.Sleep(20 * time.Millisecond)
			return nil
		}); err != nil {
			pool.CloseAndWait()
			return PoolCheck{OK: false, Submitted: int64(i)}
		}
	}
	pool.CloseAndWait()
	stats := pool.Stats()
	return PoolCheck{
		OK:          stats.Submitted == jobs && stats.Completed == jobs && executed.Load() == jobs && stats.MaxInFlight <= maxWorkers && stats.MaxInFlight > 1 && stats.Blocked > 0,
		Submitted:   stats.Submitted,
		Completed:   stats.Completed,
		Blocked:     stats.Blocked,
		MaxInFlight: stats.MaxInFlight,
		MaxWorkers:  maxWorkers,
		BufferSize:  buffer,
	}
}

func healthyEditOp(project domain.Project, batchID string) *domain.EditOp {
	if len(project.Segments) < 2 {
		return nil
	}
	seg := project.Segments[1]
	ch, _, ok := seg.ChannelByID("velocity.main")
	if !ok || len(ch.Layers) == 0 || len(ch.Layers[0].Points) == 0 {
		return nil
	}
	point := ch.Layers[0].Points[0]
	return &domain.EditOp{
		Kind: domain.EditMovePoint, SegmentID: seg.ID, ChannelID: ch.ID, LayerID: ch.Layers[0].ID,
		PointIndex: 0, Beat: point.Beat, Value: point.Value + 0.031, BatchID: batchID,
	}
}

func resetDB(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func tamperSQLite(ctx context.Context, path, statement string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, statement)
	return err
}

func traceOperationsBatches(ops []domain.EditOp) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, op := range ops {
		if !seen[op.BatchID] {
			seen[op.BatchID] = true
			ids = append(ids, op.BatchID)
		}
	}
	sort.Strings(ids)
	return ids
}

func allChecksOK(report Report) bool {
	for _, project := range report.Projects {
		if !project.Valid || project.FailedPoints != 0 || project.SourceMismatches != 0 || !project.Monotonic {
			return false
		}
	}
	for _, check := range report.Faults {
		if !check.OK {
			return false
		}
	}
	return report.Duplicate.OK && report.Isolation.OK && report.ThreePaths.Equal && report.Recovery.OK && report.Invalidation.OK && report.Concurrency.OK
}

func WriteJSON(path string, report Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func WriteMarkdown(path string, report Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Acceptance Report\n\n")
	fmt.Fprintf(&b, "- Generated: `%s`\n", report.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- Seed: `%d`\n", report.Seed)
	fmt.Fprintf(&b, "- Samples: `%s`\n", report.SampleDir)
	fmt.Fprintf(&b, "- SQLite: `%s`\n", report.DBPath)
	fmt.Fprintf(&b, "- Parameters: interpolation=`%s`, tolerance=`%g`, param_version=`%s`, algorithm=`%s`\n", report.Parameters.Interpolation, report.Parameters.Tolerance, report.Parameters.ParamVersion, report.Parameters.AlgorithmVersion)
	fmt.Fprintf(&b, "- Overall: **%s**\n\n", passFail(report.OK))

	b.WriteString("## Generated projects\n\n")
	b.WriteString("| Project | Segments | Samples | Decisions | Passed | Failed | Source mismatch | Source match rate | Max abs error | Tolerance monotonic |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---|\n")
	for _, project := range report.Projects {
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %d | %d | %d | %.2f%% | %.3g | %s |\n",
			project.ID, project.Segments, project.Samples, project.Decisions, project.PassedPoints,
			project.FailedPoints, project.SourceMismatches, project.SourceMatchRate*100, project.MaxAbsError, passFail(project.Monotonic))
	}

	b.WriteString("\n## Error-chain injection\n\n")
	b.WriteString("| Fault | Expected | Actual | Segment | Channel | Layer | Involved channels | Result |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, check := range report.Faults {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | `%s` | `%s` | `%s` | `%s` | %s |\n",
			check.Name, check.ExpectedCode, check.ActualCode, check.SegmentID, check.ChannelID, check.LayerID, strings.Join(check.Channels, ", "), passFail(check.OK))
	}

	b.WriteString("\n## Commit boundaries\n\n")
	fmt.Fprintf(&b, "- Duplicate batch (`E_DUP`, model unchanged): %s\n", passFail(report.Duplicate.OK))
	fmt.Fprintf(&b, "- Uncommitted edit isolated from export/playback: %s\n", passFail(report.Isolation.OK))
	fmt.Fprintf(&b, "- Checkpoint resume: %s (loaded %d batches, %d checkpoints)\n", passFail(report.Recovery.OK), report.Recovery.LoadedBatches, report.Recovery.CheckpointCount)
	fmt.Fprintf(&b, "- Version invalidation and replay: %s (invalidated=%t, replayed %d batches, reasons: %s)\n",
		passFail(report.Invalidation.OK), report.Invalidation.Invalidated, report.Invalidation.ReplayedBatches, strings.Join(report.Invalidation.Reasons, "; "))

	b.WriteString("\n## Three-path comparison\n\n")
	b.WriteString("| Path | Project hash | Result hash | Commits | Samples | Decisions |\n|---|---|---|---:|---:|---:|\n")
	for _, path := range report.ThreePaths.Paths {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %d | %d | %d |\n", path.Name, path.ProjectHash, path.ResultHash, path.Commits, path.Samples, path.Decisions)
	}
	b.WriteString("\n| Pair | Equal | Differences | First difference |\n|---|---|---|---|\n")
	for _, comparison := range report.ThreePaths.Comparisons {
		first := ""
		if comparison.First != nil {
			first = fmt.Sprintf("segment=%s tick=%d field=%s", comparison.First.SegmentID, comparison.First.Tick, comparison.First.Field)
		}
		fmt.Fprintf(&b, "| `%s` vs `%s` | %s | %d | %s |\n", comparison.Left, comparison.Right, passFail(comparison.Equal), comparison.Differences, first)
	}
	fmt.Fprintf(&b, "\nThree-path overall: **%s**\n", passFail(report.ThreePaths.Equal))

	b.WriteString("\n## Concurrency bounds\n\n")
	fmt.Fprintf(&b, "- Max workers: %d, per-worker buffer: %d\n", report.Concurrency.MaxWorkers, report.Concurrency.BufferSize)
	fmt.Fprintf(&b, "- Submitted/completed: %d/%d, blocked submits: %d, max in-flight: %d\n", report.Concurrency.Submitted, report.Concurrency.Completed, report.Concurrency.Blocked, report.Concurrency.MaxInFlight)
	fmt.Fprintf(&b, "- Buffer overflow policy verified (blocked, not dropped): %s\n", passFail(report.Concurrency.OK))
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}
