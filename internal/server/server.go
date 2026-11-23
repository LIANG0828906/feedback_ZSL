package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"textureworkbench/internal/engine"
	"textureworkbench/internal/sample"
	"textureworkbench/internal/tex"
)

//go:embed web/*
var embeddedWeb embed.FS

type Workbench struct {
	dataDir       string
	checkpointDir string
	manifest      *sample.Manifest
	mu            sync.RWMutex
	sessions      map[string]*engine.Session
	comparisons   map[string]engine.Comparison
}

func New(dataDir, checkpointDir string) (*Workbench, error) {
	manifest, err := sample.LoadManifest(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(checkpointDir, 0o755); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	return &Workbench{
		dataDir:       dataDir,
		checkpointDir: checkpointDir,
		manifest:      manifest,
		sessions:      make(map[string]*engine.Session),
		comparisons:   make(map[string]engine.Comparison),
	}, nil
}

func (wb *Workbench) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bootstrap", wb.handleBootstrap)
	mux.HandleFunc("GET /api/health", wb.handleHealth)
	mux.HandleFunc("POST /api/sessions", wb.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", wb.handleSnapshot)
	mux.HandleFunc("POST /api/sessions/{id}/run", wb.handleRun)
	mux.HandleFunc("POST /api/sessions/{id}/pause", wb.handlePause)
	mux.HandleFunc("POST /api/sessions/{id}/cancel", wb.handleCancel)
	mux.HandleFunc("POST /api/sessions/{id}/reconfigure", wb.handleReconfigure)
	mux.HandleFunc("POST /api/sessions/{id}/compare", wb.handleCompare)
	mux.HandleFunc("POST /api/sessions/{id}/blocks/{index}/retry", wb.handleRetry)
	mux.HandleFunc("POST /api/sessions/{id}/blocks/{index}/fault", wb.handleFault)
	mux.HandleFunc("GET /api/sessions/{id}/preview", wb.handlePreview)
	mux.HandleFunc("GET /api/sessions/{id}/heatmap", wb.handleHeatmap)
	mux.HandleFunc("GET /api/sessions/{id}/mapping/inverse", wb.handleInverseMapping)
	mux.HandleFunc("GET /api/sessions/{id}/export", wb.handleExport)
	mux.HandleFunc("GET /api/samples/{id}/preview", wb.handleSamplePreview)
	webFS, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(webFS)))
	return withHeaders(mux)
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (wb *Workbench) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"go":          runtime.Version(),
		"algorithm":   tex.AlgorithmVersion,
		"server_time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (wb *Workbench) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	preflight := wb.preflight()
	writeJSON(w, http.StatusOK, map[string]any{
		"seed":           wb.manifest.Seed,
		"samples":        wb.manifest.Samples,
		"descriptors":    wb.manifest.Descriptors,
		"algorithm":      tex.AlgorithmVersion,
		"go_version":     runtime.Version(),
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"data_dir":       wb.dataDir,
		"checkpoint_dir": wb.checkpointDir,
		"preflight":      preflight,
		"limits": map[string]any{
			"max_workers":              64,
			"max_block_dimension":      4096,
			"max_buffer_pixels_worker": 64 * 1024 * 1024,
			"max_queue_depth":          1024,
		},
		"defaults": map[string]any{
			"block_width":              32,
			"block_height":             32,
			"max_workers":              4,
			"buffer_pixels_per_worker": 1024,
			"queue_depth":              2,
			"parameter_version":        "params-v1",
		},
	})
}

func (wb *Workbench) preflight() map[string]any {
	result := map[string]any{
		"data_readable":       true,
		"checkpoint_writable": false,
		"message":             "",
	}
	if _, err := os.ReadDir(wb.dataDir); err != nil {
		result["data_readable"] = false
		result["message"] = err.Error()
		return result
	}
	temp, err := os.CreateTemp(wb.checkpointDir, ".preflight-*")
	if err != nil {
		result["message"] = err.Error()
		return result
	}
	name := temp.Name()
	if _, err := temp.WriteString("ok"); err != nil {
		temp.Close()
		os.Remove(name)
		result["message"] = err.Error()
		return result
	}
	temp.Close()
	os.Remove(name)
	result["checkpoint_writable"] = true
	return result
}

type createSessionRequest struct {
	ImageID      string          `json:"image_id"`
	DescriptorID string          `json:"descriptor_id"`
	Descriptor   *tex.Descriptor `json:"descriptor"`
	Config       engine.Config   `json:"config"`
}

