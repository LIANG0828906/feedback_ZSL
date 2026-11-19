package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"driftconverge/internal/engine"
	"driftconverge/internal/metrics"
	"driftconverge/internal/model"
	"driftconverge/internal/store"
)

type RoundRunner interface {
	RunRound(ctx context.Context) (model.RoundReport, error)
}

type StatusStore interface {
	LatestClusterStatus(ctx context.Context, cluster string) (model.ClusterStatus, error)
	Health(ctx context.Context) error
}

type MetricsRenderer interface {
	Render() string
}

type Server struct {
	engine   RoundRunner
	store    StatusStore
	metrics  MetricsRenderer
	clusters []string
}

func New(engine RoundRunner, statusStore StatusStore, registry *metrics.Registry, clusters []string) *Server {
	sorted := append([]string(nil), clusters...)
	sort.Strings(sorted)
	return &Server{engine: engine, store: statusStore, metrics: registry, clusters: sorted}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/api/v1/status", s.handleStatusQuery)
	mux.HandleFunc("/api/status", s.handleStatusQuery)
	mux.HandleFunc("/status", s.handleStatusQuery)
	mux.HandleFunc("/api/v1/clusters/", s.handleClusterPath)
	mux.HandleFunc("/api/v1/reconcile", s.handleReconcile)
	return mux
}

func (s *Server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "E_METHOD", "GET is required")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Health(ctx); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "E_DATABASE", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "E_METHOD", "GET is required")
		return
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = writer.Write([]byte(s.metrics.Render()))
}

func (s *Server) handleStatusQuery(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "E_METHOD", "GET is required")
		return
	}
	cluster := request.URL.Query().Get("cluster")
	if cluster == "" {
		writeError(writer, http.StatusBadRequest, "E_BAD_REQUEST", "cluster query parameter is required")
		return
	}
	s.writeStatus(writer, request, cluster)
}

func (s *Server) handleClusterPath(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeError(writer, http.StatusMethodNotAllowed, "E_METHOD", "GET is required")
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/api/v1/clusters/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || parts[1] != "status" || parts[0] == "" {
		writeError(writer, http.StatusNotFound, "E_NOT_FOUND", "use /api/v1/clusters/{cluster}/status")
		return
	}
	s.writeStatus(writer, request, parts[0])
}

func (s *Server) writeStatus(writer http.ResponseWriter, request *http.Request, cluster string) {
	if !s.knownCluster(cluster) {
		writeError(writer, http.StatusNotFound, "E_UNKNOWN_CLUSTER", "unknown cluster "+cluster)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	status, err := s.store.LatestClusterStatus(ctx, cluster)
	if errors.Is(err, store.ErrNotFound) {
		writeError(writer, http.StatusNotFound, "E_NO_ROUND", "no committed round for "+cluster)
		return
	}
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "E_DATABASE", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

func (s *Server) handleReconcile(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusMethodNotAllowed, "E_METHOD", "POST is required")
		return
	}
	ctx := request.Context()
	report, err := s.engine.RunRound(ctx)
	if errors.Is(err, engine.ErrBusy) {
		writeError(writer, http.StatusConflict, model.CodeBusy, err.Error())
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, struct {
			Report model.RoundReport `json:"report"`
			Error  string            `json:"error"`
		}{Report: report, Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, report)
}

func (s *Server) knownCluster(cluster string) bool {
	index := sort.SearchStrings(s.clusters, cluster)
	return index < len(s.clusters) && s.clusters[index] == cluster
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}
