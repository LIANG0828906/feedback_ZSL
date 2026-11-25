package diagnostic

import (
	_ "embed"
	"image"
	"image/png"
	"net/http"
)

//go:embed static/index.html
var indexHTML []byte

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleFramePNG(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	frame := append([]byte{}, s.wb.Frame...)
	camera := s.wb.Camera
	s.mu.Unlock()

	if len(frame) == 0 {
		http.Error(w, "frame not ready", http.StatusServiceUnavailable)
		return
	}
	img := &image.RGBA{
		Pix:    frame,
		Stride: camera.ViewportW * 4,
		Rect:   image.Rect(0, 0, camera.ViewportW, camera.ViewportH),
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_ = png.Encode(w, img)
}
