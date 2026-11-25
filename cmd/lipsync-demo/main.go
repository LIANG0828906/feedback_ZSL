package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"lipsync/diagnostics"
	"lipsync/domain"
	"lipsync/generate"
	"lipsync/pipeline"
	"lipsync/schema"
	"lipsync/store"
	"lipsync/truth"
	"lipsync/verify"
)

type report struct {
	Seed          int64             `json:"seed"`
	Truth         string            `json:"truth"`
	Fingerprint   string            `json:"truth_fingerprint"`
	Projects      map[string]string `json:"projects"`
	Versions      []versionReport   `json:"versions"`
	Paths         []pathReport      `json:"paths"`
	RecoveryReuse int               `json:"recovery_reused_shards"`
	Comparison    *verify.Report    `json:"comparison"`
	Diagnostics   map[string]any    `json:"diagnostics"`
}

type versionReport struct {
	Version        string         `json:"version"`
	Database       string         `json:"database"`
	Shards         int            `json:"shards"`
	Packages       int            `json:"packages"`
	Frames         int            `json:"frames"`
	CommitSequence []int64        `json:"commit_sequence"`
	Verification   *verify.Report `json:"verification"`
}

type pathReport struct {
	Mode           pipeline.Mode  `json:"mode"`
	Database       string         `json:"database"`
	Shards         int            `json:"shards"`
	Packages       int            `json:"packages"`
	Frames         int            `json:"frames"`
	CommitSequence []int64        `json:"commit_sequence"`
	Verification   *verify.Report `json:"verification"`
}

func main() {
	var (
		outDir     = flag.String("out", "artifacts/generated", "deterministic sample output directory")
		dbDir      = flag.String("dbdir", "artifacts/checkpoints", "SQLite checkpoint directory")
		seed       = flag.Int64("seed", 42, "deterministic sample seed")
		serve      = flag.Bool("serve", false, "serve diagnostic HTTP endpoints")
		port       = flag.Int("port", 0, "diagnostic port (0 selects first free in 8000..9000)")
		reportPath = flag.String("report", "artifacts/verification-report.json", "JSON verification report path")
	)
	flag.Parse()
	if err := run(*outDir, *dbDir, *seed, *serve, *port, *reportPath); err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			b, _ := json.MarshalIndent(de, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func run(outDir, dbDir string, seed int64, serve bool, port int, reportPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := generate.ResetOutput(dbDir); err != nil {
		return err
	}
	gen, err := generate.Generate(generate.Config{OutDir: outDir, Seed: seed})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return err
	}
	manifest, err := truth.LoadFile(gen.Truth)
	if err != nil {
		return err
	}
	rep := &report{
		Seed: seed, Truth: gen.Truth, Fingerprint: manifest.Fingerprint,
		Projects: gen.Projects,
	}

	projectByVersion := make(map[string]*domain.Project)
	for _, key := range []string{"v1", "v2", "v3"} {
		p, loadedTruth, err := schema.LoadBundle(gen.Projects[key])
		if err != nil {
			return fmt.Errorf("load %s: %w", key, err)
		}
		if loadedTruth.Fingerprint != manifest.Fingerprint {
			return &domain.Error{
				Code: domain.CodeDiff, Field: "truth_fingerprint",
				DialogueID: p.ID, Expected: manifest.Fingerprint, Actual: loadedTruth.Fingerprint,
			}
		}
		projectByVersion[key] = p
		dbPath := filepath.Join(dbDir, key+".db")
		res, err := fullRun(ctx, p, dbPath, manifest.Fingerprint)
		if err != nil {
			return fmt.Errorf("run %s: %w", key, err)
		}
		vr, err := verify.VerifyDB(ctx, dbPath, manifest)
		if err != nil {
			return fmt.Errorf("verify %s: %w", key, err)
		}
		rep.Versions = append(rep.Versions, versionReport{
			Version: p.SourceVersion, Database: dbPath, Shards: len(res.Shards),
			Packages: len(res.Packages), Frames: len(res.Frames),
			CommitSequence: res.CommitSequence, Verification: vr,
		})
	}

	v3 := projectByVersion["v3"]
	realtimeDB := filepath.Join(dbDir, "realtime.db")
	fullDB := filepath.Join(dbDir, "full.db")
	resumeDB := filepath.Join(dbDir, "resume.db")
	realtime, err := fullRun(ctx, v3, realtimeDB, manifest.Fingerprint)
	if err != nil {
		return fmt.Errorf("realtime run: %w", err)
	}
	full, err := fullRun(ctx, v3, fullDB, manifest.Fingerprint)
	if err != nil {
		return fmt.Errorf("full run: %w", err)
	}

	// Simulate forced termination after two committed shards, then resume.
	resumeStore, err := store.Open(resumeDB)
	if err != nil {
		return err
	}
	_, stopErr := pipeline.Run(ctx, v3, resumeStore, pipeline.Options{
		Mode: pipeline.ModeFull, TruthFingerprint: manifest.Fingerprint,
		StopAfterCommits: 2,
	})
	if !pipeline.IsCanceled(stopErr) {
		_ = resumeStore.Close()
		return fmt.Errorf("expected cancellation after partial commit, got %v", stopErr)
	}
	_ = resumeStore.Close()
	resumed, err := recoveryRun(ctx, v3, resumeDB, manifest.Fingerprint)
	if err != nil {
		return fmt.Errorf("resume run: %w", err)
	}

	// All-valid recovery must only read its checkpoints.
	recovered, err := recoveryRun(ctx, v3, realtimeDB, manifest.Fingerprint)
	if err != nil {
		return fmt.Errorf("checkpoint recovery: %w", err)
	}
	rep.RecoveryReuse = len(recovered.Shards)

	for _, item := range []struct {
		mode pipeline.Mode
		path string
		res  *pipeline.Result
	}{
		{pipeline.ModeRealtime, realtimeDB, realtime},
		{pipeline.ModeFull, fullDB, full},
		{pipeline.ModeRecovery, resumeDB, resumed},
	} {
		vr, err := verify.VerifyDB(ctx, item.path, manifest)
		if err != nil {
			return fmt.Errorf("verify %s: %w", item.mode, err)
		}
		rep.Paths = append(rep.Paths, pathReport{
			Mode: item.mode, Database: item.path, Shards: len(item.res.Shards),
			Packages: len(item.res.Packages), Frames: len(item.res.Frames),
			CommitSequence: item.res.CommitSequence, Verification: vr,
		})
	}
	comparison, err := verify.CompareDBs(ctx, realtimeDB, fullDB, resumeDB)
	if err != nil {
		return fmt.Errorf("three-path comparison: %w", err)
	}
	rep.Comparison = comparison
	rep.Diagnostics = map[string]any{
		"project_id":        v3.ID,
		"source_version":    v3.SourceVersion,
		"packages":          len(realtime.Packages),
		"frames":            len(realtime.Frames),
		"truth_fingerprint": manifest.Fingerprint,
	}

	if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
		return err
	}
	if err := writeJSON(reportPath, rep); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(b))

	if serve {
		return serveDiagnostics(ctx, v3, realtime, manifest, rep, port)
	}
	return nil
}

