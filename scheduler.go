package dialogue

import (
	"sort"
	"time"
)

// SchedulerConfig declares all resource budgets.
type SchedulerConfig struct {
	MaxParallelism  int                 `json:"max_parallelism"`
	SampleBufferCap int                 `json:"sample_buffer_cap"`
	QueueBudget     int                 `json:"queue_budget"`
	BeforeJob       func(clipID string) `json:"-"`
}

func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		MaxParallelism:  2,
		SampleBufferCap: FrameBufferCap,
		QueueBudget:     2,
	}
}

// QueueWaitRecord captures backpressure instead of dropping jobs.
type QueueWaitRecord struct {
	ClipID       string `json:"clip_id"`
	Reason       string `json:"reason"`
	WaitedMicros int64  `json:"waited_micros"`
}

// ClipRunReport is the isolated outcome for one clip.
type ClipRunReport struct {
	Index          int             `json:"index"`
	ClipID         string          `json:"clip_id"`
	State          State           `json:"state"`
	ErrorCode      string          `json:"error_code,omitempty"`
	Error          *Error          `json:"error,omitempty"`
	QueueWait      QueueWaitRecord `json:"queue_wait"`
	FrameCount     int             `json:"frame_count"`
	CommittedOrder int             `json:"committed_order,omitempty"`
}

// SchedulerReport contains outcomes, waits and stable commit order.
type SchedulerReport struct {
	Config       SchedulerConfig   `json:"config"`
	Runs         []ClipRunReport   `json:"runs"`
	QueueWaits   []QueueWaitRecord `json:"queue_waits"`
	State        StateMatrix       `json:"state"`
	CommitOrder  []string          `json:"commit_order"`
	OverlapError *Error            `json:"overlap_error,omitempty"`
}

type scheduledJob struct {
	index int
	clip  *Clip
	wait  QueueWaitRecord
}

type jobOutcome struct {
	index int
	id    string
	out   *ClipOutput
	err   *Error
	wait  QueueWaitRecord
}

// RunScheduled processes clips in a bounded worker pool. Checkpoint writes,
// when store is non-nil, happen in stable clip order after all work settles.
func RunScheduled(project *Project, cfg SchedulerConfig, store *FileCheckpointStore) (*SchedulerReport, *Error) {
	if cfg.MaxParallelism <= 0 {
		cfg.MaxParallelism = 2
	}
	if cfg.SampleBufferCap <= 0 {
		cfg.SampleBufferCap = FrameBufferCap
	}
	if cfg.QueueBudget <= 0 {
		cfg.QueueBudget = cfg.MaxParallelism
	}
	if overlapErr := ValidateOverlap(project.Clips); overlapErr != nil {
		if be, ok := overlapErr.(*Error); ok {
			return &SchedulerReport{Config: cfg, CommitOrder: []string{}, QueueWaits: []QueueWaitRecord{}}, be
		}
	}

	ids := make([]string, 0, len(project.Clips))
	for _, clip := range project.Clips {
		ids = append(ids, clip.ID)
	}
	machine := NewMachine(ids)
	jobs := make(chan scheduledJob, cfg.QueueBudget)
	outcomes := make(chan jobOutcome, len(project.Clips))

	for w := 0; w < cfg.MaxParallelism; w++ {
		go func() {
			for job := range jobs {
				if cfg.BeforeJob != nil {
					cfg.BeforeJob(job.clip.ID)
				}
				out, pErr := processScheduledClip(project, job.clip, machine, cfg.SampleBufferCap)
				state, _ := machine.State(job.clip.ID)
				outcomes <- jobOutcome{
					index: job.index,
					id:    job.clip.ID,
					out:   out,
					err:   pErr,
					wait:  job.wait,
				}
				_ = state
			}
		}()
	}

	waits := make([]QueueWaitRecord, len(project.Clips))
	for i, clip := range project.Clips {
		wait := QueueWaitRecord{ClipID: clip.ID, Reason: "queue_slot_available"}
		select {
		case jobs <- scheduledJob{index: i, clip: clip, wait: wait}:
		default:
			wait.Reason = "queue_budget_exhausted"
			start := time.Now()
			jobs <- scheduledJob{index: i, clip: clip, wait: wait}
			wait.WaitedMicros = time.Since(start).Microseconds()
		}
		// The sent job carries the final wait record. If the non-blocking
		// branch sent a zero record and the blocking branch updated wait after
		// constructing the job, replace the queued copy through a small caveat:
		// callers read waits[i] below and outcome.wait is corrected by index.
		waits[i] = wait
	}
	close(jobs)

	results := make([]jobOutcome, 0, len(project.Clips))
	for len(results) < len(project.Clips) {
		out := <-outcomes
		if out.wait.Reason == "queue_slot_available" {
			out.wait = waits[out.index]
		} else {
			out.wait = waits[out.index]
		}
		results = append(results, out)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].index < results[j].index })

	report := &SchedulerReport{
		Config:      cfg,
		Runs:        make([]ClipRunReport, 0, len(results)),
		QueueWaits:  append([]QueueWaitRecord(nil), waits...),
		CommitOrder: []string{},
	}
	for _, result := range results {
		run := ClipRunReport{
			Index:     result.index,
			ClipID:    result.id,
			QueueWait: result.wait,
		}
		if result.err != nil {
			run.ErrorCode = result.err.Code
			run.Error = result.err
			run.State = StateFailed
			report.Runs = append(report.Runs, run)
			continue
		}
		run.FrameCount = len(result.out.Results)
		if err := machine.Transition(result.id, StateCommitted); err != nil {
			if be, ok := err.(*Error); ok {
				run.ErrorCode = be.Code
				run.Error = be
				run.State, _ = machine.State(result.id)
				report.Runs = append(report.Runs, run)
				continue
			}
		}
		if store != nil {
			if err := store.SaveClip(result.out); err != nil {
				run.ErrorCode = CodeStage
				run.Error = &Error{Code: CodeStage, Message: err.Error(), ClipID: result.id}
				run.State = StateFailed
				report.Runs = append(report.Runs, run)
				continue
			}
		}
		report.CommitOrder = append(report.CommitOrder, result.id)
		run.CommittedOrder = len(report.CommitOrder)
		run.State = StateCommitted
		report.Runs = append(report.Runs, run)
	}
	report.State = machine.Snapshot()
	return report, nil
}

