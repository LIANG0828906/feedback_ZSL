package silhouette

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type VersionLoadReport struct {
	Version              int   `json:"version"`
	SourceVersion        int   `json:"source_version"`
	Controls             int   `json:"controls"`
	Segments             int   `json:"segments"`
	Tiles                int   `json:"tiles"`
	Tolerance            int64 `json:"tolerance_internal"`
	Budget               int   `json:"budget"`
	TransformationTraces int   `json:"transformation_traces"`
	DefaultFills         int   `json:"default_fills"`
	TruthEndpointsMatch  bool  `json:"truth_endpoints_match"`
	CanonicalFieldsMatch bool  `json:"canonical_fields_match"`
}

type RoundTripReport struct {
	Target             int  `json:"target_version"`
	ControlsExact      bool `json:"controls_exact"`
	ToleranceExact     bool `json:"tolerance_exact"`
	TilesExact         bool `json:"tiles_exact"`
	SegmentCurvesExact bool `json:"segment_curves_exact"`
	LossyIssues        int  `json:"lossy_issues"`
	ExportTraces       int  `json:"export_traces"`
	ReloadedWithLow    bool `json:"reloaded_with_low_loader"`
}

type BudgetVerification struct {
	BaseVertices       int    `json:"base_vertices"`
	RequestedBudget    int    `json:"requested_budget"`
	FinalVertices      int    `json:"final_vertices"`
	WithinBudget       bool   `json:"within_budget"`
	DegradationRecords int    `json:"degradation_records"`
	LowBudgetCode      string `json:"low_budget_code"`
	LowBudgetOverage   string `json:"low_budget_overage"`
	LowBudgetTail      string `json:"low_budget_tail_segment"`
	AllRecordsComplete bool   `json:"all_records_complete"`
}

type InvalidationVerification struct {
	InvalidatedTiles []string `json:"invalidated_tiles"`
	ReusedTiles      []string `json:"reused_tiles"`
	BaseCommits      int      `json:"base_commits"`
	NewCommits       int      `json:"new_commits"`
	StoredCommits    int      `json:"stored_commits"`
	ReusedPersisted  bool     `json:"reused_persisted"`
	PartialReuse     bool     `json:"partial_reuse"`
}

type PathVerification struct {
	Resume            *ConsistencyReport `json:"resume_vs_realtime"`
	FullRecompute     *ConsistencyReport `json:"full_vs_realtime"`
	ResumeCommitOrder []string           `json:"resume_commit_order"`
	ResumeReused      []string           `json:"resume_reused_tiles"`
	SQLiteCheckpoints int                `json:"sqlite_checkpoints"`
	SQLiteCommits     int                `json:"sqlite_commits"`
}

type WorkerVerification struct {
	Workers        int   `json:"workers"`
	BufferVertices int   `json:"buffer_vertices"`
	Events         int   `json:"events"`
	BlockedEvents  int   `json:"blocked_events"`
	MaxWaitNanos   int64 `json:"max_wait_nanos"`
	ReasonRecorded bool  `json:"reason_recorded"`
}

type ErrorVerification struct {
	UnknownSchemaCode string `json:"unknown_schema_code"`
	MissingFieldCode  string `json:"missing_field_code"`
	MissingFieldPath  string `json:"missing_field_path"`
	OverflowCode      string `json:"overflow_code"`
	OverflowField     string `json:"overflow_field"`
	OverflowOperandA  string `json:"overflow_operand_a"`
	OverflowOperandB  string `json:"overflow_operand_b"`
	ToleranceCode     string `json:"tolerance_code"`
	ToleranceSegment  string `json:"tolerance_segment"`
	ToleranceMeasured string `json:"tolerance_measured"`
	AllMatched        bool   `json:"all_matched"`
}

type VerificationReport struct {
	Seed             int64                    `json:"seed"`
	AlgorithmVersion string                   `json:"algorithm_version"`
	AllPassed        bool                     `json:"all_passed"`
	VersionLoads     []VersionLoadReport      `json:"version_loads"`
	RoundTrips       []RoundTripReport        `json:"round_trips"`
	Budget           BudgetVerification       `json:"budget"`
	Invalidation     InvalidationVerification `json:"invalidation"`
	Paths            PathVerification         `json:"paths"`
	WorkerScheduling WorkerVerification       `json:"worker_scheduling"`
	ErrorContracts   ErrorVerification        `json:"error_contracts"`
	Checks           []string                 `json:"checks"`
	DurationNanos    int64                    `json:"duration_nanos"`
}

