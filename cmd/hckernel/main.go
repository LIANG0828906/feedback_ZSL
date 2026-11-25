package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"hckernel"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "gen":
		genCmd(os.Args[2:])
	case "verify":
		verifyCmd(os.Args[2:])
	case "demo":
		demoCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("hckernel gen|verify|demo [flags]")
	fmt.Println("  gen    -out samples/seed-42 [-seed 42]")
	fmt.Println("  verify -dir samples/seed-42 [-report artifacts/verification-report.json]")
	fmt.Println("  demo   [-seed 42]")
}

func genCmd(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	out := fs.String("out", "samples/seed-42", "sample output directory")
	seed := fs.Int64("seed", hckernel.DefaultSeed, "deterministic seed")
	_ = fs.Parse(args)
	truth, err := hckernel.WriteSampleFiles(*out, *seed)
	if err != nil {
		fatal("gen failed", err)
	}
	fmt.Printf("generated seed=%d project=%s instances=%d rounds with truth at %s/truth.json\n",
		truth.Seed, truth.Fingerprint[:12], len(truth.InstanceIDs), *out)
}

func verifyCmd(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dir := fs.String("dir", "samples/seed-42", "sample directory")
	reportPath := fs.String("report", "artifacts/verification-report.json", "report output path")
	_ = fs.Parse(args)
	report, err := hckernel.VerifyDirectory(*dir)
	if err != nil {
		fatal("verify failed to run", err)
	}
	if *reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
			fatal("cannot create report directory", err)
		}
		data, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*reportPath, data, 0o644); err != nil {
			fatal("cannot write report", err)
		}
	}
	printReport(report)
	if !report.OK {
		os.Exit(1)
	}
}

func demoCmd(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	seed := fs.Int64("seed", hckernel.DefaultSeed, "deterministic seed")
	out := fs.String("out", "samples/seed-42", "sample output directory")
	_ = fs.Parse(args)
	if _, err := hckernel.WriteSampleFiles(*out, *seed); err != nil {
		fatal("sample generation failed", err)
	}
	report, err := hckernel.VerifyDirectory(*out)
	if err != nil {
		fatal("verification failed to run", err)
	}
	if err := os.MkdirAll("artifacts", 0o755); err != nil {
		fatal("artifacts dir failed", err)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile("artifacts/verification-report.json", data, 0o644); err != nil {
		fatal("report write failed", err)
	}
	printReport(report)
	if !report.OK {
		os.Exit(1)
	}
}

func printReport(r *hckernel.VerificationReport) {
	fmt.Printf("verification ok=%v seed=%d rounds=%d observations=%d dispatched=%d queue_peak=%d drops=%d checks=%d\n",
		r.OK, r.Seed, r.RoundCount, r.ObservationCount, r.DispatchCount, r.QueuePeak, r.DropCount, len(r.Checks))
	for _, c := range r.Checks {
		mark := "PASS"
		if !c.OK {
			mark = "FAIL"
		}
		if c.Code != "" {
			fmt.Printf("  [%s] %-42s %s %s\n", mark, c.Name, c.Code, c.Detail)
		} else {
			fmt.Printf("  [%s] %s\n", mark, c.Name)
		}
	}
}

func fatal(msg string, err error) {
	if he, ok := err.(*hckernel.Error); ok {
		fmt.Fprintf(os.Stderr, "%s: %s (%s)\n", msg, he.Code, he.Error())
	} else {
		fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	}
	os.Exit(1)
}
