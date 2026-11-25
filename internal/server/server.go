package server

import (
	"context"
	"embed"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"maskqa/internal/checkpoint"
	"maskqa/internal/deps"
	"maskqa/internal/engine"
	"maskqa/internal/model"
	"maskqa/internal/runner"
	"maskqa/internal/sample"
	"maskqa/internal/schemaio"
)

//go:embed index.html
var indexHTML []byte

type Server struct {
	DataDir string

	mu            sync.Mutex
	reports       map[string]*runner.ThreePathReport
	invalidations map[string]*InvalidationResponse
}

type InvalidationResponse struct {
	EditedStrokeID string                 `json:"edited_stroke_id"`
	Plan           model.InvalidationPlan `json:"plan"`
	Result         *model.RunResult       `json:"result,omitempty"`
	Error          *model.Error           `json:"error,omitempty"`
}

func New(dataDir string) *Server {
	return &Server{DataDir: dataDir, reports: map[string]*runner.ThreePathReport{}, invalidations: map[string]*InvalidationResponse{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/project", s.handleProject)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/invalidation", s.handleInvalidation)
	mux.HandleFunc("/img/sample", s.handleSampleImage)
	mux.HandleFunc("/img/mask", s.handleMaskImage)
	mux.HandleFunc("/img/strokes", s.handleStrokeImage)
	mux.HandleFunc("/img/ring", s.handleRingImage)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	manifest, err := readManifest(s.DataDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, manifest)
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.loadProject(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p, err := s.loadProject(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	report := s.reports[id]
	s.mu.Unlock()
	if report == nil {
		cfg := runner.DefaultConfig("web-verify")
		cfg.MaxParallel = 2
		cfg.QueueBudget = 4
		report, err = runner.ExecuteThreePaths(r.Context(), p, filepath.Join(s.DataDir, "checkpoints", "web"), cfg)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.mu.Lock()
		s.reports[id] = report
		s.mu.Unlock()
	}
	writeJSON(w, report)
}

func (s *Server) handleInvalidation(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p, err := s.loadProject(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	cached := s.invalidations[id]
	s.mu.Unlock()
	if cached != nil {
		writeJSON(w, cached)
		return
	}

	edited := *p
	edited.Strokes = append([]model.Stroke(nil), p.Strokes...)
	edited.Strokes[0].WeightBP += 50
	edited.Strokes[0].ContentHash = schemaio.StrokeHash(edited.Strokes[0])
	plan := deps.PlanEdit(p, &edited)

	dbPath := filepath.Join(s.DataDir, "checkpoints", "web-invalidation-"+id+".db")
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	cfg := runner.DefaultConfig("web-invalidation")
	_, fullErr := runner.Run(context.Background(), p, store, cfg, nil)
	resp := &InvalidationResponse{EditedStrokeID: edited.Strokes[0].ID, Plan: plan}
	if fullErr != nil {
		_ = store.Close()
		writeError(w, http.StatusInternalServerError, fullErr)
		return
	}
	result, runErr := runner.Run(context.Background(), &edited, store, cfg, &plan)
	_ = store.Close()
	resp.Result = result
	if runErr != nil {
		if me, ok := runErr.(*model.Error); ok {
			resp.Error = me
		} else {
			resp.Error = model.NewError(model.CodeState, "", "", "%v", runErr)
		}
	}
	s.mu.Lock()
	s.invalidations[id] = resp
	s.mu.Unlock()
	writeJSON(w, resp)
}

func (s *Server) handleSampleImage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	path := filepath.Join(s.DataDir, "samples", id, "image.png")
	http.ServeFile(w, r, path)
}

func (s *Server) handleMaskImage(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	p, report, err := s.projectAndReport(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var mask model.Mask
	switch kind {
	case "candidate":
		mask = p.Candidate
	case "reference", "truth":
		mask = p.Reference
	case "coverage":
		mask = report.Resume.FinalCoverage
	default:
		mask = report.Resume.FinalMask
	}
	img := maskToRGBA(mask, kind == "coverage")
	writePNGResponse(w, img)
}

func (s *Server) handleStrokeImage(w http.ResponseWriter, r *http.Request) {
	p, err := s.loadProject(r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	base, err := readPNG(filepath.Join(s.DataDir, "samples", p.ID, "image.png"))
	if err != nil {
		base = image.NewRGBA(image.Rect(0, 0, p.Width, p.Height))
	}
	img := image.NewRGBA(base.Bounds())
	draw.Draw(img, img.Bounds(), base, base.Bounds().Min, draw.Src)
	ordered := engine.OrderedStrokes(p.Strokes)
	for si, st := range ordered {
		rgb := paletteColor(si)
		for _, c := range p.Chunks {
			if !engine.StrokeIntersectsChunk(st, c) {
				continue
			}
			comp, err := engine.RasterizeStrokeChunk(st, c, 4096)
			if err != nil {
				continue
			}
			for y := c.Y0; y < c.Y1; y++ {
				for x := c.X0; x < c.X1; x++ {
					a := comp[(y-c.Y0)*c.Width()+(x-c.X0)]
					if a == 0 {
						continue
					}
					idx := img.PixOffset(x, y)
					alpha := uint32(a) * 70 / 100
					img.Pix[idx+0] = uint8((uint32(rgb.R)*alpha + uint32(img.Pix[idx+0])*(255-alpha)) / 255)
					img.Pix[idx+1] = uint8((uint32(rgb.G)*alpha + uint32(img.Pix[idx+1])*(255-alpha)) / 255)
					img.Pix[idx+2] = uint8((uint32(rgb.B)*alpha + uint32(img.Pix[idx+2])*(255-alpha)) / 255)
				}
			}
		}
	}
	writePNGResponse(w, img)
}

func (s *Server) handleRingImage(w http.ResponseWriter, r *http.Request) {
	_, report, err := s.projectAndReport(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	mask := report.Resume.FinalMask
	img := maskToRGBA(mask, false)
	for y := 0; y < mask.Height; y++ {
		for x := 0; x < mask.Width; x++ {
			if mask.At(x, y) == 0 {
				continue
			}
			depth := boundaryDepth(mask, x, y)
			idx := 4 * (y*mask.Width + x)
			switch depth {
			case 1:
				img.Pix[idx+0], img.Pix[idx+1], img.Pix[idx+2] = 255, 176, 32
			case 2:
				img.Pix[idx+0], img.Pix[idx+1], img.Pix[idx+2] = 45, 226, 230
			case 3:
				img.Pix[idx+0], img.Pix[idx+1], img.Pix[idx+2] = 130, 255, 170
			}
		}
	}
	writePNGResponse(w, img)
}

func (s *Server) projectAndReport(ctx context.Context, id string) (*model.Project, *runner.ThreePathReport, error) {
	p, err := s.loadProject(id)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	report := s.reports[id]
	s.mu.Unlock()
	if report == nil {
		cfg := runner.DefaultConfig("web-image")
		cfg.MaxParallel = 2
		report, err = runner.ExecuteThreePaths(ctx, p, filepath.Join(s.DataDir, "checkpoints", "web"), cfg)
		if err != nil {
			return p, nil, err
		}
		s.mu.Lock()
		s.reports[id] = report
		s.mu.Unlock()
	}
	return p, report, nil
}

func (s *Server) loadProject(id string) (*model.Project, error) {
	if id == "" {
		manifest, err := readManifest(s.DataDir)
		if err != nil || len(manifest.Projects) == 0 {
			return nil, os.ErrNotExist
		}
		id = manifest.Projects[0].ID
	}
	return sample.LoadProject(filepath.Join(s.DataDir, "samples", id, "project.internal.json"))
}

func readManifest(dataDir string) (*sample.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m sample.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func maskToRGBA(m model.Mask, grayscaleCoverage bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, m.Width, m.Height))
	for i, v := range m.Pix {
		idx := i * 4
		if grayscaleCoverage {
			img.Pix[idx+0], img.Pix[idx+1], img.Pix[idx+2], img.Pix[idx+3] = v, v, v, 255
			continue
		}
		if v != 0 {
			img.Pix[idx+0], img.Pix[idx+1], img.Pix[idx+2], img.Pix[idx+3] = 42, 157, 244, 255
		} else {
			img.Pix[idx+0], img.Pix[idx+1], img.Pix[idx+2], img.Pix[idx+3] = 18, 22, 33, 255
		}
	}
	return img
}

func boundaryDepth(m model.Mask, x, y int) int {
	for radius := 1; radius <= 3; radius++ {
		foundZero := false
		for dy := -radius; dy <= radius && !foundZero; dy++ {
			for dx := -radius; dx <= radius; dx++ {
				if maxInt(absInt(dx), absInt(dy)) != radius {
					continue
				}
				if m.At(x+dx, y+dy) == 0 {
					foundZero = true
					break
				}
			}
		}
		if foundZero {
			return radius
		}
	}
	return 4
}

func paletteColor(i int) color.RGBA {
	colors := []color.RGBA{{255, 99, 132, 255}, {255, 205, 86, 255}, {75, 192, 192, 255}, {153, 102, 255, 255}, {255, 159, 64, 255}}
	return colors[i%len(colors)]
}

func readPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

func writePNGResponse(w http.ResponseWriter, img image.Image) {
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, img)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = embed.FS{}
