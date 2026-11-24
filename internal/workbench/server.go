package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gameintegrity/internal/analyze"
	"gameintegrity/internal/apperr"
	"gameintegrity/internal/engine"
	"gameintegrity/internal/frameio"
	"gameintegrity/internal/model"
)

const (
	jobIdle      = "idle"
	jobRunning   = "running"
	jobPaused    = "paused"
	jobCompleted = "completed"
	jobFailed    = "failed"
	jobCancelled = "cancelled"
)

type Job struct {
	mu        sync.Mutex
	status    string
	err       *apperr.Error
	errText   string
	report    *model.RunReport
	paused    atomic.Bool
	cancel    context.CancelFunc
	started   time.Time
	mode      string
	config    model.Config
	committed int
	total     int
}

type JobView struct {
	Status            string           `json:"status"`
	Error             *apperr.Error    `json:"error,omitempty"`
	ErrorText         string           `json:"error_text,omitempty"`
	Report            *model.RunReport `json:"report,omitempty"`
	CommittedSegments int              `json:"committed_segments"`
	TotalSegments     int              `json:"total_segments"`
	StartedAt         string           `json:"started_at,omitempty"`
}

type Server struct {
	DataRoot string
	WorkRoot string

	mu       sync.Mutex
	jobs     map[string]*Job
	datasets map[string]*frameio.Dataset
}

func New(dataRoot, workRoot string) *Server {
	return &Server{
		DataRoot: dataRoot, WorkRoot: workRoot,
		jobs: make(map[string]*Job), datasets: make(map[string]*frameio.Dataset),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/samples", s.handleSamples)
	mux.HandleFunc("GET /api/samples/{id}", s.handleSample)
	mux.HandleFunc("POST /api/samples/{id}/verify", s.handleVerify)
	mux.HandleFunc("GET /api/samples/{id}/job", s.handleJob)
	mux.HandleFunc("POST /api/samples/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/samples/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/samples/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /api/samples/{id}/retry", s.handleRetry)
	mux.HandleFunc("GET /api/samples/{id}/segment/{index}", s.handleSegment)
	mux.HandleFunc("GET /api/samples/{id}/frame/{name}", s.handleFramePNG)
	mux.HandleFunc("GET /api/samples/{id}/residual/{name}", s.handleResidualPNG)
	mux.HandleFunc("GET /api/samples/{id}/noise/{name}", s.handleNoisePNG)
	mux.HandleFunc("GET /api/samples/{id}/export", s.handleExport)
	return mux
}

func (s *Server) dataset(id string) (*frameio.Dataset, error) {
	if _, err := frameio.NormalizeSampleID(s.DataRoot, id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ds, ok := s.datasets[id]; ok {
		return ds, nil
	}
	ds, err := frameio.Open(s.DataRoot, id)
	if err != nil {
		return nil, err
	}
	s.datasets[id] = ds
	return ds, nil
}

func (s *Server) job(id string, create bool) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok && create {
		j = &Job{status: jobIdle}
		s.jobs[id] = j
	}
	return j
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	catalog, err := frameio.ReadCatalog(s.DataRoot)
	if err != nil {
		writeError(w, err)
		return
	}
	s.mu.Lock()
	statuses := make(map[string]string, len(s.jobs))
	for id, j := range s.jobs {
		statuses[id] = j.viewStatus()
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"catalog": json.RawMessage(catalog),
		"jobs":    statuses,
	})
}

func (s *Server) handleSample(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ds, err := s.dataset(id)
	if err != nil {
		writeError(w, err)
		return
	}
	j := s.job(id, false)
	var view *JobView
	if j != nil {
		v := j.snapshot()
		view = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"manifest": ds.Manifest,
		"truth":    ds.Truth,
		"job":      view,
	})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ds, err := s.dataset(id)
	if err != nil {
		writeError(w, err)
		return
	}
	cfg, mode, fresh, err := parseConfig(r, ds)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := cfg.Validate(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount); err != nil {
		writeError(w, err)
		return
	}
	s.mu.Lock()
	old := s.jobs[id]
	s.mu.Unlock()
	if old != nil && old.active() {
		e := apperr.New(apperr.State, "a verification job is already running for this sample")
		e.Field = "job"
		e.Actual = old.viewStatus()
		writeError(w, e)
		return
	}
	workdir := filepath.Join(s.WorkRoot, id)
	if fresh {
		if err := os.RemoveAll(workdir); err != nil {
			writeError(w, apperr.Wrap(apperr.Decode, "cannot clear checkpoint directory", err))
			return
		}
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		writeError(w, apperr.Wrap(apperr.Decode, "cannot create checkpoint directory", err))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		status: jobRunning, cancel: cancel, started: time.Now(),
		mode: mode, config: cfg,
	}
	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()
	go s.runJob(ctx, ds, job, workdir, mode, cfg)
	writeJSON(w, http.StatusOK, map[string]any{"status": jobRunning, "workdir": workdir, "config": cfg})
}

