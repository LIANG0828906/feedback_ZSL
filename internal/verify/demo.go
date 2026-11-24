// Package verify runs the reproducible acceptance matrix used by the CLI.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"hdrproof/internal/apperr"
	"hdrproof/internal/audit"
	"hdrproof/internal/compare"
	"hdrproof/internal/engine"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
)

type RunSummary struct {
	Mode           engine.Mode   `json:"mode"`
	DBPath         string        `json:"db_path"`
	Frames         int           `json:"frames"`
	Records        int           `json:"records"`
	Progress       interface{}   `json:"progress"`
	SnapshotReused bool          `json:"snapshot_reused"`
	SnapshotReason string        `json:"snapshot_reason"`
	Cancelled      bool          `json:"cancelled"`
	EventCount     int           `json:"event_count"`
	BufferBlocks   int           `json:"buffer_blocks,omitempty"`
	Errors         []apperr.Info `json:"errors,omitempty"`
}

type ComparisonSummary struct {
	Left   string `json:"left"`
	Right  string `json:"right"`
	Equal  bool   `json:"equal"`
	Frames int    `json:"frames"`
}

type InjectionSummary struct {
	Name         string `json:"name"`
	ExpectedCode string `json:"expected_code"`
	ActualCode   string `json:"actual_code"`
	ControlledOK bool   `json:"controlled_ok"`
	Detail       string `json:"detail"`
	RetryPassed  bool   `json:"retry_passed,omitempty"`
}

type Report struct {
	StartedAt         string                 `json:"started_at"`
	FinishedAt        string                 `json:"finished_at"`
	Environment       map[string]string      `json:"environment"`
	Samples           map[string]interface{} `json:"samples"`
	Realtime          RunSummary             `json:"realtime"`
	ResumeInterrupted RunSummary             `json:"resume_interrupted"`
	ResumeCompleted   RunSummary             `json:"resume_completed"`
	Full              RunSummary             `json:"full"`
	Comparisons       []ComparisonSummary    `json:"comparisons"`
	Audit             *audit.Result          `json:"audit"`
	Injections        []InjectionSummary     `json:"injections"`
	BufferBlocks      int                    `json:"buffer_blocks"`
	Outcome           string                 `json:"outcome"`
}

type Options struct {
	Root    string
	LogPath string
	Workers int
	Buffer  int
}

