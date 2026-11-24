package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"integritykernel/internal/checkpoint"
	"integritykernel/internal/core"
	"integritykernel/internal/engine"
	"integritykernel/internal/loader"
	"integritykernel/internal/pipeline"
	"integritykernel/internal/review"
	"integritykernel/internal/samplegen"
)

//go:embed static/index.html
var staticFiles embed.FS

type Server struct {
	sampleDir   string
	artifactDir string
	dbPath      string
	seq         *loader.Sequence
	engine      *engine.Engine
	store       *checkpoint.Store

	mu      sync.Mutex
	crossMu sync.Mutex
}

func NewServer(sampleDir, artifactDir string) (*Server, error) {
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return nil, err
	}
	opts := samplegen.DefaultOptions(sampleDir)
	opts.Force = false
	if _, err := samplegen.Generate(opts); err != nil {
		return nil, err
	}
	seq, err := loader.Load(sampleDir)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(artifactDir, "diagnose-checkpoint.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	params := core.DefaultParams()
	eng, err := engine.New(seq, engine.Options{Params: params, Store: store})
	if err != nil {
		return nil, err
	}
	if _, txnErr, err := eng.LoadCheckpoint(); err != nil {
		return nil, err
	} else if txnErr != nil {
		if err := eng.Run(); err != nil {
			return nil, err
		}
	}
	if err := eng.Run(); err != nil {
		return nil, err
	}
	return &Server{sampleDir: sampleDir, artifactDir: artifactDir, dbPath: dbPath, seq: seq, engine: eng, store: store}, nil
}

func (s *Server) Close() error {
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
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/adjust", s.handleAdjust)
	mux.HandleFunc("/api/region", s.handleRegion)
	mux.HandleFunc("/api/crosscheck", s.handleCrossCheck)
	mux.HandleFunc("/api/review", s.handleReview)
	mux.Handle("/samples/", http.StripPrefix("/samples/", http.FileServer(http.Dir(s.sampleDir))))

	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticSub)))
	return mux
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"report":   s.engine.BuildReport(),
		"manifest": s.seq.Manifest,
		"params":   s.engine.Params(),
		"region":   s.engine.Region(),
		"db_path":  s.dbPath,
	})
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	params := s.engine.Params()
	query := r.URL.Query()
	if query.Has("block_size") {
		value, err := strconv.Atoi(query.Get("block_size"))
		if err != nil {
			writeKernelError(w, core.WithField(core.NewError(core.CodePara, "block_size must be integer"), "block_size"))
			return
		}
		params.BlockSize = value
	}
	if query.Has("verdict_threshold") {
		value, err := strconv.ParseFloat(query.Get("verdict_threshold"), 64)
		if err != nil {
			writeKernelError(w, core.WithField(core.NewError(core.CodePara, "verdict_threshold must be number"), "verdict_threshold"))
			return
		}
		params.VerdictThreshold = value
	}
	if query.Has("noise_z_threshold") {
		value, err := strconv.ParseFloat(query.Get("noise_z_threshold"), 64)
		if err != nil {
			writeKernelError(w, core.WithField(core.NewError(core.CodePara, "noise_z_threshold must be number"), "noise_z_threshold"))
			return
		}
		params.NoiseZThreshold = value
	}
	invalidations, err := s.engine.ApplyParams(params)
	if err != nil {
		writeKernelError(w, err)
		return
	}
	if err := s.engine.Run(); err != nil {
		writeKernelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invalidations": invalidations,
		"report":        s.engine.BuildReport(),
		"params":        s.engine.Params(),
	})
}

func (s *Server) handleRegion(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	region := s.engine.Region()
	query := r.URL.Query()
	var err error
	if query.Has("x") {
		region.X, err = strconv.Atoi(query.Get("x"))
		if err != nil {
			writeKernelError(w, core.WithField(core.NewError(core.CodePara, "x must be integer"), "watermark_region.x"))
			return
		}
	}
	if query.Has("y") {
		region.Y, err = strconv.Atoi(query.Get("y"))
		if err != nil {
			writeKernelError(w, core.WithField(core.NewError(core.CodePara, "y must be integer"), "watermark_region.y"))
			return
		}
	}
	invalidations, err := s.engine.CorrectRegion(region)
	if err != nil {
		writeKernelError(w, err)
		return
	}
	if err := s.engine.Run(); err != nil {
		writeKernelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invalidations": invalidations, "report": s.engine.BuildReport(), "region": region})
}

func (s *Server) handleCrossCheck(w http.ResponseWriter, r *http.Request) {
	s.crossMu.Lock()
	defer s.crossMu.Unlock()
	result, err := pipeline.RunCrossCheck(s.sampleDir, s.artifactDir)
	if err != nil {
		writeKernelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	dbPath := s.dbPath
	s.mu.Unlock()
	fingerprint := filepath.Join(s.sampleDir, "truth.fingerprint.json")
	result, err := review.Run(dbPath, fingerprint)
	if err != nil {
		writeKernelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeKernelError(w http.ResponseWriter, err error) {
	if kernelErr, ok := core.AsError(err); ok {
		writeJSON(w, http.StatusBadRequest, kernelErr)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"code": core.CodeInternal, "message": err.Error()})
}
