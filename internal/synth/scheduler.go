package synth

import "sync"

// SchedulerConfig 限制最大并行度与每任务采样缓冲上限。
type SchedulerConfig struct {
	MaxWorkers      int
	MaxBufferFrames int
}

// BackpressureEvent 记录缓冲耗尽导致的阻塞排队（不静默丢样）。
type BackpressureEvent struct {
	Channel int    `json:"channel"`
	Reason  string `json:"reason"`
	Pending int    `json:"pending"`
}

// ScheduleResult 是并发调度结果，按通道下标有序。
type ScheduleResult struct {
	Curves       []*ChannelCurve     `json:"curves"`
	Backpressure []BackpressureEvent `json:"backpressure"`
}

// SynthesizeConcurrent 用有界工作池并行合成各通道。
// 每个任务持有独立采样缓冲；在途任务数达到上限时提交方阻塞排队。
func SynthesizeConcurrent(events []Event, m *Manifest, horizon int, cfg SchedulerConfig) *ScheduleResult {
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 1
	}
	if cfg.MaxBufferFrames <= 0 {
		cfg.MaxBufferFrames = horizon
	}

	byChan := make([][]Event, m.ChannelCount)
	for _, ev := range events {
		byChan[ev.Channel] = append(byChan[ev.Channel], ev)
	}

	type job struct {
		channel int
		events  []Event
	}
	type result struct {
		channel int
		curve   *ChannelCurve
	}

	jobs := make(chan job)
	results := make(chan result, m.ChannelCount)
	sem := make(chan struct{}, cfg.MaxWorkers) // 在途任务上限，耗尽即阻塞排队
	var wg sync.WaitGroup
	for w := 0; w < cfg.MaxWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results <- result{channel: j.channel, curve: Synthesize(j.channel, j.events, m, horizon)}
				<-sem // 任务完成后释放一个在途名额
			}
		}()
	}

	bp := make([]BackpressureEvent, 0)
	var bpMu sync.Mutex
	for c := 0; c < m.ChannelCount; c++ {
		// 每通道缓冲上限检查：超限也走同一阻塞信号量并记录原因。
		if horizon > cfg.MaxBufferFrames {
			bpMu.Lock()
			bp = append(bp, BackpressureEvent{
				Channel: c, Reason: "sample-buffer-limit-queue",
				Pending: horizon,
			})
			bpMu.Unlock()
		}
		sem <- struct{}{} // 缓冲/在途耗尽时在此阻塞排队，绝不丢样
		jobs <- job{channel: c, events: byChan[c]}
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	curves := make([]*ChannelCurve, m.ChannelCount)
	for r := range results {
		curves[r.channel] = r.curve
	}
	return &ScheduleResult{Curves: curves, Backpressure: bp}
}
