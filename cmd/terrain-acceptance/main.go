package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"terrain"
)

func main() {
	dir := flag.String("dir", "artifacts/acceptance", "acceptance output directory")
	flag.Parse()

	report, err := terrain.RunAcceptance(*dir)
	if err != nil {
		log.Fatalf("acceptance runtime error: %v", err)
	}
	for _, c := range report.Checks {
		mark := "PASS"
		if !c.Passed {
			mark = "FAIL"
		}
		fmt.Printf("[%s] %s — %s\n", mark, c.Name, c.Detail)
	}
	fmt.Printf("report: %s/acceptance-report.json\n", *dir)
	if !report.OK {
		os.Exit(1)
	}
}
