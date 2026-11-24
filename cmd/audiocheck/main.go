package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"audiocheck/internal/dashboard"
	"audiocheck/internal/generator"
	"audiocheck/internal/model"
	"audiocheck/internal/pipeline"
	"audiocheck/internal/store"
	"audiocheck/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "inject":
		err = cmdInject(os.Args[2:])
	case "compare":
		err = cmdCompare(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "verify-db":
		err = cmdVerifyDB(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		var me *model.Error
		if errors.As(err, &me) {
			b, _ := json.MarshalIndent(me, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`audiocheck commands:
  gen       Generate deterministic manifest, truth and PCM files
  run       Run full, realtime or resume evaluation path
  inject    Inject truncation/gear/divisibility/stage faults
  compare   Compare two report JSON files and locate first mismatch
  verify-db Call the independent verifier package
  serve     Start local diagnostic dashboard`)
}

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	dir := fs.String("dir", "artifacts/demo", "output directory")
	rate := fs.Int("sample-rate", 48000, "sample rate")
	frame := fs.Int("frame-size", 64, "frame length in integer samples")
	hop := fs.Int("hop-size", 32, "frame shift in integer samples")
	ch := fs.Int("channels", 1, "channel count")
	count := fs.Int("frames", 32, "frame count")
	gear := fs.Int("gear", 2, "quality gear 0..3")
	seed := fs.Int64("seed", 424242, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m := model.Manifest{
		SchemaVersion: "audio-manifest-v1", SampleRate: *rate, FrameSize: *frame, HopSize: *hop,
		Channels: *ch, FrameCount: *count, QualityGear: *gear, Seed: *seed, SampleBits: 16,
	}
	seq, truth, quant, err := generator.Generate(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*dir, "manifest.json"), m); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*dir, "truth.json"), truth); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*dir, "quant.json"), quant); err != nil {
		return err
	}
	if err := generator.WritePCM(filepath.Join(*dir, "original.pcm"), seq.Original); err != nil {
		return err
	}
	if err := generator.WritePCM(filepath.Join(*dir, "reconstructed.pcm"), seq.Reconstructed); err != nil {
		return err
	}
	fmt.Printf("generated %s: frames=%d samples/channel=%d truth=%s\n", *dir, m.FrameCount, m.SamplesPerChannel(), truth.Fingerprint)
	return nil
}

type runFlags struct {
	dir       string
	db        string
	report    string
	mode      string
	chunk     int
	workers   int
	pending   int
	buffer    int
	window    int
	step      int
	sideBand  int
	failBlock int
	failTimes int
	attempts  int
}

func parseRunFlags(args []string) (*runFlags, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var f runFlags
	fs.StringVar(&f.dir, "dir", "artifacts/demo", "sample directory containing manifest.json")
	fs.StringVar(&f.db, "db", "", "SQLite checkpoint path (default <dir>/checkpoint.db)")
	fs.StringVar(&f.report, "report", "", "report JSON path (default <dir>/report-<mode>.json)")
	fs.StringVar(&f.mode, "mode", "full", "full|realtime|resume")
	fs.IntVar(&f.chunk, "chunk-frames", 4, "frames per block")
	fs.IntVar(&f.workers, "workers", 2, "maximum parallel workers")
	fs.IntVar(&f.pending, "pending", 2, "pending task queue budget")
	fs.IntVar(&f.buffer, "buffer-samples", 262144, "per-thread sample buffer budget")
	fs.IntVar(&f.window, "window", 32, "evaluation window in integer samples")
	fs.IntVar(&f.step, "step", 8, "evaluation slide in integer samples")
	fs.IntVar(&f.sideBand, "side-band", 0, "fixed side band; 0 selects exact required band")
	fs.IntVar(&f.failBlock, "fail-block", -1, "inject one stage failure into block index")
	fs.IntVar(&f.failTimes, "fail-times", 1, "number of first attempts to fail")
	fs.IntVar(&f.attempts, "attempts", 1, "worker attempts before returning E_STAGE")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if f.db == "" {
		f.db = filepath.Join(f.dir, "checkpoint.db")
	}
	if f.report == "" {
		f.report = filepath.Join(f.dir, "report-"+f.mode+".json")
	}
	return &f, nil
}

