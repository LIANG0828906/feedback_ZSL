package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"macrokit"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "inject":
		err = runInject(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "macrotool: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`macrotool - deterministic combat macro domain example

Commands:
  generate [-seed 20261125] [-out examples/sample-projects]
  verify   [-dir examples/sample-projects] [-data data] [-window 4]
  inject   [-case all|schema|ref|overflow|conflict|diff|fault-window|corrupt-checkpoint]

All state is local; SQLite uses the pure-Go modernc.org/sqlite driver.`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", macrokit.FixedSampleSeed, "deterministic generator seed")
	out := fs.String("out", filepath.Join("examples", "sample-projects"), "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set := macrokit.GenerateSamples(*seed)
	written, err := macrokit.WriteSampleSet(*out, set)
	if err != nil {
		return err
	}
	fmt.Printf("generated seed=%d projects=%d root=%s\n", *seed, len(written), *out)
	for _, w := range written {
		fmt.Printf("  - %s\n", w.Dir)
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", filepath.Join("examples", "sample-projects"), "generated sample directory")
	dataDir := fs.String("data", "data", "writable SQLite data directory")
	window := fs.Int64("window", 4, "frames per checkpoint window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(*dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	verified := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := filepath.Join(*dir, entry.Name())
		if _, err := os.Stat(filepath.Join(projectDir, "truth.json")); err != nil {
			continue
		}
		if err := verifyProject(projectDir, *dataDir, *window); err != nil {
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		fmt.Printf("PASS %s\n", entry.Name())
		verified++
	}
	if verified == 0 {
		return fmt.Errorf("no sample projects found under %s; run generate first", *dir)
	}
	fmt.Printf("VERIFY PASS projects=%d\n", verified)
	return nil
}

func verifyProject(projectDir, dataDir string, window int64) error {
	truth, err := macrokit.LoadTruthFile(filepath.Join(projectDir, "truth.json"))
	if err != nil {
		return err
	}
	events, err := macrokit.LoadEventStreamFile(filepath.Join(projectDir, "events.json"))
	if err != nil {
		return err
	}
	formatFiles := []struct {
		no   int
		name string
	}{
		{macrokit.FormatFlatFrameV1, "flat-frame-v1.json"},
		{macrokit.FormatNestedMSV2, "nested-ms-v2.json"},
		{macrokit.FormatTreeFrameV3, "tree-frame-v3.json"},
	}
	cfg := macrokit.ExecutorConfig{FrameCount: truth.FrameCount}
	var reference *macrokit.RunResult
	var firstProject *macrokit.Project
	for i, ff := range formatFiles {
		project, err := macrokit.LoadProjectFile(filepath.Join(projectDir, ff.name))
		if err != nil {
			return err
		}
		result, err := macrokit.RunProject(project, events, cfg)
		if err != nil {
			return fmt.Errorf("format %d run: %w", ff.no, err)
		}
		if err := macrokit.VerifyTruth(result, truth); err != nil {
			return fmt.Errorf("format %d truth: %w", ff.no, err)
		}
		realtime, err := macrokit.RunRealtime(project, events, cfg)
		if err != nil {
			return err
		}
		if err := macrokit.CompareResults(result, realtime); err != nil {
			return fmt.Errorf("realtime parity format %d: %w", ff.no, err)
		}
		if reference == nil {
			reference = result
			firstProject = project
		} else if err := macrokit.CompareResults(reference, result); err != nil {
			return fmt.Errorf("cross-format parity format %d: %w", ff.no, err)
		}
		_ = i
	}

	dbPath := filepath.Join(dataDir, filepath.Base(projectDir)+".sqlite")
	_ = os.Remove(dbPath)
	store, err := macrokit.OpenSQLiteStore(dbPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	stop := int64(0)
	first, err := macrokit.RunCheckpointed(ctx, store, firstProject, events, cfg, window, nil, &stop)
	if err != nil {
		_ = store.Close()
		return err
	}
	if !first.Stopped {
		_ = store.Close()
		return fmt.Errorf("checkpoint did not stop at window 0")
	}
	if err := store.Close(); err != nil {
		return err
	}
	reopened, err := macrokit.OpenSQLiteStore(dbPath)
	if err != nil {
		return err
	}
	defer reopened.Close()
	resumed, err := macrokit.RunCheckpointed(ctx, reopened, firstProject, events, cfg, window, nil, nil)
	if err != nil {
		return err
	}
	if err := macrokit.CompareResults(reference, resumed.Result); err != nil {
		return fmt.Errorf("checkpoint resume parity: %w", err)
	}
	return nil
}

func runInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	caseName := fs.String("case", "all", "injection case")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cases := []string{"schema", "ref", "overflow", "conflict", "diff", "fault-window", "corrupt-checkpoint"}
	if *caseName != "all" {
		cases = []string{*caseName}
	}
	for _, name := range cases {
		domainErr, err := executeInjection(name)
		if err != nil {
			return fmt.Errorf("injection %s did not produce domain error: %w", name, err)
		}
		payload, _ := json.MarshalIndent(domainErr, "", "  ")
		fmt.Printf("INJECT PASS %s -> %s\n%s\n", name, domainErr.Code, string(payload))
	}
	return nil
}

func executeInjection(name string) (*macrokit.Error, error) {
	set := macrokit.GenerateSamples(macrokit.FixedSampleSeed)
	ctx := context.Background()

	switch name {
	case "schema":
		p := set.Projects[0].Project
		doc := exportMap(p, macrokit.FormatFlatFrameV1)
		doc["format"] = 99
		_, err := macrokit.LoadProjectBytes(marshalMap(doc), "injected.json")
		return expectDomain(err, macrokit.ESchema)

	case "ref":
		p := set.Projects[0].Project
		doc := exportMap(p, macrokit.FormatFlatFrameV1)
		branch := doc["branches"].([]any)[0].(map[string]any)
		branch["when_ref"] = "injected-missing-ref"
		_, err := macrokit.LoadProjectBytes(marshalMap(doc), "injected.json")
		return expectDomain(err, macrokit.ERef)

	case "overflow":
		sample := findCLISample(set, "priority-conflict")
		for _, branch := range sample.Project.Branches {
			if branch.ID == "p_high" {
				branch.DelayFrames = math.MaxInt64
			}
		}
		_, err := macrokit.ExportProject(sample.Project, macrokit.FormatNestedMSV2)
		return expectDomain(err, macrokit.EOvf)

	case "conflict":
		sample := findCLISample(set, "priority-conflict")
		for _, branch := range sample.Project.Branches {
			if branch.ID == "p_high" || branch.ID == "p_low" {
				branch.Priority = 1
				branch.HasPriority = true
			}
		}
		_, err := macrokit.RunProject(sample.Project, sample.Events, macrokit.ExecutorConfig{
			FrameCount: sample.Truth.FrameCount, Arbitration: macrokit.ArbitrationStrict,
		})
		return expectDomain(err, macrokit.EConflict)

	case "diff":
		sample := findCLISample(set, "basic-timing")
		want, err := macrokit.RunProject(sample.Project, sample.Events, macrokit.ExecutorConfig{FrameCount: sample.Truth.FrameCount})
		if err != nil {
			return nil, err
		}
		got, err := macrokit.RunProject(sample.Project, sample.Events, macrokit.ExecutorConfig{FrameCount: sample.Truth.FrameCount})
		if err != nil {
			return nil, err
		}
		for i := range got.Frames {
			if len(got.Frames[i].ExecutedSteps) > 0 {
				got.Frames[i].ExecutedSteps[0].Action = "injected-drift"
				break
			}
		}
		return expectDomain(macrokit.CompareResults(want, got), macrokit.EDiff)

	case "fault-window":
		sample := findCLISample(set, "basic-timing")
		dbPath := filepath.Join("data", "inject-fault.sqlite")
		_ = os.MkdirAll("data", 0o755)
		_ = os.Remove(dbPath)
		store, err := macrokit.OpenSQLiteStore(dbPath)
		if err != nil {
			return nil, err
		}
		defer store.Close()
		faults := &macrokit.FaultInjector{FailAttempts: map[int64]int{1: 1}}
		_, err = macrokit.RunCheckpointed(ctx, store, sample.Project, sample.Events, macrokit.ExecutorConfig{FrameCount: sample.Truth.FrameCount}, 4, faults, nil)
		return expectDomain(err, macrokit.EFault)

	case "corrupt-checkpoint":
		sample := findCLISample(set, "basic-timing")
		dbPath := filepath.Join("data", "inject-corrupt.sqlite")
		_ = os.MkdirAll("data", 0o755)
		_ = os.Remove(dbPath)
		store, err := macrokit.OpenSQLiteStore(dbPath)
		if err != nil {
			return nil, err
		}
		cfg := macrokit.ExecutorConfig{FrameCount: sample.Truth.FrameCount}
		if _, err := macrokit.RunCheckpointed(ctx, store, sample.Project, sample.Events, cfg, 4, nil, nil); err != nil {
			_ = store.Close()
			return nil, err
		}
		if err := store.CorruptWindowHash(ctx, sample.Project.ID, 0); err != nil {
			_ = store.Close()
			return nil, err
		}
		_, err = store.LoadSnapshot(ctx, sample.Project.ID, sample.Project.FormatNo, sample.Project.FormatVersion)
		_ = store.Close()
		return expectDomain(err, macrokit.ECorrupt)

	default:
		return nil, fmt.Errorf("unknown injection case %q", name)
	}
}

func expectDomain(err error, code macrokit.ErrorCode) (*macrokit.Error, error) {
	var domainErr *macrokit.Error
	if !errors.As(err, &domainErr) {
		return nil, fmt.Errorf("expected %s, got %w", code, err)
	}
	if domainErr.Code != code {
		return nil, fmt.Errorf("expected %s, got %s (%v)", code, domainErr.Code, err)
	}
	return domainErr, nil
}

func findCLISample(set *macrokit.SampleSet, slug string) *macrokit.SampleProject {
	for _, sample := range set.Projects {
		if sample.Slug == slug {
			return sample
		}
	}
	panic("missing sample " + slug)
}

func exportMap(p *macrokit.Project, format int) map[string]any {
	data, err := macrokit.ExportProject(p, format)
	if err != nil {
		panic(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		panic(err)
	}
	return doc
}

func marshalMap(doc map[string]any) []byte {
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}
