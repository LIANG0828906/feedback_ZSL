package service

import (
	"context"
	"fmt"
	"time"

	"strata/internal/model"
)

func (s *Service) StartExactJob(req PreviewRequest) Envelope {
	snap, mesh := s.latestSnapshot()
	if snap == nil || mesh == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no active mesh", "job", 0, nil))
	}
	camera, clip := s.frameInputs(req.Camera, req.Clip)
	s.mu.Lock()
	s.runSeq++
	job := &jobState{
		ID: fmt.Sprintf("job-%04d", s.runSeq), Mode: "exact", Version: snap.Version,
		Status: "running", Total: len(mesh.Chunks), StartedAt: now(),
		UpdatedAt: now(), Done: make(chan struct{}),
	}
	job.Snap = snap
	job.Camera = camera
	job.Clip = clip
	s.jobs[job.ID] = job
	s.mu.Unlock()
	go s.runExactJob(job)
	return Envelope{OK: true, Data: map[string]any{"job": jobToDTO(job)}}
}

func (s *Service) PauseJob() Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var target *jobState
	for _, job := range s.jobs {
		if job.Status == "running" || job.Status == "building" {
			target = job
		}
	}
	if target == nil {
		return errorEnvelope(model.NewError(model.ESTATE, "no running exact job", "job.pause", 0, nil))
	}
	target.Pause = true
	target.Status = "pausing"
	target.UpdatedAt = now()
	return Envelope{OK: true, Data: map[string]any{"job": jobToDTO(target)}}
}

func (s *Service) ResumeJob(jobID string) Envelope {
	s.mu.Lock()
	var job *jobState
	if jobID != "" {
		job = s.jobs[jobID]
	} else {
		for _, candidate := range s.jobs {
			if candidate.Status == "paused" {
				job = candidate
			}
		}
	}
	if job == nil || job.Status != "paused" {
		s.mu.Unlock()
		return errorEnvelope(model.NewError(model.ESTATE, "no paused exact job to resume", "job.resume", 0, nil))
	}
	job.Pause = false
	job.Status = "running"
	job.UpdatedAt = now()
	s.mu.Unlock()
	go s.runExactJob(job)
	return Envelope{OK: true, Data: map[string]any{"job": jobToDTO(job)}}
}

func (s *Service) runExactJob(job *jobState) {
	snap := job.Snap
	defer func() {
		if job.Done != nil {
			close(job.Done)
			job.Done = nil
		}
	}()
	reasons := map[string]string{}
	for _, id := range MakeChunkIDs(snap) {
		reasons[id] = "exact-job"
	}
	s.setJob(job, "building", "")
	mesh, err := s.builder.BuildIncremental(snap.CleanPoints, snap.Config, nil, reasons, snap.Version, "exact-job", 1)
	if err != nil {
		s.failJob(job, err)
		return
	}
	runID := job.RunID
	if runID == "" {
		runID = s.nextRunID("job")
		job.RunID = runID
		if err := s.store.BeginRun(context.Background(), runID, "exact-job", snap.Version); err != nil {
			s.failJob(job, err)
			return
		}
	} else {
		if err := s.store.BeginRun(context.Background(), runID, "exact-job", snap.Version); err != nil {
			s.failJob(job, err)
			return
		}
	}
	existing, err := s.store.LoadCheckpoints(context.Background(), runID)
	if err != nil {
		s.failJob(job, err)
		return
	}
	committed := map[string]bool{}
	s.mu.Lock()
	job.Sequence = job.Sequence[:0]
	for _, cp := range existing {
		committed[cp.ChunkID] = true
		job.Sequence = append(job.Sequence, cp.ChunkID)
	}
	job.Committed = len(existing)
	job.Status = "running"
	job.UpdatedAt = now()
	s.mu.Unlock()
	for _, chunk := range mesh.Chunks {
		if committed[chunk.ID] {
			continue
		}
		s.mu.Lock()
		paused := job.Pause || job.Cancel
		s.mu.Unlock()
		if paused {
			_ = s.store.FinishRun(context.Background(), runID, "paused", mesh.ContentHash, "paused by user")
			s.setJob(job, "paused", "")
			return
		}
		cp, err := checkpointFromChunk(mesh, chunk, snap, runID)
		if err != nil {
			s.failJob(job, err)
			return
		}
		if _, err := s.store.CommitChunk(context.Background(), cp); err != nil {
			s.failJob(job, err)
			return
		}
		s.mu.Lock()
		job.Sequence = append(job.Sequence, chunk.ID)
		job.Committed++
		job.UpdatedAt = now()
		s.mu.Unlock()
	}
	if err := s.store.FinishRun(context.Background(), runID, "complete", mesh.ContentHash, ""); err != nil {
		s.failJob(job, err)
		return
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
	result, err := s.renderExactComparison(snap, mesh, job.Camera, job.Clip, runID, job.Sequence, 0, 0)
	if err != nil {
		s.failJob(job, err)
		return
	}
	s.mu.Lock()
	job.Result = result
	job.Status = "complete"
	job.UpdatedAt = now()
	s.mu.Unlock()
}

func (s *Service) setJob(job *jobState, status, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.Status = status
	job.Error = errText
	job.UpdatedAt = now()
}

func (s *Service) failJob(job *jobState, err error) {
	appErr := model.AsAppError(err)
	s.mu.Lock()
	defer s.mu.Unlock()
	job.Status = "failed"
	job.Error = appErr.Error()
	job.UpdatedAt = now()
	if job.RunID != "" {
		_ = s.store.FinishRun(context.Background(), job.RunID, "failed", "", appErr.Error())
	}
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
