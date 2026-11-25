package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"iconseg/internal/checkpoint"
	"iconseg/internal/core"
	"iconseg/internal/pipeline"
	"iconseg/internal/runner"
)

type Server struct {
	store  *checkpoint.Store
	icons  []core.Icon
	base   core.SegmentParams
	seed   int64
	mu     sync.RWMutex
	latest map[string]*runner.Record
}

func New(store *checkpoint.Store, icons []core.Icon, base core.SegmentParams, seed int64) (*Server, error) {
	if len(icons) == 0 {
		return nil, core.ParaError("icons", "[1,inf)", "at least one generated icon is required")
	}
	if err := base.Validate(); err != nil {
		return nil, err
	}
	return &Server{store: store, icons: icons, base: base, seed: seed, latest: make(map[string]*runner.Record)}, nil
}

func ValidateLoopbackAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return core.ParaError("addr", "loopback:port", "invalid listen address: "+err.Error())
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return core.ParaError("addr", "127.0.0.1:<port> or [::1]:<port>", "diagnostics page may only listen on a loopback address")
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/exact", s.handleExact)
	mux.HandleFunc("/icon.png", s.handleIcon)
	mux.HandleFunc("/mask.png", s.handleMask)
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	if err := ValidateLoopbackAddress(addr); err != nil {
		return err
	}
	server := &http.Server{Addr: addr, Handler: s.Handler()}
	return server.ListenAndServe()
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := strings.ReplaceAll(indexHTML, "{{ICON}}", s.icons[0].Manifest.ImageID)
	page = strings.ReplaceAll(page, "{{SEED}}", strconv.FormatInt(s.seed, 10))
	page = strings.ReplaceAll(page, "{{FLOOD}}", fmt.Sprintf("%g", s.base.FloodThreshold))
	page = strings.ReplaceAll(page, "{{MERGE}}", fmt.Sprintf("%g", s.base.MergeCostLimit))
	page = strings.ReplaceAll(page, "{{LATENCY}}", strconv.Itoa(s.icons[0].Manifest.PreviewLatencyMS))
	_, _ = w.Write([]byte(page))
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"latest": s.latest, "icons": manifests(s.icons)})
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	icon, snapshot, params, err := s.requestParameters(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, core.AsAppError(err))
		return
	}
	preview, err := pipeline.Preview(icon, snapshot)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, core.AsAppError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"icon": icon.Manifest.ImageID, "snapshot": snapshot, "params": params,
		"downsample": preview.Downsample, "flood_samples": preview.FloodSamples,
		"equivalent_samples": preview.EquivalentSamples, "elapsed_ms": float64(preview.Elapsed.Microseconds()) / 1000,
		"within_latency": preview.WithinLatency, "rationale": preview.Rationale,
		"region_count": len(preview.Components), "components": preview.Components,
		"mask_hash": preview.MaskHash, "placeholder": preview.Placeholder,
	})
}

func (s *Server) handleExact(w http.ResponseWriter, r *http.Request) {
	icon, snapshot, _, err := s.requestParameters(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, core.AsAppError(err))
		return
	}
	record, err := runner.Run(context.Background(), s.store, icon, snapshot, runner.Options{})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, core.AsAppError(err))
		return
	}
	s.mu.Lock()
	s.latest[icon.Manifest.ImageID] = record
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) requestParameters(r *http.Request) (core.Icon, core.Snapshot, core.SegmentParams, error) {
	query := r.URL.Query()
	if err := r.ParseForm(); err != nil {
		return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("form", "valid form", err.Error())
	}
	for key, values := range r.Form {
		if len(values) > 0 {
			query.Set(key, values[len(values)-1])
		}
	}
	iconIndex := 0
	if raw := query.Get("icon"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("icon", "[0,icon_count)", "icon index is not an integer")
		}
		iconIndex = parsed
	}
	if iconIndex < 0 || iconIndex >= len(s.icons) {
		return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("icon",
			fmt.Sprintf("[0,%d]", len(s.icons)-1), "icon index is outside the legal interval")
	}
	params := s.base
	var err error
	if raw := query.Get("flood"); raw != "" {
		params.FloodThreshold, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("flood_threshold", "[0,2048]", "invalid number")
		}
	}
	if raw := query.Get("merge_cost"); raw != "" {
		params.MergeCostLimit, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("merge_cost_limit", "[0,4096]", "invalid number")
		}
	}
	if raw := query.Get("merge_area"); raw != "" {
		params.MergeAreaDiff, err = strconv.Atoi(raw)
		if err != nil {
			return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("merge_area_diff_limit", "[0,100000000]", "invalid integer")
		}
	}
	if raw := query.Get("min_region"); raw != "" {
		params.MinRegionSize, err = strconv.Atoi(raw)
		if err != nil {
			return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("min_region_size", "[1,1000000]", "invalid integer")
		}
	}
	if raw := query.Get("cleanup"); raw != "" {
		params.CleanupRadius, err = strconv.Atoi(raw)
		if err != nil {
			return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("cleanup_radius", "[0,8]", "invalid integer")
		}
	}
	if raw := query.Get("gradient"); raw != "" {
		params.GradientOperator = raw
	}
	if raw := query.Get("smooth"); raw != "" {
		params.SmoothKernel, err = strconv.Atoi(raw)
		if err != nil {
			return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, core.ParaError("smooth_kernel", "{1,3,5}", "invalid integer")
		}
	}
	icon := s.icons[iconIndex]
	snapshot, err := core.NewSnapshot(s.seed, icon.Manifest.ImageID, params)
	if err != nil {
		return core.Icon{}, core.Snapshot{}, core.SegmentParams{}, err
	}
	return icon, snapshot, params, nil
}

func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	index, err := iconQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if index < 0 || index >= len(s.icons) {
		http.Error(w, "icon index out of range", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, s.icons[index].Image)
}

func (s *Server) handleMask(w http.ResponseWriter, r *http.Request) {
	index, err := iconQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if index < 0 || index >= len(s.icons) {
		http.Error(w, "icon index out of range", http.StatusBadRequest)
		return
	}
	icon := s.icons[index]
	kind := r.URL.Query().Get("kind")
	var mask []byte
	switch kind {
	case "truth":
		mask = icon.Manifest.ReferenceMask
	case "preview", "exact":
		s.mu.RLock()
		record := s.latest[icon.Manifest.ImageID]
		s.mu.RUnlock()
		if record == nil {
			mask = make([]byte, icon.Manifest.Width*icon.Manifest.Height)
		} else if kind == "preview" {
			mask = record.PreviewMask
		} else {
			mask = record.ExactMask
		}
	default:
		http.Error(w, "kind must be preview, exact, or truth", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, maskImage(mask, icon.Manifest.Width, icon.Manifest.Height))
}

func iconQuery(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("icon")
	if raw == "" {
		return 0, nil
	}
	index, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return index, nil
}

func maskImage(mask []byte, w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i, value := range mask {
		if i >= len(img.Pix) {
			break
		}
		if value != 0 {
			img.Pix[i] = 255
		}
	}
	return img
}

func manifests(icons []core.Icon) []core.TruthManifest {
	out := make([]core.TruthManifest, 0, len(icons))
	for _, icon := range icons {
		manifest := icon.Manifest
		manifest.ReferenceMask = nil
		out = append(out, manifest)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}
