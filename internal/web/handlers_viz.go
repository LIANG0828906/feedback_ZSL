package web

import (
	"archive/zip"
	"encoding/json"
	"net/http"
	"path/filepath"

	"edgerefine/internal/verify"
	"edgerefine/internal/viz"
)

func assembleMask(s *Server, id, field string) ([]byte, int, int, bool) {
	b, err := s.bundle(id)
	if err != nil {
		return nil, 0, 0, false
	}
	s.mu.Lock()
	rep := s.reports[id]
	s.mu.Unlock()
	if rep == nil {
		return nil, b.Manifest.Width, b.Manifest.Height, false
	}
	W, H := b.Manifest.Width, b.Manifest.Height
	out := make([]byte, W*H)
	for _, t := range rep.Tiles {
		var src []byte
		switch field {
		case "separation":
			src = t.SeparationMask
		case "clean":
			src = t.CleanMask
		case "skeleton":
			src = t.SkeletonMask
		default:
			src = t.RefinedMask
		}
		for y := 0; y < t.Rect.H; y++ {
			dst := (t.Rect.Y+y)*W + t.Rect.X
			copy(out[dst:dst+t.Rect.W], src[y*t.Rect.W:(y+1)*t.Rect.W])
		}
	}
	return out, W, H, true
}

func assembleDeviation(s *Server, id string) ([]float64, int, int, bool) {
	b, err := s.bundle(id)
	if err != nil {
		return nil, 0, 0, false
	}
	s.mu.Lock()
	rep := s.reports[id]
	s.mu.Unlock()
	if rep == nil {
		return nil, b.Manifest.Width, b.Manifest.Height, false
	}
	W, H := b.Manifest.Width, b.Manifest.Height
	out := make([]float64, W*H)
	for _, t := range rep.Tiles {
		for y := 0; y < t.Rect.H; y++ {
			for x := 0; x < t.Rect.W; x++ {
				out[(t.Rect.Y+y)*W+t.Rect.X+x] = t.DeviationMap[y*t.Rect.W+x]
			}
		}
	}
	return out, W, H, true
}

func (s *Server) handleViz(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	kind := r.URL.Query().Get("kind")
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch kind {
	case "original":
		http.ServeFile(w, r, filepath.Join(s.samplesDir, id, b.Manifest.ImageFile))
		return
	case "reference":
		http.ServeFile(w, r, filepath.Join(s.samplesDir, id, b.Manifest.ReferenceMaskFile))
		return
	case "heatmap":
		dev, W, H, ok := assembleDeviation(s, id)
		if !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_ = viz.Heatmap(w, dev, W, H)
		return
	case "overlay":
		mask, W, H, ok := assembleMask(s, id, "refined")
		if !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_ = viz.Overlay(w, b.Image, mask, W, H)
		return
	case "diff":
		maskA, W, H, ok := assembleMask(s, id, "refined")
		if !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		s.mu.Lock()
		prev := s.previews[id]
		s.mu.Unlock()
		if prev == nil || prev.Report == nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		previewMask := make([]byte, W*H)
		for _, t := range prev.Report.Tiles {
			for y := 0; y < t.Rect.H; y++ {
				dst := (t.Rect.Y+y)*W + t.Rect.X
				copy(previewMask[dst:dst+t.Rect.W], t.RefinedMask[y*t.Rect.W:(y+1)*t.Rect.W])
			}
		}
		w.Header().Set("Content-Type", "image/png")
		_ = viz.Difference(w, previewMask, maskA, W, H)
		return
	default:
		mask, W, H, ok := assembleMask(s, id, kind)
		if !ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_ = viz.Mask(w, mask, W, H)
	}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	rep := s.reports[id]
	s.mu.Unlock()
	if rep == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run sample before export"})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=edge-"+id+".zip")
	zw := zip.NewWriter(w)
	defer zw.Close()

	addJSON := func(name string, v any) {
		f, err := zw.Create(name)
		if err != nil {
			return
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
	}
	addPNG := func(name, kind string) {
		mask, W, H, ok := assembleMask(s, id, kind)
		if !ok {
			return
		}
		f, _ := zw.Create(name)
		_ = viz.Mask(f, mask, W, H)
	}
	addJSON("truth.json", b.Manifest)
	addJSON("run-report.json", runSummary(id, rep, nil))
	if vr, err := verify.Verify(s.db, b.Manifest, sampleFingerprint(b)); err == nil {
		addJSON("verification.json", vr)
	}
	addPNG("separation.png", "separation")
	addPNG("clean.png", "clean")
	addPNG("skeleton.png", "skeleton")
	addPNG("refined.png", "refined")
	if dev, W, H, ok := assembleDeviation(s, id); ok {
		f, _ := zw.Create("deviation-heatmap.png")
		_ = viz.Heatmap(f, dev, W, H)
	}
}
