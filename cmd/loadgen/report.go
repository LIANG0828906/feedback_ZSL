package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"edgegateway/internal/model"
)

type Report struct {
	Schema          string                   `json:"schema"`
	Seed            int64                    `json:"seed"`
	GeneratedAt     time.Time                `json:"generated_at"`
	DurationSeconds float64                  `json:"duration_seconds"`
	Window          string                   `json:"target_host_profile"`
	DeviceCount     int                      `json:"device_count"`
	MigratedDevices int                      `json:"devices_with_address_switch"`
	Ingresses       map[string]IngressReport `json:"ingresses"`
	Sessions        []SessionReport          `json:"sessions"`
	Backend         BackendReport            `json:"backend"`
	SessionLimit    SessionLimitReport       `json:"session_concurrency_limit"`
	Timeline        []TimelineEvent          `json:"timeline"`
	Notes           []string                 `json:"notes"`
}

type IngressReport struct {
	SessionCount                  int            `json:"session_count"`
	MigrationCount                int            `json:"migration_count"`
	SessionsWithMigration         int            `json:"sessions_with_migration"`
	MigrationAckPreservedSessions int            `json:"migration_ack_preserved_sessions"`
	MigrationAckPreservedRatio    float64        `json:"migration_ack_preserved_ratio"`
	MigrationsWithPreservedLimit  int            `json:"migrations_with_preserved_limit"`
	MigrationLimitPreservedRatio  float64        `json:"migration_limit_preserved_ratio"`
	SuccessFragments              int64          `json:"success_fragments"`
	FailedFragments               int64          `json:"failed_fragments"`
	RejectionResponses            int64          `json:"rejection_responses"`
	DuplicateResponses            int64          `json:"duplicate_responses"`
	RateLimitedRequests           int64          `json:"rate_limited_requests"`
	BackendBacklogPeak            int            `json:"backend_backlog_peak"`
	BackendTimeoutCount           int64          `json:"backend_timeout_count"`
	ForwardP99MS                  float64        `json:"forward_processing_p99_ms"`
	ForwardSamples                int            `json:"forward_processing_samples"`
	RateLimitEvents               int            `json:"rate_limit_events"`
	RateLimitedSessions           int            `json:"rate_limited_sessions"`
	AllLimitsCausedByBackend      bool           `json:"all_limits_have_backend_basis"`
	Protocols                     map[string]int `json:"protocols"`
}

type SessionReport struct {
	ID                              string                 `json:"session_id"`
	DeviceID                        string                 `json:"device_id"`
	Ingress                         string                 `json:"ingress"`
	Protocol                        string                 `json:"negotiated_protocol"`
	ConfiguredFPS                   int                    `json:"configured_fps"`
	Active                          bool                   `json:"active"`
	MigrationCount                  int                    `json:"migration_count"`
	Migrations                      []MigrationReport      `json:"migrations,omitempty"`
	AckPreservedAcrossAllMigrations bool                   `json:"ack_preserved_across_all_migrations"`
	SuccessFragments                int64                  `json:"success_fragments"`
	FailedFragments                 int64                  `json:"failed_fragments"`
	RejectionResponses              int64                  `json:"rejection_responses"`
	DuplicateResponses              int64                  `json:"duplicate_responses"`
	RateLimitedRequests             int64                  `json:"rate_limited_requests"`
	BackendBacklogPeak              int                    `json:"backend_backlog_peak"`
	SendWindowPeak                  int                    `json:"send_window_peak"`
	BackendTimeouts                 int64                  `json:"backend_timeout_count"`
	FinalAckThrough                 int64                  `json:"final_ack_through"`
	AcceptedRanges                  []model.Range          `json:"accepted_ranges"`
	RateLimitEvents                 []RateLimitEventReport `json:"rate_limit_events"`
	Rejections                      []model.Rejection      `json:"rejections,omitempty"`
}

type MigrationReport struct {
	FromAddress        string        `json:"from_address"`
	ToAddress          string        `json:"to_address"`
	At                 time.Time     `json:"at"`
	AckThroughBefore   int64         `json:"ack_through_before"`
	AckThroughAfter    int64         `json:"ack_through_after"`
	AckRangesPreserved bool          `json:"ack_ranges_preserved"`
	PreservedLimit     bool          `json:"preserved_limit_active"`
	PreservedRate      float64       `json:"preserved_rate_limit_per_sec"`
	PreservedReason    string        `json:"preserved_limit_reason"`
	Trigger            string        `json:"trigger"`
	TransportRebuilt   bool          `json:"transport_rebuilt"`
	LocalAckBefore     []model.Range `json:"local_ack_before,omitempty"`
	LocalAckAfter      []model.Range `json:"local_ack_after,omitempty"`
}

type RateLimitEventReport struct {
	At          time.Time `json:"at"`
	Source      string    `json:"source"`
	Basis       string    `json:"basis"`
	Reason      string    `json:"reason"`
	Backlog     int       `json:"backend_backlog"`
	RTTMedianMS float64   `json:"rtt_median_ms"`
	PriorRate   float64   `json:"prior_rate_limit_per_sec"`
	NewRate     float64   `json:"new_rate_limit_per_sec"`
}

type BackendReport struct {
	Requests          int64 `json:"requests"`
	Accepted          int64 `json:"accepted_fragments"`
	Rejected          int64 `json:"rejected_fragments"`
	Duplicates        int64 `json:"duplicate_fragment_requests"`
	InFlightPeak      int   `json:"in_flight_peak"`
	ActiveRejectRules int   `json:"active_reject_rules"`
}

type SessionLimitReport struct {
	ConfiguredMax         int    `json:"configured_max_sessions"`
	ObservedActive        int    `json:"observed_active_sessions"`
	Respected             bool   `json:"respected"`
	OverLimitHTTPStatus   int    `json:"overlimit_http_status,omitempty"`
	OverLimitBusinessCode string `json:"overlimit_business_error_code,omitempty"`
	ProbeAttempted        bool   `json:"probe_attempted"`
}

type TimelineEvent struct {
	At     time.Time `json:"at"`
	Event  string    `json:"event"`
	Detail string    `json:"detail,omitempty"`
}

type latencyCollector struct {
	mu      sync.Mutex
	samples map[string][]float64
}

func newLatencyCollector() *latencyCollector {
	return &latencyCollector{samples: map[string][]float64{"quic": {}, "tcp": {}, "all": {}}}
}

func (l *latencyCollector) add(ingress string, ms float64) {
	if ms <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.samples[ingress] = append(l.samples[ingress], ms)
	l.samples["all"] = append(l.samples["all"], ms)
}

func (l *latencyCollector) p99(ingress string) (float64, int) {
	l.mu.Lock()
	v := append([]float64(nil), l.samples[ingress]...)
	l.mu.Unlock()
	if len(v) == 0 {
		return 0, 0
	}
	sort.Float64s(v)
	idx := int(float64(len(v)-1) * 0.99)
	return v[idx], len(v)
}

func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	cp := append([]float64(nil), v...)
	sort.Float64s(cp)
	idx := int(float64(len(cp)-1) * p)
	return cp[idx]
}

func writeReport(path string, report *Report, stdout bool) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if stdout {
		_, _ = os.Stdout.Write(raw)
	}
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}
