package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

const (
	exitOK       = 0
	exitInternal = 1
	exitBusiness = 2
	exitUsage    = 64
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return exitUsage
	}
	switch args[0] {
	case "gen":
		return commandGen(args[1:])
	case "run":
		return commandRun(args[1:])
	case "compare":
		return commandCompare(args[1:])
	case "incremental":
		return commandIncremental(args[1:])
	case "anomaly":
		return commandAnomaly(args[1:])
	case "checkpoint":
		return commandCheckpoint(args[1:])
	case "verify":
		return commandVerify(args[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return exitOK
	default:
		return fail(registry.NewError(registry.CodeSchemaField, "unknown command", registry.ErrorContext{Path: args[0]}), exitUsage)
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, `registryctl - deterministic service registry diagnostics

Usage:
  registryctl gen        --out samples [--seed 42]
  registryctl run        --project FILE [--checkpoint FILE] [--until TICK]
                         [--no-resume] [--report FILE] [--step-delay-ms N]
                         [--crash-after-commit TICK]
  registryctl compare    [--samples DIR] [--seed 42]
  registryctl incremental --project FILE --instance ID --field FIELD --value N
                         [--value2 N] [--scale per_mille|per_10k]
                         --at-tick TICK [--committed-through TICK] [--report FILE]
  registryctl anomaly    [--seed 42] [--checkpoint FILE]
  registryctl checkpoint dump --path FILE
  registryctl verify     [--seed 42]

Business failures are emitted as JSON with an E_xxx code. Exit codes:
0 success, 1 internal, 2 business error, 64 usage, 130/143 graceful signals.`)
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func fail(err error, fallbackExit int) int {
	biz, ok := registry.AsError(err)
	if !ok {
		biz = registry.WrapError(registry.CodeInternal, err.Error(), registry.ErrorContext{}, err)
	}
	_ = json.NewEncoder(os.Stderr).Encode(map[string]any{
		"ok":    false,
		"code":  biz.Code,
		"error": biz,
	})
	if fallbackExit == exitBusiness && biz.Code == registry.CodeInternal {
		return exitInternal
	}
	return fallbackExit
}

func emit(value any) int {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fail(err, exitInternal)
	}
	return exitOK
}

func parseFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return registry.WrapError(registry.CodeSchemaField, "invalid command flags", registry.ErrorContext{Path: flags.Name()}, err)
	}
	return nil
}

func loadBundle(samplesDir string, seed int64) (*samplegen.Bundle, error) {
	if samplesDir != "" {
		documents := make(map[registry.SchemaVersion][]byte)
		for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
			path := filepath.Join(samplesDir, string(schema)+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				return samplegen.BuildBundle(seed)
			}
			documents[schema] = data
		}
		first, err := registry.LoadDocument(documents[registry.SchemaV1])
		if err != nil {
			return nil, err
		}
		if first.Truth == nil {
			return nil, registry.NewError(registry.CodeSchemaField, "sample document has no embedded truth", registry.ErrorContext{Path: filepath.Join(samplesDir, "v1.json") + ".truth"})
		}
		return &samplegen.Bundle{Project: first.Project, Truth: *first.Truth, Files: documents}, nil
	}
	return samplegen.BuildBundle(seed)
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return registry.WrapError(registry.CodeCheckpointIO, "cannot write JSON report", registry.ErrorContext{Path: path}, err)
	}
	return nil
}
