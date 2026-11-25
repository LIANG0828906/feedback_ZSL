package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"audiobus/internal/graph"
	"audiobus/internal/loader"
	"audiobus/internal/pipeline"
	"audiobus/internal/review"
	"audiobus/internal/store"
)

type verifyReport struct {
	SampleDir          string `json:"sampleDir"`
	Database           string `json:"database"`
	BranchEvents       int    `json:"branchEventsMapped"`
	CanonicalEvents    int    `json:"canonicalEvents"`
	Packets            int    `json:"packets"`
	CrossBranchEqual   bool   `json:"crossBranchEqual"`
	RoundTripEqual     bool   `json:"roundTripEqual"`
	ThreePathsEqual    bool   `json:"threePathsEqual"`
	TruthReview        any    `json:"truthReview"`
	LegacyDBReview     any    `json:"legacyDBReview,omitempty"`
	EditInvalidations  int    `json:"editInvalidations"`
	AddInvalidations   int    `json:"addInvalidations"`
	QuantInvalidations int    `json:"quantInvalidations"`
	CapInvalidations   int    `json:"capInvalidations"`
	FirstInvalidations []any  `json:"firstInvalidations,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify":
		verifyCmd(os.Args[2:])
	case "review":
		reviewCmd(os.Args[2:])
	case "gen":
		genCmd(os.Args[2:])
	case "faults":
		faultsCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`audioctl commands:
  verify  [-samples artifacts/sample] [-db artifacts/db/audiobus-verify.db]
  review  -db artifacts/db/resume.db [-truth artifacts/sample/truth.json]
  gen     [-out artifacts/generated] [-seed 42]
  faults  [-samples artifacts/sample]`)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	sampleDir := fs.String("samples", "artifacts/sample", "deterministic sample directory")
	dbPath := fs.String("db", "artifacts/db/audiobus-verify.db", "sqlite checkpoint path")
	_ = fs.Parse(args)
	ctx := context.Background()

	set, err := loader.LoadSampleSet(*sampleDir)
	fatal(err)
	fatal(loader.CrossBranchEquivalent(set))
	events := loader.CanonicalEvents(set)
	for _, e := range events {
		if _, err := loader.RoundTripConversion(e, set.Manifest); err != nil {
			fatal(err)
		}
	}

	realtime := pipeline.NewEngine(set.Manifest, events, nil)
	full, err := realtime.RunRealtime(ctx)
	fatal(err)

	_ = os.Remove(*dbPath)
	_ = os.Remove(*dbPath + "-wal")
	_ = os.Remove(*dbPath + "-shm")
	st, err := store.Open(*dbPath)
	fatal(err)
	stop := pipeline.NewEngine(set.Manifest, events, st)
	stop.StopAfterWindow = 0
	_, err = stop.RunRealtime(ctx)
	fatal(err)
	fatal(st.Close())

	resume, err := pipeline.Resume(ctx, set.Manifest, events, *dbPath)
	fatal(err)

	replayStore, err := store.OpenReadOnly(*dbPath)
	fatal(err)
	replay, err := pipeline.ReplayFromStore(ctx, replayStore)
	fatal(err)
	fatal(replayStore.Close())
	fatal(pipeline.CompareReports(full, resume, replay, set.Manifest.MaxSamplesPerPack))

	truthPath := *sampleDir + "/truth.json"
	truthReview, err := review.ReviewSQLite(*dbPath, truthPath)
	fatal(err)
	legacy, legacyErr := review.ReviewSQLite("artifacts/db/resume.db", truthPath)

	invalidations := graph.EventEdit(full.Report.Packets, 6)
	addInvalidations := graph.AddedEvent(full.Report.Packets, 2, 2208)
	quantInvalidations := graph.QuantUnitChange(full.Report.Packets, set.Manifest.QuantUnit, set.Manifest.QuantUnit*2)
	capInvalidations := graph.PacketCapChange(full.Report.Packets, set.Manifest.MaxSamplesPerPack, set.Manifest.MaxSamplesPerPack/2)
	first := []any{}
	for i, inv := range invalidations {
		if i >= 6 {
			break
		}
		first = append(first, inv)
	}
	report := verifyReport{
		SampleDir: *sampleDir, Database: *dbPath,
		BranchEvents: len(set.Events), CanonicalEvents: len(events), Packets: len(full.Report.Packets),
		CrossBranchEqual: true, RoundTripEqual: true, ThreePathsEqual: true,
		TruthReview: truthReview, EditInvalidations: len(invalidations),
		AddInvalidations: len(addInvalidations), QuantInvalidations: len(quantInvalidations),
		CapInvalidations: len(capInvalidations), FirstInvalidations: first,
	}
	if legacyErr == nil {
		report.LegacyDBReview = legacy
	}
	writeJSON(report)
}

func reviewCmd(args []string) {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	dbPath := fs.String("db", "artifacts/db/resume.db", "persistent sqlite packet database")
	truthPath := fs.String("truth", "artifacts/sample/truth.json", "truth fingerprints")
	_ = fs.Parse(args)
	result, err := review.ReviewSQLite(*dbPath, *truthPath)
	fatal(err)
	writeJSON(result)
}

func genCmd(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	out := fs.String("out", "artifacts/generated", "output directory")
	seed := fs.Int64("seed", 42, "fixed seed")
	_ = fs.Parse(args)
	fatal(GenerateSample(*out, *seed))
	fmt.Printf("generated deterministic sample seed=%d out=%s\n", *seed, *out)
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
