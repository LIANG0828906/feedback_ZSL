package main

import (
	"flag"
	"fmt"
	"os"

	"saveinterop/internal/generator"
)

func main() {
	out := flag.String("out", "testdata/samples", "output directory")
	seed := flag.Int64("seed", 20261124, "deterministic RNG seed")
	count := flag.Int("count", 6, "number of samples per branch")
	flag.Parse()

	catalog, err := generator.Generate(*out, *seed, *count)
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d samples for branches %v at %s (seed=%d, mapping=%s)\n",
		len(catalog.Samples), catalog.Branches, *out, catalog.Seed, catalog.RuleVersion)
}
