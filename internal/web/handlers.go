package web

import (
	"encoding/json"
	"image"
	"net/http"
	"strconv"

	"maskpipe/internal/model"
	"maskpipe/internal/pipeline"
)

func serveArtifact(w http.ResponseWriter, r *http.Request, m *Manager) {
	x, err := strconv.Atoi(r.URL.Query().Get("x"))
	if err != nil {
		http.Error(w, "invalid x", http.StatusBadRequest)
		return
	}
	y, err := strconv.Atoi(r.URL.Query().Get("y"))
	if err != nil {
		http.Error(w, "invalid y", http.StatusBadRequest)
		return
	}
	stage := model.Stage(r.URL.Query().Get("stage"))
	if stage != model.StageGrow && stage != model.StageClean {
		http.Error(w, "stage must be grow or clean", http.StatusBadRequest)
		return
	}
	rec, err := m.StageRecord(r.Context(), x, y, stage)
	if err != nil {
		http.Error(w, "artifact not ready: "+err.Error(), http.StatusNotFound)
		return
	}
	var summary pipeline.StageSummary
	if err := json.Unmarshal([]byte(rec.SummaryJSON), &summary); err != nil {
		http.Error(w, "invalid summary", http.StatusInternalServerError)
		return
	}
	serveMaskPNG(w, rec.Blob, summary.Width, summary.Height)
}

func overlayImage(base *image.NRGBA, mask []byte) *image.NRGBA {
	out := image.NewNRGBA(base.Bounds())
	copy(out.Pix, base.Pix)
	w := base.Bounds().Dx()
	h := base.Bounds().Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if i >= len(mask) || mask[i] == 0 {
				continue
			}
			off := out.PixOffset(x, y)
			out.Pix[off] = uint8((int(out.Pix[off]) + 255) / 2)
			out.Pix[off+1] = uint8(int(out.Pix[off+1]) * 6 / 10)
			out.Pix[off+2] = uint8(int(out.Pix[off+2]) * 6 / 10)
		}
	}
	return out
}
