package main

import (
	"flag"
	"fmt"

	"combatjudge/internal/sample"
)

func main() {
	out := flag.String("out", "samples", "output directory for deterministic local battle samples")
	flag.Parse()
	paths, err := sample.GenerateDirectory(*out)
	if err != nil {
		panic(err)
	}
	fmt.Println("generated deterministic samples:")
	for version, path := range paths {
		fmt.Printf("  %s -> %s\n", version, path)
	}
	fmt.Printf("  truth -> %s/truth.json\n", *out)
}
