package pipeline

import (
	"time"

	"audiobus/domain"
	"audiobus/errs"
)

type ResourcePool struct {
	workers   chan struct{}
	buffers   chan struct{}
	bufferCap int
	Records   []domain.QueueRecord
}

func NewResourcePool(maxWorkers, bufferPerWorker int) *ResourcePool {
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if bufferPerWorker <= 0 {
		bufferPerWorker = 1
	}
	return &ResourcePool{
		workers:   make(chan struct{}, maxWorkers),
		buffers:   make(chan struct{}, maxWorkers),
		bufferCap: bufferPerWorker,
	}
}

func (p *ResourcePool) Acquire(channel, samples int) (func(), error) {
	if samples > p.bufferCap {
		return nil, &errs.Error{Code: errs.EOvf, Channel: channel, Reason: "sample buffer exceeds per-thread limit"}
	}
	start := time.Now()
	select {
	case p.buffers <- struct{}{}:
	default:
		p.buffers <- struct{}{}
		p.Records = append(p.Records, domain.QueueRecord{Reason: "buffer_exhausted", Channel: channel, WaitNanos: time.Since(start).Nanoseconds()})
	}
	p.workers <- struct{}{}
	return func() {
		<-p.workers
		<-p.buffers
	}, nil
}

func (p *ResourcePool) QueueRecords() []domain.QueueRecord {
	return append([]domain.QueueRecord(nil), p.Records...)
}
