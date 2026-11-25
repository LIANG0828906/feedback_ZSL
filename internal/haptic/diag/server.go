package diag

import (
	"embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/liang/hapticfeedback/internal/haptic/branches"
	"github.com/liang/hapticfeedback/internal/haptic/checkpoint"
	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/packetstate"
	"github.com/liang/hapticfeedback/internal/haptic/pipeline"
	"github.com/liang/hapticfeedback/internal/haptic/reference"
	"github.com/liang/hapticfeedback/internal/haptic/samplegen"
	"github.com/liang/hapticfeedback/internal/haptic/verify"
)

//go:embed index.html
var content embed.FS

type Server struct {
	mu            sync.Mutex
	sampleDir     string
	outDir        string
	set           *pipeline.SampleSet
	store         *checkpoint.Store
	tracker       *packetstate.Tracker
	plan          *pipeline.Plan
	report        *verify.Report
	losses        any
	lastInjection *InjectionResult
}

type InjectionResult struct {
	Kind    string `json:"kind"`
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	FrameNo int    `json:"frame_no,omitempty"`
	Channel string `json:"channel,omitempty"`
	Seq     int    `json:"packet_seq,omitempty"`
	Message string `json:"message"`
}

type stateResponse struct {
	OK          bool                  `json:"ok"`
	SampleDir   string                `json:"sample_dir"`
	OutDir      string                `json:"out_dir"`
	Capability  any                   `json:"capability"`
	Fingerprint string                `json:"fingerprint"`
	Packets     any                   `json:"packets"`
	Matrix      []packetstate.Machine `json:"matrix"`
	Committed   int                   `json:"committed"`
	Total       int                   `json:"total"`
	Traces      []any                 `json:"traces"`
	Losses      any                   `json:"losses"`
	Report      *verify.Report        `json:"report"`
	Injection   *InjectionResult      `json:"injection"`
	Review      *ReviewResponse       `json:"review"`
}

type ReviewResponse struct {
	OK        bool                 `json:"ok"`
	Complete  bool                 `json:"complete"`
	Packets   int                  `json:"packets"`
	Violation *reference.Violation `json:"violation,omitempty"`
}

func NewServer(sampleDir, outDir string) (*Server, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	set, err := pipeline.GenerateAndLoad(sampleDir)
	if err != nil {
		return nil, err
	}
	report, _, err := verify.Run(sampleDir, outDir)
	if err != nil {
		return nil, err
	}
	reportBytes, _ := json.MarshalIndent(report, "", "  ")
	_ = os.WriteFile(filepath.Join(outDir, "report.json"), reportBytes, 0o644)

	lossBytes, _ := os.ReadFile(filepath.Join(sampleDir, "export_losses.json"))
	var losses any
	_ = json.Unmarshal(lossBytes, &losses)

	dbPath := filepath.Join(outDir, "diagnostic.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	if err := store.Reset(); err != nil {
		return nil, err
	}
	tracker := packetstate.NewTracker()
	stream := set.Streams[1]
	run, err := pipeline.RunCheckpointed(stream, set.Capability, 1, pipeline.RunOptions{
		Store: store, Tracker: tracker,
	})
	if err != nil {
		return nil, err
	}

	return &Server{
		sampleDir: sampleDir, outDir: outDir, set: set,
		store: store, tracker: tracker, plan: run.Plan, report: report, losses: losses,
	}, nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/inject", s.handleInject)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/review", s.handleReview)
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	b, err := content.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) snapshotState() stateResponse {
	committed, total := s.tracker.Progress()
	traces := []any{}
	for _, ev := range s.set.Streams[1].Events {
		for _, trace := range ev.Traces {
			traces = append(traces, trace)
		}
	}
	review := s.reviewLocked()
	return stateResponse{
		OK: true, SampleDir: s.sampleDir, OutDir: s.outDir, Capability: s.set.Capability,
		Fingerprint: s.set.Truth.SemanticFingerprint, Packets: s.plan.Packets,
		Matrix: s.tracker.Matrix(), Committed: committed, Total: total, Traces: traces,
		Losses: s.losses, Report: s.report, Injection: s.lastInjection, Review: review,
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.snapshotState())
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.reviewLocked())
}

