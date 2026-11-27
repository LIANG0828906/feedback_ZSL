package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"layercache"
)

func main() {
	file := flag.String("file", "samples/v3.json", "schema document")
	at := flag.Int64("time", -1, "logical time to print")
	db := flag.String("db", "", "checkpoint database path")
	flag.Parse()
	raw, err := os.ReadFile(*file)
	if err != nil {
		log.Fatal(err)
	}
	project, err := layercache.LoadProject(raw)
	if err != nil {
		log.Fatal(err)
	}
	engine, err := layercache.NewEngine(project)
	if err != nil {
		log.Fatal(err)
	}
	opts := layercache.RunOptions{}
	if *db != "" {
		opts.Checkpoint = layercache.NewCheckpointStore(*db)
		opts.CheckpointVersion = layercache.CheckpointFormatVersion
	}
	run, err := engine.Run(context.Background(), opts)
	if err != nil {
		log.Fatal(err)
	}
	for _, tick := range run.Ticks {
		if tick.TimeMS%100 != 0 && *at < 0 {
			continue
		}
		if *at >= 0 && tick.TimeMS != *at {
			continue
		}
		out, _ := json.Marshal(tick)
		fmt.Println(string(out))
	}
}
