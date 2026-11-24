package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"net/http"
	"path/filepath"
	"sync"

	"iconbench/internal/checkpoint"
	"iconbench/internal/imaging"
	"iconbench/internal/model"
	"iconbench/internal/pipeline"
	"iconbench/internal/sample"
	"iconbench/internal/verify"
)

//go:embed static/*
var staticFS embed.FS

type App struct {
	DataDir  string
	Manifest *model.Manifest
	Store    *checkpoint.Store
	Pipe     *pipeline.Pipeline

	mu       sync.Mutex
	previews map[string]*pipeline.PreviewResult
	results  map[string]*pipeline.ImageResult
}

type paramsRequest struct {
	ImageID             string       `json:"image_id"`
	Params              model.Params `json:"params"`
	PreviewParamVersion int64        `json:"preview_param_version"`
	Factor              int          `json:"factor"`
}

type apiResponse struct {
	OK    bool         `json:"ok"`
	Data  any          `json:"data,omitempty"`
	Error *model.Error `json:"error,omitempty"`
}

func NewApp(dataDir string, prepare bool) (*App, error) {
	manifestPath := filepath.Join(dataDir, "manifest.json")
	manifest, err := sample.LoadManifest(manifestPath)
	if err != nil {
		manifest, err = sample.Generate(dataDir)
		if err != nil {
			return nil, err
		}
	}
	store, err := checkpoint.Open(filepath.Join(dataDir, "checkpoints.db"))
	if err != nil {
		return nil, err
	}
	pool := pipeline.NewResourcePool(manifest.Budget)
	app := &App{
		DataDir: dataDir, Manifest: manifest, Store: store,
		Pipe:     &pipeline.Pipeline{Manifest: manifest, DataDir: dataDir, Store: store, Pool: pool},
		previews: map[string]*pipeline.PreviewResult{},
		results:  map[string]*pipeline.ImageResult{},
	}
	if prepare {
		params := model.DefaultParams()
		for _, rec := range manifest.Images {
			res, err := app.Pipe.RunImage(context.Background(), rec.ID, params, pipeline.RunOptions{})
			if err != nil {
				return nil, err
			}
			app.results[rec.ID] = res
		}
	}
	return app, nil
}

func (a *App) Close() error { return a.Store.Close() }

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(mustStatic())))
	mux.HandleFunc("GET /api/manifest", a.handleManifest)
	mux.HandleFunc("GET /api/sample-image", a.handleSampleImage)
	mux.HandleFunc("POST /api/preview", a.handlePreview)
	mux.HandleFunc("POST /api/recompute", a.handleRecompute)
	mux.HandleFunc("POST /api/tolerance-curve", a.handleToleranceCurve)
	mux.HandleFunc("GET /api/result", a.handleResult)
	mux.HandleFunc("GET /api/judgments", a.handleJudgments)
	mux.HandleFunc("GET /api/sequence", a.handleSequence)
	mux.HandleFunc("GET /api/verify", a.handleVerify)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, apiResponse{OK: true, Data: map[string]string{"status": "ok"}})
	})
	return mux
}

func mustStatic() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var me *model.Error
	if errors.As(err, &me) {
		status := http.StatusUnprocessableEntity
		switch me.Code {
		case model.CodeNotFound:
			status = http.StatusNotFound
		case model.CodeState, model.CodeVer:
			status = http.StatusConflict
		case model.CodeResource:
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, apiResponse{OK: false, Error: me})
		return
	}
	writeJSON(w, http.StatusInternalServerError, apiResponse{OK: false, Error: &model.Error{Code: "E_INTERNAL", Message: err.Error()}})
}

func (a *App) handleManifest(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, apiResponse{OK: true, Data: a.Manifest})
}

func (a *App) handleSampleImage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	rec, err := findManifestImage(a.Manifest, id)
	if err != nil {
		writeError(w, err)
		return
	}
	http.ServeFile(w, r, filepath.Join(a.DataDir, filepath.FromSlash(rec.File)))
}