func RunAcceptance(ctx context.Context, opts Options) (*Report, error) {
	if opts.Root == "" {
		opts.Root = filepath.Join("artifacts", "demo")
	}
	if opts.LogPath == "" {
		opts.LogPath = filepath.Join(opts.Root, "demo.log")
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Buffer <= 0 {
		opts.Buffer = 2
	}
	if err := os.MkdirAll(opts.Root, 0o755); err != nil {
		return nil, err
	}
	logFile, err := os.Create(opts.LogPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags|log.Lmicroseconds)
	report := &Report{
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Environment: map[string]string{
			"go_version": runtime.Version(),
			"goos":       runtime.GOOS, "goarch": runtime.GOARCH,
			"max_workers":             fmt.Sprint(opts.Workers),
			"frame_buffer_per_worker": fmt.Sprint(opts.Buffer),
		},
	}
	samplesDir := filepath.Join(opts.Root, "samples")
	gen, err := sample.Generate(sample.GenerateOptions{Dir: samplesDir, Seed: 42})
	if err != nil {
		return report, err
	}
	logger.Printf("generated samples dir=%s routes=%d frames=%d fingerprint=%s",
		gen.Dir, gen.RouteCount, gen.FrameCount, gen.TruthFingerprint)
	report.Samples = map[string]interface{}{
		"dir": samplesDir, "manifest_path": gen.ManifestPath, "truth_path": gen.TruthPath,
		"truth_fingerprint": gen.TruthFingerprint, "route_count": gen.RouteCount,
		"frame_count": gen.FrameCount,
	}
	realtimeDB := filepath.Join(opts.Root, "realtime.db")
	resumeDB := filepath.Join(opts.Root, "resume.db")
	fullDB := filepath.Join(opts.Root, "full.db")
	for _, db := range []string{realtimeDB, resumeDB, fullDB} {
		removeDB(db)
	}
	baseCfg := engine.Config{
		DatasetDir: samplesDir, Params: model.DefaultParams(), MaxWorkers: opts.Workers,
		FrameBufferPerWorker: opts.Buffer,
		EventSink: func(ev engine.Event) {
			logger.Printf("%s route=%s segment=%d frame=%d reason=%s %s",
				ev.Kind, ev.RouteID, ev.Segment, ev.Frame, ev.Reason, ev.Message)
		},
	}
	run := func(mode engine.Mode, db string, cancelAfter int, autoRetry bool) (*engine.RunResult, error) {
		cfg := baseCfg
		cfg.Mode, cfg.DBPath, cfg.CancelAfterCommits, cfg.AutoRetryInjectedFailure = mode, db, cancelAfter, autoRetry
		return engine.Execute(ctx, cfg)
	}
	rtResult, err := run(engine.ModeRealtime, realtimeDB, 0, false)
	if err != nil {
		return report, fmt.Errorf("realtime run: %w", err)
	}
	report.Realtime = summarizeRun(rtResult)
	logger.Printf("realtime complete frames=%d records=%d progress=%+v", rtResult.Frames, rtResult.Records, rtResult.Progress)

	resumeInterrupted, err := run(engine.ModeResume, resumeDB, 5, false)
	if err != nil {
		return report, fmt.Errorf("interrupted resume: %w", err)
	}
	report.ResumeInterrupted = summarizeRun(resumeInterrupted)
	logger.Printf("resume interrupted frames=%d records=%d progress=%+v cancelled=%v",
		resumeInterrupted.Frames, resumeInterrupted.Records, resumeInterrupted.Progress, resumeInterrupted.Cancelled)
	resumeCompleted, err := run(engine.ModeResume, resumeDB, 0, false)
	if err != nil {
		return report, fmt.Errorf("resume completion: %w", err)
	}
	report.ResumeCompleted = summarizeRun(resumeCompleted)
	logger.Printf("resume completed frames=%d records=%d progress=%+v reused=%v",
		resumeCompleted.Frames, resumeCompleted.Records, resumeCompleted.Progress, resumeCompleted.SnapshotReused)

	fullResult, err := run(engine.ModeFull, fullDB, 0, false)
	if err != nil {
		return report, fmt.Errorf("full run: %w", err)
	}
	report.Full = summarizeRun(fullResult)
	logger.Printf("full complete frames=%d records=%d progress=%+v", fullResult.Frames, fullResult.Records, fullResult.Progress)
	report.BufferBlocks = rtResult.BufferBlocks + report.ResumeInterrupted.BufferBlocks +
		report.ResumeCompleted.BufferBlocks + fullResult.BufferBlocks

	pairs := [][3]string{
		{"realtime_vs_full", realtimeDB, fullDB},
		{"resume_vs_full", resumeDB, fullDB},
		{"realtime_vs_resume", realtimeDB, resumeDB},
	}
	for _, pair := range pairs {
		cmp, cmpErr := compare.Snapshots(ctx, pair[1], pair[2])
		summary := ComparisonSummary{Left: pair[0], Right: "snapshot", Equal: cmpErr == nil && cmp.Equal}
		if cmp != nil {
			summary.Frames = cmp.FramesCompared
		}
		report.Comparisons = append(report.Comparisons, summary)
		if cmpErr != nil {
			return report, fmt.Errorf("%s: %w", pair[0], cmpErr)
		}
	}
	auditResult, err := audit.Audit(ctx, samplesDir, fullDB)
	if err != nil {
		return report, fmt.Errorf("independent audit: %w", err)
	}
	report.Audit = auditResult
	logger.Printf("audit passed=%v routes=%d segments=%d frames=%d",
		auditResult.Passed, auditResult.RoutesAudited, auditResult.SegmentsAudited, auditResult.FramesAudited)

	injectionDir := filepath.Join(samplesDir, "injections")
	entries, err := os.ReadDir(injectionDir)
	if err != nil {
		return report, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		injectionPath := filepath.Join(injectionDir, name)
		dbPath := filepath.Join(opts.Root, "injection-"+strings.TrimSuffix(name, ".json")+".db")
		removeDB(dbPath)
		cfg := baseCfg
		cfg.Mode, cfg.DBPath, cfg.InjectionPath = engine.ModeFull, dbPath, injectionPath
		cfg.AutoRetryInjectedFailure = true
		result, runErr := engine.Execute(ctx, cfg)
		code := string(apperr.CodeOf(runErr))
		expected := expectedInjectionCode(name)
		summary := InjectionSummary{
			Name: strings.TrimSuffix(name, ".json"), ExpectedCode: expected,
			ActualCode: code, ControlledOK: code == expected, Detail: errorString(runErr),
		}
		if result != nil && strings.Contains(name, "segment_failure") {
			summary.RetryPassed = result.Progress.SegmentsCommitted == result.Progress.SegmentsTotal &&
				result.Progress.RoutesCommitted == result.Progress.RoutesTotal
		}
		report.Injections = append(report.Injections, summary)
		logger.Printf("injection name=%s expected=%s actual=%s retry=%v detail=%s",
			summary.Name, summary.ExpectedCode, summary.ActualCode, summary.RetryPassed, summary.Detail)
		if !summary.ControlledOK {
			return report, fmt.Errorf("injection %s expected %s received %s: %s",
				summary.Name, expected, code, summary.Detail)
		}
		if strings.Contains(name, "segment_failure") && !summary.RetryPassed {
			return report, fmt.Errorf("segment failure retry did not complete")
		}
	}
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	report.Outcome = "pass"
	reportPath := filepath.Join(opts.Root, "report.json")
	if err := writeJSON(reportPath, report); err != nil {
		return report, err
	}
	logger.Printf("demo complete report=%s buffer_blocks=%d", reportPath, report.BufferBlocks)
	return report, nil
}

func summarizeRun(result *engine.RunResult) RunSummary {
	if result == nil {
		return RunSummary{}
	}
	return RunSummary{
		Mode: result.Mode, DBPath: result.DBPath, Frames: result.Frames, Records: result.Records,
		Progress: result.Progress, SnapshotReused: result.SnapshotReused,
		SnapshotReason: result.SnapshotReason, Cancelled: result.Cancelled,
		EventCount: result.EventCount, BufferBlocks: result.BufferBlocks, Errors: result.Errors,
	}
}

func expectedInjectionCode(name string) string {
	switch strings.TrimSuffix(name, ".json") {
	case "bad_frame":
		return "E_FRAME"
	case "meta_conflict":
		return "E_META"
	case "parameter_out_of_range":
		return "E_PARA"
	case "segment_failure":
		return "E_SEG"
	default:
		return "E_META"
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func removeDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
}

func writeJSON(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
