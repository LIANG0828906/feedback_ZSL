package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var waitBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05,
	0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

type entryKey struct {
	protocol string
	priority string
}

type entryMetrics struct {
	requests map[string]uint64
	errors   map[string]uint64
	active   int64
}

type upstreamMetrics struct {
	protocol string
	requests uint64
	active   int64
	errors   map[string]uint64
	retries  map[string]uint64
}

type waitHistogram struct {
	buckets []uint64
	count   uint64
	sum     float64
}

func (h *waitHistogram) observe(v float64) {
	if v < 0 {
		v = 0
	}
	h.count++
	h.sum += v
	for i, b := range waitBuckets {
		if v <= b {
			h.buckets[i]++
		}
	}
}

func (h *waitHistogram) quantile(q float64) float64 {
	if h.count == 0 {
		return 0
	}
	rank := uint64(q*float64(h.count) + 0.999999)
	if rank == 0 {
		rank = 1
	}
	for i, b := range waitBuckets {
		if h.buckets[i] >= rank {
			return b
		}
	}
	return waitBuckets[len(waitBuckets)-1]
}

type ProtocolError struct {
	Protocol  string    `json:"protocol"`
	StreamID  string    `json:"stream_id"`
	Code      string    `json:"code"`
	Message   string    `json:"message,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type Registry struct {
	mu       sync.Mutex
	entries  map[string]*entryMetrics
	upstream map[string]*upstreamMetrics
	waits    map[entryKey]*waitHistogram
	migrate  map[string]uint64
	lastErr  *ProtocolError
}

func New() *Registry {
	r := &Registry{
		entries:  make(map[string]*entryMetrics),
		upstream: make(map[string]*upstreamMetrics),
		waits:    make(map[entryKey]*waitHistogram),
		migrate:  make(map[string]uint64),
	}
	for _, p := range []string{"h2", "h3"} {
		r.entries[p] = &entryMetrics{requests: make(map[string]uint64), errors: make(map[string]uint64)}
	}
	for _, result := range []string{"observed", "succeeded", "failed"} {
		r.migrate[result] = 0
	}
	return r
}

func (r *Registry) RegisterUpstream(id, protocol string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.upstream[id] == nil {
		r.upstream[id] = &upstreamMetrics{
			protocol: protocol,
			errors:   make(map[string]uint64),
			retries:  make(map[string]uint64),
		}
	}
}

func (r *Registry) EntryStart(protocol, priority string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[protocol]
	if e == nil {
		e = &entryMetrics{requests: make(map[string]uint64), errors: make(map[string]uint64)}
		r.entries[protocol] = e
	}
	e.requests[priority]++
	e.active++
}

func (r *Registry) EntryFinish(protocol string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[protocol]; e != nil && e.active > 0 {
		e.active--
	}
}

func (r *Registry) EntryError(protocol, code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[protocol]
	if e == nil {
		e = &entryMetrics{requests: make(map[string]uint64), errors: make(map[string]uint64)}
		r.entries[protocol] = e
	}
	e.errors[code]++
}

func (r *Registry) ProtocolError(protocol, streamID, code, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastErr = &ProtocolError{
		Protocol:  protocol,
		StreamID:  streamID,
		Code:      code,
		Message:   message,
		Timestamp: time.Now().UTC(),
	}
}

func (r *Registry) LastProtocolError() *ProtocolError {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastErr == nil {
		return nil
	}
	cp := *r.lastErr
	return &cp
}

func (r *Registry) UpstreamStart(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.ensureUpstreamLocked(id)
	u.requests++
	u.active++
}

func (r *Registry) UpstreamFinish(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := r.upstream[id]; u != nil && u.active > 0 {
		u.active--
	}
}

func (r *Registry) UpstreamError(id, phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureUpstreamLocked(id).errors[phase]++
}

func (r *Registry) Retry(id, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureUpstreamLocked(id).retries[reason]++
}

func (r *Registry) ensureUpstreamLocked(id string) *upstreamMetrics {
	u := r.upstream[id]
	if u == nil {
		u = &upstreamMetrics{errors: make(map[string]uint64), retries: make(map[string]uint64)}
		r.upstream[id] = u
	}
	return u
}

func (r *Registry) ObservePriorityWait(protocol, priority string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := entryKey{protocol: protocol, priority: priority}
	h := r.waits[key]
	if h == nil {
		h = &waitHistogram{buckets: make([]uint64, len(waitBuckets))}
		r.waits[key] = h
	}
	h.observe(d.Seconds())
}

func (r *Registry) Migration(result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.migrate[result]++
}

type WaitQuantiles struct {
	Protocol string  `json:"protocol"`
	Priority string  `json:"priority"`
	P50      float64 `json:"p50_ms"`
	P95      float64 `json:"p95_ms"`
	P99      float64 `json:"p99_ms"`
	Count    uint64  `json:"count"`
}

func (r *Registry) WaitQuantiles() []WaitQuantiles {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []WaitQuantiles
	for key, h := range r.waits {
		out = append(out, WaitQuantiles{
			Protocol: key.protocol,
			Priority: key.priority,
			P50:      h.quantile(0.50) * 1000,
			P95:      h.quantile(0.95) * 1000,
			P99:      h.quantile(0.99) * 1000,
			Count:    h.count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Protocol != out[j].Protocol {
			return out[i].Protocol < out[j].Protocol
		}
		return out[i].Priority < out[j].Priority
	})
	return out
}

func (r *Registry) MigrationCounts() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]uint64, len(r.migrate))
	for k, v := range r.migrate {
		out[k] = v
	}
	return out
}

type UpstreamSnapshot struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"`
	Requests uint64 `json:"requests"`
	Active   int64  `json:"active"`
	Errors   uint64 `json:"errors"`
	Retries  uint64 `json:"retries"`
}

