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
	"syscall"

	mirror "mirrorlab"
)

type verificationReport struct {
	Seed          int64                       `json:"seed"`
	SessionID     string                      `json:"sessionId"`
	Documents     map[string]string           `json:"documentHashes"`
	Loaded        map[string]bool             `json:"loaded"`
	Equivalent    map[string]bool             `json:"equivalentToCanonical"`
	RoundTrip     map[string]bool             `json:"roundTrip"`
	ThreePaths    threePathSummary            `json:"threePaths"`
	Truth         map[string]truthCheckResult `json:"truth"`
	WindowCount   int                         `json:"windowCount"`
	PacketCount   int                         `json:"packetCount"`
	CommittedHash string                      `json:"lastCheckpointHash"`
}

type threePathSummary struct {
	Matched   bool   `json:"matched"`
	FirstDiff string `json:"firstDiff,omitempty"`
}

type truthCheckResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println("mirrorlab generate -seed 42 -out samples/seed42")
	fmt.Println("mirrorlab demo     -seed 42 -dir artifacts/demo")
	fmt.Println("mirrorlab serve    -seed 42 -dir artifacts/demo -addr 127.0.0.1:8090")
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	out := fs.String("out", "samples/seed42", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sample, err := mirror.Generate(*seed)
	if err != nil {
		return err
	}
	if err := mirror.WriteSample(*out, sample); err != nil {
		return err
	}
	fmt.Printf("generated session=%s seed=%d dir=%s\n", sample.Session.ID, sample.Seed, *out)
	for _, version := range []string{mirror.SchemaV1, mirror.SchemaV2, mirror.SchemaV3} {
		fmt.Printf("  schema %s sha256=%s\n", version, sample.Hashes[version])
	}
	count, bytes := countPackets(sample.Truth)
	fmt.Printf("  truth windows=%d packets=%d bytes=%d\n", len(sample.Truth.Windows), count, bytes)
	return nil
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	dir := fs.String("dir", "artifacts/demo", "verification workspace")
	serve := fs.Bool("serve", false, "serve diagnostics after verification")
	addr := fs.String("addr", "127.0.0.1:8090", "diagnostic listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	app, report, err := verify(*seed, *dir)
	if err != nil {
		return err
	}
	printReport(report)
	if *serve {
		ctx, stop := signalContext()
		defer stop()
		fmt.Printf("diagnostics on http://%s (ctrl-c to stop)\n", *addr)
		return mirror.ServeDiagnostics(ctx, *addr, app)
	}
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic seed")
	dir := fs.String("dir", "artifacts/demo", "verification workspace")
	addr := fs.String("addr", "127.0.0.1:8090", "diagnostic listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	app, report, err := verify(*seed, *dir)
	if err != nil {
		return err
	}
	printReport(report)
	ctx, stop := signalContext()
	defer stop()
	fmt.Printf("diagnostics on http://%s (ctrl-c to stop)\n", *addr)
	return mirror.ServeDiagnostics(ctx, *addr, app)
}

func verify(seed int64, dir string) (*mirror.DiagnosticApp, verificationReport, error) {
	report := verificationReport{
		Seed: seed, Documents: map[string]string{}, Loaded: map[string]bool{},
		Equivalent: map[string]bool{}, RoundTrip: map[string]bool{},
		Truth: map[string]truthCheckResult{},
	}
	sample, err := mirror.Generate(seed)
	if err != nil {
		return nil, report, err
	}
	report.SessionID = sample.Session.ID
	report.Documents = sample.Hashes
	report.WindowCount = len(sample.Session.Windows)
	if err := mirror.WriteSample(filepath.Join(dir, "sample"), sample); err != nil {
		return nil, report, err
	}
	loaded := map[string]*mirror.Session{}
	for _, version := range []string{mirror.SchemaV1, mirror.SchemaV2, mirror.SchemaV3} {
		s, err := mirror.LoadVersion(sample.Documents[version], version)
		if err != nil {
			return nil, report, fmt.Errorf("load schema %s: %w", version, err)
		}
		loaded[version] = s
		report.Loaded[version] = true
		if path, a, b, diff := mirror.FirstCoreDifference(sample.Session, s); diff {
			return nil, report, fmt.Errorf("schema %s not equivalent at %s: %v != %v", version, path, a, b)
		}
		report.Equivalent[version] = true
		if _, _, _, err := mirror.RoundTripSession(s, version); err != nil {
			return nil, report, fmt.Errorf("schema %s round-trip: %w", version, err)
		}
		report.RoundTrip[version] = true
	}
	paths, err := mirror.RunThreePaths(context.Background(), sample.Session, filepath.Join(dir, "checkpoints"), mirror.DefaultBudget())
	if err != nil {
		if paths != nil && paths.FirstDiff != nil {
			report.ThreePaths.FirstDiff = paths.FirstDiff.Error()
		}
		return nil, report, err
	}
	report.ThreePaths.Matched = paths.Matched
	checks := map[string]*mirror.RunResult{
		"realtime": paths.Realtime, "resume": paths.Resume, "replay": paths.Replay,
	}
	for mode, result := range checks {
		err := mirror.CompareResultToTruth(result, sample.Truth)
		if err != nil {
			report.Truth[mode] = truthCheckResult{OK: false, Error: err.Error()}
		} else {
			report.Truth[mode] = truthCheckResult{OK: true}
		}
	}
	packets, _ := countPackets(sample.Truth)
	report.PacketCount = packets
	if paths.Realtime != nil && paths.Realtime.Checkpoint != nil {
		report.CommittedHash = paths.Realtime.Checkpoint.ContentHash
	}
	app := &mirror.DiagnosticApp{
		Sample: sample, Loaded: loaded, Paths: paths,
		TruthCheck: firstTruthError(report.Truth),
	}
	if paths.Realtime != nil {
		app.Checkpoint = paths.Realtime.Checkpoint
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, report, err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, report, err
	}
	if err := os.WriteFile(filepath.Join(dir, "verification.json"), data, 0o644); err != nil {
		return nil, report, err
	}
	return app, report, nil
}

func firstTruthError(checks map[string]truthCheckResult) error {
	for _, mode := range []string{"realtime", "resume", "replay"} {
		if check, ok := checks[mode]; ok && !check.OK {
			return errors.New(mode + ": " + check.Error)
		}
	}
	return nil
}

func countPackets(truth *mirror.Truth) (int, int64) {
	if truth == nil {
		return 0, 0
	}
	count := 0
	var bytes int64
	for _, w := range truth.Windows {
		for _, sc := range w.Screens {
			count += len(sc.Packets)
			for _, p := range sc.Packets {
				bytes += p.Bytes
			}
		}
	}
	return count, bytes
}

func printReport(report verificationReport) {
	fmt.Printf("session=%s seed=%d windows=%d packets=%d\n",
		report.SessionID, report.Seed, report.WindowCount, report.PacketCount)
	for _, version := range []string{mirror.SchemaV1, mirror.SchemaV2, mirror.SchemaV3} {
		fmt.Printf("  schema %s load=%t equivalent=%t roundtrip=%t\n",
			version, report.Loaded[version], report.Equivalent[version], report.RoundTrip[version])
	}
	fmt.Printf("  three paths matched=%t\n", report.ThreePaths.Matched)
	for _, mode := range []string{"realtime", "resume", "replay"} {
		check := report.Truth[mode]
		fmt.Printf("  truth %s ok=%t", mode, check.OK)
		if check.Error != "" {
			fmt.Printf(" error=%s", check.Error)
		}
		fmt.Println()
	}
	fmt.Printf("  checkpoint sha256=%s\n", report.CommittedHash)
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}
