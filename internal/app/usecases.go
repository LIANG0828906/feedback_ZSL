package app

import (
	"encoding/json"
	"os"
	"path/filepath"

	"multiplexgw/internal/bizerr"
	"multiplexgw/internal/checkpoint"
	"multiplexgw/internal/engine"
	"multiplexgw/internal/incremental"
	"multiplexgw/internal/model"
	"multiplexgw/internal/parallel"
	"multiplexgw/internal/sample"
	"multiplexgw/internal/schema"
)

func verifyIncremental(p *model.Project) (IncrementalSummary, []*incremental.Report) {
	mgr, err := incremental.NewManager(p, engineOptions())
	summary := IncrementalSummary{AllMatch: err == nil}
	var reports []*incremental.Report
	if err != nil {
		summary.Cases = append(summary.Cases, IncrementalCaseSummary{Name: "manager", Failure: err.Error()})
		return summary, nil
	}
	edits := []struct {
		name string
		edit model.Edit
	}{
		{"weight-stream-5", model.Edit{Kind: model.EditWeight, StreamID: 5, Weight: 4000}},
		{"parent-stream-9-to-5", model.Edit{Kind: model.EditParent, StreamID: 9, ParentID: 5}},
		{"window-stream-7", model.Edit{Kind: model.EditWindow, StreamID: 7, WindowBytes: 4096}},
		{"update-wu-stream-1", model.Edit{Kind: model.EditUpdateEvent, EventIndex: 31, Event: eventCopy(p, 31, 4096)}},
		{"delete-data-event-20", model.Edit{Kind: model.EditDeleteEvent, EventIndex: 20}},
		{"append-wu-stream-1", model.Edit{Kind: model.EditAppendEvent, EventIndex: len(p.Events), Event: &model.Event{
			Type: model.EventWindowUpdate, StreamID: 1, IncrementBytes: 1024,
		}}},
	}
	summary.AllMatch = true
	for _, tc := range edits {
		rep, _, applyErr := mgr.Apply(tc.edit)
		cs := IncrementalCaseSummary{Name: tc.name, Kind: tc.edit.Kind}
		if rep != nil {
			cs.StartIndex = rep.Closure.EventStart
			cs.ReusedEvents = len(rep.ReusedEvents)
			cs.RecomputedEvents = len(rep.RecomputedEvents)
			cs.SemanticEqual = rep.SemanticEqual
			cs.RootSource = rep.Closure.RootSource
			rep.Actual = nil
			reports = append(reports, rep)
		}
		if applyErr != nil {
			cs.Failure = applyErr.Error()
			if be, ok := bizerr.AsError(applyErr); ok && len(be.Context) > 0 {
				cs.Failure += " context=" + contextString(be.Context)
			}
		}
		if !cs.SemanticEqual || cs.Failure != "" {
			summary.AllMatch = false
		}
		summary.Cases = append(summary.Cases, cs)
	}
	return summary, reports
}

func eventCopy(p *model.Project, idx int, newIncrement int64) *model.Event {
	if idx < 0 || idx >= len(p.Events) {
		return nil
	}
	ev := p.Events[idx]
	ev.IncrementBytes = newIncrement
	return &ev
}

func contextString(m map[string]any) string {
	b, _ := json.Marshal(m)
	return string(b)
}

func verifyParallel(p *model.Project, cfg parallel.Config) ParallelSummary {
	ids := []string{"C1", "C2", "C3", "C4"}
	jobs := make([]parallel.Job, 0, len(ids))
	for _, id := range ids {
		cp, err := incremental.CloneProject(p)
		if err != nil {
			continue
		}
		cp.Connection.ID = id
		jobs = append(jobs, parallel.Job{ConnectionID: id, Project: cp})
	}
	reports := parallel.RunConnections(jobs, cfg)
	sum := ParallelSummary{Workers: cfg.Workers, BufferBudget: cfg.MaxPendingFrames, AllOK: true}
	for _, r := range reports {
		sum.ConnectionIDs = append(sum.ConnectionIDs, r.ConnectionID)
		sum.Codes = append(sum.Codes, r.Code)
		if r.Result != nil {
			sum.TraceCounts = append(sum.TraceCounts, len(r.Result.Traces))
		} else {
			sum.TraceCounts = append(sum.TraceCounts, 0)
		}
		if r.Code != bizerr.OK {
			sum.AllOK = false
		}
	}
	if code, _, err := parallel.DeterministicBackpressureProbe(cfg.Workers); err == nil {
		sum.ProbeCode = code
	} else {
		sum.ProbeCode = bizerr.CodeOf(err)
	}
	backpressureReports := parallel.RunConnections([]parallel.Job{{
		ConnectionID: "BUFFER-PROBE",
		Project:      bufferFullProject(),
	}}, parallel.Config{Workers: 1, MaxPendingFrames: 1})
	if len(backpressureReports) == 1 {
		sum.BufferProbeCode = backpressureReports[0].Code
		sum.BufferProbeOK = sum.BufferProbeCode == bizerr.BufferFull
	}
	sum.AllOK = sum.AllOK && sum.BufferProbeOK
	return sum
}

