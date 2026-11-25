package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"example.com/multiscreen-cast/samplegen"
)

func main() {
	var options samplegen.Options
	var output string
	flag.Int64Var(&options.Seed, "seed", 42, "deterministic sample seed")
	flag.IntVar(&options.Screens, "screens", 4, "controlled screen count (1-8)")
	flag.IntVar(&options.Windows, "windows", 12, "frame window count")
	flag.IntVar(&options.Sessions, "sessions", 3, "session count")
	flag.StringVar(&output, "out", "samples/sample.json", "output sample JSON path")
	flag.Parse()

	set, err := samplegen.WriteFile(output, options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "castgen: %v\n", err)
		os.Exit(1)
	}
	payload, _ := json.MarshalIndent(struct {
		OK       bool   `json:"ok"`
		Path     string `json:"path"`
		Sessions int    `json:"sessions"`
		Windows  int    `json:"windows"`
		Screens  int    `json:"screens"`
		Seed     int64  `json:"seed"`
	}{
		OK: true, Path: output, Sessions: len(set.Sessions), Windows: options.Windows, Screens: options.Screens, Seed: options.Seed,
	}, "", "  ")
	fmt.Println(string(payload))
	if len(set.Sessions) == 0 {
		fmt.Fprintln(os.Stderr, "castgen: no sessions generated")
		os.Exit(1)
	}
}
