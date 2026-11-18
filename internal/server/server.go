package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"example.com/dualring-config/internal/model"
	"example.com/dualring-config/internal/store"
)

type Server struct {
	store    *store.Store
	logger   *log.Logger
	observer time.Duration
	handler  http.Handler
}

func New(configStore *store.Store, logger *log.Logger, observerInterval time.Duration) *Server {
	if logger == nil {
		logger = log.Default()
	}
	if observerInterval <= 0 {
		observerInterval = 100 * time.Millisecond
	}
	server := &Server{
		store:    configStore,
		logger:   logger,
		observer: observerInterval,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.HandleFunc("GET /v1/config", server.handleGetConfig)
	mux.HandleFunc("GET /v1/config/pull", server.handlePull)
	mux.HandleFunc("GET /v1/namespaces", server.handleNamespaces)
	mux.HandleFunc("POST /v1/admin/bootstrap", server.handleBootstrap)
	mux.HandleFunc("PUT /v1/admin/items", server.handleUpdateItem)
	mux.HandleFunc("POST /v1/admin/items", server.handleUpdateItem)
	mux.HandleFunc("GET /v1/admin/items/history", server.handleItemHistory)
	mux.HandleFunc("GET /v1/admin/clients", server.handleListClients)
	mux.HandleFunc("POST /v1/admin/clients/{id}/ring", server.handleSetClientRing)
	mux.HandleFunc("GET /v1/admin/audit", server.handleAudit)
	mux.HandleFunc("GET /v1/admin/stats", server.handleStats)
	mux.HandleFunc("POST /v1/releases", server.handleCreateRelease)
	mux.HandleFunc("GET /v1/releases", server.handleListReleases)
	mux.HandleFunc("GET /v1/releases/{id}", server.handleGetRelease)
	mux.HandleFunc("POST /v1/releases/{id}/transition", server.handleTransition)
	mux.HandleFunc("POST /v1/releases/{id}/report", server.handleReport)
	mux.HandleFunc("POST /v1/releases/{id}/reports", server.handleReport)
	server.handler = server.withMiddleware(mux)
	return server
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) RunObserver(ctx context.Context) {
	ticker := time.NewTicker(s.observer)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.store.EvaluateDueReleases(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("observer evaluation failed: %v", err)
			}
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		writer := &statusWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, recovered)
				if writer.status == 0 {
					writeError(writer, model.Biz(model.CodeInternal, "internal server error"))
				}
			}
			status := writer.status
			if status == 0 {
				status = http.StatusOK
			}
			s.logger.Printf("%s %s status=%d bytes=%d duration=%s",
				r.Method, r.URL.RequestURI(), status, writer.bytes, time.Since(start).Round(time.Microsecond))
		}()
		next.ServeHTTP(writer, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	biz := model.AsBusiness(err)
	status := biz.HTTPStatus
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{
		"code":    biz.Code,
		"message": biz.Message,
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return model.Biz(model.CodeInvalidArgument, "invalid JSON body: "+err.Error())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return model.Biz(model.CodeInvalidArgument, "request body must contain exactly one JSON value")
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Health(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unhealthy",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
	config, err := s.store.HeadConfig(r.Context(), namespace)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, config)
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	instanceID := strings.TrimSpace(query.Get("instance_id"))
	namespace := strings.TrimSpace(query.Get("namespace"))
	localVersion, _ := strconv.ParseInt(query.Get("local_version"), 10, 64)
	response, err := s.store.ResolvePull(r.Context(), instanceID, namespace)
	if err != nil {
		writeError(w, err)
		return
	}
	response.ClientVersion = localVersion
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	namespaces, err := s.store.ListNamespaces(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": namespaces})
}

type bootstrapRequest struct {
	Namespace string            `json:"namespace"`
	Operator  string            `json:"operator"`
	Items     map[string]string `json:"items"`
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var request bootstrapRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	config, err := s.store.Bootstrap(r.Context(), request.Namespace, request.Operator, request.Items)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, config)
}

type updateItemRequest struct {
	Namespace       string `json:"namespace"`
	Key             string `json:"key"`
	Value           string `json:"value"`
	Delete          bool   `json:"delete"`
	ExpectedVersion int64  `json:"expected_version"`
	Operator        string `json:"operator"`
	RequestID       string `json:"request_id"`
}

func (s *Server) handleUpdateItem(w http.ResponseWriter, r *http.Request) {
	var request updateItemRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.store.UpdateItem(
		r.Context(), request.Namespace, request.Key, request.Value, request.Delete,
		request.ExpectedVersion, request.Operator, request.RequestID,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleItemHistory(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if namespace == "" || key == "" {
		writeError(w, model.Biz(model.CodeInvalidArgument, "namespace and key are required"))
		return
	}
	records, err := s.store.History(r.Context(), namespace, key, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace": namespace,
		"key":       key,
		"versions":  records,
	})
}

func (s *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	ring := strings.TrimSpace(r.URL.Query().Get("ring"))
	clients, err := s.store.ListClients(r.Context(), ring)
	if err != nil {
		writeError(w, err)
		return
	}
	stats, err := s.store.GetStats(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clients":        clients,
		"clients_canary": stats.ClientsCanary,
		"clients_full":   stats.ClientsFull,
		"canary_percent": stats.CanaryPercent,
	})
}

type setRingRequest struct {
	Ring     string `json:"ring"`
	Operator string `json:"operator"`
}

func (s *Server) handleSetClientRing(w http.ResponseWriter, r *http.Request) {
	var request setRingRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	client, err := s.store.SetClientRing(
		r.Context(), r.PathValue("id"), request.Ring, request.Operator,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, client)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.store.ListAudit(r.Context(), namespace, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": entries})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetStats(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

type createReleaseRequest struct {
	Namespace          string             `json:"namespace"`
	Operator           string             `json:"operator"`
	Changes            []model.ItemChange `json:"changes"`
	ObservationSeconds float64            `json:"observation_seconds"`
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	var request createReleaseRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	release, err := s.store.CreateRelease(r.Context(), store.ReleaseInput{
		Namespace:          request.Namespace,
		Operator:           request.Operator,
		Changes:            request.Changes,
		ObservationSeconds: request.ObservationSeconds,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, release)
}

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	releases, err := s.store.ListReleases(r.Context(), namespace, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": releases})
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	release, err := s.store.GetRelease(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, release)
}

type transitionRequest struct {
	Target   string `json:"target"`
	Operator string `json:"operator"`
	Reason   string `json:"reason"`
}

func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request) {
	var request transitionRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	release, err := s.store.TransitionRelease(
		r.Context(), r.PathValue("id"), request.Target, request.Operator, request.Reason,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, release)
}

type reportRequest struct {
	InstanceID   string `json:"instance_id"`
	Successes    int64  `json:"success_count"`
	Errors       int64  `json:"error_count"`
	LocalVersion int64  `json:"local_version"`
	Offline      bool   `json:"offline"`
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	var request reportRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, err)
		return
	}
	stats, err := s.store.ReportObservation(r.Context(), store.ObservationInput{
		ReleaseID:    r.PathValue("id"),
		InstanceID:   request.InstanceID,
		Successes:    request.Successes,
		Errors:       request.Errors,
		LocalVersion: request.LocalVersion,
		Offline:      request.Offline,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
