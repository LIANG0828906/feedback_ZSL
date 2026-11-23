package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"levelsession/internal/level"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "sample":
		code = cmdSample(os.Args[2:])
	case "run":
		code = cmdRun(os.Args[2:])
	case "resume":
		code = cmdResume(os.Args[2:])
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Println(`levelsession - deterministic level-generation editing session

Usage:
  levelsession sample -scenario basic|budget|conn|conn-fail|hist -out DIR
  levelsession run    -sample DIR -store DIR [-report DIR]
  levelsession resume -sample DIR -store DIR`)
}

func fail(err error) int {
	if e, ok := err.(*level.Error); ok {
		data, _ := json.Marshal(e)
		fmt.Fprintf(os.Stderr, "error: %s\n", data)
		return 1
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

func cmdSample(args []string) int {
	fs := flag.NewFlagSet("sample", flag.ContinueOnError)
	scenario := fs.String("scenario", "basic", "sample scenario")
	out := fs.String("out", "samples/basic", "output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := level.WriteSample(*out, *scenario); err != nil {
		return fail(err)
	}
	data, _ := os.ReadFile(filepath.Join(*out, "meta.json"))
	fmt.Printf("sample generated: %s\n", data)
	return 0
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	sampleDir := fs.String("sample", "samples/basic", "sample directory")
	storeDir := fs.String("store", "artifacts/store", "commit store directory")
	reportDir := fs.String("report", "artifacts/report", "report output directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	proj, err := level.LoadProject(filepath.Join(*sampleDir, "project.json"))
	if err != nil {
		return fail(err)
	}
	ops, err := level.LoadOps(filepath.Join(*sampleDir, "ops.json"))
	if err != nil {
		return fail(err)
	}
	if err := level.ValidateOps(proj, ops); err != nil {
		return fail(err)
	}
	truthPath := filepath.Join(*sampleDir, "truth.json")
	if data, rerr := os.ReadFile(truthPath); rerr == nil {
		var truth level.Truth
		if err := json.Unmarshal(data, &truth); err != nil {
			return fail(err)
		}
		if err := level.CheckTruth(proj, &truth); err != nil {
			return fail(err)
		}
		fmt.Println("[1/5] truth manifest check: OK")
	} else {
		fmt.Println("[1/5] no truth manifest, skipped")
	}

	if err := os.MkdirAll(*reportDir, 0o755); err != nil {
		return fail(err)
	}
	store, err := level.OpenStore(*storeDir)
	if err != nil {
		return fail(err)
	}
	sess, err := level.OpenSession(proj, ops, store, nil)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("[2/5] session opened: %d committed op(s) resumed\n", len(sess.Commits())-1)

	previews := 0
	for sess.HasNext() {
		op := sess.NextOp()
		preview, err := sess.PreviewOp(op)
		if err != nil {
			return fail(err)
		}
		reportPath := filepath.Join(*reportDir, fmt.Sprintf("diverge-op%03d.json", op.Seq))
		if _, err := sess.FinalizePreview(preview, reportPath); err != nil {
			return fail(err)
		}
		commit, err := sess.ApplyNextOp()
		if err != nil {
			return fail(err)
		}
		reused := 0
		for _, e := range commit.Manifest.Entries {
			if e.Reused {
				reused++
			}
		}
		fmt.Printf("  op %2d %-20s p%-2d preview=%6.2fms affected=%d reused=%d connected=%v resources=%d\n",
			op.Seq, op.Kind, op.Partition, preview.ElapsedMs,
			len(commit.Manifest.Entries)-reused, reused,
			commit.Manifest.Connected, commit.Manifest.GlobalResourceTotal)
		previews++
	}
	fmt.Printf("[3/5] incremental edit + preview/finalize: %d op(s) applied\n", previews)

	mid := 0
	if len(ops) > 1 {
		mid = len(ops) / 2
	}
	if err := level.CompareThreePaths(sess, mid); err != nil {
		return fail(err)
	}
	fmt.Printf("[4/5] three-path byte-identical compare (incremental vs rollback-replay@%d vs full regen): OK\n", mid)

	manifests := make([]*level.Manifest, 0, len(sess.Commits()))
	for _, c := range sess.Commits() {
		manifests = append(manifests, c.Manifest)
	}
	data, _ := json.MarshalIndent(manifests, "", "  ")
	if err := os.WriteFile(filepath.Join(*reportDir, "manifests.json"), data, 0o644); err != nil {
		return fail(err)
	}
	fmt.Printf("[5/5] manifests exported to %s\n", filepath.Join(*reportDir, "manifests.json"))
	fmt.Println("RUN OK")
	return 0
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	sampleDir := fs.String("sample", "samples/basic", "sample directory")
	storeDir := fs.String("store", "artifacts/store", "commit store directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	proj, err := level.LoadProject(filepath.Join(*sampleDir, "project.json"))
	if err != nil {
		return fail(err)
	}
	ops, err := level.LoadOps(filepath.Join(*sampleDir, "ops.json"))
	if err != nil {
		return fail(err)
	}
	if err := level.ValidateOps(proj, ops); err != nil {
		return fail(err)
	}
	store, err := level.OpenStore(*storeDir)
	if err != nil {
		return fail(err)
	}
	sess, err := level.OpenSession(proj, ops, store, nil)
	if err != nil {
		return fail(err)
	}
	fmt.Printf("resumed at committed op %d of %d\n", len(sess.Commits())-1, len(ops))
	for sess.HasNext() {
		if _, err := sess.ApplyNextOp(); err != nil {
			return fail(err)
		}
	}
	fmt.Printf("applied remaining ops; now at committed op %d\n", len(sess.Commits())-1)
	return 0
}