func Verify(ctx context.Context, seed int64) (*VerificationReport, error) {
	started := time.Now()
	if seed == 0 {
		seed = DefaultSeed
	}
	report := &VerificationReport{Seed: seed, AlgorithmVersion: AlgorithmVersion}
	canonical := GenerateScene(seed)
	truth := GenerateTruth(canonical, VersionCurrent)
	var loadedScenes = make(map[int]*Scene)
	for _, version := range []int{1, 2, 3} {
		generated, err := Generate(seed, version)
		if err != nil {
			return nil, err
		}
		scene, err := Load(generated.Schema)
		if err != nil {
			return nil, err
		}
		loadedScenes[version] = scene
		item := VersionLoadReport{
			Version: version, SourceVersion: scene.SourceVersion,
			Controls: len(scene.Surface.Controls), Segments: len(scene.Segments),
			Tiles: len(scene.Tiles), Tolerance: scene.Tolerance.Value, Budget: scene.Budget.MaxVertices,
			TransformationTraces: len(scene.Traces),
		}
		for _, trace := range scene.Traces {
			if trace.Action == "default_fill" {
				item.DefaultFills++
			}
		}
		item.TruthEndpointsMatch = truthEndpointsMatch(scene, truth)
		item.CanonicalFieldsMatch = canonicalFieldsMatch(scene, canonical)
		report.VersionLoads = append(report.VersionLoads, item)
	}
	for _, target := range []int{1, 2} {
		result, err := Export(canonical, target)
		if err != nil {
			return nil, err
		}
		low, err := Load(result.Data)
		if err != nil {
			return nil, err
		}
		back, err := Export(low, 3)
		if err != nil {
			return nil, err
		}
		reloaded, err := Load(back.Data)
		if err != nil {
			return nil, err
		}
		item := RoundTripReport{
			Target: target, LossyIssues: len(result.Lossy), ExportTraces: len(result.Traces),
			ReloadedWithLow:    low.SourceVersion == target,
			ControlsExact:      canonical.Surface.Controls != nil && vec3SlicesEqual(canonical.Surface.Controls, reloaded.Surface.Controls),
			ToleranceExact:     canonical.Tolerance.Value == reloaded.Tolerance.Value,
			TilesExact:         tilesEqual(canonical.Tiles, reloaded.Tiles),
			SegmentCurvesExact: segmentCurvesEqual(canonical.Segments, reloaded.Segments),
		}
		report.RoundTrips = append(report.RoundTrips, item)
	}

	baseSample, err := SampleScene(canonical, SamplingOptions{Tolerance: canonical.Tolerance.Value, Budget: 100000})
	if err != nil {
		return nil, err
	}
	degraded, err := SampleScene(canonical, SamplingOptions{Tolerance: canonical.Tolerance.Value, Budget: canonical.Budget.MaxVertices})
	if err != nil {
		return nil, err
	}
	report.Budget = BudgetVerification{
		BaseVertices:       baseSample.Budget.FinalVertices,
		RequestedBudget:    canonical.Budget.MaxVertices,
		FinalVertices:      degraded.Budget.FinalVertices,
		WithinBudget:       degraded.Budget.FinalVertices <= canonical.Budget.MaxVertices,
		DegradationRecords: len(degraded.Budget.Degradations),
		AllRecordsComplete: degradationRecordsComplete(degraded.Budget.Degradations),
	}
	_, lowBudgetErr := SampleScene(canonical, SamplingOptions{Budget: 3})
	if de, ok := lowBudgetErr.(*DomainError); ok {
		report.Budget.LowBudgetCode = de.Code
		report.Budget.LowBudgetOverage = de.Details["overage_vertices"]
		report.Budget.LowBudgetTail = de.Details["last_priority_segment"]
	}

	dir, err := os.MkdirTemp("", "silhouette-verify-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	invalidation, err := verifyInvalidation(ctx, dir, canonical)
	if err != nil {
		return nil, err
	}
	report.Invalidation = *invalidation
	paths, err := verifyPaths(ctx, dir, canonical)
	if err != nil {
		return nil, err
	}
	report.Paths = *paths
	worker, err := verifyWorkers(ctx, canonical, baseSample)
	if err != nil {
		return nil, err
	}
	report.WorkerScheduling = *worker
	report.ErrorContracts = verifyErrors(canonical)

	for _, load := range report.VersionLoads {
		report.Checks = append(report.Checks, fmt.Sprintf("schema-v%d: canonical=%t truth=%t traces=%d defaults=%d", load.Version, load.CanonicalFieldsMatch, load.TruthEndpointsMatch, load.TransformationTraces, load.DefaultFills))
	}
	allRoundTrips := true
	for _, rt := range report.RoundTrips {
		ok := rt.ControlsExact && rt.ToleranceExact && rt.TilesExact && rt.SegmentCurvesExact && rt.ReloadedWithLow && rt.LossyIssues > 0
		allRoundTrips = allRoundTrips && ok
		report.Checks = append(report.Checks, fmt.Sprintf("roundtrip-v%d: controls=%t tolerance=%t tiles=%t curves=%t E_LOSSY=%d", rt.Target, rt.ControlsExact, rt.ToleranceExact, rt.TilesExact, rt.SegmentCurvesExact, rt.LossyIssues))
	}
	allLoads := true
	for _, load := range report.VersionLoads {
		allLoads = allLoads && load.CanonicalFieldsMatch && load.TruthEndpointsMatch && load.SourceVersion == load.Version
	}
	budgetOK := report.Budget.WithinBudget && report.Budget.DegradationRecords > 0 && report.Budget.AllRecordsComplete && report.Budget.LowBudgetCode == CodeBudget && report.Budget.LowBudgetOverage != "" && report.Budget.LowBudgetTail != ""
	invalidationOK := report.Invalidation.PartialReuse && report.Invalidation.ReusedPersisted && report.Invalidation.NewCommits == len(report.Invalidation.InvalidatedTiles)
	pathsOK := report.Paths.Resume != nil && report.Paths.Resume.Consistent && report.Paths.FullRecompute != nil && report.Paths.FullRecompute.Consistent && report.Paths.SQLiteCheckpoints == len(canonical.Tiles)
	workerOK := report.WorkerScheduling.Events > 0 && report.WorkerScheduling.BlockedEvents > 0 && report.WorkerScheduling.ReasonRecorded
	report.AllPassed = allLoads && allRoundTrips && budgetOK && invalidationOK && pathsOK && workerOK && report.ErrorContracts.AllMatched
	report.Checks = append(report.Checks,
		fmt.Sprintf("budget: %d -> %d <= %d, degradation-records=%d, low-budget=%s", report.Budget.BaseVertices, report.Budget.FinalVertices, report.Budget.RequestedBudget, report.Budget.DegradationRecords, report.Budget.LowBudgetCode),
		fmt.Sprintf("invalidation: invalidated=%v reused=%v commits=%d/%d", report.Invalidation.InvalidatedTiles, report.Invalidation.ReusedTiles, report.Invalidation.NewCommits, report.Invalidation.StoredCommits),
		fmt.Sprintf("paths: resume=%t full=%t sqlite=%d/%d", report.Paths.Resume.Consistent, report.Paths.FullRecompute.Consistent, report.Paths.SQLiteCheckpoints, report.Paths.SQLiteCommits),
		fmt.Sprintf("workers: events=%d blocked=%d max-wait-ns=%d", report.WorkerScheduling.Events, report.WorkerScheduling.BlockedEvents, report.WorkerScheduling.MaxWaitNanos),
		fmt.Sprintf("errors: schema=%s missing=%s(%s) overflow=%s tol=%s", report.ErrorContracts.UnknownSchemaCode, report.ErrorContracts.MissingFieldCode, report.ErrorContracts.MissingFieldPath, report.ErrorContracts.OverflowCode, report.ErrorContracts.ToleranceCode),
	)
	report.DurationNanos = int64(time.Since(started))
	return report, nil
}

func verifyInvalidation(ctx context.Context, dir string, scene *Scene) (*InvalidationVerification, error) {
	store, err := OpenCheckpointStore(filepath.Join(dir, "invalidation.db"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	cfg := RunConfig{Workers: 3, VerticesPerWorker: DefaultVerticesPerWorker}
	base, err := RunRealtime(ctx, scene, store, cfg)
	if err != nil {
		return nil, err
	}
	seg, _ := scene.Segment("seg-0")
	point := seg.Controls[1]
	point.Y += 40 * FixedScale
	edited, err := RunEdit(ctx, scene, base.Assembly, Edit{
		Kind: EditSegmentControl, SegmentID: "seg-0", Control: 1, Point: point,
	}, store, cfg)
	if err != nil {
		return nil, err
	}
	checkpoints, err := store.ListCheckpoints(ctx, scene.ID)
	if err != nil {
		return nil, err
	}
	commits, err := store.ListCommits(ctx, scene.ID)
	if err != nil {
		return nil, err
	}
	cpByID := make(map[string]Checkpoint, len(checkpoints))
	for _, cp := range checkpoints {
		cpByID[cp.TileID] = cp
	}
	reusedPersisted := true
	for _, id := range edited.ReusedTiles {
		cp, ok := cpByID[id]
		if !ok {
			reusedPersisted = false
			continue
		}
		var tile TileAssembly
		for _, t := range edited.Assembly.Tiles {
			if t.TileID == id {
				tile = t
				break
			}
		}
		if cp.ContentHash != tile.ContentHash || cp.CacheKey != tile.CacheKey {
			reusedPersisted = false
		}
	}
	item := &InvalidationVerification{
		InvalidatedTiles: append([]string(nil), edited.Invalidation.InvalidatedTiles...),
		ReusedTiles:      append([]string(nil), edited.ReusedTiles...),
		BaseCommits:      len(base.Commits), NewCommits: len(edited.Commits), StoredCommits: len(commits),
		ReusedPersisted: reusedPersisted,
		PartialReuse:    len(edited.ReusedTiles) > 0 && len(edited.Invalidation.InvalidatedTiles) > 0,
	}
	return item, nil
}

func verifyPaths(ctx context.Context, dir string, scene *Scene) (*PathVerification, error) {
	cfg := RunConfig{Workers: 3, VerticesPerWorker: DefaultVerticesPerWorker}
	realStore, err := OpenCheckpointStore(filepath.Join(dir, "realtime.db"))
	if err != nil {
		return nil, err
	}
	defer realStore.Close()
	realtime, err := RunRealtime(ctx, scene, realStore, cfg)
	if err != nil {
		return nil, err
	}
	resumeStore, err := OpenCheckpointStore(filepath.Join(dir, "resume.db"))
	if err != nil {
		return nil, err
	}
	defer resumeStore.Close()
	interruptedCfg := cfg
	interruptedCfg.StopAfterCommits = 2
	interrupted, err := RunRealtime(ctx, scene, resumeStore, interruptedCfg)
	if err != nil {
		return nil, err
	}
	if !interrupted.Interrupted || len(interrupted.Commits) != 2 {
		return nil, NewDomainError(CodeInternal, "$.verify.resume", "interrupted run did not stop after two commits")
	}
	resumed, err := RunResume(ctx, scene, resumeStore, cfg)
	if err != nil {
		return nil, err
	}
	fullStore, err := OpenCheckpointStore(filepath.Join(dir, "full.db"))
	if err != nil {
		return nil, err
	}
	defer fullStore.Close()
	full, err := RunFullRecompute(ctx, scene, fullStore, cfg)
	if err != nil {
		return nil, err
	}
	checkpoints, err := resumeStore.ListCheckpoints(ctx, scene.ID)
	if err != nil {
		return nil, err
	}
	commits, err := resumeStore.ListCommits(ctx, scene.ID)
	if err != nil {
		return nil, err
	}
	return &PathVerification{
		Resume: CompareRuns(realtime, resumed), FullRecompute: CompareRuns(realtime, full),
		ResumeCommitOrder: append([]string(nil), resumed.CommitOrder...),
		ResumeReused:      append([]string(nil), resumed.ReusedTiles...),
		SQLiteCheckpoints: len(checkpoints), SQLiteCommits: len(commits),
	}, nil
}

func verifyWorkers(ctx context.Context, scene *Scene, sampling *SamplingResult) (*WorkerVerification, error) {
	assembly, err := AssembleContext(ctx, scene, sampling, 2, DefaultVerticesPerWorker)
	if err != nil {
		return nil, err
	}
	events := assembly.WorkerEvents
	blocked := 0
	maxWait := int64(0)
	reason := false
	for _, event := range events {
		if event.WaitNanos > 0 {
			blocked++
			if event.WaitNanos > maxWait {
				maxWait = event.WaitNanos
			}
			if event.Reason != "" {
				reason = true
			}
		}
	}
	return &WorkerVerification{
		Workers: 2, BufferVertices: DefaultVerticesPerWorker,
		Events: len(events), BlockedEvents: blocked, MaxWaitNanos: maxWait, ReasonRecorded: reason,
	}, nil
}

func verifyErrors(scene *Scene) ErrorVerification {
	var out ErrorVerification
	_, err := Load([]byte(`{"schema_version":9}`))
	if de, ok := err.(*DomainError); ok {
		out.UnknownSchemaCode = de.Code
	}
	_, err = Load([]byte(`{"schema_version":3,"scene_id":"missing"}`))
	if de, ok := err.(*DomainError); ok {
		out.MissingFieldCode = de.Code
		out.MissingFieldPath = de.Path
	}
	_, err = ScaleInt(math.MaxInt64, 2, 1, RoundTowardZero, "$.control.x")
	if de, ok := err.(*DomainError); ok {
		out.OverflowCode = de.Code
		out.OverflowField = de.Path
		out.OverflowOperandA = de.Details["operand_a"]
		out.OverflowOperandB = de.Details["operand_b"]
	}
	_, err = SampleSegment(scene.Segments[0], 0, MaxSubdivisionDepth)
	if de, ok := err.(*DomainError); ok {
		out.ToleranceCode = de.Code
		out.ToleranceSegment = de.Details["segment_id"]
		out.ToleranceMeasured = de.Details["measured"]
	}
	out.AllMatched = out.UnknownSchemaCode == CodeSchema &&
		out.MissingFieldCode == CodeSchema && out.MissingFieldPath != "" &&
		out.OverflowCode == CodeOverflow && out.OverflowField != "" && out.OverflowOperandA != "" && out.OverflowOperandB != "" &&
		out.ToleranceCode == CodeTolerance && out.ToleranceSegment != "" && out.ToleranceMeasured != ""
	return out
}

func canonicalFieldsMatch(scene, canonical *Scene) bool {
	return vec3SlicesEqual(scene.Surface.Controls, canonical.Surface.Controls) &&
		segmentCurvesEqual(scene.Segments, canonical.Segments) &&
		tilesEqual(scene.Tiles, canonical.Tiles) &&
		scene.Tolerance.Value == canonical.Tolerance.Value &&
		scene.Budget.MaxVertices == canonical.Budget.MaxVertices
}

func vec3SlicesEqual(a, b []Vec3) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func segmentCurvesEqual(a, b []CurveSegment) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]CurveSegment(nil), a...)
	bs := append([]CurveSegment(nil), b...)
	sort.SliceStable(as, func(i, j int) bool { return as[i].ID < as[j].ID })
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].ID < bs[j].ID })
	for i := range as {
		if as[i].ID != bs[i].ID || as[i].Controls != bs[i].Controls {
			return false
		}
	}
	return true
}

