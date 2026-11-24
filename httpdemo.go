package syncgame

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

//go:embed web/index.html
var webFiles embed.FS

type DemoServer struct {
	sample *Sample
	store  *Store
	mu     sync.Mutex
}

func NewDemoServer(sample *Sample, store *Store) (*DemoServer, error) {
	if sample == nil {
		return nil, fmt.Errorf("sample is nil")
	}
	if err := ValidateSample(sample); err != nil {
		return nil, err
	}
	return &DemoServer{sample: sample, store: store}, nil
}

func (d *DemoServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sample", d.handleSample)
	mux.HandleFunc("/api/demo", d.handleDemo)
	mux.HandleFunc("/api/replay", d.handleReplay)
	mux.HandleFunc("/api/verify", d.handleVerify)
	mux.HandleFunc("/api/windows", d.handleWindows)
	mux.HandleFunc("/api/windows/", d.handleWindowAction)
	content, err := fs.Sub(webFiles, "web")
	if err == nil {
		mux.Handle("/", http.FileServer(http.FS(content)))
	}
	return mux
}

func (d *DemoServer) handleSample(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, &GameError{Code: CodeUnsupported, Message: "method not allowed"})
		return
	}
	type summary struct {
		Seed             uint64        `json:"seed"`
		AlgorithmVersion string        `json:"algorithm_version"`
		TickRate         int           `json:"tick_rate"`
		RenderFPS        int           `json:"render_fps"`
		MaxTicks         int           `json:"max_ticks"`
		Units            []UnitSeed    `json:"units"`
		Clients          []ClientSeed  `json:"clients"`
		LatencyTiers     []LatencyTier `json:"latency_tiers"`
		TruthHash        string        `json:"truth_hash"`
		StreamHash       string        `json:"stream_fingerprint"`
		Settlement       *Settlement   `json:"settlement"`
	}
	writeJSON(w, http.StatusOK, summary{
		Seed:             d.sample.Seed,
		AlgorithmVersion: d.sample.AlgorithmVersion,
		TickRate:         d.sample.TickRate,
		RenderFPS:        d.sample.RenderFPS,
		MaxTicks:         d.sample.MaxTicks,
		Units:            d.sample.Units,
		Clients:          d.sample.Clients,
		LatencyTiers:     d.sample.LatencyTiers,
		TruthHash:        d.sample.Truth.ContentHash,
		StreamHash:       d.sample.StreamFingerprint(),
		Settlement:       d.sample.Truth.Settlement,
	})
}

