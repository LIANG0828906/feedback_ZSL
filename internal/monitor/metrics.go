package monitor

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"s3gw/internal/store"
)

type requestKey struct {
	Version VersionLabel
	Bucket  string
	Op      string
}

type failureKey struct {
	Version VersionLabel
	Bucket  string
	Reason  string
}

type VersionLabel string

const (
	V2   VersionLabel = "v2"
	V4   VersionLabel = "v4"
	None VersionLabel = "none"
)

type Metrics struct {
	mu            sync.Mutex
	requests      map[requestKey]uint64
	failures      map[failureKey]uint64
	lastReapBytes map[string]int64
	reapFailures  map[string]uint64
}

func New() *Metrics {
	return &Metrics{
		requests:      map[requestKey]uint64{},
		failures:      map[failureKey]uint64{},
		lastReapBytes: map[string]int64{},
		reapFailures:  map[string]uint64{},
	}
}

func (m *Metrics) IncRequest(version VersionLabel, bucket, operation string) {
	if bucket == "" {
		bucket = "-"
	}
	m.mu.Lock()
	m.requests[requestKey{Version: version, Bucket: bucket, Op: operation}]++
	m.mu.Unlock()
}

func (m *Metrics) IncFailure(version VersionLabel, bucket, reason string) {
	if bucket == "" {
		bucket = "-"
	}
	m.mu.Lock()
	m.failures[failureKey{Version: version, Bucket: bucket, Reason: reason}]++
	m.mu.Unlock()
}

func (m *Metrics) SetLastReapBytes(bucket string, bytes int64) {
	m.mu.Lock()
	m.lastReapBytes[bucket] = bytes
	m.mu.Unlock()
}

func (m *Metrics) AddReapBytes(bucket string, bytes int64) {
	m.mu.Lock()
	m.lastReapBytes[bucket] += bytes
	m.mu.Unlock()
}

func (m *Metrics) AddReapFailures(bucket string, n uint64) {
	if n == 0 {
		return
	}
	m.mu.Lock()
	m.reapFailures[bucket] += n
	m.mu.Unlock()
}