func (r *Registry) Upstreams() []UpstreamSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []UpstreamSnapshot
	for id, u := range r.upstream {
		var errs, retries uint64
		for _, n := range u.errors {
			errs += n
		}
		for _, n := range u.retries {
			retries += n
		}
		out = append(out, UpstreamSnapshot{
			ID: id, Protocol: u.protocol, Requests: u.requests, Active: u.active,
			Errors: errs, Retries: retries,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type entrySnapshot struct {
	protocol string
	priority string
	count    uint64
}

func (r *Registry) Render(queue map[string]int) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	line("# HELP gateway_entry_requests_total Requests accepted by an entry protocol.\n")
	line("# TYPE gateway_entry_requests_total counter\n")
	var entries []entrySnapshot
	for protocol, e := range r.entries {
		for priority, n := range e.requests {
			entries = append(entries, entrySnapshot{protocol: protocol, priority: priority, count: n})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].protocol != entries[j].protocol {
			return entries[i].protocol < entries[j].protocol
		}
		return entries[i].priority < entries[j].priority
	})
	for _, e := range entries {
		line("gateway_entry_requests_total{entry=%q,priority=%q} %d\n", e.protocol, e.priority, e.count)
	}

	line("# HELP gateway_entry_active_streams Requests currently active on an entry protocol.\n")
	line("# TYPE gateway_entry_active_streams gauge\n")
	for _, protocol := range sortedEntries(r.entries) {
		line("gateway_entry_active_streams{entry=%q} %d\n", protocol, r.entries[protocol].active)
	}

	line("# HELP gateway_entry_errors_total Entry or gateway failures by stable code.\n")
	line("# TYPE gateway_entry_errors_total counter\n")
	var errRows []entrySnapshot
	for protocol, e := range r.entries {
		for code, n := range e.errors {
			errRows = append(errRows, entrySnapshot{protocol: protocol, priority: code, count: n})
		}
	}
	sort.Slice(errRows, func(i, j int) bool {
		if errRows[i].protocol != errRows[j].protocol {
			return errRows[i].protocol < errRows[j].protocol
		}
		return errRows[i].priority < errRows[j].priority
	})
	for _, e := range errRows {
		line("gateway_entry_errors_total{entry=%q,code=%q} %d\n", e.protocol, e.priority, e.count)
	}

	line("# HELP gateway_upstream_requests_total Requests forwarded to an upstream.\n")
	line("# TYPE gateway_upstream_requests_total counter\n")
	line("# HELP gateway_upstream_active_requests Requests currently active on an upstream.\n")
	line("# TYPE gateway_upstream_active_requests gauge\n")
	line("# HELP gateway_upstream_errors_total Upstream failures by disconnect phase.\n")
	line("# TYPE gateway_upstream_errors_total counter\n")
	line("# HELP gateway_upstream_retries_total Automatic retries by reason.\n")
	line("# TYPE gateway_upstream_retries_total counter\n")
	ids := sortedUpstreamIDs(r.upstream)
	for _, id := range ids {
		u := r.upstream[id]
		line("gateway_upstream_requests_total{upstream=%q,protocol=%q} %d\n", id, u.protocol, u.requests)
		line("gateway_upstream_active_requests{upstream=%q,protocol=%q} %d\n", id, u.protocol, u.active)
		var phases []string
		for phase := range u.errors {
			phases = append(phases, phase)
		}
		sort.Strings(phases)
		for _, phase := range phases {
			line("gateway_upstream_errors_total{upstream=%q,protocol=%q,phase=%q} %d\n", id, u.protocol, phase, u.errors[phase])
		}
		var reasons []string
		for reason := range u.retries {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			line("gateway_upstream_retries_total{upstream=%q,protocol=%q,reason=%q} %d\n", id, u.protocol, reason, u.retries[reason])
		}
	}

	line("# HELP gateway_priority_wait_seconds Time a request waited in the priority scheduler.\n")
	line("# TYPE gateway_priority_wait_seconds summary\n")
	keys := make([]entryKey, 0, len(r.waits))
	for key := range r.waits {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].protocol != keys[j].protocol {
			return keys[i].protocol < keys[j].protocol
		}
		return keys[i].priority < keys[j].priority
	})
	for _, key := range keys {
		h := r.waits[key]
		for _, q := range []struct {
			label string
			value float64
		}{{"0.5", 0.5}, {"0.95", 0.95}, {"0.99", 0.99}} {
			line("gateway_priority_wait_seconds{entry=%q,priority=%q,quantile=%q} %s\n",
				key.protocol, key.priority, q.label, formatFloat(h.quantile(q.value)))
		}
		line("gateway_priority_wait_seconds_sum{entry=%q,priority=%q} %s\n", key.protocol, key.priority, formatFloat(h.sum))
		line("gateway_priority_wait_seconds_count{entry=%q,priority=%q} %d\n", key.protocol, key.priority, h.count)
	}

	line("# HELP gateway_priority_queue_depth Requests waiting for an upstream forwarding slot.\n")
	line("# TYPE gateway_priority_queue_depth gauge\n")
	var priorities []string
	for p := range queue {
		priorities = append(priorities, p)
	}
	sort.Strings(priorities)
	for _, p := range priorities {
		line("gateway_priority_queue_depth{priority=%q} %d\n", p, queue[p])
	}

	line("# HELP gateway_h3_connection_migrations_total HTTP/3 address-change continuity outcomes.\n")
	line("# TYPE gateway_h3_connection_migrations_total counter\n")
	var results []string
	for result := range r.migrate {
		results = append(results, result)
	}
	sort.Strings(results)
	for _, result := range results {
		line("gateway_h3_connection_migrations_total{result=%q} %d\n", result, r.migrate[result])
	}
	return b.String()
}

func sortedEntries(entries map[string]*entryMetrics) []string {
	out := make([]string, 0, len(entries))
	for k := range entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUpstreamIDs(up map[string]*upstreamMetrics) []string {
	out := make([]string, 0, len(up))
	for k := range up {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
