package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"settlement/internal/api"
	"settlement/internal/bizcodes"
)

func (s *Service) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/status", s.handleStatusCollection)
	mux.HandleFunc("/v1/status/", s.handleStatusItem)
	mux.HandleFunc("/v1/locks/", s.handleStatusItem)
	mux.HandleFunc("/", s.handleIndex)
	return mux
}

func (s *Service) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "settlement-coordinator",
		"endpoints": []string{
			"/healthz",
			"/metrics",
			"/v1/status",
			"/v1/status/{batch_id}",
			"/v1/locks/{batch_id}",
		},
	})
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error_code": bizcodes.InvalidArgument,
			"message":    "method not allowed",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if _, err := s.client.Get(ctx, s.key("health"), clientv3.WithLimit(1)); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	body, err := s.RenderMetrics(ctx)
	if err != nil {
		http.Error(w, "failed to collect metrics: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func (s *Service) handleStatusCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error_code": bizcodes.InvalidArgument,
			"message":    "method not allowed",
		})
		return
	}
	if batchID := strings.TrimSpace(r.URL.Query().Get("batch_id")); batchID != "" {
		s.writeBatchStatus(w, r, batchID)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	statuses, err := s.listStatuses(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error_code": bizcodes.Internal,
			"message":    err.Error(),
		})
		return
	}
	items := make([]batchStatusJSON, 0, len(statuses))
	for _, status := range statuses {
		items = append(items, toBatchStatusJSON(status))
	}
	writeJSON(w, http.StatusOK, map[string]any{"statuses": items})
}

func (s *Service) handleStatusItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error_code": bizcodes.InvalidArgument,
			"message":    "method not allowed",
		})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/status/")
	if strings.HasPrefix(r.URL.Path, "/v1/locks/") {
		path = strings.TrimPrefix(r.URL.Path, "/v1/locks/")
	}
	path = strings.Trim(path, "/")
	if path == "" || strings.Contains(path, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error_code": bizcodes.InvalidArgument,
			"message":    "batch_id is required",
		})
		return
	}
	s.writeBatchStatus(w, r, path)
}

func (s *Service) writeBatchStatus(w http.ResponseWriter, r *http.Request, batchID string) {
	if !validBatchID(batchID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error_code": bizcodes.InvalidArgument,
			"message":    "invalid batch_id",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	item, err := s.buildBatchStatus(ctx, batchID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error_code": bizcodes.Internal,
			"message":    err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, toBatchStatusJSON(item))
}

type batchStatusJSON struct {
	BatchID               string             `json:"batch_id"`
	Held                  bool               `json:"held"`
	HolderID              string             `json:"holder_id"`
	LeaseID               string             `json:"lease_id"`
	ExpiresAtUnixMs       int64              `json:"expires_at_unix_ms"`
	LeaseExpiresAt        string             `json:"lease_expires_at,omitempty"`
	LeaseRemainingSeconds float64            `json:"lease_remaining_seconds"`
	Committed             bool               `json:"committed"`
	CommittedBy           string             `json:"committed_by"`
	TaskCount             int32              `json:"task_count"`
	TotalAmount           int64              `json:"total_amount"`
	ResultDigest          string             `json:"result_digest"`
	CommittedAtUnixMs     int64              `json:"committed_at_unix_ms"`
	CommittedAt           string             `json:"committed_at,omitempty"`
	ResultJSON            string             `json:"result_json,omitempty"`
	ReleaseEvents         []releaseEventJSON `json:"release_events"`
}

type releaseEventJSON struct {
	BatchID              string `json:"batch_id"`
	HolderID             string `json:"holder_id"`
	Generation           string `json:"generation"`
	LeaseID              string `json:"lease_id"`
	LeaseExpiresAtUnixMs int64  `json:"lease_expires_at_unix_ms"`
	LeaseExpiresAt       string `json:"lease_expires_at"`
	ReleasedAtUnixMs     int64  `json:"released_at_unix_ms"`
	ReleasedAt           string `json:"released_at"`
	Reason               string `json:"reason"`
}

func toBatchStatusJSON(status *api.BatchStatus) batchStatusJSON {
	result := batchStatusJSON{
		BatchID:               status.GetBatchId(),
		Held:                  status.GetHeld(),
		HolderID:              status.GetHolderId(),
		LeaseID:               status.GetLeaseId(),
		ExpiresAtUnixMs:       status.GetExpiresAtUnixMs(),
		LeaseRemainingSeconds: status.GetLeaseRemainingSeconds(),
		Committed:             status.GetCommitted(),
		CommittedBy:           status.GetCommittedBy(),
		TaskCount:             status.GetTaskCount(),
		TotalAmount:           status.GetTotalAmount(),
		ResultDigest:          status.GetResultDigest(),
		CommittedAtUnixMs:     status.GetCommittedAtUnixMs(),
		ResultJSON:            status.GetResultJson(),
		ReleaseEvents:         make([]releaseEventJSON, 0, len(status.GetReleaseEvents())),
	}
	if result.ExpiresAtUnixMs > 0 {
		result.LeaseExpiresAt = time.UnixMilli(result.ExpiresAtUnixMs).Format(time.RFC3339Nano)
	}
	if result.CommittedAtUnixMs > 0 {
		result.CommittedAt = time.UnixMilli(result.CommittedAtUnixMs).Format(time.RFC3339Nano)
	}
	for _, event := range status.GetReleaseEvents() {
		item := releaseEventJSON{
			BatchID:              event.GetBatchId(),
			HolderID:             event.GetHolderId(),
			Generation:           event.GetGeneration(),
			LeaseID:              event.GetLeaseId(),
			LeaseExpiresAtUnixMs: event.GetLeaseExpiresAtUnixMs(),
			ReleasedAtUnixMs:     event.GetReleasedAtUnixMs(),
			Reason:               event.GetReason(),
		}
		if item.LeaseExpiresAtUnixMs > 0 {
			item.LeaseExpiresAt = time.UnixMilli(item.LeaseExpiresAtUnixMs).Format(time.RFC3339Nano)
		}
		if item.ReleasedAtUnixMs > 0 {
			item.ReleasedAt = time.UnixMilli(item.ReleasedAtUnixMs).Format(time.RFC3339Nano)
		}
		result.ReleaseEvents = append(result.ReleaseEvents, item)
	}
	return result
}

func writeJSON(w http.ResponseWriter, statusCode int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(statusCode)
	_, _ = w.Write(append(data, '\n'))
}
