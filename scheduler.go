package dialoguecal

import (
	"context"
	"sort"
	"sync"
	"time"
)

type ScheduleOptions struct {
	MaxParallelism      int
	WorkerBufferSamples int
	QueueBudget         int
}

func ScheduleOptionsFromProject(p *Project) ScheduleOptions {
	return ScheduleOptions{
		MaxParallelism: p.MaxParallelism, WorkerBufferSamples: p.WorkerBufferSamples,
		QueueBudget: p.QueueBudget,
	}
}

type QueueEvent struct {
	Reason     string
	WaitMicros int64
	IntervalID string
	FirstFrame int
	LastFrame  int
}

type Injection struct {
	CorruptProject       bool
	InvertPhoneme        bool
	ContradictScale      bool
	FailSegmentID        string
	FailFrame            int
	HasFailFrame         bool
	CancelAfterIntervals int
	CrashAfterIntervals  int
}

type frameJob struct {
	index      int
	intervalID string
	dialogueID string
	segmentID  string
	slots      []FrameSlot
}

// ExtractFrames runs a bounded worker pool. The pending queue is bounded by
// QueueBudget; a producer that cannot enqueue immediately blocks and records
// the reason and wait, rather than dropping a frame. Results are stored in
// input order before flattening, so completion order cannot affect output.
func ExtractFrames(ctx context.Context, p *Project, intervals []Interval, opts ScheduleOptions, inj Injection) ([]FrameFeature, []QueueEvent, error) {
	if p == nil {
		return nil, nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	workers := opts.MaxParallelism
	if workers <= 0 {
		workers = 1
	}
	queueBudget := opts.QueueBudget
	if queueBudget < 1 {
		queueBudget = 1
	}
	bufferLimit := opts.WorkerBufferSamples
	if bufferLimit <= 0 {
		return nil, nil, &Error{Code: CodeBudget, Field: "workerBufferSamples", OperandA: int64(bufferLimit), Message: "worker sample buffer must be positive"}
	}

	jobs, err := buildFrameJobs(p, intervals, bufferLimit)
	if err != nil {
		return nil, nil, err
	}
	if len(jobs) == 0 {
		return []FrameFeature{}, nil, nil
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}
	jobCh := make(chan frameJob, queueBudget)
	results := make([][]FrameFeature, len(jobs))
	jobErrs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobCh {
				select {
				case <-ctx.Done():
					jobErrs[job.index] = &Error{
						Code: CodeCancel, SegmentID: job.segmentID,
						Frame: job.slots[0].Frame, HasFrame: true, Message: "extraction canceled",
					}
					continue
				default:
				}
				failFrame := inj.FailFrame
				if !inj.HasFailFrame {
					failFrame = job.slots[0].Frame
				}
				if inj.FailSegmentID == job.segmentID &&
					(!inj.HasFailFrame || (failFrame >= job.slots[0].Frame && failFrame <= job.slots[len(job.slots)-1].Frame)) {
					jobErrs[job.index] = &Error{
						Code: CodeStage, Path: "frames", SegmentID: job.segmentID,
						Frame: failFrame, HasFrame: true,
						Message: "injected stage failure inside frame interval",
					}
					continue
				}
				d := dialogueByID(p, job.dialogueID)
				if d == nil {
					jobErrs[job.index] = &Error{Code: CodeSchema, Path: "dialogues", Message: "unknown dialogue " + job.dialogueID}
					continue
				}
				fs := make([]FrameFeature, 0, len(job.slots))
				for _, slot := range job.slots {
					f, err := ComputeFrameFeatures(job.dialogueID, job.segmentID, d.PCM, slot.Frame, slot.StartSample, slot.EndSample, slot.Weight)
					if err != nil {
						jobErrs[job.index] = err
						break
					}
					if f.Energy > p.FrameEnergyLimit {
						jobErrs[job.index] = &Error{
							Code: CodeCal, Field: "frameEnergyLimit", SegmentID: job.segmentID,
							Frame: f.Frame, HasFrame: true, OperandA: f.Energy, OperandB: p.FrameEnergyLimit,
							Message: "frame energy exceeds declared limit",
						}
						break
					}
					fs = append(fs, f)
				}
				if jobErrs[job.index] == nil {
					results[job.index] = fs
				}
			}
		}()
	}

	var events []QueueEvent
	var sendErr error
	for _, job := range jobs {
		select {
		case jobCh <- job:
		default:
			start := time.Now()
			select {
			case jobCh <- job:
				events = append(events, QueueEvent{
					Reason: "queue_budget_exhausted", WaitMicros: time.Since(start).Microseconds(),
					IntervalID: job.intervalID, FirstFrame: job.slots[0].Frame,
					LastFrame: job.slots[len(job.slots)-1].Frame,
				})
			case <-ctx.Done():
				sendErr = &Error{
					Code: CodeCancel, SegmentID: job.segmentID,
					Frame: job.slots[0].Frame, HasFrame: true, Message: "producer canceled while queued",
				}
			}
		}
		if sendErr != nil {
			break
		}
	}
	close(jobCh)
	wg.Wait()

	for _, e := range jobErrs {
		if e != nil {
			return nil, events, e
		}
	}
	if sendErr != nil {
		return nil, events, sendErr
	}
	flat := make([]FrameFeature, 0)
	for _, fs := range results {
		flat = append(flat, fs...)
	}
	sort.Slice(flat, func(i, j int) bool {
		if flat[i].DialogueID != flat[j].DialogueID {
			return flat[i].DialogueID < flat[j].DialogueID
		}
		return flat[i].Frame < flat[j].Frame
	})
	return flat, events, nil
}

func buildFrameJobs(p *Project, intervals []Interval, bufferLimit int) ([]frameJob, error) {
	ivs := append([]Interval(nil), intervals...)
	sort.Slice(ivs, func(i, j int) bool {
		if ivs[i].DialogueID != ivs[j].DialogueID {
			return ivs[i].DialogueID < ivs[j].DialogueID
		}
		if ivs[i].StartFrame != ivs[j].StartFrame {
			return ivs[i].StartFrame < ivs[j].StartFrame
		}
		return ivs[i].SegmentID < ivs[j].SegmentID
	})
	jobs := make([]frameJob, 0)
	for _, iv := range ivs {
		if len(iv.Frames) == 0 {
			continue
		}
		chunk := make([]FrameSlot, 0)
		samples := int64(0)
		flush := func() {
			if len(chunk) == 0 {
				return
			}
			jobs = append(jobs, frameJob{
				index: len(jobs), intervalID: iv.ID, dialogueID: iv.DialogueID,
				segmentID: iv.SegmentID, slots: append([]FrameSlot(nil), chunk...),
			})
			chunk = chunk[:0]
			samples = 0
		}
		for _, slot := range iv.Frames {
			n := slot.EndSample - slot.StartSample
			if n > int64(bufferLimit) {
				return nil, &Error{
					Code: CodeBudget, Field: "workerBufferSamples", SegmentID: iv.SegmentID,
					Frame: slot.Frame, HasFrame: true, OperandA: n, OperandB: int64(bufferLimit),
					Message: "single frame window exceeds worker sample buffer",
				}
			}
			if samples+n > int64(bufferLimit) && len(chunk) > 0 {
				flush()
			}
			chunk = append(chunk, slot)
			samples += n
		}
		flush()
	}
	return jobs, nil
}
