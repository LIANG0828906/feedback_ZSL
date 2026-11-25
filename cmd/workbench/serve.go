package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"

	"strokeforge/internal/engine"
	"strokeforge/internal/generator"
	"strokeforge/internal/model"
	"strokeforge/internal/service"
)

//go:embed all:webroot
var webroot embed.FS

func runServe(args []string) {
	fs2 := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs2.String("addr", "127.0.0.1:8787", "listen address")
	seed := fs2.Int64("seed", generator.DefaultSeed, "deterministic seed")
	_ = fs2.Parse(args)

	svc, err := service.New(*seed, engine.DefaultPoolConfig(), "artifacts/checkpoint-serve")
	must(err)
	_, err = svc.FullRender()
	must(err)

	mux := http.NewServeMux()
	static, err := fs.Sub(webroot, "webroot")
	must(err)
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/initial", func(w http.ResponseWriter, r *http.Request) {
		snap, err := svc.Snapshot()
		writeJSON(w, snap, err)
	})
	mux.HandleFunc("/api/diagnose", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind      string `json:"kind"`
			GlyphID   string `json:"glyph_id"`
			SegmentID int    `json:"segment_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, nil, err)
			return
		}
		err := svc.Diagnose(req.Kind, req.GlyphID, req.SegmentID)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()}, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"}, nil)
	})
	mux.HandleFunc("/api/params", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params model.StrokeParams `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, nil, err)
			return
		}
		entries, _, diff, err := svc.Incremental(req.Params)
		writeJSON(w, map[string]any{"invalidation": entries, "compare": diff}, err)
	})

	fmt.Println("StrokeForge workbench: http://" + *addr)
	must(http.ListenAndServe(*addr, mux))
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