func (wb *Workbench) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var request createSessionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	info, ok := wb.manifest.Sample(request.ImageID)
	if !ok {
		writeError(w, tex.ErrPara("image_id", fmt.Sprintf("unknown sample %q", request.ImageID)))
		return
	}
	image, err := tex.DecodeImageFile(filepath.Join(wb.dataDir, info.File))
	if err != nil {
		writeError(w, err)
		return
	}
	image.ID = info.ID
	image.EnsureHash()
	if image.ContentHash != info.ContentHash {
		writeError(w, tex.ErrDecode("sample content hash does not match manifest"))
		return
	}
	var descriptor tex.Descriptor
	if request.Descriptor != nil {
		descriptor = request.Descriptor.Normalize()
	} else {
		found := false
		descriptor, found = wb.manifest.Descriptor(request.DescriptorID)
		if !found {
			writeError(w, tex.ErrPara("descriptor_id", fmt.Sprintf("unknown descriptor %q", request.DescriptorID)))
			return
		}
	}
	if descriptor.SourceImage == "" {
		descriptor.SourceImage = info.ID
	}
	if descriptor.SourceLayout == "" {
		descriptor.SourceLayout = info.Layout
	}
	if err := descriptor.ValidateForImage(image); err != nil {
		writeError(w, err)
		return
	}
	cfg := request.Config.Normalize()
	cfg.CheckpointRoot = wb.checkpointDir
	if cfg.SessionKey == "" {
		cfg.SessionKey = info.ID
	}
	if cfg.BufferPixelsPerWorker == 0 {
		cfg.BufferPixelsPerWorker = cfg.BlockWidth * cfg.BlockHeight
	}
	sessionKey := cfg.SessionKey
	wb.mu.RLock()
	existing := wb.sessions[sessionKey]
	wb.mu.RUnlock()
	if existing != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":    tex.CodeState,
			"message": "a session with this key already exists; reconfigure it explicitly to avoid silently replacing committed blocks",
			"session": existing.Snapshot(),
		})
		return
	}
	session, err := engine.OpenSession(image, descriptor, cfg)
	if err != nil {
		writeError(w, err)
		return
	}
	wb.mu.Lock()
	wb.sessions[sessionKey] = session
	delete(wb.comparisons, sessionKey)
	wb.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"session": session.Snapshot()})
}

func (wb *Workbench) session(r *http.Request) (*engine.Session, bool) {
	id := r.PathValue("id")
	wb.mu.RLock()
	session := wb.sessions[id]
	wb.mu.RUnlock()
	return session, session != nil
}

func (wb *Workbench) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	wb.mu.RLock()
	comparison, hasComparison := wb.comparisons[session.ID()]
	wb.mu.RUnlock()
	response := map[string]any{"session": session.Snapshot()}
	if hasComparison {
		response["comparison"] = comparison
	}
	writeJSON(w, http.StatusOK, response)
}

func (wb *Workbench) handleRun(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	var request engine.RunOptions
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := session.StartSegmented(request); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"session": session.Snapshot()})
}

func (wb *Workbench) handlePause(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	session.Pause()
	writeJSON(w, http.StatusAccepted, map[string]any{"session": session.Snapshot()})
}

func (wb *Workbench) handleCancel(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	session.Cancel()
	writeJSON(w, http.StatusAccepted, map[string]any{"session": session.Snapshot()})
}

type reconfigureRequest struct {
	Descriptor       tex.Descriptor `json:"descriptor"`
	ParameterVersion string         `json:"parameter_version"`
}

func (wb *Workbench) handleReconfigure(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	var request reconfigureRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	invalidated, err := session.Reconfigure(request.Descriptor, request.ParameterVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	wb.mu.Lock()
	delete(wb.comparisons, session.ID())
	wb.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"session":              session.Snapshot(),
		"invalidated_blocks":   invalidated,
		"snapshot_invalidated": len(invalidated) > 0,
	})
}

func (wb *Workbench) handleCompare(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	comparison, err := engine.CompareThreePaths(session.Image(), session.Descriptor(), session.Config())
	if err != nil {
		if texErr, ok := err.(*tex.Error); ok && texErr.Code == tex.CodeDiff {
			writeJSON(w, http.StatusConflict, map[string]any{
				"code":       texErr.Code,
				"message":    texErr.Message,
				"details":    texErr.Details,
				"comparison": comparison,
			})
			return
		}
		writeError(w, err)
		return
	}
	wb.mu.Lock()
	wb.comparisons[session.ID()] = comparison
	wb.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"comparison": comparison})
}

func (wb *Workbench) handleRetry(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeError(w, tex.ErrPara("block_index", "block index must be an integer"))
		return
	}
	block, err := session.RetryBlock(index)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":    tex.CodeState,
			"message": err.Error(),
			"block":   block,
			"session": session.Snapshot(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"block": block, "session": session.Snapshot()})
}

type faultRequest struct {
	Times int `json:"times"`
}

func (wb *Workbench) handleFault(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeError(w, tex.ErrPara("block_index", "block index must be an integer"))
		return
	}
	var request faultRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, err)
			return
		}
	}
	if err := session.InjectFault(index, request.Times); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"block_index": index, "armed": true})
}

