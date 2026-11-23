package main

import (
	"flag"
	"fmt"
	"os"

	"storyworkbench/samplegen"
)

func main() {
	out := flag.String("out", "samples", "output directory for deterministic story projects")
	seed := flag.Int64("seed", 20261124, "base deterministic seed")
	count := flag.Int("count", 3, "number of projects to generate")
	flag.Parse()
	manifest, err := samplegen.GenerateSet(*out, *seed, *count)
	if err != nil {
		fmt.Fprintln(os.Stderr, "storygen:", err)
		os.Exit(1)
	}
	fmt.Printf("generated %d projects in %s (base seed %d)\n", len(manifest.Projects), *out, *seed)
	for _, project := range manifest.Projects {
		fmt.Printf("  %-28s shots=%d choices=%d reachable=%d\n",
			project.ID, project.ShotCount, project.ChoiceCount, project.ReachableCount)
	}
}
