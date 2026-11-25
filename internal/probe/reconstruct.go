package probe

import (
	"context"
	"math"
	"sort"
	"sync"

	"lightprobe/internal/checkpoint"
	"lightprobe/internal/diag"
	"lightprobe/internal/scene"
)

type FaultController struct {
	mu        sync.Mutex
	remaining map[string]int
}

func NewFaultController() *FaultController {
	return &FaultController{remaining: make(map[string]int)}
}

func (f *FaultController) FailNext(probeID string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remaining[probeID] = times
}

func (f *FaultController) Clear(probeID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.remaining, probeID)
}

func (f *FaultController) Take(probeID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.remaining[probeID] <= 0 {
		return false
	}
	f.remaining[probeID]--
	return true
}

func (e *Engine) InjectProbeFault(probeID string, times int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.recordByIDLocked(probeID); err != nil {
		return err
	}
	e.faults.FailNext(probeID, times)
	return nil
}

func (e *Engine) ClearProbeFault(probeID string) {
	e.faults.Clear(probeID)
}

func reconstructJob(_ context.Context, job Job, faults *FaultController) Result {
	if faults != nil && faults.Take(job.ProbeID) {
		return Result{
			ProbeID: job.ProbeID, RoomID: job.RoomID,
			ErrCode: diag.CodeFault, ErrMessage: "injected single-probe reconstruction exception",
		}
	}

	position := scene.Vec3(job.Position)
	cell, _ := scene.PhotonCellIndex(job.PhotonGrid, position)
	photonCell, ok := job.PhotonGrid.CellAt(cell)
	sh := [9][3]int64{}
	if ok {
		sh = photonCell.Coeff
	}
	scale := job.PhotonGrid.Scale
	if scale <= 0 {
		scale = scene.DefaultFixedPointScale
	}

	radiance := scene.Vec3(job.InputRadiance)
	for channel := 0; channel < scene.ColorChannelCount; channel++ {
		encodedRadiance := int64(math.Round(radiance[channel] * float64(scale)))
		sh[0][channel] += encodedRadiance
		sh[1][channel] += int64(job.VisibilitySamples)
	}
	digest, err := checkpoint.HashJSON(sh)
	if err != nil {
		return Result{ProbeID: job.ProbeID, RoomID: job.RoomID, ErrCode: diag.CodeSchema, ErrMessage: err.Error()}
	}
	return Result{
		ProbeID: job.ProbeID, RoomID: job.RoomID,
		Radiance: job.InputRadiance, SH: sh, PhotonCell: cell, SHDigest: digest,
	}
}

func (e *Engine) jobsForIDsLocked(ids []string) []Job {
	jobs := make([]Job, 0, len(ids))
	for seq, id := range ids {
		record := e.records[id]
		jobs = append(jobs, Job{
			Seq: seq, ProbeID: id, RoomID: record.RoomID,
			I: record.I, J: record.J, K: record.K,
			Position: record.Position, InputRadiance: record.InputRadiance,
			VisibilitySamples: record.VisibilitySamples, PhotonGrid: e.scene.PhotonGrid,
			SHCost: scene.SHCoefficientCount * scene.ColorChannelCount,
		})
	}
	return jobs
}

func (e *Engine) runProbeIDsLocked(ctx context.Context, ids []string) ([]Submission, []QueueEvent, []string) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	sortedIDs := append([]string{}, ids...)
	sort.Strings(sortedIDs)
	orderIndex := make(map[string]int, len(e.order))
	for index, id := range e.order {
		orderIndex[id] = index
	}

	for _, id := range sortedIDs {
		record := e.records[id]
		record.Status = ProbeSampling
		record.Attempts++
		record.LastError = ""
		e.records[id] = record
	}

	jobs := e.jobsForIDsLocked(sortedIDs)
	newSubmissions := make([]Submission, 0, len(sortedIDs))
	failed := make([]string, 0)
	events := runBoundedJobs(
		ctx, e.config, e.metrics, jobs,
		func(ctx context.Context, job Job) Result {
			return reconstructJob(ctx, job, e.faults)
		},
		func(result Result) {
			id := result.ProbeID
			old := e.records[id]
			updated := *old
			if result.ErrCode != "" {
				updated.Status = ProbeFailed
				updated.Dirty = true
				updated.LastError = result.ErrCode + ": " + result.ErrMessage
				failed = append(failed, id)
			} else {
				updated.Status = ProbeReconstructed
				updated.Dirty = false
				updated.Radiance = scene.Vec3(result.Radiance)
				updated.SH = result.SH
				updated.PhotonCell = result.PhotonCell
				updated.SHDigest = result.SHDigest
				updated.LastError = ""
				updated.RecomputeCount++
			}
			e.records[id] = &updated
			submission := Submission{
				Seq: len(newSubmissions), Order: orderIndex[id], ProbeID: id,
				RoomID: result.RoomID, Radiance: updated.Radiance,
				SHDigest: updated.SHDigest, Recomputed: result.ErrCode == "",
			}
			newSubmissions = append(newSubmissions, submission)
			e.submissions = append(e.submissions, submission)
		},
	)

	sort.Strings(failed)
	return newSubmissions, events, failed
}

func (e *Engine) allProbeIDsForRoomLocked(roomID string) []string {
	ids := make([]string, 0)
	for _, id := range e.order {
		if e.records[id].RoomID == roomID {
			ids = append(ids, id)
		}
	}
	return ids
}

func (e *Engine) dirtyProbeIDsLocked() []string {
	ids := make([]string, 0)
	for _, id := range e.order {
		record := e.records[id]
		if record.Dirty || record.Status == ProbePending || record.Status == ProbeFailed {
			ids = append(ids, id)
		}
	}
	return ids
}