func (a *App) handlePreview(w http.ResponseWriter, r *http.Request) {
	var req paramsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, &model.Error{Code: model.CodeDecode, Message: "invalid JSON request: " + err.Error()})
		return
	}
	rec, err := findManifestImage(a.Manifest, req.ImageID)
	if err != nil {
		writeError(w, err)
		return
	}
	img, _, err := sample.LoadImage(a.DataDir, rec)
	if err != nil {
		writeError(w, err)
		return
	}
	rgb := imaging.ToRGB(img)
	preview, err := pipeline.RunPreview(rec.ID, rgb, req.Params, a.Manifest.Budget, req.Factor)
	if err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	a.previews[previewKey(rec.ID, req.Params.ParamVersion)] = preview
	a.mu.Unlock()
	maskB64 := maskPNGBase64(preview.Mask, preview.Width, preview.Height)
	overlayB64 := overlayPNGBase64(img, preview.Mask, preview.Width, preview.Height)
	writeJSON(w, 200, apiResponse{OK: true, Data: map[string]any{
		"preview": preview, "mask_png_base64": maskB64, "overlay_png_base64": overlayB64,
	}})
}

func (a *App) handleRecompute(w http.ResponseWriter, r *http.Request) {
	var req paramsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, &model.Error{Code: model.CodeDecode, Message: "invalid JSON request: " + err.Error()})
		return
	}
	if req.PreviewParamVersion != 0 && req.PreviewParamVersion != req.Params.ParamVersion {
		previewParams := req.Params
		previewParams.ParamVersion = req.PreviewParamVersion
		writeError(w, model.VersionError(previewParams, req.Params))
		return
	}

	res, err := a.Pipe.RunImage(r.Context(), req.ImageID, req.Params, pipeline.RunOptions{})
	if err != nil {
		writeError(w, err)
		return
	}
	a.mu.Lock()
	a.results[req.ImageID] = res
	preview := a.previews[previewKey(req.ImageID, req.Params.ParamVersion)]
	a.mu.Unlock()

	var judgment model.Judgment
	var judgeErr *model.Error
	if preview != nil {
		if verr := pipeline.CheckVersions(preview.Params, req.Params); verr != nil {
			writeError(w, verr)
			return
		}
		judgment, judgeErr = pipeline.JudgePreviewExact(req.ImageID, req.Params.ParamVersion, preview.Mask, res.Mask, res.Width, res.Height, a.Manifest.Tolerances)
		judgment.DownsampleFactor = preview.DownsampleFactor
		judgment.FloodSamples = preview.FloodSamples
		judgment.Basis = preview.Basis
		detail, _ := json.Marshal(map[string]any{"preview_elapsed_ms": preview.ElapsedMS, "preview_basis": preview.Basis})
		id, insErr := a.Store.InsertJudgment(r.Context(), judgment, string(detail))
		if insErr == nil {
			judgment.ID = id
		}
	}

	maskB64 := maskPNGBase64(res.Mask, res.Width, res.Height)
	overlayB64 := overlayPNGBase64(mustLoadImage(a, req.ImageID), res.Mask, res.Width, res.Height)
	payload := map[string]any{
		"result": sanitizeResult(res), "mask_png_base64": maskB64, "overlay_png_base64": overlayB64,
		"judgment": judgment, "sequence": res.Sequence, "queue_records": res.Queue,
	}
	if res.Report != nil && res.Report.StateError != nil {
		payload["region_error"] = res.Report.StateError
		writeJSON(w, http.StatusUnprocessableEntity, apiResponse{OK: false, Error: res.Report.StateError, Data: payload})
		return
	}
	if judgeErr != nil {
		payload["judgment_error"] = judgeErr
		writeJSON(w, http.StatusUnprocessableEntity, apiResponse{OK: false, Error: judgeErr, Data: payload})
		return
	}
	writeJSON(w, 200, apiResponse{OK: true, Data: payload})
}

type curveRequest struct {
	ImageID             string `json:"image_id"`
	PreviewParamVersion int64  `json:"preview_param_version"`
}

