package main

import (
	"flag"
	"fmt"
	"os"

	"multiplexgw/internal/app"
)

func main() {
	out := flag.String("out", "data", "directory for three schema projects and truth")
	seed := flag.Int64("seed", 424242, "deterministic seed (recorded for provenance)")
	flag.Parse()

	bundle, err := app.GenerateSeed(*seed, *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", app.MarshalReport(map[string]any{"ok": false, "error": err.Error()}))
		os.Exit(1)
	}
	fmt.Printf("generated deterministic frame engineering in %s\n", *out)
	fmt.Printf("project=%s events=%d streams=%d sends=%d\n",
		bundle.Canonical.ProjectID, len(bundle.Canonical.Events), len(bundle.Canonical.Streams), len(bundle.Truth.ScheduleTruth))
}