func (m *Metrics) Render(ctx context.Context, s *store.Store) (string, error) {
	counts, err := s.SnapshotVersionCounts(ctx)
	if err != nil {
		return "", err
	}
	mp, err := s.SnapshotMultipartStats(ctx)
	if err != nil {
		return "", err
	}
	bucketSet := map[string]struct{}{}
	for b := range counts {
		bucketSet[b] = struct{}{}
	}
	for b := range mp {
		bucketSet[b] = struct{}{}
	}

	m.mu.Lock()
	requests := make([]requestKey, 0, len(m.requests))
	requestValues := make(map[requestKey]uint64, len(m.requests))
	for k, v := range m.requests {
		requests = append(requests, k)
		requestValues[k] = v
	}
	failures := make([]failureKey, 0, len(m.failures))
	failureValues := make(map[failureKey]uint64, len(m.failures))
	for k, v := range m.failures {
		failures = append(failures, k)
		failureValues[k] = v
	}
	for b := range m.lastReapBytes {
		bucketSet[b] = struct{}{}
	}
	for b := range m.reapFailures {
		bucketSet[b] = struct{}{}
	}
	lastReap := make(map[string]int64, len(m.lastReapBytes))
	for k, v := range m.lastReapBytes {
		lastReap[k] = v
	}
	reapFailures := make(map[string]uint64, len(m.reapFailures))
	for k, v := range m.reapFailures {
		reapFailures[k] = v
	}
	m.mu.Unlock()

	sort.Slice(requests, func(i, j int) bool {
		a, b := requests[i], requests[j]
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Bucket != b.Bucket {
			return a.Bucket < b.Bucket
		}
		return a.Op < b.Op
	})
	sort.Slice(failures, func(i, j int) bool {
		a, b := failures[i], failures[j]
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Bucket != b.Bucket {
			return a.Bucket < b.Bucket
		}
		return a.Reason < b.Reason
	})

	var b strings.Builder
	writeMetricHeader(&b, "s3gw_signature_requests_total", "counter", "Signature-authenticated requests by signature version and operation.")
	for _, k := range requests {
		writeMetric(&b, "s3gw_signature_requests_total",
			[]label{{"signature_version", string(k.Version)}, {"bucket", k.Bucket}, {"operation", k.Op}},
			requestValues[k])
	}
	writeMetricHeader(&b, "s3gw_signature_failures_total", "counter", "Rejected signature requests by signature version and stable error code.")
	for _, k := range failures {
		writeMetric(&b, "s3gw_signature_failures_total",
			[]label{{"signature_version", string(k.Version)}, {"bucket", k.Bucket}, {"reason", k.Reason}},
			failureValues[k])
	}

	buckets := make([]string, 0, len(bucketSet))
	for bucket := range bucketSet {
		buckets = append(buckets, bucket)
	}
	sort.Strings(buckets)
	writeMetricHeader(&b, "s3gw_versions_current", "gauge", "Current committed versions, including delete markers.")
	for _, bucket := range buckets {
		writeMetric(&b, "s3gw_versions_current", []label{{"bucket", bucket}}, counts[bucket].Current)
	}
	writeMetricHeader(&b, "s3gw_versions_historical", "gauge", "Non-current committed versions for each bucket.")
	for _, bucket := range buckets {
		writeMetric(&b, "s3gw_versions_historical", []label{{"bucket", bucket}}, counts[bucket].Historical)
	}
	writeMetricHeader(&b, "s3gw_multipart_sessions_incomplete", "gauge", "Multipart sessions not yet completed, aborted or reaped.")
	for _, bucket := range buckets {
		writeMetric(&b, "s3gw_multipart_sessions_incomplete", []label{{"bucket", bucket}}, mp[bucket].Sessions)
	}
	writeMetricHeader(&b, "s3gw_multipart_bytes_incomplete", "gauge", "Stored bytes belonging to incomplete multipart sessions.")
	for _, bucket := range buckets {
		writeMetric(&b, "s3gw_multipart_bytes_incomplete", []label{{"bucket", bucket}}, mp[bucket].Bytes)
	}
	writeMetricHeader(&b, "s3gw_reaper_bytes_reclaimed_total", "counter", "Bytes reclaimed by the multipart reaper.")
	for _, bucket := range buckets {
		total, _ := s.ReclaimTotal(ctx, bucket)
		writeMetric(&b, "s3gw_reaper_bytes_reclaimed_total", []label{{"bucket", bucket}}, total)
	}
	writeMetricHeader(&b, "s3gw_reaper_bytes_last_run", "gauge", "Bytes reclaimed in the most recent reaper run for a bucket.")
	for _, bucket := range buckets {
		writeMetric(&b, "s3gw_reaper_bytes_last_run", []label{{"bucket", bucket}}, lastReap[bucket])
	}
	writeMetricHeader(&b, "s3gw_reaper_failures_total", "counter", "Multipart reaper failures.")
	for _, bucket := range buckets {
		writeMetric(&b, "s3gw_reaper_failures_total", []label{{"bucket", bucket}}, reapFailures[bucket])
	}
	return b.String(), nil
}

type label struct {
	name  string
	value string
}

func writeMetricHeader(w io.Writer, name, typ, help string) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeMetric(w io.Writer, name string, labels []label, value any) {
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, l.name+`="`+escapeLabel(l.value)+`"`)
	}
	switch v := value.(type) {
	case uint64:
		_, _ = fmt.Fprintf(w, "%s{%s} %s\n", name, strings.Join(parts, ","), strconv.FormatUint(v, 10))
	case int64:
		_, _ = fmt.Fprintf(w, "%s{%s} %d\n", name, strings.Join(parts, ","), v)
	case int:
		_, _ = fmt.Fprintf(w, "%s{%s} %d\n", name, strings.Join(parts, ","), v)
	default:
		_, _ = fmt.Fprintf(w, "%s{%s} %v\n", name, strings.Join(parts, ","), v)
	}
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
