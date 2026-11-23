// Command compare runs the pure three-path checkpoint comparison and reports
// the first divergent pixel/field, or E_DIVERGE if the stores differ.
package main

import (
	"flag"

	"vqa/internal/cliutil"
	"vqa/internal/pipeline"
)

func main() {
	storeA := flag.String("a", "store/realtime", "first checkpoint directory")
	storeB := flag.String("b", "store/rebuild", "second checkpoint directory")
	cliutil.Parse()

	if err := pipeline.CompareDirectories(*storeA, *storeB); err != nil {
		cliutil.Fail(err)
	}
	cliutil.EmitJSON(map[string]any{
		"status":  "identical",
		"store_a": *storeA,
		"store_b": *storeB,
	})
}
