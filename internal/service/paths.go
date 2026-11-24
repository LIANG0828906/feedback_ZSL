package service

import (
	"context"
	"encoding/json"
	"fmt"

	"strata/internal/model"
	"strata/internal/pipeline"
	"strata/internal/render"
	"strata/internal/store"
)

func (s *Service) VerifyPaths() Envelope {
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "verify", 0, nil))
	}
	result := &PathResult{Runs: map[string]string{}}

	fullMesh, fullSeq, fullRun, err := s.buildRun(snap, mesh, "full", false, 1.0)
	if err != nil {
		return errorEnvelope(err)
	}
	result.FullMeshHash = fullMesh.ContentHash
	result.FullSequence = fullSeq
	result.Runs["full"] = fullRun

	incMesh, incSeq, incRun, err := s.buildRun(snap, fullMesh, "incremental", false, 1.0)
	if err != nil {
		return errorEnvelope(err)
	}
	result.IncrementalMeshHash = incMesh.ContentHash
	result.IncrementalSequence = incSeq
	result.Runs["incremental"] = incRun

	resumeMesh, resumeSeq, resumeRun, err := s.buildResumeRun(snap, fullMesh)
	if err != nil {
		return errorEnvelope(err)
	}
	result.ResumeMeshHash = resumeMesh.ContentHash
	result.ResumeSequence = resumeSeq
	result.Runs["resume"] = resumeRun

	previewMesh, previewSeq, previewRun, err := s.buildRun(snap, fullMesh, "preview", true, snap.Config.PreviewSample)
	if err != nil {
		return errorEnvelope(err)
	}
	result.PreviewMeshHash = previewMesh.ContentHash
	result.PreviewSequence = previewSeq
	result.Runs["preview"] = previewRun

	hashes := []string{fullMesh.ContentHash, incMesh.ContentHash, resumeMesh.ContentHash, previewMesh.ContentHash}
	seqs := [][]string{fullSeq, incSeq, resumeSeq, previewSeq}
	for i := 1; i < len(hashes); i++ {
		if hashes[i] != hashes[0] {
			result.FirstDifferingChunk = firstMeshChunkDifference(fullMesh, []*model.Mesh{fullMesh, incMesh, resumeMesh, previewMesh}[i])
			result.Passed = false
			return Envelope{OK: false, Error: pathDiffError("verify_paths.mesh", result.FirstDifferingChunk, map[string]any{
				"hashes": hashes,
			}), Data: result}
		}
		if chunk, ok := firstSequenceDifference(fullSeq, seqs[i]); !ok {
			result.FirstDifferingChunk = chunk
			result.Passed = false
			return Envelope{OK: false, Error: pathDiffError("verify_paths.sequence", chunk, map[string]any{
				"path": []string{"full", "incremental", "resume", "preview"}[i],
			}), Data: result}
		}
	}
	result.Passed = true
	return Envelope{OK: true, Data: result}
}

func (s *Service) buildRun(snap *snapshot, previous *model.Mesh, mode string, preview bool, sampleRate float64) (*model.Mesh, []string, string, error) {
	reasons := map[string]string{}
	if previous == nil {
		for _, id := range MakeChunkIDs(snap) {
			reasons[id] = "verify-" + mode
		}
	}
	buildMode := "verify-" + mode
	mesh, err := s.builder.BuildIncremental(snap.CleanPoints, snap.Config, previous, reasons, snap.Version, buildMode, sampleRate)
	if err != nil {
		return nil, nil, "", err
	}
	if preview {
		_, _ = render.Render(mesh, s.session.Camera, s.session.Clip, render.RenderOptions{
			Mode: "preview", SampleRate: snap.Config.PreviewSample, FlatShade: true,
			FrameID: s.nextFrameID("V"), Version: snap.Version,
		})
	}
	runID := s.nextRunID("verify-" + mode)
	if err := s.store.BeginRun(context.Background(), runID, "verify-"+mode, snap.Version); err != nil {
		return nil, nil, "", err
	}
	seq, err := s.commitMesh(context.Background(), snap, mesh, runID)
	if err != nil {
		_ = s.store.FinishRun(context.Background(), runID, "failed", "", err.Error())
		return nil, nil, "", err
	}
	if err := s.store.FinishRun(context.Background(), runID, "complete", mesh.ContentHash, ""); err != nil {
		return nil, nil, "", err
	}
	return mesh, seq, runID, nil
}

func (s *Service) buildResumeRun(snap *snapshot, previous *model.Mesh) (*model.Mesh, []string, string, error) {
	mesh, err := s.builder.BuildIncremental(snap.CleanPoints, snap.Config, previous, map[string]string{}, snap.Version, "verify-resume", 1)
	if err != nil {
		return nil, nil, "", err
	}
	runID := s.nextRunID("verify-resume")
	if err := s.store.BeginRun(context.Background(), runID, "verify-resume", snap.Version); err != nil {
		return nil, nil, "", err
	}
	half := len(mesh.Chunks) / 2
	seq := make([]string, 0, len(mesh.Chunks))
	for i := 0; i < half; i++ {
		cp, err := checkpointFromChunk(mesh, mesh.Chunks[i], snap, runID)
		if err != nil {
			return nil, nil, "", err
		}
		if _, err := s.store.CommitChunk(context.Background(), cp); err != nil {
			return nil, nil, "", err
		}
		seq = append(seq, mesh.Chunks[i].ID)
	}
	// Simulate reopening the database: continue strictly from committed rows.
	existing, err := s.store.LoadCheckpoints(context.Background(), runID)
	if err != nil {
		return nil, nil, "", err
	}
	seq = seq[:0]
	committed := map[string]bool{}
	for _, cp := range existing {
		seq = append(seq, cp.ChunkID)
		committed[cp.ChunkID] = true
	}
	for _, chunk := range mesh.Chunks {
		if committed[chunk.ID] {
			continue
		}
		cp, err := checkpointFromChunk(mesh, chunk, snap, runID)
		if err != nil {
			return nil, nil, "", err
		}
		if _, err := s.store.CommitChunk(context.Background(), cp); err != nil {
			return nil, nil, "", err
		}
		seq = append(seq, chunk.ID)
	}
	if err := s.store.FinishRun(context.Background(), runID, "complete", mesh.ContentHash, ""); err != nil {
		return nil, nil, "", err
	}
	return mesh, seq, runID, nil
}

