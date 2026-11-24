package web

import (
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"strconv"

	"maskpipe/internal/model"
	"maskpipe/internal/pipeline"
	"maskpipe/internal/sample"
)

type Config struct {
	Addr      string
	SampleDir string
	DBPath    string
	SceneID   string
	Loaded    *sample.LoadedScene
}

func Serve(ctx context.Context, cfg Config) error {
	manager, err := NewManager(cfg)
	if err != nil {
		return err
	}
	defer manager.Close()
	_ = manager.StartRun(pipeline.ModeResume)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, manager.State(r.Context()))
	})
	mux.HandleFunc("/api/start", func(w http.ResponseWriter, r *http.Request) {
		mode := pipeline.RunMode(r.URL.Query().Get("mode"))
		if mode == "" {
			mode = pipeline.ModeFull
		}
		params, err := paramsFromRequest(r, manager.loaded.Scene.Parameters)
		if err != nil {
			writeAction(w, err)
			return
		}
		err = manager.StartRunWithParams(mode, &params)
		writeAction(w, err)
	})
	mux.HandleFunc("/api/preview", func(w http.ResponseWriter, r *http.Request) {
		params, err := paramsFromRequest(r, manager.loaded.Scene.Parameters)
		if err != nil {
			writeAction(w, err)
			return
		}
		preview, err := manager.PreviewWithParams(r.Context(), params, r.URL.Query().Get("compare") == "1")
		if err != nil {
			writeAction(w, err)
			return
		}
		writeJSONResponse(w, preview)
	})
	mux.HandleFunc("/preview.png", func(w http.ResponseWriter, r *http.Request) {
		params, err := paramsFromRequest(r, manager.loaded.Scene.Parameters)
		if err != nil {
			writeAction(w, err)
			return
		}
		preview, err := manager.PreviewWithParams(r.Context(), params, false)
		if err != nil {
			writeAction(w, err)
			return
		}
		w.Header().Set("Content-Disposition", `inline; filename="preview.png"`)
		serveMaskPNG(w, preview.Mask, manager.loaded.Scene.Width, manager.loaded.Scene.Height)
	})
	mux.HandleFunc("/api/pause", func(w http.ResponseWriter, r *http.Request) {
		manager.Pause()
		writeAction(w, nil)
	})
	mux.HandleFunc("/api/resume-control", func(w http.ResponseWriter, r *http.Request) {
		manager.Resume()
		writeAction(w, nil)
	})
	mux.HandleFunc("/api/cancel", func(w http.ResponseWriter, r *http.Request) {
		manager.Cancel()
		writeAction(w, nil)
	})
	mux.HandleFunc("/api/retry", func(w http.ResponseWriter, r *http.Request) {
		x, _ := strconv.Atoi(r.URL.Query().Get("x"))
		y, _ := strconv.Atoi(r.URL.Query().Get("y"))
		stage := model.Stage(r.URL.Query().Get("stage"))
		if stage == "" {
			stage = model.StageGrow
		}
		writeAction(w, manager.RetryChunk(x, y, stage))
	})
	mux.HandleFunc("/api/chunk", func(w http.ResponseWriter, r *http.Request) {
		x, _ := strconv.Atoi(r.URL.Query().Get("x"))
		y, _ := strconv.Atoi(r.URL.Query().Get("y"))
		out := map[string]any{}
		for _, stage := range []model.Stage{model.StageGrow, model.StageClean, model.StageStats} {
			rec, err := manager.StageRecord(r.Context(), x, y, stage)
			if err == nil {
				out[string(stage)] = rec
			}
		}
		writeJSONResponse(w, out)
	})
	mux.HandleFunc("/artifact", func(w http.ResponseWriter, r *http.Request) {
		serveArtifact(w, r, manager)
	})
	mux.HandleFunc("/image/original", func(w http.ResponseWriter, r *http.Request) {
		servePNG(w, manager.loaded.Image)
	})
	mux.HandleFunc("/image/truth", func(w http.ResponseWriter, r *http.Request) {
		servePNG(w, manager.loaded.TruthImage)
	})
	mux.HandleFunc("/image/final", func(w http.ResponseWriter, r *http.Request) {
		result := manager.currentResult()
		if result == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		serveMaskPNG(w, result.Mask, manager.loaded.Scene.Width, manager.loaded.Scene.Height)
	})
	mux.HandleFunc("/image/overlay", func(w http.ResponseWriter, r *http.Request) {
		result := manager.currentResult()
		if result == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		img := overlayImage(manager.loaded.Image, result.Mask)
		servePNG(w, img)
	})
	mux.HandleFunc("/api/export/mask.png", func(w http.ResponseWriter, r *http.Request) {
		result := manager.currentResult()
		if result == nil {
			http.Error(w, "no result", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="mask.png"`)
		serveMaskPNG(w, result.Mask, manager.loaded.Scene.Width, manager.loaded.Scene.Height)
	})
	mux.HandleFunc("/api/export/report.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="diagnostic-report.json"`)
		writeJSONResponse(w, manager.State(r.Context()))
	})
	mux.HandleFunc("/api/threeway", func(w http.ResponseWriter, r *http.Request) {
		res, err := pipeline.RunThreeWay(r.Context(), manager.loaded, manager.loaded.Scene.Parameters, "reports/web-threeway-"+manager.loaded.Scene.ID)
		if err != nil {
			writeAction(w, err)
			return
		}
		writeJSONResponse(w, res.Report)
	})

	srv := &http.Server{Addr: cfg.Addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}

func (m *Manager) currentResult() *pipeline.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.result
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeAction(w http.ResponseWriter, err error) {
	if err == nil {
		writeJSONResponse(w, map[string]any{"ok": true})
		return
	}
	if pe, ok := model.AsError(err); ok {
		w.WriteHeader(http.StatusBadRequest)
		writeJSONResponse(w, pe)
		return
	}
	w.WriteHeader(http.StatusInternalServerError)
	writeJSONResponse(w, map[string]any{"ok": false, "error": err.Error()})
}

func servePNG(w http.ResponseWriter, img image.Image) {
	w.Header().Set("Content-Type", "image/png")
	_ = png.Encode(w, img)
}

func serveMaskPNG(w http.ResponseWriter, mask []byte, width, height int) {
	img := image.NewGray(image.Rect(0, 0, width, height))
	for i, v := range mask {
		if v != 0 {
			img.Pix[i] = 255
		}
	}
	servePNG(w, img)
}