func (wb *Workbench) handlePreview(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	switch r.URL.Query().Get("side") {
	case "", "source":
		image := session.Image()
		data := &tex.Converted{
			Width:    image.Width,
			Height:   image.Height,
			BitDepth: image.BitDepth,
			Layout:   image.Layout,
			Pixels:   image.Pixels,
		}
		w.Header().Set("Content-Type", "image/png")
		if err := tex.EncodePNG(w, data, true); err != nil {
			writeError(w, err)
		}
	case "target":
		result, err := session.Result()
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		if err := tex.EncodePNG(w, result, true); err != nil {
			writeError(w, err)
		}
	default:
		writeError(w, tex.ErrPara("side", "side must be source or target"))
	}
}

func (wb *Workbench) handleSamplePreview(w http.ResponseWriter, r *http.Request) {
	info, ok := wb.manifest.Sample(r.PathValue("id"))
	if !ok {
		writeError(w, tex.ErrPara("id", "unknown sample"))
		return
	}
	image, err := tex.DecodeImageFile(filepath.Join(wb.dataDir, info.File))
	if err != nil {
		writeError(w, err)
		return
	}
	data := &tex.Converted{
		Width:    image.Width,
		Height:   image.Height,
		BitDepth: image.BitDepth,
		Layout:   image.Layout,
		Pixels:   image.Pixels,
	}
	w.Header().Set("Content-Type", "image/png")
	if err := tex.EncodePNG(w, data, true); err != nil {
		writeError(w, err)
	}
}

func (wb *Workbench) handleHeatmap(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	result, err := session.Result()
	if err != nil {
		writeError(w, err)
		return
	}
	channels := result.Layout.ChannelCount()
	out := image.NewGray(image.Rect(0, 0, result.Width, result.Height))
	var maxAbs int64
	for _, value := range result.Errors {
		abs := int64(value)
		if abs < 0 {
			abs = -abs
		}
		if abs > maxAbs {
			maxAbs = abs
		}
	}
	for y := 0; y < result.Height; y++ {
		for x := 0; x < result.Width; x++ {
			var pixelMax int64
			for c := 0; c < channels; c++ {
				value := int64(result.Errors[(y*result.Width+x)*channels+c])
				if value < 0 {
					value = -value
				}
				if value > pixelMax {
					pixelMax = value
				}
			}
			intensity := uint8(0)
			if maxAbs > 0 {
				intensity = uint8((pixelMax*255 + maxAbs/2) / maxAbs)
			}
			out.SetGray(x, y, color.Gray{Y: intensity})
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, out)
}

func (wb *Workbench) handleInverseMapping(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	recovered, err := session.InverseMapping()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reversible":         true,
		"source_from_target": recovered,
		"message":            "recovered[source_channel] = target_slot",
	})
}

func (wb *Workbench) handleExport(w http.ResponseWriter, r *http.Request) {
	session, ok := wb.session(r)
	if !ok {
		writeError(w, tex.ErrPara("session_id", "unknown session"))
		return
	}
	result, err := session.Result()
	if err != nil {
		writeError(w, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "png"
	}
	switch format {
	case "png":
		filename := fmt.Sprintf("%s-%dbit-%s.png", session.Image().ID, result.BitDepth, result.Layout)
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		if err := tex.EncodePNG(w, result, false); err != nil {
			writeError(w, err)
		}
	case "twimg":
		img := &tex.Image{
			ID:       session.Image().ID + "-converted",
			Width:    result.Width,
			Height:   result.Height,
			BitDepth: result.BitDepth,
			Layout:   result.Layout,
			Pixels:   result.Pixels,
		}
		img.EnsureHash()
		filename := fmt.Sprintf("%s-%dbit-%s.twimg", session.Image().ID, result.BitDepth, result.Layout)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		if err := tex.EncodeImage(w, img); err != nil {
			writeError(w, err)
		}
	default:
		writeError(w, tex.ErrPara("format", "format must be png or twimg"))
	}
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return tex.ErrPara("body", fmt.Sprintf("invalid JSON request: %v", err))
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var texErr *tex.Error
	if errors.As(err, &texErr) {
		switch texErr.Code {
		case tex.CodePara, tex.CodeDecode:
			status = http.StatusBadRequest
		case tex.CodeLayout, tex.CodeLoss:
			status = http.StatusUnprocessableEntity
		case tex.CodeState, tex.CodeDiff, tex.CodeBusy, tex.CodeCommit:
			status = http.StatusConflict
		}
		writeJSON(w, status, texErr)
		return
	}
	writeJSON(w, status, map[string]any{
		"code":    "E_INTERNAL",
		"message": err.Error(),
	})
}
