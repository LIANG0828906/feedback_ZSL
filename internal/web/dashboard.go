package web

import (
	"context"
	"os"
	"path/filepath"

	"combatjudge/internal/engine"
	"combatjudge/internal/model"
	"combatjudge/internal/sample"
	"combatjudge/internal/schema"
)

type VersionInfo struct {
	Version    string             `json:"version"`
	Bytes      int                `json:"bytes"`
	Losses     []model.LossNotice `json:"losses"`
	TraceCount int                `json:"trace_count"`
}

type Dashboard struct {
	Project          *model.Project          `json:"project"`
	Truth            *model.TruthFile        `json:"truth"`
	Versions         []VersionInfo           `json:"versions"`
	Live             *engine.Report          `json:"live"`
	Full             *engine.Report          `json:"full"`
	Resume           *engine.Report          `json:"resume"`
	LiveMismatch     *engine.CompareMismatch `json:"live_mismatch"`
	ResumeMismatch   *engine.CompareMismatch `json:"resume_mismatch"`
	Invalidations    []engine.Invalidation   `json:"invalidations"`
	Incremental      *engine.Report          `json:"incremental"`
	IncrementalMatch bool                    `json:"incremental_match"`
	QueueWaits       []QueueWaitView         `json:"queue_waits"`
}

type QueueWaitView struct {
	Path       string `json:"path"`
	IntervalNo int    `json:"interval_no"`
	Reason     string `json:"reason"`
	WaitedMS   int64  `json:"waited_ms"`
}

func BuildDashboard(ctx context.Context, workDir string) (*Dashboard, error) {
	if workDir == "" {
		var err error
		workDir, err = os.MkdirTemp("", "combatjudge-web-*")
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}

	project := sample.BuildProject()
	truth := sample.BuildTruth()

	live, err := engine.Run(ctx, project, engine.Options{
		PathLabel: "real-time", SnapshotID: "live", DBPath: filepath.Join(workDir, "live.db"),
		MaxParallel: 1, MaxQueuedIntervals: 1,
	})
	if err != nil {
		return nil, err
	}
	full, err := engine.Run(ctx, project, engine.Options{
		PathLabel: "full-parallel", SnapshotID: "full", DBPath: filepath.Join(workDir, "full.db"),
		MaxParallel: 4, MaxQueuedIntervals: 2, GeometryBufferBytes: engine.DefaultBufferBytes,
	})
	if err != nil {
		return nil, err
	}
	resumeDB := filepath.Join(workDir, "resume.db")
	if _, err := engine.Run(ctx, project, engine.Options{
		PathLabel: "resume-seed", SnapshotID: "resume", DBPath: resumeDB, StopAfterInterval: 2, MaxParallel: 2,
	}); err != nil {
		return nil, err
	}
	resume, err := engine.Run(ctx, project, engine.Options{
		PathLabel: "breakpoint-resume", SnapshotID: "resume", DBPath: resumeDB, MaxParallel: 2,
	})
	if err != nil {
		return nil, err
	}

	oldDB := filepath.Join(workDir, "incremental.db")
	if _, err := engine.Run(ctx, project, engine.Options{PathLabel: "old", SnapshotID: "inc", DBPath: oldDB}); err != nil {
		return nil, err
	}
	edited := sample.BuildProject()
	edited.Colliders[2].Min.X = 8_500_000
	invalidations := engine.PlanEdit(project, edited, engine.DefaultIntervalFrames, engine.EditSpec{
		Kind: "collider_size", ColliderNo: 3, Field: "min.x",
	})
	incremental, err := engine.Run(ctx, edited, engine.Options{PathLabel: "incremental", SnapshotID: "inc", DBPath: oldDB})
	if err != nil {
		return nil, err
	}
	editedFull, err := engine.Run(ctx, edited, engine.Options{
		PathLabel: "full-edited", SnapshotID: "edited-full", DBPath: filepath.Join(workDir, "edited-full.db"), MaxParallel: 4,
	})
	if err != nil {
		return nil, err
	}
	incMismatch := engine.CompareReports(editedFull, incremental)

	versions := []VersionInfo{}
	for _, v := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		data, losses, err := schema.Export(project, v)
		if err != nil {
			return nil, err
		}
		info := VersionInfo{Version: v, Bytes: len(data), Losses: losses}
		if v != model.SchemaV3 {
			loaded, err := schema.Load(data)
			if err != nil {
				return nil, err
			}
			info.TraceCount = len(loaded.Traces)
		}
		versions = append(versions, info)
	}

	queueWaits := []QueueWaitView{}
	for _, r := range []*engine.Report{live, full, resume, incremental} {
		for _, w := range r.QueueWaits {
			queueWaits = append(queueWaits, QueueWaitView{Path: r.Path, IntervalNo: w.IntervalNo, Reason: w.Reason, WaitedMS: w.WaitedMS})
		}
	}

	return &Dashboard{
		Project: project, Truth: truth, Versions: versions,
		Live: live, Full: full, Resume: resume,
		LiveMismatch: engine.CompareReports(full, live), ResumeMismatch: engine.CompareReports(full, resume),
		Invalidations: invalidations, Incremental: incremental, IncrementalMatch: incMismatch == nil,
		QueueWaits: queueWaits,
	}, nil
}
