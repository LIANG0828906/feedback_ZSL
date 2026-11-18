package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"archive/internal/model"
	"archive/internal/seed"
	"archive/internal/service"
	"archive/internal/store"
)

type Server struct {
	svc *service.Service
	log *log.Logger
	mux *http.ServeMux
}

func New(svc *service.Service, logger *log.Logger) *Server {
	h := &Server{svc: svc, log: logger, mux: http.NewServeMux()}
	h.register()
	return h
}

func (h *Server) Handler() http.Handler {
	return h.logMiddleware(h.mux)
}

func (h *Server) register() {
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.HandleFunc("GET /metrics", h.metrics)
	h.mux.HandleFunc("GET /v1/config", h.config)
	h.mux.HandleFunc("GET /v1/status", h.status)
	h.mux.HandleFunc("GET /v1/dirs", h.dirs)
	h.mux.HandleFunc("POST /v1/dirs/{index}/offline", h.offline)
	h.mux.HandleFunc("POST /v1/dirs/{index}/online", h.online)
	h.mux.HandleFunc("POST /v1/seed", h.seed)
	h.mux.HandleFunc("GET /v1/objects", h.objects)
	h.mux.HandleFunc("POST /v1/objects", h.createObject)
	h.mux.HandleFunc("PUT /v1/objects/{id}", h.putObject)
	h.mux.HandleFunc("GET /v1/objects/{id}/meta", h.objectMeta)
	h.mux.HandleFunc("GET /v1/objects/{id}", h.getObject)
	h.mux.HandleFunc("GET /v1/repairs/queue", h.repairQueue)
	h.mux.HandleFunc("POST /v1/repairs/run", h.repairRun)
	h.mux.HandleFunc("POST /v1/repairs/drain", h.repairDrain)
	h.mux.HandleFunc("GET /v1/quota", h.quota)
	h.mux.HandleFunc("POST /v1/reconcile", h.reconcile)
	h.mux.HandleFunc("GET /v1/reconcile/last", h.reconcileLast)
	h.mux.HandleFunc("GET /v1/reconcile/{id}", h.reconcileGet)
	h.mux.HandleFunc("GET /v1/audit/stripes/{id}", h.auditStripe)
}

