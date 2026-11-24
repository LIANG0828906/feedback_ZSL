package diagnostic

import (
	"errors"
	"os"
	"path/filepath"

	"terrain-occlusion/internal/checkpoint"
	"terrain-occlusion/internal/model"
	"terrain-occlusion/internal/runner"
)

type Options struct {
	RealtimeDB string
	ResumeDB   string
	ReplayDB   string
	BreakAt    int
}

func ArtifactOptions(dir string) Options {
	_ = os.MkdirAll(dir, 0o755)
	return Options{
		RealtimeDB: filepath.Join(dir, "realtime-checkpoints.db"),
		ResumeDB:   filepath.Join(dir, "resume-checkpoints.db"),
		ReplayDB:   filepath.Join(dir, "replay-checkpoints.db"),
		BreakAt:    3,
	}
}

func RunThreePaths(project *model.Project, opt Options) *model.DiagnosticReport {
	report := &model.DiagnosticReport{Project: *project, OK: true}
	resetDB(opt.RealtimeDB)
	resetDB(opt.ResumeDB)
	resetDB(opt.ReplayDB)

	rt, rtRunner, err := runFresh(project, opt.RealtimeDB)
	report.Realtime = rt
	if rtRunner != nil {
		report.ShadowFrames = rtRunner.ShadowFrames
	}
	if err != nil {
		report.OK = false
		report.VerificationErrors = append(report.VerificationErrors, "realtime: "+err.Error())
	}

	resume, err := runResume(project, opt.ResumeDB, opt.BreakAt)
	report.Resume = resume
	if err != nil {
		report.OK = false
		report.VerificationErrors = append(report.VerificationErrors, "resume: "+err.Error())
	}

	replay, _, err := runFresh(project, opt.ReplayDB)
	report.Replay = replay
	if err != nil {
		report.OK = false
		report.VerificationErrors = append(report.VerificationErrors, "replay: "+err.Error())
	}

	report.Comparison = Compare(report.Realtime.Results, report.Resume.Results, report.Replay.Results)
	report.TotalSamples = len(project.CameraTrack)
	report.CommittedSamples = len(report.Realtime.Results)
	if !report.Comparison.Identical {
		report.OK = false
		report.VerificationErrors = append(report.VerificationErrors, "comparison: "+report.Comparison.Detail)
	}
	report.TruthMatched = report.OK
	return report
}

func resetDB(path string) {
	if path == "" || path == ":memory:" {
		return
	}
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func runFresh(project *model.Project, dbPath string) (model.PathReport, *runner.Runner, error) {
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return model.PathReport{Name: pathName(dbPath), CheckpointPath: dbPath}, nil, err
	}
	r := runner.New(project, store, true)
	results, runErr := r.AdvanceAll()
	closeErr := store.Close()
	if runErr != nil {
		return model.PathReport{Name: "realtime", Results: results, CheckpointPath: dbPath}, r, runErr
	}
	if closeErr != nil {
		return model.PathReport{Name: "realtime", Results: results, CheckpointPath: dbPath}, r, closeErr
	}
	return model.PathReport{Name: "realtime", Results: results, CheckpointPath: dbPath}, r, nil
}

func runResume(project *model.Project, dbPath string, breakAt int) (model.PathReport, error) {
	if breakAt <= 0 || breakAt >= len(project.CameraTrack) {
		breakAt = 3
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return model.PathReport{Name: "resume", CheckpointPath: dbPath}, err
	}
	first := runner.New(project, store, true)
	var firstErr error
	for i := 0; i < breakAt; i++ {
		if _, firstErr = first.Advance(); firstErr != nil {
			break
		}
	}
	if closeErr := store.Close(); closeErr != nil && firstErr == nil {
		firstErr = closeErr
	}
	if firstErr != nil {
		return model.PathReport{Name: "resume", Results: first.Results, CheckpointPath: dbPath}, firstErr
	}

	store2, err := checkpoint.Open(dbPath)
	if err != nil {
		return model.PathReport{Name: "resume", CheckpointPath: dbPath}, err
	}
	records, err := store2.LoadVerified(checkpoint.ProjectHash(project))
	if err != nil {
		_ = store2.Close()
		return model.PathReport{Name: "resume", CheckpointPath: dbPath}, err
	}
	second := runner.New(project, store2, true)
	if err := second.Restore(records); err != nil {
		_ = store2.Close()
		return model.PathReport{Name: "resume", CheckpointPath: dbPath}, err
	}
	_, runErr := second.AdvanceAll()
	closeErr := store2.Close()
	if runErr != nil {
		return model.PathReport{Name: "resume", Results: second.Results, CheckpointPath: dbPath}, runErr
	}
	if closeErr != nil {
		return model.PathReport{Name: "resume", Results: second.Results, CheckpointPath: dbPath}, closeErr
	}
	return model.PathReport{Name: "resume", Results: second.Results, CheckpointPath: dbPath}, nil
}

