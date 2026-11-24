package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

type StateRecord struct {
	SessionID string
	ShotIndex int
	Payload   any
}

type Task func(ctx context.Context, buffer *Buffer) error

type Buffer struct {
	sessionID string
	ch        chan StateRecord
	ctx       context.Context
}

func (b *Buffer) SessionID() string { return b.sessionID }

func (b *Buffer) Push(record StateRecord) error {
	if record.SessionID == "" {
		record.SessionID = b.sessionID
	}
	if record.SessionID != b.sessionID {
		return errors.New("buffer record session id does not match worker buffer")
	}
	select {
	case <-b.ctx.Done():
		return b.ctx.Err()
	case b.ch <- record:
		return nil
	}
}

type Pool struct {
	MaxParallel    int
	BufferCapacity int
	Write          func(record StateRecord) error

	sem     chan struct{}
	writeMu sync.Mutex
	active  atomic.Int32
	peak    atomic.Int32
}

func New(maxParallel, bufferCapacity int, writer func(StateRecord) error) *Pool {
	if maxParallel <= 0 {
		maxParallel = 1
	}
	if bufferCapacity <= 0 {
		bufferCapacity = 1
	}
	return &Pool{
		MaxParallel:    maxParallel,
		BufferCapacity: bufferCapacity,
		Write:          writer,
		sem:            make(chan struct{}, maxParallel),
	}
}

func (p *Pool) Active() int32 { return p.active.Load() }
func (p *Pool) Peak() int32   { return p.peak.Load() }

func (p *Pool) Execute(ctx context.Context, sessionID string, task Task) error {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.sem }()

	current := p.active.Add(1)
	for {
		peak := p.peak.Load()
		if current <= peak || p.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	defer p.active.Add(-1)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	buffer := &Buffer{sessionID: sessionID, ch: make(chan StateRecord, p.BufferCapacity), ctx: runCtx}
	flushErr := make(chan error, 1)
	go func() {
		var firstErr error
		for record := range buffer.ch {
			if p.Write != nil {
				p.writeMu.Lock()
				err := p.Write(record)
				p.writeMu.Unlock()
				if err != nil && firstErr == nil {
					firstErr = err
					cancel()
				}
			}
		}
		flushErr <- firstErr
	}()

	taskErr := task(runCtx, buffer)
	close(buffer.ch)
	writeErr := <-flushErr
	return errors.Join(taskErr, writeErr)
}

func (p *Pool) RunAll(ctx context.Context, tasks map[string]Task) map[string]error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make(map[string]error, len(tasks))
	for sessionID, task := range tasks {
		sessionID := sessionID
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.Execute(ctx, sessionID, task)
			mu.Lock()
			results[sessionID] = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}
