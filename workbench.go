package lightprobe

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var staticFiles embed.FS

type InvalidationSets struct {
	Radiance   []Invalidation `json:"radiance"`
	Resolution []Invalidation `json:"resolution"`
	Query      []Invalidation `json:"query"`
}

type Workbench struct {
	Acceptance     *AcceptanceReport `json:"acceptance"`
	Invalidations  InvalidationSets  `json:"invalidations"`
	QueryDiff      QueryDiff         `json:"query_diff"`
	DemoProbeID    string            `json:"demo_probe_id"`
	DemoResolution string            `json:"demo_resolution"`
}

func NewWorkbench(workDir string) (*Workbench, error) {
	report, err := RunAcceptance(workDir)
	if err != nil {
		return nil, err
	}
	graph := report.Full.Graph
	probeID := "probe-room-b-020101"
	resolutionVersion := "res-c-9x5x5-v2-demo"

	oldQueries := append([]Query(nil), report.Scene.Queries...)
	newQueries := append([]Query(nil), oldQueries...)
	changedIndex := -1
	for i, q := range newQueries {
		if q.ID == "q-08-04" {
			changedIndex = i
			break
		}
	}
	if changedIndex >= 0 {
		newQueries[changedIndex].Position.X += 0.18
	} else if len(newQueries) > 50 {
		newQueries[50].Position.X += 0.18
	}
	diff := DiffQueryTrajectory(oldQueries, newQueries)

	return &Workbench{
		Acceptance:     report,
		DemoProbeID:    probeID,
		DemoResolution: resolutionVersion,
		QueryDiff:      diff,
		Invalidations: InvalidationSets{
			Radiance:   graph.InvalidateProbeRadiance(probeID),
			Resolution: graph.InvalidateRoomResolution("room-c", resolutionVersion),
			Query:      graph.InvalidateQueryDiff(diff),
		},
	}, nil
}

func WorkbenchHandler(wb *Workbench) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(wb); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	staticSub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticSub)))
	return mux
}

func ServeWorkbench(addr, workDir string) error {
	wb, err := NewWorkbench(workDir)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: addr, Handler: WorkbenchHandler(wb)}
	return server.ListenAndServe()
}
