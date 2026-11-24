package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"mediaforge/internal/errs"
	"mediaforge/internal/paths"
	"mediaforge/internal/project"
	"mediaforge/internal/runner"
	"mediaforge/internal/samplegen"
	"mediaforge/internal/scale"
	"mediaforge/internal/state"
	"mediaforge/internal/store"
	"mediaforge/internal/verifier"
	"mediaforge/internal/web"
)

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
	case "run":
		err = cmdRun(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "compare-paths":
		err = cmdComparePaths(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "state":
		err = cmdState(os.Args[2:])
	case "version":
		printJSON(map[string]any{"ok": true, "algorithmVersion": "mfx-transcode/1.0.0"})
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		writeErr(err)
		os.Exit(exitCode(err))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `MFX Forge - deterministic media asset workbench

Commands:
  sample        --out DIR [--seed N] [--profile small|demo|large]
  validate      --project PROJECT.json
  run           --project PROJECT.json --db CHECKPOINT.sqlite [--out DIR]
                [--mode realtime|full|resume] [--workers N] [--pause-after N]
  serve         [--data DIR] [--db DIR] [--addr 127.0.0.1:8787]
  verify        --db CHECKPOINT.sqlite
  compare-paths --project PROJECT.json [--workers N] [--pause-after N]
  export        --db CHECKPOINT.sqlite --variant ID --out FILE
  state         --db CHECKPOINT.sqlite
  version`)
}

func cmdSample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	out := fs.String("out", "data", "output directory")
	seed := fs.Int64("seed", 42, "deterministic seed")
	profile := fs.String("profile", "demo", "small, demo or large")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	idx, err := samplegen.Generate(samplegen.Options{OutDir: *out, Seed: *seed, Profile: *profile})
	if err != nil {
		return err
	}
	printJSON(map[string]any{"ok": true, "index": idx})
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	projectPath := fs.String("project", "", "path to project.json")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	if *projectPath == "" {
		return errs.Field(errs.EPara, "project", "project path is required")
	}
	p, err := project.Load(*projectPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(*projectPath)
	rp, err := project.Validate(p, dir)
	if err != nil {
		return err
	}
	fp := project.Fingerprint(p, rp)
	printJSON(map[string]any{
		"ok": true, "projectId": p.ID, "fingerprint": fp,
		"variants": rp.Variants, "scale": scale.Decide(rp),
	})
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	projectPath := fs.String("project", "", "path to project.json")
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	outDir := fs.String("out", "", "optional output directory")
	mode := fs.String("mode", "realtime", "realtime, full or resume")
	workers := fs.Int("workers", 0, "worker count")
	pauseAfter := fs.Int("pause-after", 0, "pause after N new commits")
	forcePath := fs.String("force-path", "", "fine or adaptive (diagnostic override)")
	only := fs.String("only", "", "single variant ID (diagnostic retry)")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	if *projectPath == "" || *dbPath == "" {
		return errs.New(errs.EPara, "--project and --db are required")
	}
	p, err := project.Load(*projectPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(*projectPath)
	rp, err := project.Validate(p, dir)
	if err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	opts := runner.Options{
		Mode: runner.Mode(*mode), Workers: *workers, OutDir: *outDir,
		PauseAfter: *pauseAfter, ForcePath: *forcePath,
	}
	if *only != "" {
		opts.Only = []string{*only}
	}
	rep, err := runner.New(st, p, rp, project.Fingerprint(p, rp), dir).Run(ctx, opts)
	if err != nil && errs.CodeOf(err) == errs.ECancel {
		return errs.New(errs.ECancel, "run interrupted; no exit report is guaranteed")
	}
	if rep != nil {
		printJSON(map[string]any{"ok": err == nil, "report": rep})
	}
	if err != nil {
		if ctx.Err() != nil {
			return errs.New(errs.ECancel, "run interrupted; no exit report is guaranteed")
		}
		return err
	}
	if rep != nil && rep.Progress.Failed > 0 {
		os.Exit(3)
	}
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	data := fs.String("data", "data", "sample data directory")
	db := fs.String("db", "work", "SQLite checkpoint directory")
	addr := fs.String("addr", "127.0.0.1:8787", "loopback HTTP address")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	srv, err := web.New(web.Config{DataDir: *data, DBRoot: *db, Addr: *addr})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "MFX Forge listening on http://%s\n", *addr)
	return srv.ListenAndServe()
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	if *dbPath == "" {
		return errs.Field(errs.EPara, "db", "checkpoint database is required")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := (&verifier.Verifier{Store: st}).Verify(context.Background())
	if rep != nil {
		printJSON(map[string]any{"ok": err == nil, "report": rep})
	}
	return err
}

func cmdComparePaths(args []string) error {
	fs := flag.NewFlagSet("compare-paths", flag.ContinueOnError)
	projectPath := fs.String("project", "", "path to project.json")
	workers := fs.Int("workers", 0, "worker count")
	pauseAfter := fs.Int("pause-after", 0, "resume pause boundary")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	if *projectPath == "" {
		return errs.Field(errs.EPara, "project", "project path is required")
	}
	rep, err := paths.Compare(context.Background(), *projectPath, *workers, *pauseAfter)
	if rep != nil {
		printJSON(map[string]any{"ok": err == nil, "comparison": rep})
	}
	return err
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	variant := fs.String("variant", "", "variant ID")
	out := fs.String("out", "", "output .mfx path")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	if *dbPath == "" || *variant == "" || *out == "" {
		return errs.New(errs.EPara, "--db, --variant and --out are required")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	rec, err := st.Get(context.Background(), *variant)
	if err != nil {
		return err
	}
	if rec.State != state.Committed || len(rec.OutputBlob) == 0 {
		return errs.New(errs.EState, "variant has no committed output")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return errs.Wrap(errs.EIO, "create export directory", err)
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, rec.OutputBlob, 0o644); err != nil {
		return errs.Wrap(errs.EIO, "write export", err)
	}
	if err := os.Rename(tmp, *out); err != nil {
		return errs.Wrap(errs.EIO, "finalize export", err)
	}
	printJSON(map[string]any{
		"ok": true, "variant": rec.ID, "path": *out,
		"sha256": rec.OutputSHA256, "sizeBytes": rec.OutputSize,
	})
	return nil
}

func cmdState(args []string) error {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return errs.Wrap(errs.EPara, "parse arguments", err)
	}
	if *dbPath == "" {
		return errs.Field(errs.EPara, "db", "checkpoint database is required")
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	recs, err := st.Variants(context.Background())
	if err != nil {
		return err
	}
	prog, err := st.Progress(context.Background())
	if err != nil {
		return err
	}
	printJSON(map[string]any{"ok": true, "progress": prog, "variants": recs})
	return nil
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(err error) {
	app := errs.As(err)
	enc := json.NewEncoder(os.Stderr)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"ok": false, "error": app})
}

func exitCode(err error) int {
	switch errs.CodeOf(err) {
	case errs.EDiff:
		return 6
	case errs.EVerify, errs.EScale:
		return 4
	case errs.EState:
		return 5
	case errs.ECancel:
		return 130
	default:
		return 1
	}
}