func (s *Server) runJob(ctx context.Context, ds *frameio.Dataset, job *Job, workdir, mode string, cfg model.Config) {
	defer job.cancel()
	opts := engine.Options{
		Path: "resume", Mode: mode, WorkDir: workdir,
		BeforeSegment: func(ctx context.Context) error {
			for job.paused.Load() {
				job.setStatus(jobPaused)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
			job.setStatus(jobRunning)
			return nil
		},
		Progress: func(committed, total int) {
			job.setProgress(committed, total)
		},
	}
	report, err := engine.Verify(ctx, ds, cfg, opts)
	job.mu.Lock()
	defer job.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			job.status = jobCancelled
			job.errText = err.Error()
			return
		}
		job.status = jobFailed
		job.err = apperr.As(err)
		job.errText = err.Error()
		if job.err == nil {
			job.err = &apperr.Error{Code: apperr.Decode, Message: err.Error(), Frame: -1}
		}
		return
	}
	job.report = report
	if report.Paused {
		job.status = jobPaused
	} else {
		job.status = jobCompleted
	}
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j := s.job(id, false)
	if j == nil {
		writeJSON(w, http.StatusOK, JobView{Status: jobIdle})
		return
	}
	writeJSON(w, http.StatusOK, j.snapshot())
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	j := s.job(r.PathValue("id"), false)
	if j == nil || !j.active() {
		e := apperr.New(apperr.State, "no active verification job to pause")
		e.Field = "job"
		writeError(w, e)
		return
	}
	j.paused.Store(true)
	j.setStatus(jobPaused)
	writeJSON(w, http.StatusOK, j.snapshot())
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j := s.job(id, false)
	if j != nil && j.active() {
		j.paused.Store(false)
		j.setStatus(jobRunning)
		writeJSON(w, http.StatusOK, j.snapshot())
		return
	}
	ds, err := s.dataset(id)
	if err != nil {
		writeError(w, err)
		return
	}
	cfg := model.DefaultConfig(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount, ds.Manifest.BlockSize, ds.Manifest.SamplingBudget)
	cfg.SegmentSeconds = ds.Manifest.SegmentSeconds
	mode := "auto"
	if j != nil && j.config.BlockSize != 0 {
		cfg = j.config
		mode = j.mode
	}
	workdir := filepath.Join(s.WorkRoot, id)
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{status: jobRunning, cancel: cancel, started: time.Now(), mode: mode, config: cfg}
	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()
	go s.runJob(ctx, ds, job, workdir, mode, cfg)
	writeJSON(w, http.StatusOK, map[string]any{"status": jobRunning})
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	j := s.job(r.PathValue("id"), false)
	if j == nil || !j.active() {
		e := apperr.New(apperr.State, "no active verification job to cancel")
		e.Field = "job"
		writeError(w, e)
		return
	}
	j.cancel()
	j.paused.Store(false)
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelling"})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old := s.job(id, false)
	if old != nil && old.active() {
		e := apperr.New(apperr.State, "a verification job is already running for this sample")
		e.Field = "job"
		e.Actual = old.viewStatus()
		writeError(w, e)
		return
	}
	ds, err := s.dataset(id)
	if err != nil {
		writeError(w, err)
		return
	}
	cfg := model.DefaultConfig(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount, ds.Manifest.BlockSize, ds.Manifest.SamplingBudget)
	cfg.SegmentSeconds = ds.Manifest.SegmentSeconds
	mode := "auto"
	if old != nil && old.config.BlockSize != 0 {
		cfg = old.config
		mode = old.mode
	}
	workdir := filepath.Join(s.WorkRoot, id)
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{status: jobRunning, cancel: cancel, started: time.Now(), mode: mode, config: cfg}
	s.mu.Lock()
	s.jobs[id] = job
	s.mu.Unlock()
	go s.runJob(ctx, ds, job, workdir, mode, cfg)
	writeJSON(w, http.StatusOK, map[string]any{"status": jobRunning, "resumed_from_checkpoint": true})
}