func (s *Server) reviewLocked() *ReviewResponse {
	packets, err := s.store.LoadPackets()
	if err != nil {
		return &ReviewResponse{OK: false, Violation: &reference.Violation{Item: "load", Message: err.Error()}}
	}
	v := reference.ReviewRecords(packets, s.set.Truth.SemanticFingerprint)
	return &ReviewResponse{OK: v == nil, Complete: len(packets) == len(s.plan.Packets), Packets: len(packets), Violation: v}
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	seq := atoiDefault(r.URL.Query().Get("seq"), -1)
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded, _ := s.store.LoadPackets()
	if seq < 0 || seq != len(loaded) {
		err := errs.New(errs.EState, "only the next uncommitted packet can be retried")
		err.Seq = seq
		writeJSON(w, map[string]any{"ok": false, "error": err})
		return
	}
	options := pipeline.RunOptions{Store: s.store, Tracker: s.tracker, Resume: true}
	if r.URL.Query().Get("continue") == "0" {
		options.StopAfter = 1
	}
	_, err := pipeline.RunCheckpointed(s.set.Streams[1], s.set.Capability, 1, options)
	if err != nil {
		s.lastInjection = injectionFromError("retry", err)
	} else {
		s.lastInjection = nil
	}
	writeJSON(w, s.snapshotState())
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.store.Reset()
	s.tracker = packetstate.NewTracker()
	run, err := pipeline.RunCheckpointed(s.set.Streams[1], s.set.Capability, 1, pipeline.RunOptions{
		Store: s.store, Tracker: s.tracker,
	})
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.plan = run.Plan
	s.lastInjection = nil
	writeJSON(w, s.snapshotState())
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind == "stage" {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.store.Reset()
		s.tracker = packetstate.NewTracker()
		_, err := pipeline.RunCheckpointed(s.set.Streams[1], s.set.Capability, 1, pipeline.RunOptions{
			Store: s.store, Tracker: s.tracker, FailPacketSeq: 2,
		})
		s.lastInjection = injectionFromError(kind, err)
		writeJSON(w, s.snapshotState())
		return
	}

	branch := atoiDefault(r.URL.Query().Get("branch"), 1)
	data, err := samplegen.InjectedBranchBytes(kind, branch)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	stream, err := branches.Load(data)
	if err == nil && kind == samplegen.InjectOrder {
		_, err = pipeline.BuildPlan(stream, s.set.Capability, branch)
	}
	s.mu.Lock()
	s.lastInjection = injectionFromError(kind, err)
	resp := s.snapshotState()
	s.mu.Unlock()
	writeJSON(w, resp)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	branch := atoiDefault(r.URL.Query().Get("branch"), 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := branches.Export(*s.set.Streams[1], branch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename=branch_export_"+itoa(branch)+".json")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result.Data)
		return
	}
	writeJSON(w, result)
}

func injectionFromError(kind string, err error) *InjectionResult {
	if err == nil {
		return &InjectionResult{Kind: kind, OK: true, Message: "injection was accepted by kernel"}
	}
	he, ok := errs.As(err)
	if !ok {
		return &InjectionResult{Kind: kind, OK: false, Code: string(errs.CodeOf(err)), Message: err.Error()}
	}
	return &InjectionResult{
		Kind: kind, OK: false, Code: string(he.Code), FrameNo: he.FrameNo, Channel: he.Channel,
		Seq: he.Seq, Message: he.Error(),
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	neg := false
	for _, c := range s {
		if c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for v > 0 {
		n--
		b[n] = byte('0' + v%10)
		v /= 10
	}
	return string(b[n:])
}
