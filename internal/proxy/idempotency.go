package proxy

import (
	"context"
	"net/http"
	"sync"
)

type idempotencyCache struct {
	mu      sync.Mutex
	entries map[string]*idempotencyEntry
}

type idempotencyEntry struct {
	done     chan struct{}
	response idempotentResponse
}

type idempotentResponse struct {
	status int
	header http.Header
	body   []byte
}

func newIdempotencyCache() *idempotencyCache {
	return &idempotencyCache{entries: make(map[string]*idempotencyEntry)}
}

func (c *idempotencyCache) begin(key string) (*idempotencyEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[key]; entry != nil {
		return entry, false
	}
	entry := &idempotencyEntry{done: make(chan struct{})}
	c.entries[key] = entry
	return entry, true
}

func (e *idempotencyEntry) complete(status int, header http.Header, body []byte) {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	e.response = idempotentResponse{
		status: status,
		header: cloneHeader(header),
		body:   append([]byte(nil), body...),
	}
	close(e.done)
}

func (e *idempotencyEntry) wait(ctx context.Context) (idempotentResponse, bool) {
	select {
	case <-e.done:
		return e.response, true
	case <-ctx.Done():
		return idempotentResponse{}, false
	}
}

func (r idempotentResponse) replay(w http.ResponseWriter) {
	for k, values := range r.header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Gateway-Idempotent-Replay", "true")
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body)
}

type bufferedResponse struct {
	header    http.Header
	status    int
	body      []byte
	maxBody   int64
	overflow  bool
	abandoned bool
	wrote     bool
}

func newBufferedResponse(maxBody int64) *bufferedResponse {
	return &bufferedResponse{header: make(http.Header), maxBody: maxBody}
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.wrote {
		return
	}
	b.status = status
	b.wrote = true
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	if b.maxBody > 0 && int64(len(b.body)+len(p)) > b.maxBody {
		b.overflow = true
		return 0, context.Canceled
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

func (b *bufferedResponse) Flush() {}

func (b *bufferedResponse) result() (int, http.Header, []byte) {
	if b.overflow {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		header.Set("X-Gateway-Error-Code", "IDEMPOTENCY_RESPONSE_TOO_LARGE")
		body := []byte(`{"code":"IDEMPOTENCY_RESPONSE_TOO_LARGE","message":"idempotent response exceeded the cache limit"}`)
		return http.StatusBadGateway, header, body
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	return status, cloneHeader(b.header), append([]byte(nil), b.body...)
}

func (b *bufferedResponse) flush(w http.ResponseWriter) {
	if b.abandoned {
		return
	}
	status, header, body := b.result()
	for k, values := range header {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
