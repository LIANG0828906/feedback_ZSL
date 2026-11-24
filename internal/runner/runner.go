package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"terrain-occlusion/internal/checkpoint"
	"terrain-occlusion/internal/cull"
	"terrain-occlusion/internal/estimate"
	"terrain-occlusion/internal/geo"
	"terrain-occlusion/internal/model"
	"terrain-occlusion/internal/parallel"
	"terrain-occlusion/internal/projectio"
	"terrain-occlusion/internal/statemachine"
	"terrain-occlusion/internal/stream"
)

type Runner struct {
	Project      *model.Project
	Params       model.QuantParameters
	Store        *checkpoint.Store
	Planner      *stream.Planner
	Machine      *statemachine.Machine
	Results      []model.SampleResult
	ShadowFrames []model.ShadowFrame
	PoseCache    map[string]int
	ProjectHash  string
	VerifyTruth  bool

	scheduler *parallel.Scheduler
}

func New(p *model.Project, store *checkpoint.Store, verifyTruth bool) *Runner {
	workers := p.Budgets.MaxParallelTasks
	if workers <= 0 {
		workers = p.Parameters.MaxWorkers
	}
	bufferLimit := p.Budgets.DepthBuffersPerTask
	if bufferLimit <= 0 {
		bufferLimit = p.Parameters.DepthBufferLimit
	}
	return &Runner{
		Project: p, Params: p.Parameters, Store: store,
		Planner: stream.NewPlanner(p.Budgets),
		Machine: statemachine.New(len(p.CameraTrack)),
		Results: []model.SampleResult{}, PoseCache: map[string]int{},
		ProjectHash: checkpoint.ProjectHash(p), VerifyTruth: verifyTruth,
		scheduler: &parallel.Scheduler{MaxWorkers: workers, BufferLimit: bufferLimit},
	}
}

func ParameterVersion(q model.QuantParameters) string {
	sum := sha256.Sum256(model.CanonicalJSON(q))
	return fmt.Sprintf("shadow-r%d-az%.1f-el%.1f-%s", q.ShadowResolution, q.LightAzimuthDeg, q.LightElevationDeg, hex.EncodeToString(sum[:4]))
}

func ResultHash(r model.SampleResult) string {
	r.ContentHash = ""
	sum := sha256.Sum256(model.CanonicalJSON(r))
	return hex.EncodeToString(sum[:16])
}

func (r *Runner) AdvanceAll() ([]model.SampleResult, error) {
	for len(r.Results) < len(r.Project.CameraTrack) {
		if _, err := r.Advance(); err != nil {
			return r.Results, err
		}
	}
	return r.Results, nil
}

func (r *Runner) Advance() (*model.SampleResult, error) {
	i := len(r.Results)
	if i >= len(r.Project.CameraTrack) {
		return nil, model.NewError(model.EState, "sample_index", "all camera samples are already committed")
	}
	if err := projectio.ValidateParameters(r.Params); err != nil {
		return nil, err
	}
	if err := r.Machine.Transition(i, statemachine.Culling); err != nil {
		return nil, err
	}

	evalProject := *r.Project
	evalProject.Parameters = r.Params
	camera := r.Project.CameraTrack[i]
	version := ParameterVersion(r.Params)
	poseKey := camera.PoseKey
	if poseKey == "" {
		poseKey = fmt.Sprintf("pose-%.3f-%.3f-%.3f", camera.Position.X, camera.Position.Y, camera.Position.Z)
	}
	cacheKey := poseKey + "|" + version

	var verdicts []model.ChunkVerdict
	revisitHit := false
	if prior, ok := r.PoseCache[cacheKey]; ok {
		verdicts = append([]model.ChunkVerdict(nil), r.Results[prior].Verdicts...)
		revisitHit = true
	} else {
		shadow := geo.RenderShadowMap(evalProject)
		tasks := make([]parallel.Task, 0, len(r.Project.Chunks))
		for _, chunk := range r.Project.Chunks {
			chunk := chunk
			tasks = append(tasks, parallel.Task{
				ChunkID: chunk.ID, DepthEntries: len(chunk.SamplePoints),
				Run: func(_ []float64) model.ChunkVerdict {
					return cull.EvaluateChunk(&evalProject, camera, shadow, chunk)
				},
			})
		}
		verdicts = r.scheduler.Run(i, tasks)
		r.ShadowFrames = append(r.ShadowFrames, model.ShadowFrame{SampleIndex: i, Resolution: r.Params.ShadowResolution, Depth: append([]float64(nil), shadow.Depth...)})
	}
	resourceWaits := r.scheduler.DrainWaits()

	visible, occluded, uncertain := cull.IDsByVerdict(verdicts)
	coarseOccluded := []int{}
	for _, v := range verdicts {
		if v.Coarse == "fully_occluded" {
			coarseOccluded = append(coarseOccluded, v.ChunkID)
		}
	}

	if r.VerifyTruth {
		if err := verifyVerdictTruth(r.Project, i, verdicts); err != nil {
			_ = r.Machine.Transition(i, statemachine.Failed)
			return nil, err
		}
	}

	desired := append(append([]int(nil), visible...), uncertain...)
	sort.Ints(desired)
	distances := stream.Distances(r.Project, camera.Position)
	reconciled, err := r.Planner.Reconcile(i, r.Project, desired, distances)
	if err != nil {
		_ = r.Machine.Transition(i, statemachine.Failed)
		return nil, err
	}
	if err := r.Machine.Transition(i, statemachine.Loaded); err != nil {
		return nil, err
	}
	if r.VerifyTruth {
		if err := verifyStreamingTruth(r.Project, i, reconciled); err != nil {
			_ = r.Machine.Transition(i, statemachine.Failed)
			return nil, err
		}
	}

	errorReport := estimate.Report(r.Params, i, len(reconciled.Delayed))
	result := model.SampleResult{
		SampleIndex: i, LogicalTimeMs: camera.TimeMs, PoseKey: poseKey,
		State: statemachine.Committed, ShadowResolution: r.Params.ShadowResolution,
		ShadowVersion: version, CoarseOccludedIDs: coarseOccluded, Verdicts: verdicts,
		VisibleIDs: visible, OccludedIDs: occluded, UncertainIDs: uncertain,
		ResidentIDs: reconciled.ResidentIDs, QueuedIDs: reconciled.QueuedIDs,
		CancelledIDs: reconciled.CancelledIDs,
		LoadEvents:   reconciled.Loads, UnloadEvents: reconciled.Unloads,
		Delayed: reconciled.Delayed, ErrorReport: errorReport,
		ResourceWaits: resourceWaits, RevisitHit: revisitHit, CameraOffset: i + 1,
	}
	result.ContentHash = ResultHash(result)

	engineJSON := model.CanonicalJSON(r.Planner)
	record := checkpoint.Record{
		SampleIndex: i, LogicalTimeMs: camera.TimeMs,
		VisibleDigest: checkpoint.DigestIDs(visible),
		UncertainJSON: string(model.CanonicalJSON(uncertain)),
		ResidentJSON:  string(model.CanonicalJSON(reconciled.ResidentIDs)),
		ShadowVersion: version, CameraOffset: i + 1,
		ResultJSON: string(model.CanonicalJSON(result)), EngineJSON: string(engineJSON),
	}
	if r.Store != nil {
		if _, err := r.Store.CommitSnapshot(r.ProjectHash, record); err != nil {
			_ = r.Machine.Transition(i, statemachine.Failed)
			return nil, model.NewError(model.EHash, "sqlite.commit", "commit checkpoint: %v", err)
		}
	}
	if err := r.Machine.Transition(i, statemachine.Committed); err != nil {
		return nil, err
	}

	r.Results = append(r.Results, result)
	r.PoseCache[cacheKey] = i
	return &result, nil
}