func (d *DemoServer) handleDemo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, &GameError{Code: CodeUnsupported, Message: "method not allowed"})
		return
	}
	tierID, err := d.resolveTier(r.URL.Query().Get("tier"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	opts := d.runOptions(tierID)
	result, runErr := RunRealtime(d.sample, opts)
	if runErr != nil {
		writeAPIError(w, statusForError(runErr), runErr)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (d *DemoServer) handleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, &GameError{Code: CodeUnsupported, Message: "method not allowed"})
		return
	}
	var body struct {
		Tier string `json:"tier"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	tierID, err := d.resolveTier(body.Tier)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	opts := d.runOptions(tierID)
	opts.Store = nil
	result, runErr := RunReplay(d.sample, opts)
	if runErr != nil {
		writeAPIError(w, statusForError(runErr), runErr)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (d *DemoServer) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, &GameError{Code: CodeUnsupported, Message: "method not allowed"})
		return
	}
	var body struct {
		Tier string `json:"tier"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	tierID, err := d.resolveTier(body.Tier)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	opts := d.runOptions(tierID)
	opts.Store = nil
	report, verifyErr := VerifyThreePaths(d.sample, VerificationOptions{
		RunOptions: opts,
	})
	status := http.StatusOK
	if verifyErr != nil {
		status = statusForError(verifyErr)
	}
	payload := map[string]any{
		"passed": report != nil && report.Passed,
		"report": report,
	}
	if verifyErr != nil {
		payload["error"] = verifyErr
	}
	writeJSON(w, status, payload)
}

func (d *DemoServer) handleWindows(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusMethodNotAllowed, &GameError{Code: CodeUnsupported, Message: "method not allowed"})
		return
	}
	if d.store == nil {
		writeJSON(w, http.StatusOK, []WindowStatus{})
		return
	}
	tierID, err := d.resolveTier(r.URL.Query().Get("tier"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	statuses, err := d.store.Windows(context.Background(), d.runID(tierID))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (d *DemoServer) handleWindowAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusMethodNotAllowed, &GameError{Code: CodeUnsupported, Message: "method not allowed"})
		return
	}
	if d.store == nil {
		writeAPIError(w, http.StatusBadRequest, &GameError{Code: CodeUnsupported, Message: "store is not configured"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/windows/"), "/")
	if len(parts) != 2 || parts[1] != "retry" {
		writeAPIError(w, http.StatusNotFound, &GameError{Code: CodeUnsupported, Message: "unknown window action"})
		return
	}
	index, err := strconv.Atoi(parts[0])
	if err != nil || index < 0 {
		writeAPIError(w, http.StatusBadRequest, &GameError{Code: CodeBounds, Message: "invalid window index"})
		return
	}
	tierID, err := d.resolveTier(r.URL.Query().Get("tier"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	runID := d.runID(tierID)
	if err := d.store.RetryWindow(context.Background(), runID, index); err != nil {
		writeAPIError(w, statusForError(err), err)
		return
	}
	d.mu.Lock()
	result, runErr := RunRealtime(d.sample, d.runOptions(tierID))
	d.mu.Unlock()
	if runErr != nil {
		writeAPIError(w, statusForError(runErr), runErr)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func NewLoopbackServer(handler http.Handler, listenAddr string) (*http.Server, net.Listener, error) {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return nil, nil, err
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, nil, fmt.Errorf("demo server must bind to a loopback address, got %q", host)
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, nil, err
	}
	return &http.Server{Handler: handler, Addr: listenAddr}, listener, nil
}

func (d *DemoServer) runOptions(tierID string) RunOptions {
	return RunOptions{
		Parallel:    true,
		Workers:     4,
		TierID:      tierID,
		Store:       d.store,
		RunID:       d.runID(tierID),
		WindowTicks: 20,
		PoolConfig: PoolConfig{
			Workers:         4,
			QueuePerWorker:  4,
			StateBufferSize: 8,
		},
	}
}

func (d *DemoServer) runID(tierID string) string {
	return "demo-" + d.sample.TruthFingerprint()[:12] + "-" + tierID
}

func (d *DemoServer) resolveTier(requested string) (string, error) {
	if requested == "" {
		if len(d.sample.LatencyTiers) == 0 {
			return "", &GameError{Code: CodeTick, Message: "no latency tier is declared"}
		}
		return d.sample.LatencyTiers[0].ID, nil
	}
	if _, ok := d.sample.Tier(requested); !ok {
		return "", &GameError{
			Code:    CodeTick,
			Message: "requested latency tier is not declared",
			Field:   "tier",
			Left:    requested,
		}
	}
	return requested, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	payload := any(err)
	var gameError *GameError
	if !errors.As(err, &gameError) {
		payload = &GameError{Code: CodeUnsupported, Message: err.Error()}
	}
	writeJSON(w, status, map[string]any{"error": payload})
}

func statusForError(err error) int {
	switch {
	case IsCode(err, CodeConflict):
		return http.StatusConflict
	case IsCode(err, CodeTick), IsCode(err, CodeBounds):
		return http.StatusBadRequest
	case IsCode(err, CodeDiverge), IsCode(err, CodeRollback), IsCode(err, CodeLatency):
		return http.StatusUnprocessableEntity
	case IsCode(err, CodeSnapshot):
		return http.StatusGone
	default:
		return http.StatusInternalServerError
	}
}
