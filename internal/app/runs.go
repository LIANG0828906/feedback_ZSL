package app

import (
	"context"
	"time"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/pipeline"
)

func (a *App) StartRun(sampleID, mode string, params pipeline.Params, stopAfter int) error {
	a.mu.Lock()
	if a.runs == nil {
		a.runs = map[string]*RunStatus{}
	}
	if a.cancels == nil {
		a.cancels = map[string]context.CancelFunc{}
	}
	if status, ok := a.runs[sampleID]; ok && status.Running {
		a.mu.Unlock()
		return errs.New(errs.ESTATE, "sample %s is already running", sampleID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancels[sampleID] = cancel
	now := time.Now().UTC()
	a.runs[sampleID] = &RunStatus{SampleID: sampleID, Mode: mode, Running: true, StartedAt: now, UpdatedAt: now, Message: "running"}
	a.mu.Unlock()

	go func() {
		defer cancel()
		st, err := a.SharedStore()
		if err != nil {
			a.finishRun(sampleID, err)
			return
		}
		defer st.Close()
		s, err := a.Sample(sampleID)
		if err != nil {
			a.finishRun(sampleID, err)
			return
		}
		_, err = pipeline.NewOrchestrator().RunOrResume(ctx, sampleID, &s.Mesh, &s.Manifest, params, st, pipeline.RunOptions{
			Path: mode, StopAfter: stopAfter,
		})
		a.finishRun(sampleID, err)
	}()
	return nil
}

func (a *App) finishRun(sampleID string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	status := a.runs[sampleID]
	if status == nil {
		status = &RunStatus{SampleID: sampleID}
		a.runs[sampleID] = status
	}
	status.Running = false
	status.UpdatedAt = time.Now().UTC()
	if err != nil {
		status.ErrorCode = errs.Code(err)
		status.Message = err.Error()
		if status.ErrorCode == "" && err == context.Canceled {
			status.ErrorCode = "E_CANCELED"
			status.Message = "canceled by user at committed boundary"
		}
	} else {
		status.Message = "committed"
	}
	if cancel, ok := a.cancels[sampleID]; ok {
		cancel()
		delete(a.cancels, sampleID)
	}
}

func (a *App) CancelRun(sampleID string) error {
	a.mu.Lock()
	cancel, ok := a.cancels[sampleID]
	status := a.runs[sampleID]
	a.mu.Unlock()
	if !ok {
		return errs.New(errs.ESTATE, "no active run for %s", sampleID)
	}
	cancel()
	if status != nil {
		status.Message = "cancel requested; stopping before next stage"
	}
	return nil
}

func (a *App) RunStatuses() map[string]*RunStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]*RunStatus, len(a.runs))
	for k, v := range a.runs {
		cp := *v
		out[k] = &cp
	}
	return out
}
