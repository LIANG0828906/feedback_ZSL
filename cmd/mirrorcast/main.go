package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"mirrorcast/internal/acceptance"
	"mirrorcast/internal/generator"
	"mirrorcast/internal/verify"
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
	case "acceptance":
		err = runAcceptance(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`mirrorcast commands:
  generate [-seed 1118544158] [-out samples/generated]
  verify   [-dir samples/generated] [-json]
  acceptance [-work artifacts/acceptance] [-json]`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", generator.DefaultSeed, "fixed deterministic seed")
	out := fs.String("out", filepath.Join("samples", "generated"), "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	suite, err := generator.GenerateSuite(*seed)
	if err != nil {
		return err
	}
	manifest, report, err := verify.WriteSuiteFiles(suite, *out)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d projects with seed 0x%X (%d) in %s\n", len(manifest.Projects), *seed, *seed, *out)
	fmt.Printf("verification: ok=%v projects=%d critical_cases=%d\n", report.OK, len(report.Projects), len(report.Critical))
	if !report.OK {
		return fmt.Errorf("generated suite failed verification")
	}
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", filepath.Join("samples", "generated"), "generated sample directory")
	asJSON := fs.Bool("json", false, "print full JSON report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := verify.VerifyDirectory(*dir)
	if err != nil {
		return err
	}
	if *asJSON {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("verify %s: ok=%v projects=%d critical=%d\n", *dir, report.OK, len(report.Projects), len(report.Critical))
		for _, project := range report.Projects {
			fmt.Printf("  %s versions=%v truth=%v precision_items=%d\n", project.Name, project.VersionsEquivalent, project.TruthMatches, len(project.Precision))
			for _, mismatch := range project.Mismatches {
				fmt.Println("    -", mismatch)
			}
		}
	}
	if !report.OK {
		return fmt.Errorf("verification failed")
	}
	return nil
}

func runAcceptance(args []string) error {
	fs := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	work := fs.String("work", filepath.Join("artifacts", "acceptance"), "checkpoint/incremental working directory")
	asJSON := fs.Bool("json", false, "print full JSON report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := acceptance.Run(*work)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*work, 0o755); err == nil {
		if data, marshalErr := json.MarshalIndent(report, "", "  "); marshalErr == nil {
			_ = os.WriteFile(filepath.Join(*work, "acceptance-report.json"), data, 0o644)
		}
	}
	if *asJSON {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("acceptance: ok=%v checks=%d\n", report.OK, len(report.Checks))
		for _, check := range report.Checks {
			mark := "PASS"
			if !check.OK {
				mark = "FAIL"
			}
			fmt.Printf("  [%s] %s", mark, check.Name)
			if check.Code != "" {
				fmt.Printf(" (%s)", check.Code)
			}
			fmt.Println()
			if check.Detail != "" && !check.OK {
				fmt.Println("       ", check.Detail)
			}
		}
	}
	if !report.OK {
		return fmt.Errorf("acceptance checks failed")
	}
	return nil
}
