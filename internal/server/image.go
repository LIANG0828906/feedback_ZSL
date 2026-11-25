package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/engine"
)

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	sess := s.currentSession()
	if sess == nil {
		writeError(w, apperr.New(apperr.ESTATE, "no active session"))
		return
	}
	comp := sess.engine.Composite()
	kind := r.URL.Query().Get("kind")
	var img image.Image
	var err error
	switch kind {
	case "", "composite":
		img, err = compositeImage(comp, r.URL.Query().Get("overlay") == "1")
	case "band":
		img, err = bandImage(comp, r.URL.Query().Get("band"))
	case "heatmap":
		sess.mu.Lock()
		verify := sess.verify
		sess.mu.Unlock()
		img = heatmapImage(comp.Width, comp.Height, verify)
	default:
		writeError(w, apperr.New(apperr.EPARAM, "unknown image kind"))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if img == nil {
		writeError(w, apperr.New(apperr.ESTATE, "image is not available yet"))
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		writeError(w, apperr.New(apperr.EIO, "encode png: %v", err))
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func compositeImage(comp *engine.Composite, overlay bool) (*image.NRGBA, error) {
	if comp == nil || comp.Width <= 0 || comp.Height <= 0 {
		return nil, nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, comp.Width, comp.Height))
	oog := make(map[int]bool, len(comp.OutOfGamut))
	for _, p := range comp.OutOfGamut {
		oog[p.Y*comp.Width+p.X] = true
	}
	for y := 0; y < comp.Height; y++ {
		for x := 0; x < comp.Width; x++ {
			idx := (y*comp.Width + x) * 3
			r := linearToByte(comp.LinearRGB[idx])
			g := linearToByte(comp.LinearRGB[idx+1])
			b := linearToByte(comp.LinearRGB[idx+2])
			if overlay && oog[y*comp.Width+x] {
				r, g, b = 238, 70, 210
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img, nil
}

func compositePNG(comp *engine.Composite) ([]byte, error) {
	img, err := compositeImage(comp, false)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, apperr.New(apperr.ESTATE, "composite image is unavailable")
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func bandImage(comp *engine.Composite, bandID string) (*image.NRGBA, error) {
	if comp == nil || comp.Width <= 0 || comp.Height <= 0 {
		return nil, nil
	}
	bi := -1
	for i, id := range comp.BandIDs {
		if id == bandID {
			bi = i
			break
		}
	}
	if bi < 0 {
		return nil, apperr.New(apperr.EPARAM, "unknown band %q", bandID)
	}
	bands := len(comp.BandIDs)
	maxVal := 0.0
	for p := 0; p < comp.Width*comp.Height; p++ {
		v := comp.Fused[p*bands+bi]
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal <= 0 {
		maxVal = 1
	}
	tr, tg, tb := wavelengthTint(bandID)
	img := image.NewNRGBA(image.Rect(0, 0, comp.Width, comp.Height))
	for y := 0; y < comp.Height; y++ {
		for x := 0; x < comp.Width; x++ {
			v := comp.Fused[(y*comp.Width+x)*bands+bi] / maxVal
			if v < 0 {
				v = 0
			} else if v > 1 {
				v = 1
			}
			v = math.Pow(v, 1.0/2.2)
			r := uint8(math.Round(255 * v * (0.58 + 0.42*tr)))
			g := uint8(math.Round(255 * v * (0.58 + 0.42*tg)))
			b := uint8(math.Round(255 * v * (0.58 + 0.42*tb)))
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img, nil
}

func heatmapImage(w, h int, report *engine.VerificationReport) *image.NRGBA {
	if w <= 0 || h <= 0 {
		return nil
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	var maxDelta float64
	if report != nil {
		for _, v := range report.Heatmap {
			if v > maxDelta {
				maxDelta = v
			}
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 0.0
			if report != nil && y*w+x < len(report.Heatmap) {
				v = report.Heatmap[y*w+x]
			}
			r, g, b := heatColor(v, maxDelta)
			if maxDelta == 0 {
				r, g, b = 22, 32, 38
			}
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}
	return img
}

func heatColor(v, maxValue float64) (uint8, uint8, uint8) {
	if maxValue <= 0 {
		return 22, 32, 38
	}
	t := v / maxValue
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	r := clampByte(255 * math.Min(1, t*2.2))
	g := clampByte(255 * math.Max(0, math.Min(1, 1.6-t)))
	b := clampByte(255 * math.Max(0, 0.5-t))
	return r, g, b
}

func clampByte(v float64) uint8 {
	if v < 0 {
		v = 0
	} else if v > 255 {
		v = 255
	}
	return uint8(math.Round(v))
}

func linearToByte(v float64) uint8 {
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	return uint8(math.Round(255 * math.Pow(v, 1.0/2.2)))
}

func wavelengthTint(bandID string) (float64, float64, float64) {
	switch bandID {
	case "B450":
		return 0.35, 0.58, 1.0
	case "B530":
		return 0.38, 1.0, 0.46
	case "B610":
		return 1.0, 0.66, 0.24
	case "B680":
		return 1.0, 0.28, 0.22
	case "B750":
		return 0.82, 0.18, 0.38
	default:
		return 1, 1, 1
	}
}
