package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quota-coordinator/internal/otlp"
	"quota-coordinator/internal/store"
)

const (
	CodeInvalidTenant  = "INVALID_TENANT"
	CodeInvalidRequest = "INVALID_REQUEST"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) http.Handler {
	h := &Handler{service: service}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.route)
	return mux
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	switch {
	case path == "/" && r.Method == http.MethodGet:
		h.index(w, r)
	case path == "/health" && r.Method == http.MethodGet:
		h.health(w, r)
	case path == "/metrics" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, h.service.PrometheusMetrics(r.Context()))
	case (path == "/v1/logs" || path == "/ingest") && r.Method == http.MethodPost:
		h.ingest(w, r)
	case path == "/status" && r.Method == http.MethodGet:
		h.status(w, r)
	case path == "/v1/status" && r.Method == http.MethodGet:
		h.status(w, r)
	case path == "/v1/cursors" && r.Method == http.MethodGet:
		h.cursors(w, r)
	case path == "/v1/ack" && r.Method == http.MethodPost:
		h.ack(w, r)
	case path == "/admin/fault" || path == "/v1/admin/fault":
		h.fault(w, r)
	case strings.HasPrefix(path, "/v1/tenants/"):
		h.tenantRoute(w, r, strings.TrimPrefix(path, "/v1/tenants/"))
	case strings.HasPrefix(path, "/v1/receipts/"):
		h.receipt(w, r, strings.TrimPrefix(path, "/v1/receipts/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"code":  "NOT_FOUND",
			"error": "route not found",
		})
	}
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "tenant-log-quota-coordinator",
		"status":  "/v1/status",
		"metrics": "/metrics",
		"ingest":  "/v1/logs",
	})
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := h.service.Store().Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "degraded",
			"redis":  "unavailable",
			"code":   CodeRedisUnavailable,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"redis":  "ok",
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.Status(r.Context()))
}

func (h *Handler) ingest(w http.ResponseWriter, r *http.Request) {
	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if requestID == "" {
		requestID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	body := http.MaxBytesReader(w, r.Body, h.service.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(body)
	if err != nil {
		h.ingestError(w, requestID, http.StatusRequestEntityTooLarge, CodeInvalidBatch, "request body is too large")
		return
	}
	decoded, err := otlp.DecodeBody(raw, r.Header.Get("Content-Encoding"))
	if err != nil {
		h.ingestError(w, requestID, http.StatusBadRequest, CodeInvalidBatch, err.Error())
		return
	}
	headerBatchID := firstHeader(r, "X-Batch-ID", "Idempotency-Key")
	records, err := otlp.Parse(decoded, r.Header.Get("Content-Type"), otlp.ParseOptions{
		HeaderBatchID: headerBatchID,
		Now:           time.Now().UTC(),
	})
	if err != nil {
		status := http.StatusBadRequest
		code := CodeInvalidBatch
		if errors.Is(err, otlp.ErrRecordWithoutTenant) {
			code = CodeInvalidTenant
		}
		h.ingestError(w, requestID, status, code, err.Error())
		return
	}
	requestHash := otlp.RequestHash(headerBatchID, records)
	batches := otlp.GroupRecords(records, requestHash)
	result := h.service.Ingest(r.Context(), batches)
	result.RequestID = requestID
	status := ingestStatus(result)
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Backpressure", strconv.FormatBool(result.Backpressure))
	if result.Backpressure {
		w.Header().Set("X-Vector-Backpressure", "true")
		w.Header().Set("X-Backpressure-Ratio", fmt.Sprintf("%.3f", h.service.cfg.Backpressure.Ratio))
		w.Header().Set("Retry-After", strconv.Itoa(h.service.retryAfterSeconds()))
	} else {
		w.Header().Set("X-Vector-Backpressure", "false")
	}
	writeJSON(w, status, result)
}

func (h *Handler) ingestError(w http.ResponseWriter, requestID string, status int, code, message string) {
	writeJSON(w, status, IngestResponse{
		RequestID: requestID,
		OK:        false,
		Code:      code,
		Results:   []GroupResult{},
		Message:   message,
	})
}

func (h *Handler) statusCodeForResult(result IngestResponse) int {
	return ingestStatus(result)
}

func ingestStatus(result IngestResponse) int {
	if result.Code == CodeGlobalBackpressure {
		return http.StatusTooManyRequests
	}
	if result.OK {
		return http.StatusOK
	}
	if result.Accepted > 0 && result.Dropped > 0 && result.Code == CodePartialAccept {
		return http.StatusMultiStatus
	}
	if len(result.Results) == 0 {
		return http.StatusBadRequest
	}
	for _, item := range result.Results {
		switch item.Code {
		case CodeUnknownTenant:
			return http.StatusNotFound
		case CodeInvalidBatch, CodeInvalidQuota, CodeInvalidTenant:
			return http.StatusBadRequest
		case CodeRedisUnavailable:
			return http.StatusServiceUnavailable
		case CodeQuotaExceeded, CodeTenantDegraded:
			return http.StatusUnprocessableEntity
		}
	}
	return http.StatusUnprocessableEntity
}

func (h *Handler) tenantRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found")
		return
	}
	tenant := parts[0]
	switch parts[1] {
	case "quota":
		h.quota(w, r, tenant)
	case "cursor":
		h.cursor(w, r, tenant)
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found")
	}
}

