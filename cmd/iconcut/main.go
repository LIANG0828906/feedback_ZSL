package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"iconcut"
	"iconcut/ui"
	"iconcut/verifier"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = generateCmd(os.Args[2:])
	case "run":
		err = runCmd(os.Args[2:])
	case "compare":
		err = compareCmd(os.Args[2:])
	case "verify":
		err = verifyCmd(os.Args[2:])
	case "serve", "ui":
		err = serveCmd(os.Args[2:])
	case "retry":
		err = retryCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		if e, ok := iconcut.AsError(err); ok {
			b, _ := json.MarshalIndent(e, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`iconcut commands:
  generate -out samples
  run -bundle samples/icon-192 -db checkpoints/icon-192.db
  compare -bundle samples/icon-192
  verify -db checkpoints/icon-192.db
  retry -bundle samples/icon-192 -db checkpoints/icon-192.db -tile 7 -stage clean
  serve -addr 127.0.0.1:8787 -workspace .ui-workspace`)
}

func compareCmd(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	bundle := fs.String("bundle", "", "bundle directory")
	stop := fs.Int("stop-after", 7, "simulated crash prefix commit count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bundle == "" {
		return fmt.Errorf("-bundle is required")
	}
	b, err := iconcut.LoadBundle(*bundle)
	if err != nil {
		return err
	}
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "iconcut-compare-*")
	if err != nil {
		return err
	}
	fullPath := filepath.Join(dir, "full.db")
	resumePath := filepath.Join(dir, "resume.db")
	full, err := iconcut.RunBundle(ctx, b, iconcut.RunOptions{DBPath: fullPath, SkipTruthCheck: true})
	if err != nil {
		return err
	}
	prefix, err := iconcut.RunBundle(ctx, b, iconcut.RunOptions{DBPath: resumePath, StopAfterCommits: *stop, SkipTruthCheck: true})
	if err != nil {
		return err
	}
	resume, err := iconcut.RunBundle(ctx, b, iconcut.RunOptions{DBPath: resumePath, SkipTruthCheck: true})
	if err != nil {
		return err
	}
	maskDiff := iconcut.CompareReports(full, resume)
	fullCommits, err := iconcut.LoadCommitOrder(ctx, fullPath, b.Manifest.ImageID)
	if err != nil {
		return err
	}
	resumeCommits, err := iconcut.LoadCommitOrder(ctx, resumePath, b.Manifest.ImageID)
	if err != nil {
		return err
	}
	commitDiff := iconcut.CompareCommitSequences(fullCommits, resumeCommits)
	result := map[string]any{
		"prefix_interrupted": prefix.Interrupted,
		"mask_or_stats_diff": maskDiff,
		"commit_diff":        commitDiff,
		"ok":                 maskDiff == nil && commitDiff == nil,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return err
	}
	if maskDiff != nil || commitDiff != nil {
		os.Exit(2)
	}
	return nil
}

func generateCmd(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "samples", "output root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, sp := range iconcut.DefaultSpecs() {
		dir, _, err := iconcut.GenerateBundle(*out, sp)
		if err != nil {
			return err
		}
		fmt.Println("generated", dir)
	}
	return nil
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	bundle := fs.String("bundle", "", "bundle directory")
	db := fs.String("db", "checkpoint.db", "sqlite path")
	out := fs.String("out", "", "optional artifact output directory")
	stop := fs.Int("stop-after", 0, "stop after N commits (test/resume)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bundle == "" {
		return fmt.Errorf("-bundle is required")
	}
	b, err := iconcut.LoadBundle(*bundle)
	if err != nil {
		return err
	}
	rep, err := iconcut.RunBundle(context.Background(), b, iconcut.RunOptions{DBPath: *db, StopAfterCommits: *stop})
	if rep != nil {
		printReport(rep)
		if *out != "" {
			if err := writeReportImages(*out, rep); err != nil {
				return err
			}
		}
	}
	return err
}

func printReport(rep *iconcut.Report) {
	fmt.Printf("image=%s commits=%d regions=%d interrupted=%v reset=%v waits=%d\n",
		rep.ImageID, len(rep.CommittedKeys), len(rep.Stats), rep.Interrupted, rep.SnapshotReset, len(rep.Waits))
	for _, s := range rep.Stats {
		fmt.Printf("region %d area=%d centroid=(%.3f,%.3f) holes=%d bbox=%+v\n", s.ID, s.Area, s.Centroid.X, s.Centroid.Y, s.Holes, s.BBox)
	}
	if rep.ValidationError != nil {
		fmt.Println("validation:", rep.ValidationError)
	}
}

func writeReportImages(dir string, rep *iconcut.Report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if rep.Corrected != nil {
		if err := iconcut.WritePNG(filepath.Join(dir, "corrected.png"), rep.Corrected); err != nil {
			return err
		}
	}
	if rep.Mask != nil {
		if err := iconcut.WriteLabelPNG(filepath.Join(dir, "mask.png"), rep.Mask); err != nil {
			return err
		}
	}
	return nil
}

func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	db := fs.String("db", "checkpoint.db", "sqlite path")
	jsonOut := fs.Bool("json", true, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rep, err := verifier.VerifyDB(context.Background(), *db)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else if rep.OK {
		fmt.Println("OK", rep.CommitsChecked, "commits")
	}
	if !rep.OK {
		os.Exit(2)
	}
	return nil
}

func retryCmd(args []string) error {
	fs := flag.NewFlagSet("retry", flag.ContinueOnError)
	bundle := fs.String("bundle", "", "bundle directory")
	db := fs.String("db", "checkpoint.db", "sqlite path")
	tile := fs.Int("tile", -1, "tile index")
	stage := fs.String("stage", iconcut.StageClean, "stage")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *bundle == "" || *tile < 0 {
		return fmt.Errorf("-bundle and -tile are required")
	}
	b, err := iconcut.LoadBundle(*bundle)
	if err != nil {
		return err
	}
	if err := iconcut.RetryTile(context.Background(), *db, b.Manifest.ImageID, *tile, *stage); err != nil {
		return err
	}
	rep, err := iconcut.RunBundle(context.Background(), b, iconcut.RunOptions{DBPath: *db})
	if rep != nil {
		printReport(rep)
	}
	return err
}

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	workspace := fs.String("workspace", ".ui-workspace", "workspace for generated samples and checkpoints")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv, err := ui.NewServer(*workspace)
	if err != nil {
		return err
	}
	fmt.Println("iconcut UI listening on http://" + *addr)
	return srv.ListenAndServe(*addr)
}
