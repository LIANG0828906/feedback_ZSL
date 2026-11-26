package cli

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"skillbudget/budget"
)

// Main is the shared command-line entry. It exits the process on command
// errors via flag.ExitOnError or os.Exit(1).
func Main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		runDemo(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: skillbudget <demo|serve> [flags]")
	fmt.Fprintln(os.Stderr, "  demo  [-seed 42] [-out demo-out]")
	fmt.Fprintln(os.Stderr, "  serve [-dir demo-out] [-addr 127.0.0.1:8080]")
}

func runDemo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	seed := fs.Int64("seed", budget.DefaultSeed, "deterministic generator seed")
	out := fs.String("out", "demo-out", "output directory")
	_ = fs.Parse(args)

	result, err := budget.RunDemo(*seed, *out)
	if err != nil {
		fail(err)
	}

	abs, _ := filepath.Abs(*out)
	fmt.Println("skill budget/cooldown demo completed")
	fmt.Printf("output: %s\n", abs)
	for _, load := range result.Loads {
		fmt.Printf("  loaded %-24s traces=%-3d ticks=%-2d truth=match hash=%s\n",
			load.Version, load.TraceCount, load.TickCount, shortHash(load.Hash))
	}
	fmt.Printf("scale round-trip: %d‱ -> %d‰ residual=%d -> %d‱ exact=%v\n",
		result.ScaleRoundTrip.OriginalMyriad,
		result.ScaleRoundTrip.LowPermille,
		result.ScaleRoundTrip.Residual,
		result.ScaleRoundTrip.RestoredMyriad,
		result.ScaleRoundTrip.Exact,
	)
	fmt.Printf("three paths: realtime/resume/full matched; commits=%d\n", len(result.ThreePaths.Realtime.Commits))
	fmt.Printf("invalidation: reused=%v recomputed=%v\n", result.Incremental.ReusedTicks, result.Incremental.RecomputedTicks)
	fmt.Printf("diagnostics: %s\n", result.DiagnosticsPath)
	fmt.Println("fault probes:")
	for _, probe := range result.Faults {
		status := "PASS"
		if !probe.OK {
			status = "FAIL"
		}
		fmt.Printf("  [%s] %-28s code=%-7s location=%s\n", status, probe.Name, probe.Code, probe.Location)
	}
	if !result.AllFaultsPass {
		os.Exit(1)
	}
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "demo-out", "diagnostics directory")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	seed := fs.Int64("seed", budget.DefaultSeed, "generate demo if directory is missing")
	_ = fs.Parse(args)

	if _, err := os.Stat(filepath.Join(*dir, "diagnostics.html")); err != nil {
		if _, err := budget.RunDemo(*seed, *dir); err != nil {
			fail(err)
		}
	}
	fsrv := http.FileServer(http.Dir(*dir))
	http.Handle("/", fsrv)
	fmt.Printf("diagnostics serving on http://%s/diagnostics.html (dir=%s)\n", *addr, *dir)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		fail(err)
	}
}

func fail(err error) {
	if be, ok := budget.AsError(err); ok {
		fmt.Fprintln(os.Stderr, be.Error())
	} else {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
	}
	os.Exit(1)
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