func tilesEqual(a, b []Tile) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]Tile(nil), a...)
	bs := append([]Tile(nil), b...)
	sort.SliceStable(as, func(i, j int) bool { return as[i].ID < as[j].ID })
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].ID < bs[j].ID })
	for i := range as {
		if as[i].ID != bs[i].ID || as[i].Min != bs[i].Min || as[i].Max != bs[i].Max {
			return false
		}
	}
	return true
}

func truthEndpointsMatch(scene *Scene, truth TruthManifest) bool {
	if len(scene.Segments) != len(truth.Segments) {
		return false
	}
	byID := make(map[string]SegmentTruth, len(truth.Segments))
	for _, item := range truth.Segments {
		byID[item.ID] = item
	}
	for _, seg := range scene.Segments {
		item, ok := byID[seg.ID]
		if !ok || seg.Controls[0] != item.Start || seg.Controls[3] != item.End {
			return false
		}
	}
	return true
}

func degradationRecordsComplete(items []Degradation) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.SegmentID == "" || item.Basis == "" || item.FromTolerance <= 0 || item.ToTolerance <= item.FromTolerance ||
			item.BeforeVertices <= 0 || item.AfterVertices <= 0 || item.BudgetBefore <= 0 || item.BudgetAfter <= 0 {
			return false
		}
	}
	return true
}

func CompactVerification(report *VerificationReport) string {
	status := "PASS"
	if !report.AllPassed {
		status = "FAIL"
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("verification %s seed=%d algorithm=%s", status, report.Seed, report.AlgorithmVersion))
	for _, check := range report.Checks {
		lines = append(lines, "  "+check)
	}
	lines = append(lines, "  duration="+strconv.FormatInt(report.DurationNanos, 10)+"ns")
	return joinLines(lines)
}

func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}