func (s *Service) commitMesh(ctx context.Context, snap *snapshot, mesh *model.Mesh, runID string) ([]string, error) {
	seq := make([]string, 0, len(mesh.Chunks))
	for _, chunk := range mesh.Chunks {
		cp, err := checkpointFromChunk(mesh, chunk, snap, runID)
		if err != nil {
			return nil, err
		}
		if _, err := s.store.CommitChunk(ctx, cp); err != nil {
			return nil, err
		}
		seq = append(seq, chunk.ID)
	}
	return seq, nil
}

func (s *Service) RestartResume(runID string) Envelope {
	ctx := context.Background()
	if runID == "" {
		latest, err := s.store.LatestRun(ctx, "exact")
		if err != nil {
			return errorEnvelope(err)
		}
		if latest == nil {
			return errorEnvelope(model.NewError(model.ESTATE, "no exact run to resume", "run.id", 0, nil))
		}
		runID = latest.ID
	}
	run, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return errorEnvelope(err)
	}
	if run.Status == "complete" {
		return Envelope{OK: true, Data: map[string]any{"runId": runID, "status": "complete", "alreadyComplete": true}}
	}
	versionSnap, err := s.store.LoadVersion(ctx, run.Version)
	if err != nil {
		return errorEnvelope(err)
	}
	var cfg pipeline.Config
	if err := json.Unmarshal(versionSnap.ParamsJSON, &cfg); err != nil {
		return errorEnvelope(model.NewError(model.EDECODE, "invalid checkpoint config", "version.params", 0, nil))
	}
	if err := cfg.Validate(); err != nil {
		return errorEnvelope(err)
	}
	snap := &snapshot{
		Version: run.Version, SourceVersion: versionSnap.SourceVersion,
		CleanPoints: versionSnap.Points, Config: cfg,
	}
	mesh, err := s.builder.BuildIncremental(snap.CleanPoints, cfg, nil, nil, snap.Version, "resume-restart", 1)
	if err != nil {
		return errorEnvelope(err)
	}
	existing, err := s.store.LoadCheckpoints(ctx, runID)
	if err != nil {
		return errorEnvelope(err)
	}
	committed := map[string]bool{}
	seq := make([]string, 0, len(mesh.Chunks))
	for _, cp := range existing {
		committed[cp.ChunkID] = true
		seq = append(seq, cp.ChunkID)
	}
	if err := s.store.BeginRun(ctx, runID, run.Mode, run.Version); err != nil {
		return errorEnvelope(err)
	}
	for _, chunk := range mesh.Chunks {
		if committed[chunk.ID] {
			continue
		}
		cp, err := checkpointFromChunk(mesh, chunk, snap, runID)
		if err != nil {
			return errorEnvelope(err)
		}
		if _, err := s.store.CommitChunk(ctx, cp); err != nil {
			return errorEnvelope(err)
		}
		seq = append(seq, chunk.ID)
	}
	if err := s.store.FinishRun(ctx, runID, "complete", mesh.ContentHash, ""); err != nil {
		return errorEnvelope(err)
	}
	s.mu.Lock()
	s.session.ExactByVersion[snap.Version] = mesh
	if s.session.Committed[snap.Version] == nil {
		s.session.Committed[snap.Version] = map[string]bool{}
	}
	for _, c := range mesh.Chunks {
		s.session.Committed[snap.Version][c.ID] = true
	}
	s.mu.Unlock()
	return Envelope{OK: true, Data: map[string]any{
		"runId": runID, "status": "complete", "resumedFrom": len(existing),
		"sequence": seq, "meshHash": mesh.ContentHash,
	}}
}

func firstMeshChunkDifference(a *model.Mesh, b *model.Mesh) string {
	if a == nil || b == nil {
		return "C00"
	}
	n := min(len(a.Chunks), len(b.Chunks))
	for i := 0; i < n; i++ {
		if a.Chunks[i].ContentHash != b.Chunks[i].ContentHash {
			return a.Chunks[i].ID
		}
	}
	if len(a.Chunks) != len(b.Chunks) {
		if n < len(a.Chunks) {
			return a.Chunks[n].ID
		}
		return b.Chunks[n].ID
	}
	return ""
}

func firstSequenceDifference(a, b []string) (string, bool) {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return fmt.Sprintf("%s!=%s", a[i], b[i]), false
		}
	}
	if len(a) != len(b) {
		return "sequence-length", false
	}
	return "", true
}

func pathDiffError(path, firstChunk string, extra map[string]any) *model.AppError {
	if extra == nil {
		extra = map[string]any{}
	}
	extra["firstDifferingChunk"] = firstChunk
	return model.NewError(model.EDIFF, "preview, resume and full reconstruction differ", path, 0, extra)
}

var _ = store.Run{}
