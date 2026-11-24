package main

import (
	"flag"
	"fmt"
	"log"

	"terrain"
)

func main() {
	seed := flag.Int64("seed", 42, "deterministic sample seed")
	out := flag.String("out", "samples/terrain-seed-42.json", "output project JSON path")
	flag.Parse()

	project := terrain.GenerateSample(*seed)
	if err := terrain.WriteProject(*out, project); err != nil {
		log.Fatalf("write sample: %v", err)
	}
	fmt.Printf("generated %s: %d blocks, %d boundaries, %d fault cases\n",
		*out, len(project.Blocks), len(project.SharedBoundaries), len(project.Truth.FaultCases))
}
