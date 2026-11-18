package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"configcenter/internal/model"
	"configcenter/internal/notify"
	"configcenter/internal/store"
	"configcenter/internal/telemetry"
)

type Options struct {
	MergeWindow        time.Duration
	MergeMaxWait       time.Duration
	DefaultPollTimeout time.Duration
	MaxPollTimeout     time.Duration
}

type Server struct {
	store     *store.Store
	notifier  *notify.Notifier
	telemetry *telemetry.Telemetry
	options   Options
	mux       *http.ServeMux
}

type StatusResponse struct {
	GeneratedAt string            `json:"generated_at"`
	Namespaces  []NamespaceStatus `json:"namespaces"`
}

type NamespaceStatus struct {
	Name          string                  `json:"name"`
	LatestVersion int64                   `json:"latest_version"`
	MergeWindowMS int64                   `json:"merge_window_ms"`
	Clients       []telemetry.ClientState `json:"clients"`
	RecentMerge   *RecentMergeStatus      `json:"recent_merge"`
}

type RecentMergeStatus struct {
	Version        int64              `json:"version"`
	RawChangeCount int                `json:"raw_change_count"`
	Items          []notify.MergeItem `json:"items"`
	WindowStart    string             `json:"window_start"`
	WindowEnd      string             `json:"window_end"`
	BroadcastAt    string             `json:"broadcast_at"`
}

