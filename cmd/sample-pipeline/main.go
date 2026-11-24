package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"fixedsampler/internal/checkpoint"
	"fixedsampler/internal/engine"
	"fixedsampler/internal/loader"
	"fixedsampler/internal/model"
	"fixedsampler/internal/outputio"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "compare":
		err = compareCmd(os.Args[2:])
	case "edit":
		err = editCmd(os.Args[2:])
	case "swap-order":
		err = swapCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		var pe *model.PipelineError
		if errors.As(err, &pe) {
			b, _ := json.MarshalIndent(pe, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`sample-pipeline <run|compare|edit|swap-order> [flags]

Run 'sample-pipeline <command> -h' for command flags.`)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	projectPath := fs.String("project", "samples/blocks420/project.json", "project JSON path")
	dbPath := fs.String("db", "checkpoints.db", "SQLite checkpoint path")
	outDir := fs.String("out", "out", "diagnostic output directory")
	workers := fs.Int("workers", 2, "maximum sampling worker parallelism")
	buffer := fs.Int("buffer", 2, "per-worker sampling buffer limit")
	chunkSize := fs.Int("chunk-size", 8, "square chunk size in full-resolution coordinates")
	fresh := fs.Bool("fresh", false, "discard persisted project snapshot before running")
	crashAfter := fs.Int("crash-after-commit", 0, "test hook: exit immediately after N chunk commits")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p, err := loader.LoadProject(*projectPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	if *fresh {
		if err := store.ResetProject(ctx, p.ProjectID); err != nil {
			return err
		}
	}
	currentFP := engine.ConfigFingerprint(p)
	lastFP, metaErr := store.LastConfigFP(ctx, p.ProjectID)
	if metaErr != nil {
		return metaErr
	}
	var previous map[int]*engine.PreviousChunk
	if lastFP == currentFP {
		previous, err = store.LoadSameConfig(ctx, p.ProjectID, currentFP)
	} else {
		previous, err = store.LoadPrevious(ctx, p.ProjectID)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint recovery: %v; starting full recompute\n", err)
		previous = nil
	}

	opts := engine.Options{
		ChunkSize: *chunkSize, Workers: *workers, BufferLimit: *buffer, Previous: previous,
		Commit: func(ctx context.Context, c *engine.ChunkCommit) error {
			return store.CommitChunk(ctx, c)
		},
	}
	if *crashAfter > 0 {
		opts.OnChunkCommitted = func(_ int, committed int) {
			if committed == *crashAfter {
				fmt.Fprintf(os.Stderr, "simulated hard termination after %d durable chunk(s)\n", committed)
				os.Exit(137)
			}
		}
	}

	res, runErr := engine.Run(ctx, p, opts)
	report := engine.BuildErrorReport(p, res, *workers, *buffer)
	if writeErr := outputio.WriteRunOutputs(*outDir, p, res, report); writeErr != nil {
		fmt.Fprintf(os.Stderr, "write outputs failed: %v\n", writeErr)
	}
	if runErr != nil {
		return runErr
	}
	if err := store.MarkComplete(ctx, p.ProjectID, res.ConfigFP, res.Grid.Count()); err != nil {
		return err
	}
	if err := store.SaveProjectMeta(ctx, p, res.ConfigFP); err != nil {
		return err
	}
	summary := map[string]any{
		"project_id": p.ProjectID, "config_fingerprint": res.ConfigFP,
		"chunks": res.Grid.Count(), "invalidated_nodes": len(res.Invalidations),
		"queue_waits": len(res.QueueEvents), "commit_sequence": res.CommitSequence,
		"out_dir": *outDir, "db": *dbPath,
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(b))
	return nil
}

func compareCmd(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	aPath := fs.String("a", "incremental.db", "first SQLite DB")
	bPath := fs.String("b", "full.db", "second SQLite DB")
	projectID := fs.String("project-id", "", "project ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *projectID == "" {
		return model.SchemaError("compare requires --project-id")
	}
	ctx := context.Background()
	a, err := checkpoint.Open(*aPath)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := checkpoint.Open(*bPath)
	if err != nil {
		return err
	}
	defer b.Close()
	aa, seqA, err := a.LoadArtifacts(ctx, *projectID)
	if err != nil {
		return err
	}
	bb, seqB, err := b.LoadArtifacts(ctx, *projectID)
	if err != nil {
		return err
	}
	if loc := engine.CompareChunkMaps(aa, bb, seqA, seqB); loc != nil {
		data, _ := json.MarshalIndent(loc, "", "  ")
		fmt.Println(string(data))
		return loc
	}
	fmt.Printf("COMPARE_OK project=%s chunks=%d sequences and all level fields match\n", *projectID, len(aa))
	return nil
}

func editCmd(args []string) error {
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	in := fs.String("in", "samples/blocks420/project.json", "input project")
	out := fs.String("out", "edited.json", "output edited project")
	seq := fs.Int("seq", 0, "config sequence to edit")
	factor := fs.Int("factor", 0, "new downsample factor")
	kernel := fs.String("kernel", "", "new kernel: nearest|box")
	bitDepth := fs.Int("bit-depth", 0, "new quantize bit depth")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := loader.LoadProject(*in)
	if err != nil {
		return err
	}
	if *seq < 1 || *seq > len(p.Config) {
		return model.SchemaError("edit --seq must select a config entry")
	}
	stage := &p.Config[*seq-1]
	if *factor > 0 {
		stage.Factor = *factor
		stage.FactorX = *factor
		stage.FactorY = *factor
		if stage.Kernel == "box" {
			stage.Taps = *factor * *factor
		}
	}
	if *kernel != "" {
		stage.Kernel = *kernel
		if *kernel == "nearest" {
			stage.Taps = 1
		} else if *kernel == "box" && stage.FactorX > 0 {
			stage.Taps = stage.FactorX * stage.FactorY
		}
	}
	if *bitDepth > 0 {
		stage.BitDepth = *bitDepth
	}
	if err := loader.ValidateProject(p); err != nil {
		return err
	}
	return loader.SaveProject(*out, p)
}

func swapCmd(args []string) error {
	fs := flag.NewFlagSet("swap-order", flag.ContinueOnError)
	in := fs.String("in", "samples/mixed422/project.json", "input project")
	out := fs.String("out", "reordered.json", "output project")
	a := fs.Int("a", 1, "first config sequence")
	b := fs.Int("b", 2, "second config sequence")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := loader.LoadProject(*in)
	if err != nil {
		return err
	}
	if *a < 1 || *b < 1 || *a > len(p.Config) || *b > len(p.Config) {
		return model.SchemaError("swap sequences out of range")
	}
	p.Config[*a-1], p.Config[*b-1] = p.Config[*b-1], p.Config[*a-1]
	for i := range p.Config {
		p.Config[i].Seq = i + 1
	}
	if err := loader.ValidateProject(p); err != nil {
		return err
	}
	return loader.SaveProject(*out, p)
}
