package main

import (
	_ "embed"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strconv"

	"maskqc"
)

//go:embed dashboard.html
var dashboardHTML string

type dashboardServer struct {
	report *maskqc.WorkbenchReport
}

func (s *dashboardServer) result(name string) *maskqc.RunResult {
	switch name {
	case "recovery":
		return s.report.ThreePath.Recovery
	case "full":
		return s.report.ThreePath.Full
	case "edited-live":
		return s.report.EditedThreePath.Live
	case "edited-recovery":
		return s.report.EditedThreePath.Recovery
	case "edited-full":
		return s.report.EditedThreePath.Full
	default:
		return s.report.ThreePath.Live
	}
}

func (s *dashboardServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})
	mux.HandleFunc("/api/report", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(s.report)
	})
	mux.HandleFunc("/api/result", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(s.result(r.URL.Query().Get("path")))
	})
	mux.HandleFunc("/mask.png", s.maskPNG)
	return mux
}

func (s *dashboardServer) maskPNG(w http.ResponseWriter, r *http.Request) {
	result := s.result(r.URL.Query().Get("path"))
	if result == nil || result.Mask == nil {
		http.NotFound(w, r)
		return
	}
	scale := 6
	if v := r.URL.Query().Get("scale"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 32 {
			scale = n
		}
	}
	srcW, srcH := result.Mask.Width, result.Mask.Height
	srcX, srcY := 0, 0
	if v := r.URL.Query().Get("chunk"); v != "" {
		if id, err := strconv.Atoi(v); err == nil {
			if c, ok := result.Grid.Chunk(id); ok {
				srcX, srcY, srcW, srcH = c.X, c.Y, c.Width, c.Height
			}
		}
	}
	img := image.NewGray(image.Rect(0, 0, srcW*scale, srcH*scale))
	for y := 0; y < srcH; y++ {
		for x := 0; x < srcW; x++ {
			v := result.Mask.Pix[(srcY+y)*result.Mask.Width+srcX+x]
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetGray(x*scale+dx, y*scale+dy, color.Gray{Y: v})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, img)
}
