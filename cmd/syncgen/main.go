package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"syncforge"
)

func main() {
	out := flag.String("out", "samples/demo.json", "output sample path")
	seed := flag.Uint64("seed", 0x5eedc0de, "deterministic sample seed")
	players := flag.Int("players", 4, "number of players")
	ticks := flag.Int("ticks", 180, "maximum ticks")
	flag.Parse()

	sample, err := syncgame.Generate(syncgame.GeneratorOptions{
		Seed:      *seed,
		Players:   *players,
		MaxTicks:  *ticks,
		TickRate:  30,
		RenderFPS: 30,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	file, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(sample); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (seed=%d, players=%d, ticks=%d, final=%d, winner=%s)\n",
		*out, sample.Seed, len(sample.Units), sample.MaxTicks,
		sample.Truth.Settlement.LockedTick, sample.Truth.Settlement.Winner)
}
