package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
	"spectralforge/internal/render"
	"spectralforge/internal/sample"
	"spectralforge/internal/store"
	"spectralforge/internal/verify"
	"spectralforge/internal/web"
)

const defaultSeed int64 = 20261125

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "sample":
		err = cmdSample(os.Args[2:])
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "render":
		err = cmdRender(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = errs.Para("command", `["sample","validate","render","verify","compare","serve"]`, "unknown command %q", os.Args[1])
	}
	if err != nil {
		var quiet *exitError
		if errors.As(err, &quiet) {
			os.Exit(quiet.code)
		}
		printError(err)
		os.Exit(2)
	}
}

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func cmdSample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	dir := fs.String("dir", "sample", "output sample directory")
	seed := fs.Int64("seed", defaultSeed, "deterministic generator seed")
	if err := fs.Parse(args); err != nil {
		return errs.Para("flags", "valid flags", "%v", err)
	}
	bundle, err := sample.Generate(*dir, *seed)
	if err != nil {
		return err
	}
	fmt.Println(sample.Summary(bundle))
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	sampleDir := fs.String("sample", "sample", "sample directory")
	if err := fs.Parse(args); err != nil {
		return errs.Para("flags", "valid flags", "%v", err)
	}
	bundle, err := domain.LoadBundle(*sampleDir)
	if err != nil {
		return err
	}
	out := map[string]any{
		"ok":            true,
		"width":         bundle.Scene.Width,
		"height":        bundle.Scene.Height,
		"meshes":        len(bundle.Scene.Meshes),
		"wavelengths":   len(bundle.Spectral.Wavelengths),
		"basis":         len(bundle.Spectral.Basis),
		"lights":        len(bundle.Spectral.Lights),
		"truth_points":  len(bundle.Truth.Points),
		"content_hash":  bundle.ContentHash,
		"scene_hash":    bundle.SceneHash,
		"spectral_hash": bundle.SpectralHash,
	}
	return printJSON(out)
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	sampleDir := fs.String("sample", "sample", "sample directory")
	checkpointDir := fs.String("checkpoints", "checkpoints", "checkpoint directory")
	outDir := fs.String("out", "render", "render output directory")
	mode := fs.String("mode", "full", "full, realtime, or resume")
	configPath := fs.String("config", "", "optional render config JSON override")
	stopAfter := fs.Int("stop-after", 0, "pause after N newly committed tiles (0 = unlimited)")
	failTile := fs.Int("fail-tile", -1, "inject one transient failure into this tile")
	failCount := fs.Int("fail-count", 1, "number of transient failures to inject")
	snapshotVersion := fs.String("snapshot-version", "", "expected parameter version")
	injectGamut := fs.String("inject-gamut", "", "test hook x:y:channel:value, e.g. 20:20:r:-0.5")
	if err := fs.Parse(args); err != nil {
		return errs.Para("flags", "valid flags", "%v", err)
	}
	bundle, err := loadBundle(*sampleDir, *configPath)
	if err != nil {
		return err
	}
	if *injectGamut != "" {
		inject, err := parseGamutInjection(*injectGamut)
		if err != nil {
			return err
		}
		bundle.Config.InjectedGamut = inject
	}
	resume := false
	switch *mode {
	case "full", "realtime":
	case "resume":
		resume = true
	default:
		return errs.Para("mode", `["full","realtime","resume"]`, "unknown render mode %q", *mode)
	}
	failPlan := map[int]int{}
	if *failTile >= 0 {
		if *failCount < 1 {
			return errs.Para("fail-count", "[1,16]", "failure count must be positive")
		}
		failPlan[*failTile] = *failCount
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, renderErr := render.Render(ctx, bundle, render.Options{
		Config:          *bundle.Config,
		CheckpointDir:   *checkpointDir,
		OutputDir:       *outDir,
		Resume:          resume,
		Mode:            *mode,
		StopAfter:       *stopAfter,
		SnapshotVersion: *snapshotVersion,
		FailOnce:        failPlan,
	})
	summary := summaryOf(report, renderErr)
	if err := os.MkdirAll(*outDir, 0o755); err == nil {
		_ = store.WriteJSONAtomic(filepath.Join(*outDir, "render-report.json"), summary)
	}
	if err := printJSON(summary); err != nil {
		return err
	}
	if renderErr != nil {
		if e, ok := errs.As(renderErr); ok && (e.Code == errs.CodePaused || e.Code == errs.CodeCancel) {
			return nil
		}
		return &exitError{code: 2}
	}
	return nil
}