func pathName(p string) string {
	if p == ":memory:" || p == "" {
		return "memory"
	}
	return filepath.Base(p)
}

func Compare(realtime, resume, replay []model.SampleResult) model.PathComparison {
	if len(realtime) != len(resume) || len(realtime) != len(replay) {
		return model.PathComparison{Identical: false, FirstSample: minLenSample(realtime, resume, replay), Field: "sample_count", Detail: "path result lengths differ"}
	}
	for i := range realtime {
		if mismatch := compareSample(i, realtime[i], resume[i], "resume"); mismatch != nil {
			return *mismatch
		}
		if mismatch := compareSample(i, realtime[i], replay[i], "replay"); mismatch != nil {
			return *mismatch
		}
	}
	comp := model.PathComparison{Identical: true, FirstSample: -1}
	if len(realtime) > 0 {
		comp.RealtimeHash = realtime[len(realtime)-1].ContentHash
		comp.ResumeHash = resume[len(resume)-1].ContentHash
		comp.ReplayHash = replay[len(replay)-1].ContentHash
	}
	return comp
}

func minLenSample(a, b, c []model.SampleResult) int {
	n := min(len(a), len(b), len(c))
	if n == 0 {
		return 0
	}
	return n
}

func compareSample(index int, base, other model.SampleResult, otherName string) *model.PathComparison {
	if !equalIDs(base.VisibleIDs, other.VisibleIDs) {
		return mismatch(index, 0, "visible_ids", "visible set differs against "+otherName)
	}
	if !equalIDs(base.UncertainIDs, other.UncertainIDs) {
		return mismatch(index, firstDifferentID(base.UncertainIDs, other.UncertainIDs), "uncertain_ids", "uncertain set differs against "+otherName)
	}
	if !equalIDs(base.OccludedIDs, other.OccludedIDs) {
		return mismatch(index, firstDifferentID(base.OccludedIDs, other.OccludedIDs), "occluded_ids", "occluded set differs against "+otherName)
	}
	if !equalIDs(base.ResidentIDs, other.ResidentIDs) {
		return mismatch(index, firstDifferentID(base.ResidentIDs, other.ResidentIDs), "resident_ids", "resident set differs against "+otherName)
	}
	for ci := range base.Verdicts {
		if ci >= len(other.Verdicts) {
			return mismatch(index, base.Verdicts[ci].ChunkID, "verdicts", otherName+" has missing verdict")
		}
		bv, ov := base.Verdicts[ci], other.Verdicts[ci]
		if bv.Final != ov.Final || bv.LowerBound != ov.LowerBound || bv.UpperBound != ov.UpperBound {
			return mismatch(index, bv.ChunkID, "verdict", "chunk verdict/bounds differ against "+otherName)
		}
	}
	if ids := loadEventIDs(base.LoadEvents); !equalIDs(ids, loadEventIDs(other.LoadEvents)) {
		return mismatch(index, firstDifferentID(ids, loadEventIDs(other.LoadEvents)), "load_events", "load sequence differs against "+otherName)
	}
	if ids := unloadEventIDs(base.UnloadEvents); !equalIDs(ids, unloadEventIDs(other.UnloadEvents)) {
		return mismatch(index, firstDifferentID(ids, unloadEventIDs(other.UnloadEvents)), "unload_events", "unload sequence differs against "+otherName)
	}
	return nil
}

func mismatch(sample, chunk int, field, detail string) *model.PathComparison {
	return &model.PathComparison{Identical: false, FirstSample: sample, FirstChunkID: chunk, Field: field, Detail: detail}
}

func equalIDs(a, b []int) bool {
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

func firstDifferentID(a, b []int) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i]
		}
	}
	if len(a) > 0 {
		return a[len(a)-1]
	}
	if len(b) > 0 {
		return b[0]
	}
	return 0
}

func loadEventIDs(events []model.LoadEvent) []int {
	out := make([]int, 0, len(events))
	for _, e := range events {
		out = append(out, e.ChunkID)
	}
	return out
}
func unloadEventIDs(events []model.UnloadEvent) []int {
	out := make([]int, 0, len(events))
	for _, e := range events {
		out = append(out, e.ChunkID)
	}
	return out
}

var ErrInvalidReport = errors.New("diagnostic report is invalid")
