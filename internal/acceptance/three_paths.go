package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"channelpipe"
	"channelpipe/internal/checkpoint"
	"channelpipe/internal/runner"
	"channelpipe/internal/sample"
)

type RevisionState struct {
	Revision       int                         `json:"revision"`
	Name           string                      `json:"name"`
	Fingerprint    string                      `json:"fingerprint"`
	Summaries      []channelpipe.BufferSummary `json:"summaries"`
	CommitSequence []int                       `json:"commit_sequence"`
}

type PathSummary struct {
	Name   string          `json:"name"`
	DBPath string          `json:"db_path"`
	States []RevisionState `json:"states"`
}

type ThreePathReport struct {
	OK               bool                              `json:"ok"`
	ImageID          string                            `json:"image_id"`
	ChunkCount       int                               `json:"chunk_count"`
	Paths            []PathSummary                     `json:"paths"`
	Invalidations    []*channelpipe.InvalidationReport `json:"invalidations,omitempty"`
	FirstMismatch    *channelpipe.Mismatch             `json:"first_mismatch,omitempty"`
	SequenceMismatch string                            `json:"sequence_mismatch,omitempty"`
}

type retainedPath struct {
	summary PathSummary
	buffers [][]*channelpipe.Buffer
}

func RunThreePaths(baseDir string, sampleSet *sample.SampleSet, workers, tileWidth, tileHeight int) (*ThreePathReport, error) {
	dir := filepath.Join(baseDir, "three-paths")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	image := sampleSet.Images[0]
	basePlan := sampleSet.Set.Plans[0]
	chunks := channelpipe.BuildChunks(basePlan.Width, basePlan.Height, tileWidth, tileHeight)
	revisions := buildRevisions(basePlan)
	fingerprints := make([]string, len(revisions))
	for i, plan := range revisions {
		fingerprint, err := channelpipe.Fingerprint(plan)
		if err != nil {
			return nil, err
		}
		fingerprints[i] = fingerprint
	}
	invalidations := make([]*channelpipe.InvalidationReport, len(revisions)-1)
	for i := 1; i < len(revisions); i++ {
		report, err := channelpipe.InvalidatePlanEdit(revisions[i-1], revisions[i], chunks)
		if err != nil {
			return nil, err
		}
		invalidations[i-1] = report
	}

	incremental, err := runIncrementalLikePath(dir, "incremental.db", "realtime-incremental", image.Buffer, revisions, fingerprints, invalidations, workers, tileWidth, tileHeight, false)
	if err != nil {
		return nil, err
	}
	resume, err := runIncrementalLikePath(dir, "resume.db", "checkpoint-resume", image.Buffer, revisions, fingerprints, invalidations, workers, tileWidth, tileHeight, true)
	if err != nil {
		return nil, err
	}
	full, err := runFullPath(dir, image.Buffer, revisions, fingerprints, workers, tileWidth, tileHeight)
	if err != nil {
		return nil, err
	}

	report := &ThreePathReport{
		ImageID:       image.ID,
		ChunkCount:    len(chunks),
		Paths:         []PathSummary{incremental.summary, resume.summary, full.summary},
		Invalidations: invalidations,
	}

	paths := []*retainedPath{incremental, resume, full}
	for revision := range revisions {
		baseBuffers := paths[2].buffers[revision]
		for _, candidate := range paths[:2] {
			if mismatch := channelpipe.CompareStageSlices(image.ID, baseBuffers, candidate.buffers[revision]); mismatch != nil {
				report.FirstMismatch = mismatch
				return report, channelpipe.DiffError(mismatch)
			}
		}
		expectedSequence := paths[2].summary.States[revision].CommitSequence
		for _, candidate := range paths[:2] {
			actualSequence := candidate.summary.States[revision].CommitSequence
			if !intsEqual(expectedSequence, actualSequence) {
				report.SequenceMismatch = fmt.Sprintf("revision %d path %s expected %v got %v", revision, candidate.summary.Name, expectedSequence, actualSequence)
				return report, channelpipe.NewError(channelpipe.CodeDiff, report.SequenceMismatch)
			}
		}
	}
	report.OK = true
	return report, nil
}

func buildRevisions(base channelpipe.Plan) []channelpipe.Plan {
	r0 := clonePlan(base)
	r1 := clonePlan(base)
	r1.Stages[1].TargetDepth = 16
	r2 := clonePlan(r1)
	r2.Stages[2].Fills[3] = channelpipe.FillSpec{Kind: channelpipe.FillZero}
	r2.Stages[2].AlphaMode = channelpipe.AlphaPassthrough
	r3 := clonePlan(r2)
	r3.Stages[1].Swizzle = []int{0, 1, 2}
	r4 := clonePlan(r3)
	r4.Stages[1], r4.Stages[2] = r4.Stages[2], r4.Stages[1]
	for i := range r4.Stages {
		r4.Stages[i].Index = i
	}
	return []channelpipe.Plan{r0, r1, r2, r3, r4}
}

func clonePlan(plan channelpipe.Plan) channelpipe.Plan {
	data, _ := json.Marshal(plan)
	var clone channelpipe.Plan
	_ = json.Unmarshal(data, &clone)
	return clone
}

