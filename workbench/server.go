package workbench

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"iconflow/checkpoint"
	"iconflow/errs"
	"iconflow/graph"
	"iconflow/model"
	"iconflow/pipeline"
	"iconflow/schemax"
	"iconflow/segment"
)

type Server struct {
	Project        *model.Project
	CheckpointPath string
	mux            *http.ServeMux
}

func NewServer(project *model.Project, checkpointPath string) *Server {
	s := &Server{Project: project, CheckpointPath: checkpointPath, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/overview", s.handleOverview)
	s.mux.HandleFunc("/api/consistency", s.handleConsistency)
	s.mux.HandleFunc("/api/invalidate", s.handleInvalidate)
	s.mux.HandleFunc("/api/checkpoint", s.handleCheckpoint)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err *errs.Error) {
	status := http.StatusInternalServerError
	switch err.Code {
	case errs.ESchema:
		status = http.StatusBadRequest
	case errs.EState, errs.EDiff:
		status = http.StatusConflict
	case errs.EBudget:
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": err})
}

type VersionOverview struct {
	Version        string `json:"version"`
	Loaded         bool   `json:"loaded"`
	Images         int    `json:"images"`
	Blocks         int    `json:"blocks"`
	UpgradeRecords int    `json:"upgrade_records"`
	Traces         int    `json:"traces"`
	LossItems      int    `json:"loss_items"`
	Error          string `json:"error,omitempty"`
}

type Overview struct {
	ProjectID      string                   `json:"project_id"`
	Seed           int64                    `json:"seed"`
	BlockCount     int                      `json:"block_count"`
	PanelCount     int                      `json:"panel_count"`
	DirtyCount     int                      `json:"dirty_count"`
	Versions       []VersionOverview        `json:"versions"`
	LegacyUpgrades []model.ConversionRecord `json:"legacy_upgrades"`
	Dirty          []model.DirtyRecord      `json:"dirty"`
	DirtyManifest  *graph.Manifest          `json:"dirty_manifest"`
}

func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	overview := Overview{
		ProjectID: s.Project.ProjectID, Seed: s.Project.Seed,
		BlockCount: len(s.Project.SortedBlockCoords()),
		PanelCount: len(s.Project.Panels), DirtyCount: len(s.Project.Dirty),
		Dirty:         s.Project.Dirty,
		DirtyManifest: graph.ImpactDirty(graph.ProjectGraph(s.Project), s.Project.Dirty),
	}
	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		entry := VersionOverview{Version: version}
		export, exportErr := schemax.Export(s.Project, version)
		if exportErr != nil {
			entry.Error = exportErr.Error()
			overview.Versions = append(overview.Versions, entry)
			continue
		}
		if export.Loss != nil {
			entry.LossItems = len(export.Loss.Items)
		}
		loaded, loadErr := schemax.LoadDocument(export.Data)
		if loadErr != nil {
			entry.Error = loadErr.Error()
		} else {
			entry.Loaded = true
			entry.Images = len(loaded.Project.Images)
			entry.Blocks = len(loaded.Project.SortedBlockCoords())
			entry.UpgradeRecords = len(loaded.UpgradeRecords)
			entry.Traces = len(loaded.Traces)
		}
		overview.Versions = append(overview.Versions, entry)
	}

	v1, _ := schemax.Export(s.Project, model.SchemaV1)
	var legacyDoc map[string]any
	_ = json.Unmarshal(v1.Data, &legacyDoc)
	delete(legacyDoc, "channel_sequence")
	delete(legacyDoc, "quant_step")
	legacyData, _ := json.Marshal(legacyDoc)
	if legacy, err := schemax.LoadDocument(legacyData); err == nil {
		overview.LegacyUpgrades = legacy.UpgradeRecords
	}
	writeJSON(w, http.StatusOK, overview)
}

type BlockSummary struct {
	Coord            string `json:"coord"`
	Threshold        int    `json:"threshold"`
	QuantileValue    int    `json:"quantile_value"`
	MaximumMagnitude int    `json:"maximum_magnitude"`
	Markers          int    `json:"markers"`
	Foreground       int    `json:"foreground"`
	Regions          int    `json:"regions"`
	FilledHoles      int    `json:"filled_holes"`
	RemovedArea      int    `json:"removed_area"`
	Mask             string `json:"mask"`
	Reused           bool   `json:"reused"`
	Validation       string `json:"validation,omitempty"`
}

func SummarizeRun(report *pipeline.RunReport) []BlockSummary {
	out := make([]BlockSummary, 0, len(report.Results))
	for _, result := range report.Results {
		summary := BlockSummary{
			Coord: result.Coord.Key(), Reused: result.Reused,
		}
		if result.Threshold != nil {
			summary.Threshold = result.Threshold.Threshold
			summary.QuantileValue = result.Threshold.QuantileValue
			summary.MaximumMagnitude = result.Threshold.MaximumMagnitude
		}
		for _, marked := range result.Markers {
			if marked {
				summary.Markers++
			}
		}
		if result.Stats != nil {
			summary.Foreground = result.Stats.Foreground
			summary.Regions = len(result.Stats.Components)
			summary.FilledHoles = result.Stats.FilledHoles
			summary.RemovedArea = result.Stats.RemovedArea
			summary.Mask = segment.FormatMask(result.Stats.Mask, result.Coord.Width)
		}
		if result.ValidationError != nil {
			summary.Validation = result.ValidationError.Code + ": " + result.ValidationError.Message
		}
		out = append(out, summary)
	}
	return out
}

