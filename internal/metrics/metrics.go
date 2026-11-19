package metrics

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"edgegateway/internal/store"
)

var durationBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

type Registry struct {
	mu       sync.Mutex
	counters map[string]float64
	hist     map[string][]uint64
	start    time.Time
	store    *store.Store
}

func New(st *store.Store) *Registry {
	return &Registry{
		counters: make(map[string]float64),
		hist:     make(map[string][]uint64),
		start:    time.Now(),
		store:    st,
	}
}

func (r *Registry) Inc(name string, labels map[string]string, delta float64) {
	r.mu.Lock()
	r.counters[key(name, labels)] += delta
	r.mu.Unlock()
}

func (r *Registry) Set(name string, labels map[string]string, value float64) {
	r.mu.Lock()
	r.counters[key(name, labels)] = value
	r.mu.Unlock()
}

func (r *Registry) ObserveDuration(ingress string, seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key("edge_gateway_forward_duration_seconds", map[string]string{"ingress": ingress})
	buckets := r.hist[k]
	if buckets == nil {
		buckets = make([]uint64, len(durationBuckets))
	}
	for i, bound := range durationBuckets {
		if seconds <= bound {
			buckets[i]++
		}
	}
	r.hist[k] = buckets
	r.counters[key("edge_gateway_forward_duration_seconds_count", map[string]string{"ingress": ingress})]++
	r.counters[key("edge_gateway_forward_duration_seconds_sum", map[string]string{"ingress": ingress})] += seconds
}

func key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Quote(labels[k]))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func (r *Registry) Snapshot() map[string]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]float64, len(r.counters))
	for k, v := range r.counters {
		out[k] = v
	}
	return out
}

func (r *Registry) Render(ctx context.Context, w io.Writer) {
	r.mu.Lock()
	counters := make(map[string]float64, len(r.counters))
	for k, v := range r.counters {
		counters[k] = v
	}
	hist := make(map[string][]uint64, len(r.hist))
	for k, v := range r.hist {
		hist[k] = append([]uint64(nil), v...)
	}
	r.mu.Unlock()

	active, _ := r.store.CountActive(ctx)
	fmt.Fprintf(w, "# HELP edge_gateway_active_sessions Active sessions by ingress.\n")
	fmt.Fprintf(w, "# TYPE edge_gateway_active_sessions gauge\n")
	fmt.Fprintf(w, "edge_gateway_active_sessions{ingress=\"quic\"} %d\n", active["quic"])
	fmt.Fprintf(w, "edge_gateway_active_sessions{ingress=\"tcp\"} %d\n", active["tcp"])
	fmt.Fprintf(w, "edge_gateway_uptime_seconds %.3f\n", time.Since(r.start).Seconds())

	keys := make([]string, 0, len(counters))
	for k := range counters {
		if strings.HasPrefix(k, "edge_gateway_forward_duration_seconds_") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	typed := make(map[string]bool)
	for _, k := range keys {
		name := k
		if i := strings.IndexByte(k, '{'); i >= 0 {
			name = k[:i]
		}
		if !typed[name] {
			fmt.Fprintf(w, "# TYPE %s counter\n", name)
			typed[name] = true
		}
		fmt.Fprintf(w, "%s %g\n", k, counters[k])
	}
	hkeys := make([]string, 0, len(hist))
	for k := range hist {
		hkeys = append(hkeys, k)
	}
	sort.Strings(hkeys)
	histTyped := false
	for _, k := range hkeys {
		buckets := hist[k]
		labels := ""
		if i := strings.IndexByte(k, '{'); i >= 0 {
			labels = k[i:]
		}
		labelPrefix := ""
		if labels != "" {
			labelPrefix = strings.TrimSuffix(labels, "}") + ","
		} else {
			labelPrefix = "{"
		}
		if !histTyped {
			fmt.Fprintf(w, "# TYPE edge_gateway_forward_duration_seconds histogram\n")
			histTyped = true
		}
		for i, count := range buckets {
			fmt.Fprintf(w, "edge_gateway_forward_duration_seconds_bucket%sle=\"%g\"} %d\n",
				labelPrefix, durationBuckets[i], count)
		}
		fmt.Fprintf(w, "edge_gateway_forward_duration_seconds_bucket%sle=\"+Inf\"} %d\n",
			labelPrefix, buckets[len(buckets)-1])
		countKey := strings.Replace(k, "edge_gateway_forward_duration_seconds",
			"edge_gateway_forward_duration_seconds_count", 1)
		sumKey := strings.Replace(k, "edge_gateway_forward_duration_seconds",
			"edge_gateway_forward_duration_seconds_sum", 1)
		if labels != "" {
			fmt.Fprintf(w, "%s %g\n", countKey, counters[countKey])
			fmt.Fprintf(w, "%s %g\n", sumKey, counters[sumKey])
		} else {
			fmt.Fprintf(w, "edge_gateway_forward_duration_seconds_count %g\n", counters[countKey])
			fmt.Fprintf(w, "edge_gateway_forward_duration_seconds_sum %g\n", counters[sumKey])
		}
	}
}

// P99 returns the upper bound of the P99 bucket. It is intentionally an
// upper-bound estimate; the scenario report also records end-to-end latency
// samples from the generator.
func (r *Registry) P99(ingress string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := key("edge_gateway_forward_duration_seconds", map[string]string{"ingress": ingress})
	buckets := r.hist[k]
	if len(buckets) == 0 {
		return 0
	}
	total := buckets[len(buckets)-1]
	target := uint64(float64(total) * 0.99)
	for i, count := range buckets {
		if count >= target {
			return durationBuckets[i]
		}
	}
	return durationBuckets[len(durationBuckets)-1]
}
