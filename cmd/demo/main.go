// demo runs the interactive desktop example: it serves a single-page
// visualisation of the caption pipeline (segment states, caption
// timeline, per-character highlight replay, error curves) and opens it
// in the system browser. Speech-rate and weight adjustments trigger a
// recompute that only touches uncommitted chunks; committed chunks stay
// frozen per the checkpoint contract.
//
// Usage: demo [-project DIR] [-samples DIR] [-addr ADDR] [-work DIR] [-no-open]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"captionx/internal/checkpoint"
	"captionx/internal/loader"
	"captionx/internal/model"
	"captionx/internal/pipeline"
	"captionx/internal/samplegen"
)

func main() {
	projectDir := flag.String("project", "", "project directory (default: first under -samples)")
	samplesDir := flag.String("samples", "samples", "samples directory")
	addr := flag.String("addr", "127.0.0.1:18080", "listen address")
	workDir := flag.String("work", "demo-work", "checkpoint working directory")
	noOpen := flag.Bool("no-open", false, "do not open the browser")
	flag.Parse()

	if *projectDir == "" {
		if _, err := os.Stat(filepath.Join(*samplesDir, "project-1")); os.IsNotExist(err) {
			if _, err := samplegen.Generate(samplegen.Options{OutDir: *samplesDir, Count: 3, Seed: 20261124}); err != nil {
				log.Fatal(err)
			}
		}
		*projectDir = filepath.Join(*samplesDir, "project-1")
	}
	p, err := loader.Load(*projectDir)
	if err != nil {
		log.Fatal(err)
	}
	store, err := checkpoint.NewStore(*workDir)
	if err != nil {
		log.Fatal(err)
	}

	s := &server{proj: p, store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/step", s.handleStep)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/params", s.handleParams)
	mux.HandleFunc("/api/reset", s.handleReset)

	url := "http://" + *addr + "/"
	fmt.Printf("demo serving %s at %s\n", p.Manifest.ProjectID, url)
	if !*noOpen {
		openBrowser(url)
	}
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start() // best effort
}

// server holds the mutable demo session.
type server struct {
	mu       sync.Mutex
	proj     *loader.Project
	store    *checkpoint.Store
	override *model.ReadingParams // nil = manifest parameters
	last     *model.Result
}

type captionState struct {
	ID         int           `json:"id"`
	Text       string        `json:"text"`
	Window     *model.Window `json:"window,omitempty"`
	CharStarts []int64       `json:"charStarts,omitempty"`
	CharEnds   []int64       `json:"charEnds,omitempty"`
	Chunk      int           `json:"chunk"`
	Frozen     bool          `json:"frozen"`
}

type stateResponse struct {
	ProjectID    string                  `json:"projectId"`
	Frame        model.FrameParams       `json:"frame"`
	TotalSamples int64                   `json:"totalSamples"`
	ChunkSamples int64                   `json:"chunkSamples"`
	Segments     []model.SegmentManifest `json:"segments"`
	Captions     []captionState          `json:"captions"`
	Chunks       []string                `json:"chunks"`
	Committed    int                     `json:"committed"`
	Total        int                     `json:"total"`
	Reading      model.ReadingParams     `json:"reading"`
	Override     *model.ReadingParams    `json:"override,omitempty"`
	Report       *model.ErrorReport      `json:"report,omitempty"`
	Decisions    []model.Decision        `json:"decisions,omitempty"`
	Notice       string                  `json:"notice,omitempty"`
}

func (s *server) buildState(notice string) (*stateResponse, error) {
	machine, err := pipeline.Status(s.proj, s.store)
	if err != nil {
		return nil, err
	}
	m := &s.proj.Manifest
	committed, total := machine.Progress()
	resp := &stateResponse{
		ProjectID: m.ProjectID, Frame: m.Frame,
		TotalSamples: m.TotalSamples, ChunkSamples: m.SegmentChunkSamples,
		Segments: m.Segments, Reading: m.Reading, Override: s.override,
		Committed: committed, Total: total, Notice: notice,
	}
	states := machine.States()
	for _, st := range states {
		resp.Chunks = append(resp.Chunks, string(st))
	}
	res := s.last
	for i, c := range m.Captions {
		cs := captionState{ID: c.ID, Text: c.Text}
		if res != nil {
			cr := res.Captions[i]
			cs.Window = &cr.Window
			cs.CharStarts = cr.CharStarts
			cs.CharEnds = cr.CharEnds
			cs.Chunk = cr.SegmentIdx
			cs.Frozen = cr.SegmentIdx >= 0 && cr.SegmentIdx < len(states) && states[cr.SegmentIdx] == "committed"
		}
		resp.Captions = append(resp.Captions, cs)
	}
	if res != nil {
		rep := res.Report
		resp.Report = &rep
		resp.Decisions = res.Decisions
	}
	return resp, nil
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

func (s *server) writeState(w http.ResponseWriter, notice string) {
	st, err := s.buildState(notice)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeState(w, "")
}

// runChunks processes up to maxChunks uncommitted chunks (0 = all).
func (s *server) runChunks(maxChunks int) error {
	out, err := pipeline.RunChunked(s.proj, pipeline.Options{
		Store: s.store, Resume: true, Reading: s.override,
		StopAfterCommits: maxChunks,
	})
	if err != nil {
		return err
	}
	if out.Complete {
		s.last = out.Result
	}
	return nil
}

func (s *server) handleStep(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.runChunks(1); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeState(w, "committed one chunk")
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.runChunks(0); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeState(w, "run to completion")
}

type paramsRequest struct {
	SpeechRateNum int64   `json:"speechRateNum"`
	SpeechRateDen int64   `json:"speechRateDen"`
	WeightScale   float64 `json:"weightScale"`
}

func (s *server) handleParams(w http.ResponseWriter, r *http.Request) {
	var req paramsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.proj.Manifest.Reading
	ov := base
	if req.SpeechRateNum > 0 && req.SpeechRateDen > 0 {
		ov.SpeechRateNum = req.SpeechRateNum
		ov.SpeechRateDen = req.SpeechRateDen
	}
	if req.WeightScale > 0 && req.WeightScale != 1 {
		ov.Weights = make([][]int64, len(base.Weights))
		for i, row := range base.Weights {
			ov.Weights[i] = make([]int64, len(row))
			for j, wgt := range row {
				scaled := int64(float64(wgt)*req.WeightScale + 0.5)
				if scaled < 1 {
					scaled = 1
				}
				ov.Weights[i][j] = scaled
			}
		}
	}
	s.override = &ov
	// Recompute uncommitted chunks with the new parameters; committed
	// chunks stay frozen.
	if err := s.runChunks(0); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	machine, _ := pipeline.Status(s.proj, s.store)
	committed, total := machine.Progress()
	s.writeState(w, fmt.Sprintf(
		"recomputed uncommitted chunks with new parameters; %d/%d chunks committed (frozen)",
		committed, total))
}

func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store.Discard()
	s.last = nil
	s.writeState(w, "checkpoints discarded; all chunks pending")
}
