package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"saveinterop/internal/core"
	"saveinterop/internal/generator"
	"saveinterop/internal/interop"
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
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`savectl - deterministic save interoperability verifier

Commands:
  generate [-out DIR] [-seed N] [-count N]
  verify   [-dir DIR] [-db FILE] [-window N] [-sample ID]
`)
}

func runGenerate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := flags.String("out", "testdata/samples", "output directory")
	seed := flags.Int64("seed", 20261124, "deterministic seed")
	count := flags.Int("count", 6, "samples per branch")
	if err := flags.Parse(args); err != nil {
		return err
	}
	catalog, err := generator.Generate(*out, *seed, *count)
	if err != nil {
		return err
	}
	fmt.Printf("GENERATE samples=%d branches=%s seed=%d out=%s\n",
		len(catalog.Samples), strings.Join(catalog.Branches, ","), catalog.Seed, *out)
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := flags.String("dir", "testdata/samples", "generated samples directory")
	dbPath := flags.String("db", "", "checkpoint database path (default <dir>/checkpoints.db)")
	window := flags.Int("window", 3, "semantic entries per checkpoint window")
	onlySample := flags.String("sample", "", "verify only one sample id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		*dbPath = filepath.Join(*dir, "checkpoints.db")
	}
	mappingData, err := os.ReadFile(filepath.Join(*dir, "mappings.json"))
	if err != nil {
		return err
	}
	var table core.MappingTable
	if err := json.Unmarshal(mappingData, &table); err != nil {
		return err
	}
	provider, err := interop.New(table)
	if err != nil {
		return err
	}
	truthData, err := os.ReadFile(filepath.Join(*dir, "truth.json"))
	if err != nil {
		return err
	}
	var truth generator.TruthCatalog
	if err := json.Unmarshal(truthData, &truth); err != nil {
		return err
	}
	branches := make([]string, 0, len(table.Branches))
	for _, branch := range table.Branches {
		branches = append(branches, branch.ID)
	}
	sort.Strings(branches)

	var firstSample string
	var firstBranch string
	var firstFull *interop.LoadResult
	for _, sample := range truth.Samples {
		if *onlySample != "" && sample.ID != *onlySample {
			continue
		}
		if firstSample == "" {
			firstSample = sample.ID
		}
		loadedByBranch := make(map[string]*interop.LoadResult)
		fingerprints := make(map[string]string)
		fmt.Printf("SAMPLE %s\n", sample.ID)
		for _, branch := range branches {
			data, err := os.ReadFile(filepath.Join(*dir, branch, sample.ID+".json"))
			if err != nil {
				return err
			}
			loaded, err := provider.LoadBytes(data, interop.LoadOptions{ExpectedBranch: branch})
			if err != nil {
				return fmt.Errorf("%s/%s load: %w", sample.ID, branch, err)
			}
			if err := core.CompareSemantics(sample.Branches[branch], loaded.Model, branch, "truth"); err != nil {
				return fmt.Errorf("%s/%s truth: %w", sample.ID, branch, err)
			}
			fingerprint, err := core.Fingerprint(loaded.Model, table.Version)
			if err != nil {
				return err
			}
			loadedByBranch[branch] = loaded
			fingerprints[branch] = fingerprint
			if firstBranch == "" {
				firstBranch = branch
				firstFull = loaded
			}
			fmt.Printf("  LOAD branch=%s truth=EQUAL traces=%d fingerprint=%s\n",
				branch, len(loaded.Traces), shortHash(fingerprint))
		}
		for i := 1; i < len(branches); i++ {
			if fingerprints[branches[i]] != fingerprints[branches[0]] {
				return fmt.Errorf("%s branch-independent fingerprint failed: %s != %s",
					sample.ID, fingerprints[branches[0]], fingerprints[branches[i]])
			}
		}
		fmt.Printf("  FINGERPRINT branches=3 equal=YES value=%s\n", shortHash(fingerprints[branches[0]]))

		for _, source := range branches {
			for _, target := range branches {
				verified, err := provider.ExportAndVerify(loadedByBranch[source].Model, interop.ExportOptions{
					SourceBranch: source, TargetBranch: target,
					RuleVersion: table.Version, PreserveConditionals: true,
				})
				if err != nil {
					return fmt.Errorf("%s cross-export %s->%s: %w", sample.ID, source, target, err)
				}
				fmt.Printf("  CROSS %s->%s semeq=EQUAL rulings=%d archive=%s\n",
					source, target, len(verified.Export.Rulings), shortHash(verified.Export.ArchiveHash))
			}
		}
	}
	if firstSample == "" {
		return fmt.Errorf("no samples matched")
	}

	data, err := os.ReadFile(filepath.Join(*dir, firstBranch, firstSample+".json"))
	if err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(*dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	fresh, err := provider.ImportCheckpointed(context.Background(), data,
		interop.LoadOptions{ExpectedBranch: firstBranch},
		interop.CheckpointOptions{DBPath: *dbPath, WindowSize: *window})
	if err != nil {
		return err
	}
	resumed, err := provider.ImportCheckpointed(context.Background(), data,
		interop.LoadOptions{ExpectedBranch: firstBranch},
		interop.CheckpointOptions{DBPath: *dbPath, WindowSize: *window})
	if err != nil {
		return err
	}
	if err := interop.CompareThreePaths(*firstFull, fresh.LoadResult, *window); err != nil {
		return fmt.Errorf("full vs fresh checkpoint: %w", err)
	}
	if err := interop.CompareThreePaths(*firstFull, resumed.LoadResult, *window); err != nil {
		return fmt.Errorf("full vs resumed checkpoint: %w", err)
	}
	fmt.Printf("CHECKPOINT sample=%s branch=%s windows=%d fresh_resumed=%d resume_resumed=%d db=%s\n",
		firstSample, firstBranch, resumed.Windows, fresh.ResumedWindows, resumed.ResumedWindows, *dbPath)
	fmt.Printf("THREE-PATH full=fresh=resumed semantic=EQ traces=EQ rulings=EQ result=EQUAL\n")

	if *onlySample == "" {
		fmt.Printf("VERIFY samples=%d branches=%d cross_exports=%d result=PASS\n",
			len(truth.Samples), len(branches), len(truth.Samples)*len(branches)*len(branches))
	}
	return nil
}

func shortHash(value string) string {
	if len(value) <= 16 {
		return value
	}
	return value[:16]
}