func New(
	configStore *store.Store,
	notifier *notify.Notifier,
	metrics *telemetry.Telemetry,
	options Options,
) *Server {
	if options.DefaultPollTimeout <= 0 {
		options.DefaultPollTimeout = 20 * time.Second
	}
	if options.MaxPollTimeout <= 0 {
		options.MaxPollTimeout = 30 * time.Second
	}
	server := &Server{
		store:     configStore,
		notifier:  notifier,
		telemetry: metrics,
		options:   options,
		mux:       http.NewServeMux(),
	}
	server.routes()
	return server
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)
	s.mux.HandleFunc("POST /v1/clients/report", s.handleClientReport)
	s.mux.HandleFunc("GET /v1/namespaces/{namespace}/current", s.handleCurrent)
	s.mux.HandleFunc("GET /v1/namespaces/{namespace}/watch", s.handleWatch)
	s.mux.HandleFunc("POST /v1/namespaces/{namespace}/publish", s.handlePublish)
	s.mux.HandleFunc("GET /v1/namespaces/{namespace}/snapshots/{version}", s.handleSnapshot)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	version, err := s.store.SQLiteVersion(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":         "ok",
		"sqlite_version": version,
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, s.telemetry.Prometheus())
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	namespaces, err := s.store.ListNamespaces(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	status := StatusResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Namespaces:  make([]NamespaceStatus, 0, len(namespaces)),
	}
	for _, namespace := range namespaces {
		item := NamespaceStatus{
			Name:          namespace.Namespace,
			LatestVersion: namespace.Version,
			MergeWindowMS: s.options.MergeWindow.Milliseconds(),
			Clients:       s.telemetry.Clients(namespace.Namespace),
		}
		if recent, ok := s.notifier.Recent(namespace.Namespace); ok {
			item.RecentMerge = &RecentMergeStatus{
				Version:        recent.Version,
				RawChangeCount: recent.RawChanges,
				Items:          recent.Items,
				WindowStart:    recent.WindowStart.Format(time.RFC3339Nano),
				WindowEnd:      recent.WindowEnd.Format(time.RFC3339Nano),
				BroadcastAt:    recent.BroadcastAt.Format(time.RFC3339Nano),
			}
		}
		status.Namespaces = append(status.Namespaces, item)
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleCurrent(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	version, err := s.store.LatestVersion(r.Context(), namespace)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.CurrentResponse{
		Namespace: namespace,
		Version:   version,
	})
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	s.telemetry.EnsureNamespace(namespace)
	s.telemetry.RecordLongPoll(namespace)

	knownVersion, err := parseVersion(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	timeout, err := s.parsePollTimeout(r.URL.Query().Get("timeout"))
	if err != nil {
		writeError(w, err)
		return
	}

	latest, err := s.store.LatestVersion(r.Context(), namespace)
	if err != nil {
		writeError(w, err)
		return
	}
	if knownVersion > latest {
		writeError(w, model.ErrVersionAhead)
		return
	}

	targetVersion, err := s.notifier.WaitForBroadcast(r.Context(), namespace, knownVersion, latest, timeout)
	if err != nil {
		if errors.Is(err, notify.ErrBroadcastPending) {
			writeError(w, model.ErrMergePending)
			return
		}
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		writeError(w, err)
		return
	}
	if targetVersion < knownVersion {
		writeError(w, model.NewAPIError("E_INTERNAL", "notification version moved backwards", http.StatusInternalServerError))
		return
	}

	response := model.WatchResponse{
		Namespace:   namespace,
		BaseVersion: knownVersion,
		Version:     targetVersion,
		Changes:     make([]model.Item, 0),
	}
	if targetVersion > knownVersion {
		changes, err := s.store.ChangesSince(r.Context(), namespace, knownVersion, targetVersion)
		if err != nil {
			writeError(w, err)
			return
		}
		if len(changes) == 0 {
			writeError(w, model.NewAPIError(
				"E_INTERNAL",
				"version changed without a configuration change",
				http.StatusInternalServerError,
			))
			return
		}
		for _, change := range changes {
			if change.Version <= knownVersion || change.Version > targetVersion {
				writeError(w, model.NewAPIError(
					"E_INTERNAL",
					"change history fell outside the requested version range",
					http.StatusInternalServerError,
				))
				return
			}
		}
		response.Changed = true
		response.Changes = changes
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	var request model.PublishRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}

	version, err := s.notifier.Publish(r.Context(), namespace, request.Changes, func() (int64, error) {
		return s.store.Publish(r.Context(), namespace, request.Changes)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	s.telemetry.EnsureNamespace(namespace)

	keys := make([]string, 0, len(request.Changes))
	for _, change := range request.Changes {
		keys = append(keys, change.Key)
	}
	sort.Strings(keys)
	writeJSON(w, http.StatusOK, model.PublishResponse{
		Namespace:   namespace,
		Version:     version,
		ChangedKeys: keys,
		Atomic:      true,
	})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	version, err := parseVersion(r.PathValue("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	config, itemVersions, err := s.store.SnapshotAt(r.Context(), namespace, version)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.SnapshotResponse{
		Namespace:    namespace,
		Version:      version,
		Config:       config,
		ItemVersions: itemVersions,
	})
}

func (s *Server) handleClientReport(w http.ResponseWriter, r *http.Request) {
	var report model.ClientReport
	if err := decodeJSON(r, &report); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(report.ClientID) == "" || strings.TrimSpace(report.Namespace) == "" {
		writeError(w, model.NewAPIError("E_VALIDATION", "client_id and namespace are required", http.StatusBadRequest))
		return
	}
	if report.KnownVersion < 0 || report.AppliedVersion < 0 {
		writeError(w, model.NewAPIError("E_VALIDATION", "versions must be non-negative", http.StatusBadRequest))
		return
	}
	if report.AppliedVersion > report.KnownVersion {
		writeError(w, model.NewAPIError("E_VALIDATION", "applied_version cannot exceed known_version", http.StatusBadRequest))
		return
	}
	latest, err := s.store.LatestVersion(r.Context(), report.Namespace)
	if err != nil {
		writeError(w, err)
		return
	}
	if report.KnownVersion > latest {
		writeError(w, model.ErrVersionAhead)
		return
	}
	var previous int64 = -1
	for _, version := range report.AppliedHistory {
		if version < 0 || version <= previous {
			writeError(w, model.NewAPIError(
				"E_VALIDATION",
				"applied_history must be strictly increasing and non-negative",
				http.StatusBadRequest,
			))
			return
		}
		previous = version
	}
	s.telemetry.ReportClient(report)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) parsePollTimeout(raw string) (time.Duration, error) {
	if raw == "" {
		return s.options.DefaultPollTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return 0, model.NewAPIError("E_BAD_REQUEST", "timeout must be a Go duration such as 10s", http.StatusBadRequest)
	}
	if timeout < 100*time.Millisecond || timeout > s.options.MaxPollTimeout {
		return 0, model.NewAPIError(
			"E_BAD_REQUEST",
			"timeout must be between 100ms and "+s.options.MaxPollTimeout.String(),
			http.StatusBadRequest,
		)
	}
	return timeout, nil
}

func parseVersion(raw string) (int64, error) {
	if raw == "" {
		return 0, model.NewAPIError("E_BAD_REQUEST", "version is required", http.StatusBadRequest)
	}
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || version < 0 {
		return 0, model.NewAPIError("E_BAD_REQUEST", "version must be a non-negative integer", http.StatusBadRequest)
	}
	return version, nil
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return model.NewAPIError("E_BAD_REQUEST", "invalid JSON body: "+err.Error(), http.StatusBadRequest)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return model.NewAPIError("E_BAD_REQUEST", "request body must contain one JSON object", http.StatusBadRequest)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

func writeError(w http.ResponseWriter, err error) {
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		apiErr = model.NewAPIError("E_INTERNAL", err.Error(), http.StatusInternalServerError)
	}
	writeJSON(w, apiErr.HTTPStatus, model.ErrorResponse{
		Code:    apiErr.Code,
		Message: apiErr.Message,
	})
}