func verifyCheckpoint(p *model.Project, artifactDir string) CheckpointReport {
	rep := CheckpointReport{DBPath: filepath.Join(artifactDir, "checkpoint-recovery.db")}
	_ = os.MkdirAll(artifactDir, 0o755)
	_ = os.Remove(rep.DBPath)
	_ = os.Remove(rep.DBPath + "-wal")
	_ = os.Remove(rep.DBPath + "-shm")

	want := checkpoint.ExpectedVersion{
		CheckpointVersion: checkpoint.CheckpointVersion,
		SchemaVersion:     p.SchemaVersion,
		ProjectID:         p.ProjectID,
	}
	store, err := checkpoint.Open(rep.DBPath)
	if err != nil {
		rep.Mismatches = []string{err.Error()}
		return rep
	}
	if err := store.WriteMeta(checkpoint.Meta{
		CheckpointVersion: checkpoint.CheckpointVersion,
		SchemaVersion:     p.SchemaVersion,
		ProjectID:         p.ProjectID,
	}); err != nil {
		rep.Mismatches = []string{err.Error()}
		_ = store.Close()
		return rep
	}

	eng, err := engine.New(p, engineOptions())
	if err != nil {
		rep.Mismatches = []string{err.Error()}
		_ = store.Close()
		return rep
	}
	const commitBefore = 6
	for i := 0; i < commitBefore; i++ {
		trace, stepErr := eng.Step()
		if stepErr != nil {
			rep.Mismatches = []string{stepErr.Error()}
			_ = store.Close()
			return rep
		}
		if err := store.SaveEngine(p.Connection.ID, eng, trace.LogicalMS, checkpoint.FaultNone); err != nil {
			rep.Mismatches = []string{err.Error()}
			_ = store.Close()
			return rep
		}
	}
	rep.CommittedBefore = eng.CommittedThrough
	prefixTraces := append([]model.Trace(nil), eng.Traces...)
	if err := store.Close(); err != nil {
		rep.Mismatches = []string{err.Error()}
		return rep
	}

	store2, err := checkpoint.Open(rep.DBPath)
	if err != nil {
		rep.Mismatches = []string{err.Error()}
		return rep
	}
	defer store2.Close()
	restored, committed, err := store2.LoadEngine(p.Connection.ID, p, engineOptions(), want)
	if err != nil {
		rep.Mismatches = []string{err.Error()}
		return rep
	}
	rep.RecoveredFrom = committed
	for {
		trace, stepErr := restored.Step()
		if stepErr != nil {
			rep.Mismatches = []string{stepErr.Error()}
			return rep
		}
		if trace == nil {
			break
		}
		if err := store2.SaveEngine(p.Connection.ID, restored, trace.LogicalMS, checkpoint.FaultNone); err != nil {
			rep.Mismatches = []string{err.Error()}
			return rep
		}
	}
	rep.FinalCommitted = restored.CommittedThrough
	combined := append(prefixTraces, restored.Traces...)
	rep.TraceCount = len(combined)

	expected, err := engine.Run(p, engineOptions())
	if err != nil {
		rep.Mismatches = []string{err.Error()}
		return rep
	}
	rep.Mismatches = compareTraces(expected.Traces, combined)
	rep.Match = len(rep.Mismatches) == 0 && rep.FinalCommitted == len(p.Events)-1
	return rep
}

// RunCLICheckpoint is used by the demo crash/recover commands.
func RunCLICheckpoint(dataDir, dbPath, mode string, after int, recoverOnly bool) (*model.RunResult, error) {
	p, err := loadCheckpointProject(dataDir)
	if err != nil {
		return nil, err
	}
	want := checkpoint.ExpectedVersion{CheckpointVersion: 1, SchemaVersion: p.SchemaVersion, ProjectID: p.ProjectID}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	meta, err := store.LoadMeta()
	if err != nil {
		return nil, err
	}
	if meta == nil || meta.CheckpointVersion == 0 {
		if err := store.WriteMeta(checkpoint.Meta{CheckpointVersion: 1, SchemaVersion: p.SchemaVersion, ProjectID: p.ProjectID}); err != nil {
			return nil, err
		}
	}
	eng, committed, err := store.LoadEngine(p.Connection.ID, p, engineOptions(), want)
	if err != nil {
		return nil, err
	}
	if recoverOnly {
		for {
			trace, err := eng.Step()
			if err != nil {
				return eng.Result(), err
			}
			if trace == nil {
				break
			}
			if err := store.SaveEngine(p.Connection.ID, eng, trace.LogicalMS, checkpoint.FaultNone); err != nil {
				return eng.Result(), err
			}
		}
		return eng.Result(), nil
	}
	_ = committed
	for {
		trace, err := eng.Step()
		if err != nil {
			return eng.Result(), err
		}
		if trace == nil {
			break
		}
		fault := checkpoint.FaultNone
		if after >= 0 && trace.EventIndex == after {
			if mode == string(checkpoint.FaultBeforeGlobalSeq) {
				fault = checkpoint.FaultBeforeGlobalSeq
			}
			if mode == string(checkpoint.FaultAfterCommit) {
				fault = checkpoint.FaultAfterCommit
			}
		}
		if err := store.SaveEngine(p.Connection.ID, eng, trace.LogicalMS, fault); err != nil {
			return eng.Result(), err
		}
	}
	return eng.Result(), nil
}

func loadCheckpointProject(dataDir string) (*model.Project, error) {
	if dataDir != "" {
		path := filepath.Join(dataDir, "project-v1.json")
		if _, err := os.Stat(path); err == nil {
			return schema.LoadFile(path)
		}
	}
	bundle, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		return nil, err
	}
	return bundle.Canonical, nil
}
