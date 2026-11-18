package telemetry

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"configcenter/internal/model"
)

type NamespaceMetrics struct {
	LongPollRequests uint64
	MergedBroadcasts uint64
	RawChangesMerged uint64
}

type ClientState struct {
	ClientID       string    `json:"client_id"`
	KnownVersion   int64     `json:"known_version"`
	AppliedVersion int64     `json:"applied_version"`
	StaleResponses uint64    `json:"stale_responses"`
	AppliedHistory []int64   `json:"applied_history"`
	LastSeen       time.Time `json:"last_seen"`
}

type Telemetry struct {
	mu         sync.Mutex
	namespaces map[string]*NamespaceMetrics
	clients    map[string]map[string]*ClientState
}

func New() *Telemetry {
	return &Telemetry{
		namespaces: make(map[string]*NamespaceMetrics),
		clients:    make(map[string]map[string]*ClientState),
	}
}

func (t *Telemetry) EnsureNamespace(namespace string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.namespaces[namespace]; !ok {
		t.namespaces[namespace] = &NamespaceMetrics{}
	}
	if _, ok := t.clients[namespace]; !ok {
		t.clients[namespace] = make(map[string]*ClientState)
	}
}

func (t *Telemetry) RecordLongPoll(namespace string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	metrics := t.namespaceLocked(namespace)
	metrics.LongPollRequests++
}

func (t *Telemetry) RecordMerge(namespace string, rawChanges int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	metrics := t.namespaceLocked(namespace)
	metrics.MergedBroadcasts++
	metrics.RawChangesMerged += uint64(rawChanges)
}

func (t *Telemetry) ReportClient(report model.ClientReport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.namespaceLocked(report.Namespace)

	clients := t.clients[report.Namespace]
	state, ok := clients[report.ClientID]
	if !ok {
		state = &ClientState{ClientID: report.ClientID}
		clients[report.ClientID] = state
	}
	state.KnownVersion = report.KnownVersion
	state.AppliedVersion = report.AppliedVersion
	if report.StaleResponses > state.StaleResponses {
		state.StaleResponses = report.StaleResponses
	}
	state.AppliedHistory = append(state.AppliedHistory[:0], report.AppliedHistory...)
	if len(state.AppliedHistory) > 64 {
		state.AppliedHistory = append([]int64(nil), state.AppliedHistory[len(state.AppliedHistory)-64:]...)
	}
	state.LastSeen = time.Now().UTC()
}

func (t *Telemetry) Clients(namespace string) []ClientState {
	t.mu.Lock()
	defer t.mu.Unlock()
	clients := t.clients[namespace]
	result := make([]ClientState, 0, len(clients))
	ids := make([]string, 0, len(clients))
	for id := range clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		state := *clients[id]
		state.AppliedHistory = append([]int64(nil), state.AppliedHistory...)
		result = append(result, state)
	}
	return result
}

func (t *Telemetry) Prometheus() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	namespaces := make([]string, 0, len(t.namespaces))
	for namespace := range t.namespaces {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)

	var builder strings.Builder
	builder.WriteString("# HELP config_long_poll_requests_total Long-poll requests received by namespace.\n")
	builder.WriteString("# TYPE config_long_poll_requests_total counter\n")
	for _, namespace := range namespaces {
		builder.WriteString("config_long_poll_requests_total{namespace=\"")
		builder.WriteString(escapeLabel(namespace))
		builder.WriteString("\"} ")
		builder.WriteString(strconv.FormatUint(t.namespaces[namespace].LongPollRequests, 10))
		builder.WriteByte('\n')
	}

	builder.WriteString("# HELP config_merged_broadcasts_total Coalesced broadcasts emitted by namespace.\n")
	builder.WriteString("# TYPE config_merged_broadcasts_total counter\n")
	for _, namespace := range namespaces {
		builder.WriteString("config_merged_broadcasts_total{namespace=\"")
		builder.WriteString(escapeLabel(namespace))
		builder.WriteString("\"} ")
		builder.WriteString(strconv.FormatUint(t.namespaces[namespace].MergedBroadcasts, 10))
		builder.WriteByte('\n')
	}

	builder.WriteString("# HELP config_merged_raw_changes_total Raw configuration changes folded into coalesced broadcasts.\n")
	builder.WriteString("# TYPE config_merged_raw_changes_total counter\n")
	for _, namespace := range namespaces {
		builder.WriteString("config_merged_raw_changes_total{namespace=\"")
		builder.WriteString(escapeLabel(namespace))
		builder.WriteString("\"} ")
		builder.WriteString(strconv.FormatUint(t.namespaces[namespace].RawChangesMerged, 10))
		builder.WriteByte('\n')
	}

	builder.WriteString("# HELP config_client_applied_version Last applied configuration version by client.\n")
	builder.WriteString("# TYPE config_client_applied_version gauge\n")
	for _, namespace := range namespaces {
		clients := t.clients[namespace]
		ids := make([]string, 0, len(clients))
		for id := range clients {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			client := clients[id]
			builder.WriteString("config_client_applied_version{namespace=\"")
			builder.WriteString(escapeLabel(namespace))
			builder.WriteString("\",client_id=\"")
			builder.WriteString(escapeLabel(id))
			builder.WriteString("\"} ")
			builder.WriteString(strconv.FormatInt(client.AppliedVersion, 10))
			builder.WriteByte('\n')
		}
	}

	builder.WriteString("# HELP config_client_stale_responses_total Expired responses ignored by client.\n")
	builder.WriteString("# TYPE config_client_stale_responses_total counter\n")
	for _, namespace := range namespaces {
		clients := t.clients[namespace]
		ids := make([]string, 0, len(clients))
		for id := range clients {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			client := clients[id]
			builder.WriteString("config_client_stale_responses_total{namespace=\"")
			builder.WriteString(escapeLabel(namespace))
			builder.WriteString("\",client_id=\"")
			builder.WriteString(escapeLabel(id))
			builder.WriteString("\"} ")
			builder.WriteString(strconv.FormatUint(client.StaleResponses, 10))
			builder.WriteByte('\n')
		}
	}

	builder.WriteString("# HELP config_client_applied_version_distribution Number of clients at each applied version.\n")
	builder.WriteString("# TYPE config_client_applied_version_distribution gauge\n")
	for _, namespace := range namespaces {
		counts := make(map[int64]int)
		for _, client := range t.clients[namespace] {
			counts[client.AppliedVersion]++
		}
		versions := make([]int64, 0, len(counts))
		for version := range counts {
			versions = append(versions, version)
		}
		sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
		for _, version := range versions {
			builder.WriteString("config_client_applied_version_distribution{namespace=\"")
			builder.WriteString(escapeLabel(namespace))
			builder.WriteString("\",version=\"")
			builder.WriteString(strconv.FormatInt(version, 10))
			builder.WriteString("\"} ")
			builder.WriteString(strconv.Itoa(counts[version]))
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func (t *Telemetry) namespaceLocked(namespace string) *NamespaceMetrics {
	metrics, ok := t.namespaces[namespace]
	if !ok {
		metrics = &NamespaceMetrics{}
		t.namespaces[namespace] = metrics
	}
	if _, ok := t.clients[namespace]; !ok {
		t.clients[namespace] = make(map[string]*ClientState)
	}
	return metrics
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}
