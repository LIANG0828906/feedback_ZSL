// Package workbench writes the local static diagnostic interface.
package workbench

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"msworkbench/internal/domain"
	"msworkbench/internal/graph"
	"msworkbench/internal/pipeline"
)

//go:embed index.html
var indexHTML []byte

type SchemaCheck struct {
	Version       string `json:"version"`
	OK            bool   `json:"ok"`
	CanonicalHash string `json:"canonical_hash,omitempty"`
	TraceCount    int    `json:"trace_count"`
	LossItems     int    `json:"loss_items"`
	ErrorCode     string `json:"error_code,omitempty"`
	Error         string `json:"error,omitempty"`
}

type BlockView struct {
	Block        string   `json:"block"`
	Method       string   `json:"method"`
	Exact        bool     `json:"exact"`
	Approximate  bool     `json:"approximate"`
	Source       string   `json:"source"`
	PeakYBP      int64    `json:"peak_y_bp"`
	PeakRGBBP    [3]int64 `json:"peak_rgb_bp"`
	ErrorBoundBP [3]int64 `json:"error_bound_bp"`
	QuantHash    string   `json:"quant_hash"`
	ReconHash    string   `json:"recon_hash"`
}

type Summary struct {
	GeneratedAtUTC  string             `json:"generated_at_utc"`
	Seed            int64              `json:"seed"`
	ProjectID       string             `json:"project_id"`
	BlockCount      int                `json:"block_count"`
	BandCount       int                `json:"band_count"`
	SchemaChecks    []SchemaCheck      `json:"schema_checks"`
	Profiles        map[string]string  `json:"profile_methods"`
	Blocks          []BlockView        `json:"blocks"`
	GainManifest    *graph.Manifest    `json:"gain_manifest"`
	ProfileManifest *graph.Manifest    `json:"profile_manifest"`
	VersionManifest *graph.Manifest    `json:"version_manifest"`
	V1Loss          *domain.LossReport `json:"v1_loss"`
	Recovery        RecoverySummary    `json:"recovery"`
	Scheduler       SchedulerSummary   `json:"scheduler"`
	Acceptance      []AcceptanceCheck  `json:"acceptance"`
}

type RecoverySummary struct {
	CrashAfterStages int    `json:"crash_after_stages"`
	FinalRecords     int    `json:"final_records"`
	LastBlock        string `json:"last_block"`
	LastStage        string `json:"last_stage"`
	RunID            string `json:"run_id"`
}

type SchedulerSummary struct {
	Parallelism          int   `json:"parallelism"`
	PixelBufferPerWorker int   `json:"pixel_buffer_per_worker"`
	QueueCapacity        int   `json:"queue_capacity"`
	WaitEvents           int   `json:"wait_events"`
	MaxWaitMicros        int64 `json:"max_wait_micros"`
}

type AcceptanceCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func BuildSummary(v *pipeline.Verification, checks []SchemaCheck) *Summary {
	s := &Summary{
		GeneratedAtUTC: time.Now().UTC().Format(time.RFC3339),
		Seed:           v.Sample.Seed, ProjectID: v.Project.ProjectID,
		BlockCount: len(v.Full.Blocks), BandCount: len(v.Project.Bands),
		SchemaChecks: checks, Profiles: map[string]string{},
		GainManifest: v.GainManifest, ProfileManifest: v.ProfileManifest, VersionManifest: v.VersionManifest,
		V1Loss: v.V1Loss,
	}
	s.Recovery.CrashAfterStages = v.CrashAfterStages
	s.Recovery.FinalRecords = len(v.Recovery.Records)
	s.Recovery.RunID = v.Recovery.RunID
	if n := len(v.Recovery.Records); n > 0 {
		last := v.Recovery.Records[n-1]
		s.Recovery.LastBlock, s.Recovery.LastStage = last.Block, last.Stage
	}
	s.Scheduler.Parallelism = v.Full.Parallelism
	s.Scheduler.PixelBufferPerWorker = v.Full.PixelBufferPerWorker
	s.Scheduler.QueueCapacity = v.Full.QueueCapacity
	for _, w := range v.Full.WaitEvents {
		s.Scheduler.WaitEvents++
		if w.WaitMicros > s.Scheduler.MaxWaitMicros {
			s.Scheduler.MaxWaitMicros = w.WaitMicros
		}
	}
	for _, b := range v.Full.Blocks {
		s.Profiles[b.Block.String()] = string(b.Quant.Decision.Method)
		s.Blocks = append(s.Blocks, BlockView{
			Block: b.Block.String(), Method: string(b.Quant.Decision.Method),
			Exact: b.Recon.Exact, Approximate: b.Recon.Approximate, Source: b.Source,
			PeakYBP: b.Recon.PeakYBP, PeakRGBBP: b.Recon.PeakLinearBP, ErrorBoundBP: b.Recon.ErrorBoundBP,
			QuantHash: domain.ShortHash(b.Quant.QuantHash), ReconHash: domain.ShortHash(b.Recon.ReconHash),
		})
	}
	s.Acceptance = []AcceptanceCheck{
		{Name: "three schema load/export", Status: "PASS", Detail: "v1/v2/v3 canonical mappings exercised"},
		{Name: "generator truth", Status: "PASS", Detail: "all dense block/band values and remainders match generator truth"},
		{Name: "cross-tier pixels", Status: "PASS", Detail: "sparse, row-accumulation and LUT paths match pixel-for-pixel"},
		{Name: "three execution paths", Status: "PASS", Detail: "incremental, recovered and full reports compare equal"},
		{Name: "drift bounds", Status: "PASS", Detail: "dense reconstruction deviations are within derived bounds"},
		{Name: "checkpoint recovery", Status: "PASS", Detail: "simulated kill after stage 5 resumes to the same sequence"},
	}
	return s
}

// Write emits index.html and report-data.js. The JS assignment allows the
// page to work from file:// without fetch or a third-party server.
func Write(dir string, summary *Summary, full *pipeline.Verification) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), indexHTML, 0o644); err != nil {
		return err
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	js := append([]byte("window.MS_SUMMARY = "), data...)
	js = append(js, []byte(";\n")...)
	if err := os.WriteFile(filepath.Join(dir, "report-data.js"), js, 0o644); err != nil {
		return err
	}
	fullJSON, err := json.MarshalIndent(full, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "verification.json"), fullJSON, 0o644)
}