func fullRun(ctx context.Context, p *domain.Project, dbPath, fingerprint string) (*pipeline.Result, error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return pipeline.Run(ctx, p, st, pipeline.Options{
		Mode: pipeline.ModeFull, TruthFingerprint: fingerprint,
	})
}

func recoveryRun(ctx context.Context, p *domain.Project, dbPath, fingerprint string) (*pipeline.Result, error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return pipeline.Run(ctx, p, st, pipeline.Options{
		Mode: pipeline.ModeRecovery, TruthFingerprint: fingerprint,
	})
}

func serveDiagnostics(ctx context.Context, p *domain.Project, res *pipeline.Result,
	m *truth.Manifest, rep *report, port int) error {
	state := &diagnostics.State{
		ProjectID: p.ID, SourceVersion: p.SourceVersion,
		TruthFingerprint: m.Fingerprint, Render: p.Render,
		PackageCount: len(res.Packages), FrameCount: len(res.Frames),
		Verification: rep,
		Packages:     diagnostics.SortedPackages(res.Packages), Frames: res.Frames, Truth: m,
	}
	var ln net.Listener
	var err error
	if port == 0 {
		ln, port, err = diagnostics.ListenFirstFree(8000, 9000)
	} else {
		if port < 8000 || port > 9000 {
			return fmt.Errorf("diagnostic port %d is outside required range 8000..9000", port)
		}
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: diagnostics.Handler(state)}
	go func() {
		_ = srv.Serve(ln)
	}()
	fmt.Printf("diagnostics: http://127.0.0.1:%d/healthz\n", port)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}
