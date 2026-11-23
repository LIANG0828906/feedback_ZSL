// gensamples deterministically generates sample media projects.
//
// Usage: gensamples [-out DIR] [-count N] [-seed S]
package main

import (
	"flag"
	"fmt"
	"os"

	"captionx/internal/samplegen"
)

func main() {
	out := flag.String("out", "samples", "output directory")
	count := flag.Int("count", 3, "number of projects")
	seed := flag.Int64("seed", 20261124, "base random seed")
	flag.Parse()
	dirs, err := samplegen.Generate(samplegen.Options{OutDir: *out, Count: *count, Seed: *seed})
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
	for _, d := range dirs {
		fmt.Println("generated", d)
	}
}