func budgetFromRequest(r *http.Request) pipeline.Budget {
	budget := pipeline.DefaultBudget()
	if value := r.URL.Query().Get("workers"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			budget.Workers = parsed
		}
	}
	if value := r.URL.Query().Get("queue"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			budget.MaxQueuedBlocks = parsed
		}
	}
	if value := r.URL.Query().Get("buffer"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			budget.MaxPixelsPerThread = parsed
		}
	}
	return budget
}

func (s *Server) handleConsistency(w http.ResponseWriter, r *http.Request) {
	budget := budgetFromRequest(r)
	report, err := pipeline.RunConsistency(r.Context(), s.Project, pipeline.Options{Budget: budget})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": report.Passed, "diff": report.Diff,
		"realtime":          SummarizeRun(report.Realtime),
		"resume":            SummarizeRun(report.Resume),
		"full":              SummarizeRun(report.Full),
		"realtime_progress": report.Realtime.Progress,
		"resume_progress":   report.Resume.Progress,
		"full_progress":     report.Full.Progress,
		"commit_entries":    len(report.Realtime.CommitSequence),
	})
}

func editValue(field string, r *http.Request, project *model.Project, target string) any {
	switch field {
	case "channel_order":
		return []string{"B", "G", "R", "A"}
	case "quant_step":
		return 7
	case "operator_version":
		return "prewitt-v1"
	default:
		current := project.ThresholdPermyriad
		if override, ok := project.Overrides[target]; ok && override.ThresholdPermyriad != nil {
			current = *override.ThresholdPermyriad
		}
		value := r.URL.Query().Get("value")
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
		if current > 7000 {
			return 7000
		}
		return current + 500
	}
}

func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request) {
	field := r.URL.Query().Get("field")
	if field == "" {
		field = "threshold_scale"
	}
	scope := r.URL.Query().Get("scope")
	clone, cloneErr := pipeline.CloneProject(s.Project)
	if cloneErr != nil {
		writeError(w, cloneErr)
		return
	}
	coords := clone.SortedBlockCoords()
	targetIndex := 4
	if raw := r.URL.Query().Get("block_index"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 && parsed < len(coords) {
			targetIndex = parsed
		}
	}
	target := coords[targetIndex].Key()
	only := map[string]bool{}
	if scope != "global" {
		only[target] = true
	}
	manifest := graph.ProjectGraph(clone).Impact([]string{field}, only)

	ctx := context.Background()
	baseRunner, runnerErr := pipeline.NewRunner(ctx, clone, pipeline.Options{
		Mode: "invalidation-base", Budget: pipeline.DefaultBudget(),
	})
	if runnerErr != nil {
		writeError(w, runnerErr)
		return
	}
	if _, runErr := baseRunner.Run(ctx); runErr != nil {
		writeError(w, runErr)
		return
	}

	edit := pipeline.Edit{Field: field, Value: editValue(field, r, clone, target)}
	if scope != "global" {
		edit.BlockKey = target
	}
	if applyErr := pipeline.ApplyEdit(clone, edit); applyErr != nil {
		writeError(w, applyErr)
		return
	}
	afterRunner, runnerErr := pipeline.NewRunner(ctx, clone, pipeline.Options{
		Mode: "invalidation-after", Store: baseRunner.Store, Budget: pipeline.DefaultBudget(),
	})
	if runnerErr != nil {
		writeError(w, runnerErr)
		return
	}
	after, runErr := afterRunner.Run(ctx)
	if runErr != nil && runErr.Code != errs.EThr && runErr.Code != errs.ETop && runErr.Code != errs.ERegion {
		writeError(w, runErr)
		return
	}
	reused := []string{}
	recomputed := []string{}
	for _, result := range after.Results {
		if result.Reused {
			reused = append(reused, result.Coord.Key())
		} else {
			recomputed = append(recomputed, result.Coord.Key())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "edit": edit, "manifest": manifest,
		"target_block": target, "reused_blocks": reused,
		"recomputed_blocks": recomputed,
		"after_progress":    after.Progress,
		"blocks":            SummarizeRun(after),
	})
}

func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.CheckpointPath
	}
	store, err := checkpoint.OpenSQLite(path)
	if err != nil {
		fileStore, fileErr := checkpoint.OpenFileStore(path + ".json")
		if fileErr != nil {
			if domainErr, ok := errs.AsError(err); ok {
				writeError(w, domainErr)
			} else {
				writeError(w, errs.New(errs.ECheck, path, "%v", err))
			}
			return
		}
		history, _ := fileStore.StageHistory(r.Context(), s.Project.ProjectID)
		windows, _ := fileStore.WindowHistory(r.Context(), s.Project.ProjectID)
		writeJSON(w, http.StatusOK, map[string]any{
			"backend": "atomic-json-fallback", "path": path + ".json",
			"stages": len(history), "windows": len(windows),
		})
		_ = fileStore.Close()
		return
	}
	defer store.Close()
	history, hErr := store.StageHistory(r.Context(), s.Project.ProjectID)
	if hErr != nil {
		if domainErr, ok := errs.AsError(hErr); ok {
			writeError(w, domainErr)
		} else {
			writeError(w, errs.New(errs.ECheck, s.CheckpointPath, "%v", hErr))
		}
		return
	}
	windows, _ := store.WindowHistory(r.Context(), s.Project.ProjectID)
	recent := history
	if len(recent) > 30 {
		recent = recent[len(recent)-30:]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backend": "sqlite", "path": path,
		"stages": len(history), "windows": len(windows), "recent": recent,
	})
}
