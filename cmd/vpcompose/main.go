package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"vpcompose"
	"vpcompose/sample"
)

func main() {
	if len(os.Args) < 2 {
		runDemo(42, "artifacts/demo")
		return
	}
	switch os.Args[1] {
	case "generate":
		fs := flag.NewFlagSet("generate", flag.ExitOnError)
		seed := fs.Int64("seed", 42, "deterministic sample seed")
		out := fs.String("out", "artifacts/samples", "output directory")
		_ = fs.Parse(os.Args[2:])
		generated, err := sample.Write(*seed, *out)
		must(err)
		fmt.Printf("generated seed=%d files=%d dir=%s\n", generated.Seed, len(generated.Files), *out)
	case "demo":
		fs := flag.NewFlagSet("demo", flag.ExitOnError)
		seed := fs.Int64("seed", 42, "deterministic sample seed")
		work := fs.String("work", "artifacts/demo", "demo work directory")
		_ = fs.Parse(os.Args[2:])
		runDemo(*seed, *work)
	case "verify":
		fs := flag.NewFlagSet("verify", flag.ExitOnError)
		seed := fs.Int64("seed", 42, "deterministic sample seed")
		_ = fs.Parse(os.Args[2:])
		temp, err := os.MkdirTemp("", "vpcompose-verify-")
		must(err)
		defer os.RemoveAll(temp)
		runDemo(*seed, temp)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; use generate, demo, or verify\n", os.Args[1])
		os.Exit(2)
	}
}

func runDemo(seed int64, work string) {
	must(os.MkdirAll(work, 0o755))
	sampleDir := filepath.Join(work, "samples")
	generated, err := sample.Write(seed, sampleDir)
	must(err)
	fmt.Printf("SAMPLE seed=%d files=%d dir=%s\n", seed, len(generated.Files), sampleDir)

	projects := make(map[vpcompose.Version]*vpcompose.Project)
	versionFiles := []struct {
		version vpcompose.Version
		name    string
	}{
		{vpcompose.Version1, "project-v1.json"},
		{vpcompose.Version2, "project-v2.json"},
		{vpcompose.Version3, "project-v3.json"},
	}
	for _, entry := range versionFiles {
		project, err := vpcompose.LoadProject(generated.Files[entry.name])
		must(err)
		if err := vpcompose.VerifyTruth(project); err != nil {
			must(err)
		}
		projects[entry.version] = project
		fmt.Printf("LOAD %s source=%s layers=%d nodes=%d truth=exact frameDigest=%s\n",
			entry.version, project.SourceVersion, len(project.Layers), len(project.Nodes),
			frameDigest(project, 0))
	}
	assertProjectShape(projects[vpcompose.Version1], projects[vpcompose.Version3])

	down, err := vpcompose.ExportProject(projects[vpcompose.Version3], vpcompose.Version2)
	must(err)
	middle, err := vpcompose.LoadProject(down.Data)
	must(err)
	up, err := vpcompose.ExportProject(middle, vpcompose.Version3)
	must(err)
	restored, err := vpcompose.LoadProject(up.Data)
	must(err)
	assertLayerState(projects[vpcompose.Version3], restored)
	fmt.Printf("ROUNDTRIP v3->v2->v3 layers=%d alpha=%s exact=true\n",
		len(restored.Layers), restored.Layers[0].SourceAlphaMode)

	lossy, err := vpcompose.ExportProject(projects[vpcompose.Version3], vpcompose.Version1)
	if !vpcompose.IsCode(err, vpcompose.CodeLossy) {
		must(fmt.Errorf("expected E_LOSSY, got %v", err))
	}
	lossyProject, err := vpcompose.LoadProject(lossy.Data)
	must(err)
	assertLayerState(projects[vpcompose.Version3], lossyProject)
	fmt.Printf("LOSSY v3->v1 losses=%d dispositions=%s\n", len(lossy.Losses), lossyDispositions(lossy.Losses))

	realtime, realtimeStore := newEngine(projects[vpcompose.Version3], filepath.Join(work, "realtime.db"), 3)
	must(realtimeStore.Reset())
	_, err = realtime.ComposeWindow(context.Background(), 0)
	must(err)
	progressBeforeEdit := realtime.Progress()
	visible := false
	invalidation, err := realtime.EditLayer(vpcompose.LayerEdit{LayerID: "beauty-glow", Visible: &visible})
	must(err)
	run, err := realtime.Run(context.Background(), vpcompose.ModeRealtime)
	must(err)
	fmt.Printf("INVALIDATE windows=%v layer=beauty-glow committed-before=%d/%d committed-after=%d/%d\n",
		invalidation.WindowIDs(), progressBeforeEdit.Committed, progressBeforeEdit.Total,
		run.Progress.Committed, run.Progress.Total)
	must(realtimeStore.Close())

	blend := vpcompose.BlendAdd

	realtime, realtimeStore = newEngine(projects[vpcompose.Version3], filepath.Join(work, "path-realtime.db"), 3)
	must(realtimeStore.Reset())
	_, err = realtime.EditLayer(vpcompose.LayerEdit{LayerID: "talent-subject", BlendMode: &blend})
	must(err)
	_, err = realtime.Run(context.Background(), vpcompose.ModeRealtime)
	must(err)
	must(realtimeStore.Close())

	recoverStore, err := vpcompose.OpenCheckpoint(filepath.Join(work, "path-recover.db"))
	must(err)
	must(recoverStore.Reset())
	recoverInit, err := vpcompose.NewEngine(projects[vpcompose.Version3], recoverStore, vpcompose.EngineOptions{Workers: 2})
	must(err)
	_, err = recoverInit.EditLayer(vpcompose.LayerEdit{LayerID: "talent-subject", BlendMode: &blend})
	must(err)
	edited := recoverInit.Project()
	_, err = recoverInit.ComposeWindow(context.Background(), 0)
	must(err)
	must(recoverStore.Close())
	recoverStore, err = vpcompose.OpenCheckpoint(filepath.Join(work, "path-recover.db"))
	must(err)
	recovered, err := vpcompose.NewEngine(edited, recoverStore, vpcompose.EngineOptions{Workers: 2})
	must(err)
	recoverRun, err := recovered.Run(context.Background(), vpcompose.ModeRecover)
	must(err)
	fmt.Printf("RECOVERY committed-before=%v total=%d/%d\n",
		recoverRun.Recovery.Committed, recoverRun.Progress.Committed, recoverRun.Progress.Total)
	must(recoverStore.Close())

	full, fullStore := newEngine(projects[vpcompose.Version3], filepath.Join(work, "path-full.db"), 1)
	must(fullStore.Reset())
	_, err = full.EditLayer(vpcompose.LayerEdit{LayerID: "talent-subject", BlendMode: &blend})
	must(err)
	_, err = full.Run(context.Background(), vpcompose.ModeFull)
	must(err)
	must(fullStore.Close())

	comparable := make([]*vpcompose.ComparableRun, 0, 3)
	pathEngines := []struct {
		name   string
		engine *vpcompose.Engine
	}{
		{"realtime", realtime},
		{"recover", recovered},
		{"full", full},
	}
	for _, entry := range pathEngines {
		item, err := entry.engine.Comparable(entry.name)
		must(err)
		comparable = append(comparable, item)
	}
	must(vpcompose.CompareRuns(comparable...))
	fmt.Printf("THREE-PATH PASS windows=%v sampleStats=exact pixels=exact\n", comparable[0].Committed)
}

