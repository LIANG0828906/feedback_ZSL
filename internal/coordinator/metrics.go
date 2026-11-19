package coordinator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"settlement/internal/api"
)

type Metrics struct {
	mu             sync.Mutex
	acquire        map[string]uint64
	renew          map[string]uint64
	releaseRequest map[string]uint64
	submit         map[string]uint64
	waitTimeout    map[string]uint64
}

func NewMetrics() *Metrics {
	return &Metrics{
		acquire:        make(map[string]uint64),
		renew:          make(map[string]uint64),
		releaseRequest: make(map[string]uint64),
		submit:         make(map[string]uint64),
		waitTimeout:    make(map[string]uint64),
	}
}

func increment(counter map[string]uint64, batchID string) {
	counter[batchID]++
}

func (m *Metrics) ObserveAcquire(batchID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	increment(m.acquire, batchID)
}

func (m *Metrics) ObserveRenew(batchID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	increment(m.renew, batchID)
}

func (m *Metrics) ObserveReleaseRequest(batchID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	increment(m.releaseRequest, batchID)
}

func (m *Metrics) ObserveSubmit(batchID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	increment(m.submit, batchID)
}

func (m *Metrics) ObserveWaitTimeout(batchID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	increment(m.waitTimeout, batchID)
}

func (m *Metrics) snapshot() (map[string]uint64, map[string]uint64, map[string]uint64, map[string]uint64, map[string]uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copyMap := func(src map[string]uint64) map[string]uint64 {
		dst := make(map[string]uint64, len(src))
		for key, value := range src {
			dst[key] = value
		}
		return dst
	}
	return copyMap(m.acquire), copyMap(m.renew), copyMap(m.releaseRequest), copyMap(m.submit), copyMap(m.waitTimeout)
}

func (s *Service) RenderMetrics(ctx context.Context) (string, error) {
	statuses, err := s.listStatuses(ctx)
	if err != nil {
		return "", err
	}
	events, err := s.listAllReleaseEvents(ctx)
	if err != nil {
		return "", err
	}
	acquire, renew, releaseRequest, submit, waitTimeout := s.metrics.snapshot()
	batchSet := make(map[string]struct{})
	for _, status := range statuses {
		batchSet[status.GetBatchId()] = struct{}{}
	}
	for _, values := range []map[string]uint64{acquire, renew, releaseRequest, submit, waitTimeout} {
		for batchID := range values {
			batchSet[batchID] = struct{}{}
		}
	}
	for _, event := range events {
		batchSet[event.BatchID] = struct{}{}
	}
	batches := make([]string, 0, len(batchSet))
	for batchID := range batchSet {
		batches = append(batches, batchID)
	}
	sort.Strings(batches)
	statusByBatch := make(map[string]*api.BatchStatus, len(statuses))
	for _, status := range statuses {
		statusByBatch[status.GetBatchId()] = status
	}
	releaseCounts := make(map[string]map[string]uint64)
	for _, event := range events {
		if releaseCounts[event.BatchID] == nil {
			releaseCounts[event.BatchID] = make(map[string]uint64)
		}
		releaseCounts[event.BatchID][event.Reason]++
	}

	var out strings.Builder
	writeMetricHeader(&out, "settlement_lock_held", "gauge", "Whether the batch lock is currently held.")
	for _, batchID := range batches {
		value := 0
		if status := statusByBatch[batchID]; status != nil && status.GetHeld() {
			value = 1
		}
		fmt.Fprintf(&out, "settlement_lock_held{batch=%q} %d\n", escapeLabel(batchID), value)
	}
	writeMetricHeader(&out, "settlement_lock_owner_info", "gauge", "Current batch lock owner; empty holder when free.")
	for _, batchID := range batches {
		holderID := ""
		value := 0
		if status := statusByBatch[batchID]; status != nil && status.GetHeld() {
			holderID = status.GetHolderId()
			value = 1
		}
		fmt.Fprintf(&out, "settlement_lock_owner_info{batch=%q,holder=%q} %d\n",
			escapeLabel(batchID), escapeLabel(holderID), value)
	}
	writeMetricHeader(&out, "settlement_lock_lease_remaining_seconds", "gauge", "Remaining lease time for the batch lock.")
	for _, batchID := range batches {
		remaining := 0.0
		if status := statusByBatch[batchID]; status != nil {
			remaining = status.GetLeaseRemainingSeconds()
		}
		fmt.Fprintf(&out, "settlement_lock_lease_remaining_seconds{batch=%q} %.3f\n",
			escapeLabel(batchID), remaining)
	}
	writeMetricHeader(&out, "settlement_lock_acquire_requests_total", "counter", "Lock acquire requests.")
	for _, batchID := range batches {
		fmt.Fprintf(&out, "settlement_lock_acquire_requests_total{batch=%q} %d\n",
			escapeLabel(batchID), acquire[batchID])
	}
	writeMetricHeader(&out, "settlement_lock_renew_requests_total", "counter", "Explicit lock renew requests.")
	for _, batchID := range batches {
		fmt.Fprintf(&out, "settlement_lock_renew_requests_total{batch=%q} %d\n",
			escapeLabel(batchID), renew[batchID])
	}
	writeMetricHeader(&out, "settlement_lock_release_requests_total", "counter", "Explicit lock release requests.")
	for _, batchID := range batches {
		fmt.Fprintf(&out, "settlement_lock_release_requests_total{batch=%q} %d\n",
			escapeLabel(batchID), releaseRequest[batchID])
	}
	writeMetricHeader(&out, "settlement_result_submissions_total", "counter", "Result submission requests.")
	for _, batchID := range batches {
		fmt.Fprintf(&out, "settlement_result_submissions_total{batch=%q} %d\n",
			escapeLabel(batchID), submit[batchID])
	}
	writeMetricHeader(&out, "settlement_lock_release_events_total", "counter", "Recorded release events by reason.")
	for _, batchID := range batches {
		for _, reason := range []string{releaseReasonReleased, releaseReasonLeaseExpired, releaseReasonLockMissing} {
			fmt.Fprintf(&out, "settlement_lock_release_events_total{batch=%q,reason=%q} %d\n",
				escapeLabel(batchID), reason, releaseCounts[batchID][reason])
		}
	}
	writeMetricHeader(&out, "settlement_lock_wait_timeouts_total", "counter", "FIFO wait timeouts.")
	for _, batchID := range batches {
		fmt.Fprintf(&out, "settlement_lock_wait_timeouts_total{batch=%q} %d\n",
			escapeLabel(batchID), waitTimeout[batchID])
	}
	return out.String(), nil
}

func writeMetricHeader(out *strings.Builder, name, metricType, help string) {
	fmt.Fprintf(out, "# HELP %s %s\n", name, help)
	fmt.Fprintf(out, "# TYPE %s %s\n", name, metricType)
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}