func (h *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if h.log != nil {
			h.log.Printf("%s %s status=%d bytes=%d elapsed_ms=%d", r.Method, r.URL.Path, sw.status, sw.bytes, time.Since(start).Milliseconds())
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

type apiError struct {
	Code            string `json:"code"`
	Message         string `json:"message"`
	StripeID        string `json:"stripe_id,omitempty"`
	ObjectID        string `json:"object_id,omitempty"`
	MissingBlocks   []int  `json:"missing_blocks,omitempty"`
	AvailableBlocks int    `json:"available_blocks,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func (h *Server) writeError(w http.ResponseWriter, err error) {
	code := "INTERNAL"
	msg := "internal service error"
	status := http.StatusInternalServerError
	var ae *service.AppError
	if errors.As(err, &ae) {
		code = ae.Code
		msg = ae.Message
		switch ae.Code {
		case "OBJECT_NOT_FOUND":
			status = http.StatusNotFound
		case "RECONCILIATION_NOT_FOUND", "NO_RECONCILIATION":
			status = http.StatusNotFound
		case "OBJECT_EXISTS", "OBJECT_WRITE_BUSY", "INSUFFICIENT_WRITE_TARGETS",
			"STRIPE_UNRECOVERABLE", "LAYOUT_CHANGED":
			status = http.StatusConflict
		case "INVALID_OBJECT_ID", "INVALID_DIR", "INVALID_RANGE", "INVALID_PROFILE", "INVALID_REQUEST", "OBJECT_TOO_LARGE":
			status = http.StatusBadRequest
		case "STRIPE_RECONSTRUCT_FAILED", "STRIPE_INTEGRITY_FAILED", "STRIPE_METADATA_MISSING", "STRIPE_RANGE_ERROR":
			status = http.StatusServiceUnavailable
		}
	} else if errors.Is(err, store.ErrNotFound) {
		code = "NOT_FOUND"
		msg = "resource not found"
		status = http.StatusNotFound
	} else if h.log != nil {
		h.log.Printf("request error: %v", err)
	}
	resp := errorEnvelope{Error: apiError{Code: code, Message: msg}}
	if ae != nil {
		resp.Error.StripeID = ae.StripeID
		resp.Error.ObjectID = ae.ObjectID
		resp.Error.MissingBlocks = ae.MissingBlocks
		resp.Error.AvailableBlocks = ae.AvailableBlocks
	}
	h.writeJSON(w, status, resp)
}

func (h *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (h *Server) health(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, http.StatusOK, struct {
		Status       string `json:"status"`
		RedisVersion string `json:"redis_version"`
		Time         string `json:"time"`
	}{Status: "ok", RedisVersion: h.svc.RedisVersion(), Time: time.Now().UTC().Format(time.RFC3339Nano)})
}

func (h *Server) metrics(w http.ResponseWriter, r *http.Request) {
	text, err := h.svc.Metrics(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, text)
}

func (h *Server) config(w http.ResponseWriter, r *http.Request) {
	cfg := h.svc.Config()
	h.writeJSON(w, http.StatusOK, struct {
		StripeSize          int64  `json:"stripe_size"`
		DataBlocks          int    `json:"data_blocks"`
		ParityBlocks        int    `json:"parity_blocks"`
		WritebackQuotaBytes int64  `json:"writeback_quota_bytes"`
		RepairConcurrency   int    `json:"repair_concurrency"`
		Timezone            string `json:"timezone"`
		RedisVersion        string `json:"redis_version"`
	}{
		StripeSize: cfg.StripeSize, DataBlocks: cfg.DataBlocks, ParityBlocks: cfg.ParityBlocks,
		WritebackQuotaBytes: cfg.WritebackQuotaBytes, RepairConcurrency: cfg.RepairConcurrency,
		Timezone: cfg.Timezone, RedisVersion: h.svc.RedisVersion(),
	})
}

func (h *Server) status(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "queue", 10)
	if limit < 0 {
		limit = 0
	}
	st, err := h.svc.Status(r.Context(), limit)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, st)
}

func (h *Server) dirs(w http.ResponseWriter, r *http.Request) {
	dirs, err := h.svc.DirStates(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	out := make([]service.DirStatus, 0, model.BlockCount)
	for i := 0; i < model.BlockCount; i++ {
		out = append(out, service.DirStatus{
			Index: i, Name: fmt.Sprintf("block-%d", i), Online: dirs.IsOnline(i),
			Reason: dirs.Dirs[i].Reason, UpdatedAt: dirs.Dirs[i].UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	h.writeJSON(w, http.StatusOK, struct {
		LayoutEpoch int64               `json:"layout_epoch"`
		Dirs        []service.DirStatus `json:"dirs"`
	}{LayoutEpoch: dirs.Epoch, Dirs: out})
}

func (h *Server) offline(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		h.writeError(w, service.NewAppError("INVALID_DIR", "block directory index must be an integer"))
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = decodeOptional(r, &body)
	res, err := h.svc.OfflineDir(r.Context(), index, body.Reason)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, res)
}

func (h *Server) online(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		h.writeError(w, service.NewAppError("INVALID_DIR", "block directory index must be an integer"))
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = decodeOptional(r, &body)
	res, err := h.svc.OnlineDir(r.Context(), index, body.Reason)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, res)
}

func (h *Server) seed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Profile      string `json:"profile"`
		SeedBase     int64  `json:"seed_base"`
		SkipExisting *bool  `json:"skip_existing"`
	}
	if err := decodeOptional(r, &body); err != nil {
		h.writeError(w, service.NewAppError("INVALID_REQUEST", err.Error()))
		return
	}
	if body.SeedBase == 0 {
		body.SeedBase = 20261119
	}
	skip := true
	if body.SkipExisting != nil {
		skip = *body.SkipExisting
	}
	res, err := h.svc.Seed(r.Context(), body.Profile, body.SeedBase, skip)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, res)
}

func (h *Server) objects(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 0)
	objs, err := h.svc.ListObjects(r.Context(), limit)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, struct {
		Objects []service.SeedObject `json:"objects"`
		Count   int                  `json:"count"`
	}{Objects: objs, Count: len(objs)})
}

func (h *Server) createObject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ObjectID string `json:"object_id"`
		Size     int64  `json:"size"`
		Seed     int64  `json:"seed"`
	}
	if err := decodeOptional(r, &body); err != nil {
		h.writeError(w, service.NewAppError("INVALID_REQUEST", err.Error()))
		return
	}
	if body.Size < 0 || body.Size > h.svc.Config().MaxObjectBytes {
		h.writeError(w, service.NewAppError("OBJECT_TOO_LARGE", "size is invalid or too large"))
		return
	}
	if body.Seed == 0 {
		body.Seed = time.Now().UnixNano()
	}
	data := seed.Generate(body.Seed, body.Size)
	res, err := h.svc.WriteObject(r.Context(), body.ObjectID, data, body.Seed)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, res)
}

func (h *Server) putObject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	max := h.svc.Config().MaxObjectBytes
	r.Body = http.MaxBytesReader(w, r.Body, max+1)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		h.writeError(w, service.NewAppError("OBJECT_TOO_LARGE", "request body exceeds max_object_bytes"))
		return
	}
	res, err := h.svc.WriteObject(r.Context(), id, data, 0)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, res)
}

func (h *Server) objectMeta(w http.ResponseWriter, r *http.Request) {
	obj, err := h.svc.ObjectMeta(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, obj)
}

func (h *Server) getObject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	obj, err := h.svc.ObjectMeta(r.Context(), id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if obj.Size == 0 {
		w.Header().Set("Content-Length", "0")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return
	}
	start, end, partial, err := parseRange(r, obj.Size)
	if err != nil {
		if errors.Is(err, errRangeNotSatisfiable) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", obj.Size))
			h.writeError(w, service.NewAppError("INVALID_RANGE", err.Error()))
			return
		}
		h.writeError(w, service.NewAppError("INVALID_RANGE", err.Error()))
		return
	}
	res, err := h.svc.ReadRange(r.Context(), id, start, end)
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Archive-Degraded", strconv.FormatBool(res.Degraded))
	w.Header().Set("X-Archive-Reconstructed-Blocks", strconv.Itoa(res.ReconstructedBlocks))
	w.Header().Set("X-Archive-Stripes-Touched", strconv.Itoa(res.StripesTouched))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, obj.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.Write(res.Bytes)
}

var errRangeNotSatisfiable = errors.New("requested range is not satisfiable")

func parseRange(r *http.Request, size int64) (int64, int64, bool, error) {
	if q := r.URL.Query().Get("range"); q != "" {
		start, end, err := parseRangeSpec(q, size)
		return start, end, true, err
	}
	if r.URL.Query().Has("start") || r.URL.Query().Has("end") {
		start := queryInt64(r, "start", 0)
		end := queryInt64(r, "end", size-1)
		if start < 0 || end < start || start >= size {
			return 0, 0, true, errRangeNotSatisfiable
		}
		if end >= size {
			end = size - 1
		}
		return start, end, true, nil
	}
	header := strings.TrimSpace(r.Header.Get("Range"))
	if header == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, true, errRangeNotSatisfiable
	}
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, 0, true, errors.New("multiple ranges are not supported")
	}
	start, end, err := parseRangeSpec(spec, size)
	return start, end, true, err
}

func parseRangeSpec(spec string, size int64) (int64, int64, error) {
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, errRangeNotSatisfiable
	}
	if parts[0] == "" {
		n, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, errRangeNotSatisfiable
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, errRangeNotSatisfiable
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return 0, 0, errRangeNotSatisfiable
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, nil
}

func (h *Server) repairQueue(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 20)
	queue, err := h.svc.RepairQueue(r.Context(), limit)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, queue)
}

func (h *Server) repairRun(w http.ResponseWriter, r *http.Request) {
	wait := queryBool(r, "wait", false)
	if wait {
		timeout := time.Duration(queryInt(r, "timeout_sec", 30)) * time.Second
		summary, err := h.svc.DrainRepairs(r.Context(), timeout)
		if err != nil {
			h.writeError(w, err)
			return
		}
		queue, _ := h.svc.RepairQueue(r.Context(), 20)
		h.writeJSON(w, http.StatusOK, struct {
			Summary service.ScheduleSummary `json:"summary"`
			Queue   service.RepairQueue     `json:"queue"`
		}{Summary: summary, Queue: queue})
		return
	}
	summary, err := h.svc.RunRepairOnce(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusAccepted, summary)
}

func (h *Server) repairDrain(w http.ResponseWriter, r *http.Request) {
	timeout := time.Duration(queryInt(r, "timeout_sec", 30)) * time.Second
	summary, err := h.svc.DrainRepairs(r.Context(), timeout)
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, summary)
}

func (h *Server) quota(w http.ResponseWriter, r *http.Request) {
	quota, err := h.svc.Quota(r.Context())
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, quota)
}

func (h *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	wait := queryBool(r, "wait", false)
	if wait {
		timeout := time.Duration(queryInt(r, "timeout_sec", 60)) * time.Second
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		result, err := h.svc.Reconcile(ctx)
		if err != nil {
			h.writeError(w, err)
			return
		}
		h.writeJSON(w, http.StatusOK, result)
		return
	}
	id := service.NewReconcileID()
	go func() {
		if _, err := h.svc.ReconcileWithID(context.Background(), id); err != nil && h.log != nil {
			h.log.Printf("async reconcile %s: %v", id, err)
		}
	}()
	h.writeJSON(w, http.StatusAccepted, struct {
		ReconcileID string `json:"reconcile_id"`
		State       string `json:"state"`
	}{ReconcileID: id, State: "running"})
}

func (h *Server) reconcileLast(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.LastReconciliation(r.Context())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, service.NewAppError("NO_RECONCILIATION", "no reconciliation has completed"))
			return
		}
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, result)
}

func (h *Server) reconcileGet(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.GetReconciliation(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, service.NewAppError("RECONCILIATION_NOT_FOUND", "reconciliation not found"))
			return
		}
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, result)
}

func (h *Server) auditStripe(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.AuditStripe(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, result)
}

func decodeOptional(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func queryInt64(r *http.Request, key string, def int64) int64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func queryBool(r *http.Request, key string, def bool) bool {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return def
	}
	return b
}
