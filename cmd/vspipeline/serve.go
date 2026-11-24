package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"virtualstage/app"
	"virtualstage/pipeline"
	"virtualstage/webui"
)

func commandServe(ctx context.Context, args []string) error {
	fs, work, workers, maxBytes := commonFlags("serve")
	addr := fs.String("addr", "127.0.0.1:8787", "local HTTP address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	service, err := app.OpenService(app.Config{WorkDir: *work, Workers: *workers, MaxFrameBytes: *maxBytes})
	if err != nil {
		return err
	}
	defer service.Close()

	mux := http.NewServeMux()
	mux.Handle("/", webui.Handler())
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeAPI(w, http.StatusOK, map[string]any{"ok": true, "snapshot": service.Snapshot()})
	})
	mux.HandleFunc("/api/full", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, pipeline.CodeParameter, "POST required")
			return
		}
		snapshot, err := service.RunFull(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, errorCode(err), err.Error())
			return
		}
		writeAPI(w, http.StatusOK, map[string]any{"ok": true, "snapshot": snapshot})
	})
	mux.HandleFunc("/api/edit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, pipeline.CodeParameter, "POST required")
			return
		}
		var edit pipeline.EditRequest
		if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
			writeAPIError(w, http.StatusBadRequest, pipeline.CodeDecode, "decode edit: "+err.Error())
			return
		}
		invalidation, err := service.ApplyIncremental(r.Context(), edit)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, errorCode(err), err.Error())
			return
		}
		writeAPI(w, http.StatusOK, map[string]any{
			"ok": true, "invalidation": invalidation, "snapshot": service.Snapshot(),
		})
	})
	mux.HandleFunc("/api/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAPIError(w, http.StatusMethodNotAllowed, pipeline.CodeParameter, "POST required")
			return
		}
		version, err := strconv.Atoi(r.URL.Query().Get("version"))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, pipeline.CodeParameter, "version query parameter is required")
			return
		}
		verification, err := service.ExportAndVerify(r.Context(), version)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, errorCode(err), err.Error())
			return
		}
		writeAPI(w, http.StatusOK, map[string]any{"ok": true, "verification": verification})
	})
	mux.HandleFunc("/api/validate", func(w http.ResponseWriter, r *http.Request) {
		validation, err := service.ValidateTruth(r.Context())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, errorCode(err), err.Error())
			return
		}
		writeAPI(w, http.StatusOK, map[string]any{"ok": validation.OK, "validation": validation})
	})
	mux.HandleFunc("/api/heatmap", func(w http.ResponseWriter, r *http.Request) {
		window, err := queryInt(r, "window", -1)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, pipeline.CodeParameter, err.Error())
			return
		}
		frame, err := queryInt(r, "frame", -1)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, pipeline.CodeParameter, err.Error())
			return
		}
		cx, _ := queryInt(r, "cx", 32)
		cy, _ := queryInt(r, "cy", 18)
		heatmap, err := service.Heatmap(window, frame, cx, cy)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, errorCode(err), err.Error())
			return
		}
		writeAPI(w, http.StatusOK, map[string]any{"ok": true, "heatmap": heatmap})
	})
	mux.HandleFunc("/api/frame", func(w http.ResponseWriter, r *http.Request) {
		window, err := queryInt(r, "window", -1)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, pipeline.CodeParameter, err.Error())
			return
		}
		frame, err := queryInt(r, "frame", -1)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, pipeline.CodeParameter, err.Error())
			return
		}
		data, err := service.FramePNG(window, frame)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, errorCode(err), err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/api/window", func(w http.ResponseWriter, r *http.Request) {
		window, err := queryInt(r, "window", -1)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, pipeline.CodeParameter, err.Error())
			return
		}
		result, err := service.Engine().ReadWindow(window)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, errorCode(err), err.Error())
			return
		}
		writeAPI(w, http.StatusOK, map[string]any{
			"ok": true, "window": window, "start_frame": result.StartFrame, "end_frame": result.EndFrame,
			"content_hash": result.ContentHash, "frame_summaries": result.FrameSummaries,
		})
	})

	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Printf("{\"ok\":true,\"url\":\"http://%s\",\"work\":%q}\n", *addr, *work)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func queryInt(r *http.Request, key string, fallback int) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		if fallback >= 0 {
			return fallback, nil
		}
		return 0, fmt.Errorf("%s query parameter is required", key)
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return value, nil
}

func writeAPI(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write API response: %v", err)
	}
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeAPI(w, status, map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message}})
}

func errorCode(err error) string {
	var pipelineErr *pipeline.Error
	if errors.As(err, &pipelineErr) {
		return pipelineErr.Code
	}
	switch {
	case errors.Is(err, context.Canceled):
		return pipeline.CodeState
	default:
		return pipeline.CodeParameter
	}
}