func (r *Runner) Restore(records []checkpoint.Record) error {
	for _, rec := range records {
		var result model.SampleResult
		if err := json.Unmarshal([]byte(rec.ResultJSON), &result); err != nil {
			return err
		}
		r.Results = append(r.Results, result)
		r.Machine.States[rec.SampleIndex] = statemachine.Committed
		key := result.PoseKey + "|" + result.ShadowVersion
		r.PoseCache[key] = rec.SampleIndex
	}
	r.Machine.Committed = len(records)
	if len(records) > 0 {
		latest := records[len(records)-1]
		var planner stream.Planner
		if err := json.Unmarshal([]byte(latest.EngineJSON), &planner); err != nil {
			return err
		}
		r.Planner = &planner
	}
	return nil
}

func verifyVerdictTruth(p *model.Project, sampleIndex int, verdicts []model.ChunkVerdict) error {
	truthByID := map[int]model.ChunkTruth{}
	for _, ct := range p.Truth.Samples[sampleIndex].Chunks {
		truthByID[ct.ChunkID] = ct
	}
	for _, got := range verdicts {
		want := truthByID[got.ChunkID]
		if got.Final != want.Verdict {
			return &model.Error{
				Code: model.EVis, Field: fmt.Sprintf("truth.samples[%d].chunks[%d]", sampleIndex, got.ChunkID),
				Message: "visibility mismatch at sample %d chunk %d",
				Details: map[string]any{
					"sample_index": sampleIndex, "chunk_id": got.ChunkID,
					"truth": string(want.Verdict), "observed": string(got.Final),
					"truth_margin": want.ExactMargin, "observed_margin": got.DecisionMargin,
					"lower_bound": got.LowerBound, "upper_bound": got.UpperBound,
				},
			}
		}
	}
	return nil
}

func verifyStreamingTruth(p *model.Project, sampleIndex int, got *stream.ReconcileResult) error {
	want := p.Truth.Samples[sampleIndex]
	if !equalInts(got.ResidentIDs, want.ResidentIDs) {
		return streamingMismatch(sampleIndex, "resident_ids", want.ResidentIDs, got.ResidentIDs)
	}
	gotLoads := loadIDs(got.Loads)
	wantLoads := loadIDs(want.LoadEvents)
	if !equalInts(gotLoads, wantLoads) {
		return streamingMismatch(sampleIndex, "load_events", wantLoads, gotLoads)
	}
	gotUnloads := unloadIDs(got.Unloads)
	wantUnloads := unloadIDs(want.UnloadEvents)
	if !equalInts(gotUnloads, wantUnloads) {
		return streamingMismatch(sampleIndex, "unload_events", wantUnloads, gotUnloads)
	}
	return nil
}

func streamingMismatch(sample int, field string, want, got []int) error {
	err := model.NewError(model.ELoad, "truth.samples."+field, "streaming sequence mismatch at sample %d: %s", sample, field)
	err.Details = map[string]any{"sample_index": sample, "field": field, "truth": want, "observed": got}
	return err
}

func equalInts(a, b []int) bool {
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

func loadIDs(events []model.LoadEvent) []int {
	out := make([]int, 0, len(events))
	for _, e := range events {
		out = append(out, e.ChunkID)
	}
	return out
}
func unloadIDs(events []model.UnloadEvent) []int {
	out := make([]int, 0, len(events))
	for _, e := range events {
		out = append(out, e.ChunkID)
	}
	return out
}

var _ = json.Marshal