func (a *App) handleToleranceCurve(w http.ResponseWriter, r *http.Request) {
	var req curveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, &model.Error{Code: model.CodeDecode, Message: err.Error()})
		return
	}
	a.mu.Lock()
	res := a.results[req.ImageID]
	preview := a.previews[previewKey(req.ImageID, req.PreviewParamVersion)]
	a.mu.Unlock()
	if res == nil || preview == nil {
		writeError(w, &model.Error{Code: model.CodeNotFound, Message: "preview/exact pair not found; run preview and recompute first"})
		return
	}
	base := a.Manifest.Tolerances
	factors := []float64{1, 0.75, 0.5, 0.25}
	sequence := make([]model.Tolerances, 0, len(factors))
	for _, f := range factors {
		tol := base
		tol.PreviewCentroidPx = base.PreviewCentroidPx * f
		tol.PreviewAreaRel = base.PreviewAreaRel * f
		sequence = append(sequence, tol)
	}
	points := pipeline.EvaluateToleranceCurve(req.ImageID, req.PreviewParamVersion, preview.Mask, res.Mask, res.Width, res.Height, sequence)
	for i := range points {
		detail, _ := json.Marshal(map[string]any{"reported_stat": points[i].ReportedStat, "tightening_index": i, "basis": preview.Basis})
		id, err := a.Store.InsertJudgment(r.Context(), points[i].Judgment, string(detail))
		if err == nil {
			points[i].Judgment.ID = id
		}
	}
	writeJSON(w, 200, apiResponse{OK: true, Data: points})
}

func (a *App) handleResult(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	a.mu.Lock()
	res := a.results[id]
	a.mu.Unlock()
	if res == nil {
		writeError(w, &model.Error{Code: model.CodeNotFound, Message: "result not ready: " + id})
		return
	}
	writeJSON(w, 200, apiResponse{OK: true, Data: map[string]any{"result": sanitizeResult(res), "mask_png_base64": maskPNGBase64(res.Mask, res.Width, res.Height)}})
}

func (a *App) handleJudgments(w http.ResponseWriter, r *http.Request) {
	items, err := a.Store.ListJudgments(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, apiResponse{OK: true, Data: items})
}

func (a *App) handleSequence(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	seq, err := a.Store.Sequence(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, apiResponse{OK: true, Data: seq})
}

func (a *App) handleVerify(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id != "" {
		rep := verify.VerifyImage(r.Context(), a.Manifest, a.Store, a.DataDir, "", id)
		status := 200
		if !rep.OK {
			status = 422
		}
		writeJSON(w, status, apiResponse{OK: rep.OK, Data: rep})
		return
	}
	var reports []*verify.Report
	for _, rec := range a.Manifest.Images {
		reports = append(reports, verify.VerifyImage(r.Context(), a.Manifest, a.Store, a.DataDir, "", rec.ID))
	}
	ok := true
	for _, rep := range reports {
		ok = ok && rep.OK
	}
	status := 200
	if !ok {
		status = 422
	}
	writeJSON(w, status, apiResponse{OK: ok, Data: reports})
}

func findManifestImage(m *model.Manifest, id string) (model.ImageManifest, error) {
	for _, rec := range m.Images {
		if rec.ID == id {
			return rec, nil
		}
	}
	return model.ImageManifest{}, &model.Error{Code: model.CodeNotFound, Message: "image not found: " + id}
}

func previewKey(id string, version int64) string {
	return id + "#" + jsonNumber(version)
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func sanitizeResult(res *pipeline.ImageResult) map[string]any {
	return map[string]any{
		"record_id":          res.Record.ID,
		"params":             res.Params,
		"width":              res.Width,
		"height":             res.Height,
		"stats":              res.Stats,
		"truth_stats":        res.TruthStats,
		"report":             res.Report,
		"metrics":            res.Metrics,
		"mask_hash":          res.MaskHash,
		"sequence_count":     len(res.Sequence),
		"queue_records":      res.Queue,
		"snapshot_reset":     res.SnapshotReset,
		"snapshot_violation": res.SnapshotViolation,
	}
}

func maskPNGBase64(mask []byte, w, h int) string {
	gray := image.NewGray(image.Rect(0, 0, w, h))
	for i, v := range mask {
		if v != 0 {
			gray.Pix[i] = 255
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, gray)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func overlayPNGBase64(src image.Image, mask []byte, w, h int) string {
	overlay := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(src.At(src.Bounds().Min.X+x, src.Bounds().Min.Y+y)).(color.NRGBA)
			i := y*w + x
			if mask[i] != 0 {
				c.R = uint8(0.35*float64(c.R) + 0.65*239)
				c.G = uint8(0.35*float64(c.G) + 0.65*68)
				c.B = uint8(0.35*float64(c.B) + 0.65*68)
				c.A = 220
			}
			overlay.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, overlay)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func mustLoadImage(a *App, id string) image.Image {
	rec, _ := findManifestImage(a.Manifest, id)
	img, _, err := sample.LoadImage(a.DataDir, rec)
	if err != nil {
		return image.NewNRGBA(image.Rect(0, 0, rec.Width, rec.Height))
	}
	return img
}