func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeError(w, apperr.WithField(apperr.New(apperr.Para, "segment index must be an integer"), "index", "integer", r.PathValue("index"), ""))
		return
	}
	j := s.job(id, false)
	if j == nil {
		writeError(w, apperr.New(apperr.State, "no verification report is available"))
		return
	}
	view := j.snapshot()
	if view.Report == nil {
		writeError(w, apperr.New(apperr.State, "verification has not produced a report yet"))
		return
	}
	for _, seg := range view.Report.Segments {
		if seg.Index == index {
			writeJSON(w, http.StatusOK, seg)
			return
		}
	}
	e := apperr.New(apperr.State, "segment has not been committed")
	e.Field = "segment_index"
	e.Actual = index
	writeError(w, e)
}

func (s *Server) handleFramePNG(w http.ResponseWriter, r *http.Request) {
	id, n, err := routeFrame(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ds, err := s.dataset(id)
	if err != nil {
		writeError(w, err)
		return
	}
	fr, err := ds.ReadFrame(n)
	if err != nil {
		writeError(w, err)
		return
	}
	img := image.NewNRGBA(image.Rect(0, 0, fr.Width, fr.Height))
	for y := 0; y < fr.Height; y++ {
		for x := 0; x < fr.Width; x++ {
			off := (y*fr.Width + x) * fr.Channels
			img.SetNRGBA(x, y, color.NRGBA{R: fr.Pixels[off], G: fr.Pixels[off+1], B: fr.Pixels[off+2], A: 255})
		}
	}
	writePNG(w, img)
}

func (s *Server) handleResidualPNG(w http.ResponseWriter, r *http.Request) {
	id, n, err := routeFrame(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ds, err := s.dataset(id)
	if err != nil {
		writeError(w, err)
		return
	}
	cur, err := ds.ReadFrame(n)
	if err != nil {
		writeError(w, err)
		return
	}
	var prev *frameio.Frame
	if n > 0 {
		prev, err = ds.ReadFrame(n - 1)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	img := image.NewNRGBA(image.Rect(0, 0, cur.Width, cur.Height))
	for y := 0; y < cur.Height; y++ {
		for x := 0; x < cur.Width; x++ {
			off := (y*cur.Width + x) * cur.Channels
			value := 0.0
			if prev != nil {
				d := 0
				for c := 0; c < cur.Channels; c++ {
					diff := int(cur.Pixels[off+c]) - int(prev.Pixels[off+c])
					d += diff * diff
				}
				value = float64(d) / float64(cur.Channels*255*255)
			}
			img.SetNRGBA(x, y, heatColor(math.Min(1, value*8)))
		}
	}
	writePNG(w, img)
}

func (s *Server) handleNoisePNG(w http.ResponseWriter, r *http.Request) {
	id, n, err := routeFrame(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ds, err := s.dataset(id)
	if err != nil {
		writeError(w, err)
		return
	}
	fr, err := ds.ReadFrame(n)
	if err != nil {
		writeError(w, err)
		return
	}
	block := ds.Manifest.BlockSize
	values := analyze.BlockNoiseVariances(fr.Pixels, fr.Width, fr.Height, fr.Channels, block)
	blocks := model.AllBlocks(fr.Width, fr.Height, block)
	img := image.NewNRGBA(image.Rect(0, 0, fr.Width, fr.Height))
	for i, b := range blocks {
		c := heatColor(math.Min(1, values[i]/0.02))
		for y := b.Y; y < b.Y+b.H; y++ {
			for x := b.X; x < b.X+b.W; x++ {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	writePNG(w, img)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	j := s.job(r.PathValue("id"), false)
	if j == nil {
		writeError(w, apperr.New(apperr.State, "no verification report is available"))
		return
	}
	view := j.snapshot()
	if view.Report == nil {
		writeError(w, apperr.New(apperr.State, "verification report is not ready"))
		return
	}
	data, err := json.MarshalIndent(view.Report, "", "  ")
	if err != nil {
		writeError(w, apperr.Wrap(apperr.Decode, "cannot encode report", err))
		return
	}
	data = append(data, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-integrity-report.json"`, view.Report.SampleID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (j *Job) snapshot() JobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	view := JobView{
		Status: j.status, Error: j.err, ErrorText: j.errText,
		Report: j.report, CommittedSegments: j.committed,
		TotalSegments: j.total,
	}
	if j.report != nil {
		view.CommittedSegments = j.report.CommittedSegments
		view.TotalSegments = j.report.TotalSegments
	}
	if !j.started.IsZero() {
		view.StartedAt = j.started.Format(time.RFC3339Nano)
	}
	return view
}

func (j *Job) viewStatus() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status
}

func (j *Job) active() bool {
	status := j.viewStatus()
	return status == jobRunning || status == jobPaused
}

func (j *Job) setStatus(status string) {
	j.mu.Lock()
	j.status = status
	j.mu.Unlock()
}

func (j *Job) setProgress(committed, total int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.committed = committed
	j.total = total
}

func parseConfig(r *http.Request, ds *frameio.Dataset) (model.Config, string, bool, error) {
	q := r.URL.Query()
	cfg := model.DefaultConfig(ds.Manifest.Width, ds.Manifest.Height, ds.Manifest.Channels, ds.Manifest.FrameCount, ds.Manifest.BlockSize, ds.Manifest.SamplingBudget)
	cfg.SegmentSeconds = ds.Manifest.SegmentSeconds
	var err error
	if v := q.Get("block"); v != "" {
		cfg.BlockSize, err = strconv.Atoi(v)
		if err != nil {
			return cfg, "", false, fieldParseError("block", v, "integer")
		}
	}
	if v := q.Get("budget"); v != "" {
		cfg.SamplingBudget, err = strconv.Atoi(v)
		if err != nil {
			return cfg, "", false, fieldParseError("sampling_budget", v, "integer")
		}
	}
	if v := q.Get("history"); v != "" {
		cfg.History, err = strconv.Atoi(v)
		if err != nil {
			return cfg, "", false, fieldParseError("history", v, "integer")
		}
	}
	if v := q.Get("workers"); v != "" {
		cfg.MaxWorkers, err = strconv.Atoi(v)
		if err != nil {
			return cfg, "", false, fieldParseError("max_workers", v, "integer")
		}
	}
	if v := q.Get("buffer_mb"); v != "" {
		mb, parseErr := strconv.ParseInt(v, 10, 64)
		if parseErr != nil {
			return cfg, "", false, fieldParseError("max_pixel_buffer_bytes", v, "integer megabytes")
		}
		cfg.MaxPixelBufferBytes = mb << 20
	}
	if v := q.Get("residual_threshold"); v != "" {
		cfg.ResidualThreshold, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, "", false, fieldParseError("residual_threshold", v, "float")
		}
	}
	if v := q.Get("noise_threshold"); v != "" {
		cfg.NoiseThreshold, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, "", false, fieldParseError("noise_threshold", v, "float")
		}
	}
	if v := q.Get("bbox_threshold"); v != "" {
		cfg.BBoxShiftThreshold, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, "", false, fieldParseError("bbox_shift_threshold", v, "float")
		}
	}
	if v := q.Get("hash_jump"); v != "" {
		cfg.HashJumpRatio, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, "", false, fieldParseError("hash_jump_ratio", v, "float")
		}
	}
	mode := q.Get("mode")
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "auto", "fine", "candidate":
	default:
		return cfg, "", false, fieldParseError("mode", mode, "auto|fine|candidate")
	}
	fresh := q.Get("fresh") == "1" || q.Get("fresh") == "true"
	return cfg, mode, fresh, nil
}

func fieldParseError(field, value, expected string) error {
	e := apperr.New(apperr.Para, "query parameter could not be parsed")
	e.Field = field
	e.Expected = expected
	e.Actual = value
	return e
}

func routeFrame(r *http.Request) (string, int, error) {
	id := r.PathValue("id")
	raw := r.PathValue("name")
	raw = strings.TrimSuffix(raw, ".png")
	n, err := strconv.Atoi(raw)
	if err != nil {
		e := apperr.New(apperr.Para, "frame number must be an integer")
		e.Field = "frame"
		e.Actual = raw
		return "", 0, e
	}
	return id, n, nil
}

func writePNG(w http.ResponseWriter, img image.Image) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		writeError(w, apperr.Wrap(apperr.Decode, "cannot encode PNG", err))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func heatColor(v float64) color.NRGBA {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	if v < 0.5 {
		t := v * 2
		return color.NRGBA{R: 20, G: byte(40 + 180*t), B: byte(230 - 160*t), A: 255}
	}
	t := (v - 0.5) * 2
	return color.NRGBA{R: byte(20 + 235*t), G: byte(220 - 170*t), B: byte(70 - 50*t), A: 255}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	e := apperr.As(err)
	if e == nil {
		e = &apperr.Error{Code: apperr.Decode, Message: err.Error(), Frame: -1}
	}
	writeJSON(w, apperr.HTTPStatus(err), map[string]any{"error": e})
}

var indexTemplate = template.Must(template.New("index").Parse(indexHTML))