func (h *Handler) quota(w http.ResponseWriter, r *http.Request, tenant string) {
	switch r.Method {
	case http.MethodGet:
		quota, err := h.service.Quota(r.Context(), tenant)
		if err != nil {
			if errors.Is(err, ErrUnknownTenant) {
				writeError(w, http.StatusNotFound, CodeUnknownTenant, "tenant is not configured")
				return
			}
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"tenant_id":                    tenant,
			"logs_per_second":              quota.LogsPerSecond,
			"bytes_per_second":             quota.BytesPerSecond,
			"drop_policy":                  quota.DropPolicy,
			"max_degraded_pending_batches": quota.MaxPendingBatches,
			"max_degraded_pending_bytes":   quota.MaxPendingBytes,
		})
	case http.MethodPut, http.MethodPatch:
		var patch QuotaPatch
		if err := decodeLimitedJSON(w, r, &patch); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidQuota, err.Error())
			return
		}
		quota, err := h.service.UpdateQuota(tenant, patch)
		if err != nil {
			switch {
			case errors.Is(err, ErrUnknownTenant):
				writeError(w, http.StatusNotFound, CodeUnknownTenant, "tenant is not configured")
			case errors.Is(err, ErrInvalidQuota):
				writeError(w, http.StatusBadRequest, CodeInvalidQuota, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":                           true,
			"code":                         "QUOTA_UPDATED",
			"tenant_id":                    tenant,
			"logs_per_second":              quota.LogsPerSecond,
			"bytes_per_second":             quota.BytesPerSecond,
			"drop_policy":                  quota.DropPolicy,
			"max_degraded_pending_batches": quota.MaxPendingBatches,
			"max_degraded_pending_bytes":   quota.MaxPendingBytes,
		})
	default:
		w.Header().Set("Allow", "GET, PUT, PATCH")
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func (h *Handler) cursor(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	cursor, err := h.service.Cursor(r.Context(), tenant)
	if err != nil {
		if errors.Is(err, ErrUnknownTenant) {
			writeError(w, http.StatusNotFound, CodeUnknownTenant, "tenant is not configured")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cursor)
}

func (h *Handler) cursors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	cursors, err := h.service.Cursors(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": 1,
		"cursors":        cursors,
	})
}

func (h *Handler) receipt(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	receipt, err := h.service.Receipt(r.Context(), parts[0], parts[1])
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownTenant):
			writeError(w, http.StatusNotFound, CodeUnknownTenant, "tenant is not configured")
		case errors.Is(err, store.ErrReceiptNotFound):
			writeError(w, http.StatusNotFound, "RECEIPT_NOT_FOUND", "batch receipt not found")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

type ackRequest struct {
	TenantID      string `json:"tenant_id"`
	BatchID       string `json:"batch_id"`
	Seq           int64  `json:"seq"`
	StorageOffset int64  `json:"storage_offset"`
}

func (h *Handler) ack(w http.ResponseWriter, r *http.Request) {
	var request ackRequest
	if err := decodeLimitedJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	if request.TenantID == "" || request.BatchID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "tenant_id and batch_id are required")
		return
	}
	cursor, err := h.service.Acknowledge(r.Context(), request.TenantID, request.BatchID, request.Seq, request.StorageOffset)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownTenant):
			writeError(w, http.StatusNotFound, CodeUnknownTenant, "tenant is not configured")
		case errors.Is(err, store.ErrReceiptNotFound):
			writeError(w, http.StatusNotFound, "RECEIPT_NOT_FOUND", "batch receipt not found")
		default:
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"code":   "ACKNOWLEDGED",
		"cursor": cursor,
	})
}

type faultRequest struct {
	RedisDelayMS int64 `json:"redis_delay_ms"`
}

func (h *Handler) fault(w http.ResponseWriter, r *http.Request) {
	if !h.service.cfg.AdminEnabled {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "admin API is disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"redis_delay_ms": h.service.RedisDelay().Milliseconds(),
		})
	case http.MethodPost, http.MethodPut:
		var request faultRequest
		if err := decodeLimitedJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if request.RedisDelayMS < 0 || request.RedisDelayMS > 30000 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "redis_delay_ms must be between 0 and 30000")
			return
		}
		h.service.SetRedisDelay(time.Duration(request.RedisDelayMS) * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"redis_delay_ms": request.RedisDelayMS,
		})
	default:
		w.Header().Set("Allow", "GET, POST, PUT")
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	}
}

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, target any) error {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"ok":      false,
		"code":    code,
		"message": message,
	})
}

func firstHeader(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}
