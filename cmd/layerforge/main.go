package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/generator"
	"layerforge/internal/loader"
	"layerforge/internal/model"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var business *apperr.Error
		if errors.As(err, &business) {
			fmt.Fprintf(os.Stderr, "business_code=%s\n", business.Code)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return apperr.New(apperr.SchemaRequired, "cli", "command", "a subcommand is required")
	}
	switch args[0] {
	case "generate":
		return runGenerate(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "build":
		return runBuild(args[1:])
	case "demo":
		return runDemo(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return apperr.New(apperr.SchemaRequired, "cli", "command", "unknown subcommand "+args[0])
	}
}

func usage() {
	fmt.Fprintln(os.Stdout, "LayerForge deterministic layered-build demo")
	fmt.Fprintln(os.Stdout, "  layerforge generate [-seed 42] [-out artifacts/generated]")
	fmt.Fprintln(os.Stdout, "  layerforge verify   [-seed 42] [-dir artifacts/generated]")
	fmt.Fprintln(os.Stdout, "  layerforge build    -manifest file -db file [budget flags]")
	fmt.Fprintln(os.Stdout, "  layerforge demo     [-seed 42] [-workdir .layerforge-demo] [-json]")
}

func runGenerate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := flags.Int64("seed", generator.DefaultSeed, "deterministic fixture seed")
	out := flags.String("out", "artifacts/generated", "output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	fixture, err := generator.Generate(*seed)
	if err != nil {
		return err
	}
	if err := fixture.Write(*out); err != nil {
		return apperr.Wrap(apperr.CheckpointIO, "generate", *out, err)
	}
	fmt.Fprintf(os.Stdout, "generated seed=%d project=%s hash=%s dir=%s\n", *seed, fixture.Project.ID, fixture.Derivation.ProjectHash, *out)
	for _, name := range sortedMapKeys(fixture.Versions) {
		fmt.Fprintf(os.Stdout, "  %s %d bytes\n", name, len(fixture.Versions[name]))
	}
	fmt.Fprintf(os.Stdout, "  truth.json %d bytes\n", len(fixture.Truth))
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := flags.Int64("seed", generator.DefaultSeed, "deterministic fixture seed")
	dir := flags.String("dir", "artifacts/generated", "generated fixture directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	fixture, err := generator.Generate(*seed)
	if err != nil {
		return err
	}
	matches, err := verifyDirectory(*dir, fixture)
	if err != nil {
		return err
	}
	for _, match := range matches {
		fmt.Fprintf(os.Stdout, "%s schema=%s hash=%s truth=OK\n", match.File, match.Schema, match.Hash)
	}
	fmt.Fprintf(os.Stdout, "verified seed=%d steps=%d layers=%d\n", *seed, len(fixture.Derivation.Steps), len(fixture.Derivation.Layers))
	return nil
}

type versionMatch struct {
	File   string
	Schema string
	Hash   string
}

func verifyDirectory(dir string, fixture *generator.Fixture) ([]versionMatch, error) {
	var matches []versionMatch
	for _, name := range []string{"project.v1.json", "project.v2.json", "project.v3.json"} {
		path := filepath.Join(dir, name)
		loaded, err := loader.LoadFile(path)
		if err != nil {
			return nil, err
		}
		hash := model.SemanticHash(loaded.Project)
		if hash != fixture.Derivation.ProjectHash {
			return nil, apperr.New(apperr.CheckpointIntegrity, "verify", path, "semantic project hash differs from generator truth")
		}
		derivation, err := build.Derive(loaded.Graph)
		if err != nil {
			return nil, err
		}
		if err := build.CompareTruth(derivation, fixture.Derivation); err != nil {
			return nil, err
		}
		matches = append(matches, versionMatch{File: name, Schema: loaded.Schema, Hash: hash})
	}
	truthPath := filepath.Join(dir, "truth.json")
	truth, err := os.ReadFile(truthPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CheckpointIO, "verify", truthPath, err)
	}
	if string(truth) != string(fixture.Truth) {
		return nil, apperr.New(apperr.CheckpointIntegrity, "verify", truthPath, "truth file is not reproducible from seed")
	}
	return matches, nil
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
