package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"layercache"
)

func main() {
	path := flag.String("file", "", "schema document")
	flag.Parse()
	if *path == "" {
		log.Fatal("-file is required")
	}
	raw, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	project, err := layercache.LoadProject(raw)
	if err != nil {
		log.Fatal(err)
	}
	project.SchemaVersion = ""
	project.Config.TimeUnit = ""
	project.Config.CapacityUnit = ""
	project.Config.HeatScale = 0
	project.Config.MigrationScale = 0
	project.Config.WatermarkMode = ""
	for i := range project.Objects {
		for j := range project.Objects[i].Samples {
			project.Objects[i].Samples[j].Watermark = nil
			project.Objects[i].Samples[j].SourceTick = 0
			project.Objects[i].Samples[j].SourceIndex = 0
		}
	}
	out, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))
}