type runSummary struct {
	Mode               string               `json:"mode"`
	Completed          bool                 `json:"completed"`
	ContentHash        string               `json:"content_hash"`
	SamplingVersion    string               `json:"sampling_version"`
	ParamVersion       string               `json:"param_version"`
	TilesTotal         int                  `json:"tiles_total"`
	TilesRendered      int                  `json:"tiles_rendered"`
	TilesReused        int                  `json:"tiles_reused"`
	CommitSequence     []int                `json:"commit_sequence"`
	WaitStats          store.WaitStats      `json:"wait_stats"`
	TileAttempts       map[int]int          `json:"tile_attempts"`
	Failures           []store.Failure      `json:"failures"`
	Invalidated        bool                 `json:"invalidated"`
	InvalidationReason string               `json:"invalidation_reason,omitempty"`
	ElapsedSeconds     float64              `json:"elapsed_seconds"`
	QuantStats         quantStats           `json:"quant_stats"`
	TruthPass          bool                 `json:"truth_pass"`
	TruthResults       []render.TruthResult `json:"truth_results,omitempty"`
	FrameDigest        string               `json:"frame_digest,omitempty"`
	Error              any                  `json:"error,omitempty"`
}

type quantStats struct {
	Samples int     `json:"samples"`
	MeanAbs float64 `json:"mean_abs"`
	MaxAbs  float64 `json:"max_abs"`
	RMSE    float64 `json:"rmse"`
}

func summaryOf(report *render.Report, renderErr error) *runSummary {
	if report == nil {
		return &runSummary{Error: renderErr}
	}
	out := &runSummary{
		Mode:               report.Mode,
		Completed:          report.Completed,
		ContentHash:        report.ContentHash,
		SamplingVersion:    report.SamplingVersion,
		ParamVersion:       report.ParamVersion,
		TilesTotal:         report.TilesTotal,
		TilesRendered:      report.TilesRendered,
		TilesReused:        report.TilesReused,
		CommitSequence:     report.CommitSequence,
		WaitStats:          report.WaitStats,
		TileAttempts:       report.TileAttempts,
		Failures:           report.Failures,
		Invalidated:        report.Invalidated,
		InvalidationReason: report.InvalidationReason,
		ElapsedSeconds:     report.ElapsedSeconds,
		QuantStats: quantStats{
			Samples: report.QuantStats.Samples,
			MeanAbs: report.QuantStats.MeanAbs,
			MaxAbs:  report.QuantStats.MaxAbs,
			RMSE:    report.QuantStats.RMSE,
		},
	}
	if report.Frame != nil {
		out.TruthPass = report.Frame.TruthPass
		out.TruthResults = report.Frame.TruthResults
		out.FrameDigest = report.Frame.DisplayDigest
	}
	if renderErr != nil {
		if e, ok := errs.As(renderErr); ok {
			out.Error = e
		} else {
			out.Error = renderErr.Error()
		}
	}
	return out
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	sampleDir := fs.String("sample", "sample", "sample directory")
	checkpointDir := fs.String("checkpoints", "checkpoints/exact", "checkpoint directory")
	outDir := fs.String("out", "render/exact", "rendered frame directory")
	if err := fs.Parse(args); err != nil {
		return errs.Para("flags", "valid flags", "%v", err)
	}
	bundle, err := domain.LoadBundle(*sampleDir)
	if err != nil {
		return err
	}
	report, err := verify.Run(bundle, *checkpointDir, *outDir)
	if err != nil {
		return err
	}
	if err := printJSON(report); err != nil {
		return err
	}
	if !report.OK {
		os.Exit(1)
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	sampleDir := fs.String("sample", "sample", "sample directory")
	checkpointRoot := fs.String("checkpoints", "checkpoints", "checkpoint root")
	outRoot := fs.String("out", "render", "render output root")
	addr := fs.String("addr", "127.0.0.1:8080", "local listen address")
	if err := fs.Parse(args); err != nil {
		return errs.Para("flags", "valid flags", "%v", err)
	}
	bundle, err := domain.LoadBundle(*sampleDir)
	if err != nil {
		return err
	}
	server := web.NewServer(bundle, *checkpointRoot, *outRoot, *addr)
	fmt.Printf("SpectralForge listening on http://%s\n", *addr)
	return server.ListenAndServe()
}

type compareReport struct {
	PixelsEqual         bool   `json:"pixels_equal"`
	CommitSequenceEqual bool   `json:"commit_sequence_equal"`
	TruthPassA          bool   `json:"truth_pass_a"`
	TruthPassB          bool   `json:"truth_pass_b"`
	LinearDigestA       string `json:"linear_digest_a"`
	LinearDigestB       string `json:"linear_digest_b"`
	DisplayDigestA      string `json:"display_digest_a"`
	DisplayDigestB      string `json:"display_digest_b"`
	CommitSequenceA     []int  `json:"commit_sequence_a"`
	CommitSequenceB     []int  `json:"commit_sequence_b"`
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	outA := fs.String("a", "render/full", "first frame directory")
	outB := fs.String("b", "render/resume", "second frame directory")
	cpA := fs.String("a-checkpoints", "checkpoints/full", "first checkpoint directory")
	cpB := fs.String("b-checkpoints", "checkpoints/resume", "second checkpoint directory")
	if err := fs.Parse(args); err != nil {
		return errs.Para("flags", "valid flags", "%v", err)
	}
	a, err := readFrameDigest(filepath.Join(*outA, "frame.json"))
	if err != nil {
		return err
	}
	b, err := readFrameDigest(filepath.Join(*outB, "frame.json"))
	if err != nil {
		return err
	}
	commitsA, err := store.ReadCommits(*cpA)
	if err != nil {
		return err
	}
	commitsB, err := store.ReadCommits(*cpB)
	if err != nil {
		return err
	}
	report := compareReport{
		PixelsEqual:     a.LinearDigest == b.LinearDigest && a.DisplayDigest == b.DisplayDigest,
		TruthPassA:      a.TruthPass,
		TruthPassB:      b.TruthPass,
		LinearDigestA:   a.LinearDigest,
		LinearDigestB:   b.LinearDigest,
		DisplayDigestA:  a.DisplayDigest,
		DisplayDigestB:  b.DisplayDigest,
		CommitSequenceA: commitTiles(commitsA),
		CommitSequenceB: commitTiles(commitsB),
	}
	report.CommitSequenceEqual = equalInts(report.CommitSequenceA, report.CommitSequenceB)
	if err := printJSON(report); err != nil {
		return err
	}
	if !report.PixelsEqual || !report.CommitSequenceEqual || !report.TruthPassA || !report.TruthPassB {
		os.Exit(1)
	}
	return nil
}

type frameDigestRecord struct {
	LinearDigest  string `json:"linear_digest"`
	DisplayDigest string `json:"display_digest"`
	TruthPass     bool   `json:"truth_pass"`
}

func readFrameDigest(path string) (frameDigestRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return frameDigestRecord{}, errs.Decode("frame.json", "cannot read %s: %v", path, err)
	}
	var out frameDigestRecord
	if err := json.Unmarshal(data, &out); err != nil {
		return out, errs.Decode("frame.json", "invalid JSON: %v", err)
	}
	return out, nil
}