func newEngine(project *vpcompose.Project, dbPath string, workers int) (*vpcompose.Engine, *vpcompose.CheckpointStore) {
	store, err := vpcompose.OpenCheckpoint(dbPath)
	must(err)
	engine, err := vpcompose.NewEngine(project, store, vpcompose.EngineOptions{Workers: workers})
	must(err)
	return engine, store
}

func frameDigest(project *vpcompose.Project, index int) string {
	frame, err := vpcompose.ComposeFrame(project, index)
	must(err)
	return frame.Digest()[:12]
}

func assertProjectShape(a, b *vpcompose.Project) {
	if fmt.Sprint(a.LayerOrder()) != fmt.Sprint(b.LayerOrder()) {
		panic("layer order differs across formats")
	}
	for i := range a.Layers {
		if a.Layers[i].VisibleUntil != b.Layers[i].VisibleUntil || a.Layers[i].Opacity != b.Layers[i].Opacity {
			panic("layer stack state differs across formats")
		}
	}
}

func assertLayerState(a, b *vpcompose.Project) {
	if fmt.Sprint(a.LayerOrder()) != fmt.Sprint(b.LayerOrder()) {
		panic("round-trip layer order changed")
	}
	for i := range a.Layers {
		if a.Layers[i].Opacity != b.Layers[i].Opacity ||
			a.Layers[i].VisibleUntil != b.Layers[i].VisibleUntil ||
			a.Layers[i].Visible != b.Layers[i].Visible {
			panic("round-trip layer state changed")
		}
	}
}

func lossyDispositions(losses []vpcompose.Loss) string {
	seen := make(map[string]bool)
	for _, loss := range losses {
		seen[loss.Disposition] = true
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return fmt.Sprint(values)
}

func must(err error) {
	if err == nil {
		return
	}
	var ee *vpcompose.EngineError
	if errors.As(err, &ee) {
		fmt.Fprintln(os.Stderr, ee.Error())
	} else {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