func runIncrementalLikePath(dir, fileName, name string, source channelpipe.Buffer, revisions []channelpipe.Plan, fingerprints []string, invalidations []*channelpipe.InvalidationReport, workers, tileWidth, tileHeight int, withInterrupt bool) (*retainedPath, error) {
	dbPath := filepath.Join(dir, fileName)
	if err := freshSQLite(dbPath); err != nil {
		return nil, err
	}
	store, _, err := checkpoint.Open(dbPath, checkpoint.Identity{PlanFingerprint: fingerprints[0], ParamVersion: channelpipe.ParamVersion, AlgorithmVersion: channelpipe.AlgorithmVersion})
	if err != nil {
		return nil, err
	}
	defer store.Close()

	path := &retainedPath{summary: PathSummary{Name: name, DBPath: dbPath}, buffers: make([][]*channelpipe.Buffer, len(revisions))}
	normalCfg := runner.Config{Workers: workers, TileWidth: tileWidth, TileHeight: tileHeight, PerThreadBufferPixels: 1 << 22}
	pipeline := runner.New(store, normalCfg)

	if withInterrupt {
		interruptRunner := runner.New(store, runner.Config{Workers: workers, TileWidth: tileWidth, TileHeight: tileHeight, PerThreadBufferPixels: 1 << 22, InterruptAfterCommits: 2})
		outcome, err := interruptRunner.RunPlan(context.Background(), revisions[0].ID, source, revisions[0])
		if err == nil && outcome != nil && !outcome.Interrupted {
			return nil, channelpipe.NewError(channelpipe.CodeState, "expected deterministic interruption did not occur")
		}
	}
	outcome, err := pipeline.RunPlan(context.Background(), revisions[0].ID, source, revisions[0])
	if err != nil {
		return nil, err
	}
	if outcome != nil && len(outcome.Failed) > 0 {
		return nil, channelpipe.NewError(channelpipe.CodeState, "resume path has unexpected failed chunks: %+v", outcome.Failed)
	}
	if err := collectState(path, 0, "original", fingerprints[0], outcome, store); err != nil {
		return nil, err
	}

	for revision := 1; revision < len(revisions); revision++ {
		if err := store.BeginLiveFingerprint(fingerprints[revision]); err != nil {
			return nil, err
		}
		outcome, _, err = pipeline.RunIncremental(context.Background(), revisions[revision].ID, source, revisions[revision-1], revisions[revision], invalidations[revision-1])
		if err != nil {
			return nil, err
		}
		if len(outcome.Failed) > 0 {
			return nil, channelpipe.NewError(channelpipe.CodeState, "incremental revision %d failed: %+v", revision, outcome.Failed)
		}
		name := fmt.Sprintf("edit-%d", revision)
		if revision == 1 {
			name = "depth-edit"
		}
		if revision == 2 {
			name = "fill-alpha-edit"
		}
		if revision == 3 {
			name = "swizzle-edit"
		}
		if revision == 4 {
			name = "order-edit"
		}
		if err := collectState(path, revision, name, fingerprints[revision], outcome, store); err != nil {
			return nil, err
		}
	}
	return path, nil
}

func runFullPath(dir string, source channelpipe.Buffer, revisions []channelpipe.Plan, fingerprints []string, workers, tileWidth, tileHeight int) (*retainedPath, error) {
	path := &retainedPath{summary: PathSummary{Name: "full-recompute", DBPath: filepath.Join(dir, "full-r0.db")}, buffers: make([][]*channelpipe.Buffer, len(revisions))}
	for revision, plan := range revisions {
		dbPath := filepath.Join(dir, fmt.Sprintf("full-r%d.db", revision))
		if err := freshSQLite(dbPath); err != nil {
			return nil, err
		}
		store, _, err := checkpoint.Open(dbPath, checkpoint.Identity{PlanFingerprint: fingerprints[revision], ParamVersion: channelpipe.ParamVersion, AlgorithmVersion: channelpipe.AlgorithmVersion})
		if err != nil {
			return nil, err
		}
		pipeline := runner.New(store, runner.Config{Workers: workers, TileWidth: tileWidth, TileHeight: tileHeight, PerThreadBufferPixels: 1 << 22})
		outcome, err := pipeline.RunPlan(context.Background(), plan.ID, source, plan)
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		sequence, seqErr := store.CommitSequenceForFingerprint(plan.ID, fingerprints[revision])
		if seqErr != nil {
			_ = store.Close()
			return nil, seqErr
		}
		state := RevisionState{Revision: revision, Name: revisionName(revision), Fingerprint: fingerprints[revision], Summaries: outcome.Result.Summaries, CommitSequence: sequence}
		path.summary.States = append(path.summary.States, state)
		path.buffers[revision] = outcome.Result.Stages
		if err := store.Close(); err != nil {
			return nil, err
		}
	}
	return path, nil
}

func collectState(path *retainedPath, revision int, name, fingerprint string, outcome *runner.Outcome, store *checkpoint.Store) error {
	sequence, err := store.CommitSequenceForFingerprint(outcome.ImageID, fingerprint)
	if err != nil {
		return err
	}
	path.summary.States = append(path.summary.States, RevisionState{
		Revision:       revision,
		Name:           name,
		Fingerprint:    fingerprint,
		Summaries:      outcome.Result.Summaries,
		CommitSequence: sequence,
	})
	path.buffers[revision] = outcome.Result.Stages
	return nil
}

func revisionName(revision int) string {
	switch revision {
	case 0:
		return "original"
	case 1:
		return "depth-edit"
	case 2:
		return "fill-alpha-edit"
	case 3:
		return "swizzle-edit"
	default:
		return "order-edit"
	}
}

func freshSQLite(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return channelpipe.WrapError(channelpipe.CodeIO, err, path, "cannot remove stale sqlite file")
		}
	}
	return nil
}

func intsEqual(a, b []int) bool {
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