func commitTiles(commits []store.CommitRecord) []int {
	out := make([]int, len(commits))
	for i, rec := range commits {
		out[i] = rec.Tile
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func loadBundle(sampleDir, configPath string) (*domain.SampleBundle, error) {
	bundle, err := domain.LoadBundle(sampleDir)
	if err != nil {
		return nil, err
	}
	if configPath == "" {
		return bundle, nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, errs.Decode("render_config", "cannot read %s: %v", configPath, err)
	}
	cfg, err := domain.DecodeConfig(data)
	if err != nil {
		return nil, err
	}
	bundle.Config = cfg
	if err := domain.ValidateBundle(bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

func parseGamutInjection(value string) (*domain.GamutInjection, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 4 {
		return nil, errs.Para("inject-gamut", "x:y:r|g|b:value", "invalid gamut injection %q", value)
	}
	x, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, errs.Para("inject-gamut.x", "integer", "invalid x coordinate")
	}
	y, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, errs.Para("inject-gamut.y", "integer", "invalid y coordinate")
	}
	channel := parts[2]
	if channel != "r" && channel != "g" && channel != "b" {
		return nil, errs.Para("inject-gamut.channel", `["r","g","b"]`, "invalid channel")
	}
	v, err := strconv.ParseFloat(parts[3], 64)
	if err != nil {
		return nil, errs.Para("inject-gamut.value", "float", "invalid value")
	}
	return &domain.GamutInjection{X: x, Y: y, Channel: channel, Value: v}, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printError(err error) {
	if e, ok := errs.As(err); ok {
		_ = printJSON(e)
		return
	}
	_ = printJSON(map[string]any{"code": "E_IO", "message": err.Error()})
}

func usage() {
	fmt.Print(`SpectralForge - deterministic spectral material renderer

Usage:
  spectralforge sample   [--dir sample] [--seed 20261125]
  spectralforge validate [--sample sample]
  spectralforge render   [--sample sample] [--checkpoints checkpoints]
                         [--out render] [--mode full|realtime|resume]
                         [--stop-after N] [--config path]
  spectralforge verify   [--sample sample] [--checkpoints checkpoints/exact]
                         [--out render/exact]
  spectralforge compare  [--a render/full] [--b render/resume]
                         [--a-checkpoints checkpoints/full]
                         [--b-checkpoints checkpoints/resume]
  spectralforge serve    [--sample sample] [--checkpoints checkpoints]
                         [--out render] [--addr 127.0.0.1:8080]
`)
}
