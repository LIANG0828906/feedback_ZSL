package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"semanticshading/internal/diagnostics"
	"semanticshading/internal/pipeline"
	"semanticshading/internal/sample"
)

type State struct {
	Title          string               `json:"title"`
	GeneratedAt    time.Time            `json:"generated_at"`
	GOOS           string               `json:"goos"`
	GOARCH         string               `json:"goarch"`
	GoVersion      string               `json:"go_version"`
	DiagnosticsDir string               `json:"diagnostics_dir"`
	Pipeline       *pipeline.Report     `json:"pipeline"`
	Injections     []diagnostics.Report `json:"injections"`
}

type Server struct {
	state   *State
	diagDir string
	mu      sync.Mutex
}

func NewServer(state *State, diagDir string) *Server {
	if state == nil {
		state = &State{Title: "Semantic Shading Workbench"}
	}
	if state.Title == "" {
		state.Title = "Semantic Shading Workbench"
	}
	state.GOOS = runtime.GOOS
	state.GOARCH = runtime.GOARCH
	state.GoVersion = runtime.Version()
	if diagDir == "" {
		diagDir = filepath.Join("data", "diagnostics")
	}
	state.DiagnosticsDir = diagDir
	return &Server{state: state, diagDir: diagDir}
}

func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func SaveState(path string, state *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now(), "goos": runtime.GOOS})
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, http.StatusOK, s.state)
	})
	mux.HandleFunc("/api/inject", func(w http.ResponseWriter, r *http.Request) {
		kind := r.URL.Query().Get("kind")
		if kind == "" {
			kind = "decode"
		}
		report := diagnostics.Inject(r.Context(), kind, s.diagDir)
		writeJSON(w, http.StatusOK, report)
	})
	mux.HandleFunc("/api/inject-all", func(w http.ResponseWriter, r *http.Request) {
		reports := diagnostics.RunAll(r.Context(), s.diagDir)
		s.mu.Lock()
		s.state.Injections = reports
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, reports)
	})
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	httpServer := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	return httpServer.ListenAndServe()
}

func BuildState(ctx context.Context, dataDir string) (*State, error) {
	if dataDir == "" {
		dataDir = filepath.Join("data", "checkpoints")
	}
	report, err := pipeline.RunThreePaths(ctx, baselineSample(), pipeline.Options{Dir: dataDir, VerifyEachPath: true})
	if err != nil {
		return nil, err
	}
	diagDir := filepath.Join(filepath.Dir(dataDir), "diagnostics")
	injections := diagnostics.RunAll(ctx, diagDir)
	return &State{
		Title:          "游戏贴图语义着色一致性工作台",
		GeneratedAt:    time.Now(),
		Pipeline:       report,
		Injections:     injections,
		DiagnosticsDir: diagDir,
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func baselineSample() *sample.Sample {
	generated, err := sample.Generate(sample.DefaultConfig())
	if err != nil {
		panic(err)
	}
	return generated
}