func processScheduledClip(project *Project, clip *Clip, machine *Machine, bufferCap int) (*ClipOutput, *Error) {
	if err := machine.Transition(clip.ID, StateExtracting); err != nil {
		return nil, err.(*Error)
	}
	if err := ValidatePCM(clip); err != nil {
		failState(machine, clip.ID)
		return nil, asError(err, clip.ID, CodeDecode)
	}
	if bufferCap <= 0 {
		bufferCap = FrameBufferCap
	}
	buffer := make([]int16, bufferCap)
	intervals := QuantizeClip(project, clip)
	features := make([]FrameFeature, 0, len(intervals))
	featureKeys := make([]string, 0, len(intervals))
	for _, interval := range intervals {
		feature, err := ExtractFrame(project, clip, interval, buffer)
		if err != nil {
			be := asError(err, clip.ID, CodeStage)
			failState(machine, clip.ID)
			return nil, be
		}
		features = append(features, feature)
		featureKeys = append(featureKeys, featureCacheKey(project, clip, interval).String())
	}
	if err := machine.Transition(clip.ID, StateCalibrating); err != nil {
		failState(machine, clip.ID)
		return nil, err.(*Error)
	}
	if clip.ForceStageFailure {
		be := &Error{Code: CodeStage, Message: "injected single-clip internal stage failure", ClipID: clip.ID}
		failState(machine, clip.ID)
		return nil, be
	}
	cal, gap, err := CalibrateClip(project, clip, features)
	if err != nil {
		be := asError(err, clip.ID, CodeStage)
		failState(machine, clip.ID)
		return nil, be
	}
	if gap != nil {
		be := &Error{Code: CodeLoud, Message: "target loudness unreachable", ClipID: clip.ID, Details: *gap}
		failState(machine, clip.ID)
		return nil, be
	}
	out := &ClipOutput{
		ClipID:          clip.ID,
		Intervals:       intervals,
		Features:        features,
		Calibration:     cal,
		FeatureKeys:     featureKeys,
		CalibrationKeys: make([]string, 0, len(features)),
		Results:         make([]FrameResult, 0, len(features)),
	}
	for i, feature := range features {
		ckey := calibrationCacheKey(project, clip, intervals[i], featureKeys[i]).String()
		out.CalibrationKeys = append(out.CalibrationKeys, ckey)
		out.Results = append(out.Results, FrameResult{
			ClipID:         clip.ID,
			FrameNo:        feature.FrameNo,
			RangeID:        feature.RangeID,
			Feature:        feature,
			Calibration:    cal.Frames[i],
			FeatureKey:     featureKeys[i],
			CalibrationKey: ckey,
		})
	}
	return out, nil
}

func failState(machine *Machine, clipID string) {
	current, ok := machine.State(clipID)
	if !ok || current == StateFailed || current == StateCommitted {
		return
	}
	_ = machine.Transition(clipID, StateFailed)
}

func asError(err error, clipID, fallbackCode string) *Error {
	if be, ok := err.(*Error); ok {
		if be.ClipID == "" {
			be.ClipID = clipID
		}
		return be
	}
	return &Error{Code: fallbackCode, Message: err.Error(), ClipID: clipID}
}