func cmdRun(args []string) error {
	f, err := parseRunFlags(args)
	if err != nil {
		return err
	}
	m, err := loadManifest(f.dir)
	if err != nil {
		return err
	}
	seq, truth, quant, err := generator.Generate(*m)
	if err != nil {
		return err
	}
	params := model.EvalParams{WindowSizeSamples: f.window, StepSamples: f.step, SideBandSamples: f.sideBand}
	budget := model.Budget{ChunkFrames: f.chunk, MaxWorkers: f.workers, MaxPendingTasks: f.pending, PerThreadSamples: f.buffer}
	id, err := pipeline.BuildIdentity(*m, seq, truth.Fingerprint, params)
	if err != nil {
		return err
	}
	st, err := store.Open(f.db, id)
	if err != nil {
		return err
	}
	defer st.Store.Close()
	if st.Reset {
		fmt.Printf("checkpoint reset: %s (old_blocks=%d)\n", st.ResetReason, st.ExistingBlocks)
	}

	cfg := pipeline.Config{
		Manifest: *m, Sequence: seq, Truth: truth, Quant: quant, Params: params, Budget: budget,
		Store: st.Store, MaxAttempts: f.attempts, PersistSamples: true, Mode: f.mode,
	}
	if f.failBlock >= 0 {
		cfg.Failure = pipeline.NewFailureInjector()
		cfg.Failure.FailAttempts = map[int]int{f.failBlock: f.failTimes}
	}
	ctx := context.Background()
	var report *model.Report
	switch f.mode {
	case "full":
		if err := st.Store.Reset(ctx, id, "explicit full recompute"); err != nil {
			return err
		}
		report, err = pipeline.RunFull(ctx, cfg)
	case "realtime":
		report, err = pipeline.RunRealtime(ctx, cfg)
	case "resume":
		report, err = pipeline.Resume(ctx, cfg)
	default:
		return model.NewError(model.CodePara, "mode", "unsupported mode %q", f.mode)
	}
	if report != nil {
		if werr := writeJSON(f.report, report); werr != nil && err == nil {
			err = werr
		}
	}
	if err != nil {
		return err
	}
	fmt.Printf("mode=%s blocks=%d frames=%d commit_order=%v report=%s db=%s\n", f.mode, len(report.Blocks), len(report.Frames), report.CommitOrder, f.report, f.db)
	return nil
}

func cmdVerifyDB(args []string) error {
	fs := flag.NewFlagSet("verify-db", flag.ContinueOnError)
	db := fs.String("db", "", "SQLite checkpoint path")
	fp := fs.String("truth-fingerprint", "", "expected truth fingerprint")
	truthPath := fs.String("truth", "", "truth.json path; fingerprint is read from it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *truthPath != "" {
		var t generator.Truth
		if err := readJSON(*truthPath, &t); err != nil {
			return err
		}
		*fp = t.Fingerprint
	}
	res, err := verify.VerifyDatabase(*db, *fp)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if !res.OK {
		os.Exit(3)
	}
	return nil
}

func cmdCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	aPath := fs.String("a", "", "first report JSON")
	bPath := fs.String("b", "", "second report JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var a, b model.Report
	if err := readJSON(*aPath, &a); err != nil {
		return err
	}
	if err := readJSON(*bPath, &b); err != nil {
		return err
	}
	mm := verify.CompareReports(&a, &b)
	out := map[string]any{"identical": mm == nil, "first_mismatch": mm}
	payload, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(payload))
	if mm != nil {
		os.Exit(3)
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	report := fs.String("report", "artifacts/demo/report-full.json", "report JSON to serve")
	addr := fs.String("addr", "127.0.0.1:8088", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv := dashboard.NewServer(*report)
	fmt.Printf("dashboard http://%s/ (report=%s)\n", *addr, *report)
	return srv.ListenAndServe(*addr)
}

func cmdInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	kind := fs.String("kind", "", "truncation|gear|divisibility|stage")
	dir := fs.String("dir", "", "directory for stage SQLite checkpoint; empty uses temp dir")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out := map[string]any{"kind": *kind, "timestamp_unix_nano": time.Now().UnixNano()}
	var observed *model.Error
	expectedCode := ""

	m := model.Manifest{SchemaVersion: "audio-manifest-v1", SampleRate: 48000, FrameSize: 64, HopSize: 32, Channels: 1, FrameCount: 8, QualityGear: 2, Seed: 7, SampleBits: 16}
	switch *kind {
	case "truncation":
		expectedCode = model.CodeDecode
		seq, _, _, err := generator.Generate(m)
		if err != nil {
			return err
		}
		short := seq.Original[:len(seq.Original)-m.Channels*3]
		observed = asModelError(model.ValidateSequence(m, short, seq.Reconstructed))
	case "gear":
		expectedCode = model.CodePara
		m.QualityGear = 9
		b, _ := json.Marshal(m)
		_, err := model.ParseManifest(b)
		observed = asModelError(err)
	case "divisibility":
		expectedCode = model.CodeSchema
		m.HopSize = 63
		b, _ := json.Marshal(m)
		_, err := model.ParseManifest(b)
		observed = asModelError(err)
	case "stage":
		expectedCode = model.CodeStage
		if *dir == "" {
			d, err := os.MkdirTemp("", "audiocheck-inject-*")
			if err != nil {
				return err
			}
			*dir = d
		}
		seq, truth, quant, err := generator.Generate(m)
		if err != nil {
			return err
		}
		params := model.EvalParams{WindowSizeSamples: 32, StepSamples: 8}
		id, err := pipeline.BuildIdentity(m, seq, truth.Fingerprint, params)
		if err != nil {
			return err
		}
		dbPath := filepath.Join(*dir, "inject.db")
		st, err := store.Open(dbPath, id)
		if err != nil {
			return err
		}
		defer st.Store.Close()
		_ = st.Store.Reset(context.Background(), id, "injection test")
		cfg := pipeline.Config{Manifest: m, Sequence: seq, Truth: truth, Quant: quant, Params: params, Store: st.Store,
			Budget:  model.Budget{ChunkFrames: 1, MaxWorkers: 1, MaxPendingTasks: 1, PerThreadSamples: 262144},
			Failure: &pipeline.FailureInjector{FailAttempts: map[int]int{2: 1}, Calls: map[int]int{}}, MaxAttempts: 1, PersistSamples: true}
		_, err = pipeline.RunFull(context.Background(), cfg)
		observed = asModelError(err)
	default:
		return model.NewError(model.CodePara, "kind", "unknown injection kind %q", *kind)
	}

	ok := observed != nil && observed.Code == expectedCode
	out["expected_code"] = expectedCode
	out["observed_error"] = observed
	out["ok"] = ok
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	if !ok {
		return fmt.Errorf("injection %s did not produce %s", *kind, expectedCode)
	}
	return nil
}

func loadManifest(dir string) (*model.Manifest, error) {
	var m model.Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return nil, err
	}
	parsed, err := model.ParseManifest(mustRead(filepath.Join(dir, "manifest.json")))
	if err != nil {
		return nil, err
	}
	_ = m
	return parsed, nil
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func asModelError(err error) *model.Error {
	var me *model.Error
	if errors.As(err, &me) {
		return me
	}
	if err == nil {
		return nil
	}
	return &model.Error{Code: "E_UNKNOWN", Message: err.Error()}
}
