package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"roomreverb/internal/acceptance"
	"roomreverb/internal/config"
	"roomreverb/internal/engine"
	"roomreverb/internal/levelio"
	"roomreverb/internal/model"
	"roomreverb/internal/sample"
	"roomreverb/internal/store"
	"roomreverb/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "validate":
		err = runValidate(os.Args[2:])
	case "solve":
		err = runSolve(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "example-config":
		err = runExampleConfig(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %s", os.Args[1])
	}
	if err != nil {
		if merr, ok := err.(*model.Error); ok {
			data, _ := json.MarshalIndent(merr, "", "  ")
			fmt.Fprintln(os.Stderr, string(data))
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`roomreverb - deterministic room reverb zoning workbench

commands:
  generate       generate fixed-seed level and truth JSON
  validate       validate schema and geometry
  solve          solve chains/DSP, verify truths, write SQLite checkpoints
  serve          start http://127.0.0.1:8088 diagnostic workbench
  verify         run local three-path/full/incremental/recovery acceptance
  example-config write configs/example.json`)
}

func loadConfig(args []string) (config.Config, *flag.FlagSet, error) {
	fs := flag.NewFlagSet("roomreverb", flag.ContinueOnError)
	configPath := fs.String("config", "configs/example.json", "JSON config path")
	levelPath := fs.String("input", "", "level JSON path")
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	addr := fs.String("addr", "", "HTTP listen address")
	profile := fs.String("profile", "", "precision profile: coarse|standard|tight")
	parallel := fs.Int("parallel", 0, "max worker parallelism")
	buffer := fs.Int("buffer", 0, "per-worker sample buffer limit")
	if err := fs.Parse(args); err != nil {
		return config.Default(), fs, err
	}
	cfg, err := config.Load(*configPath)
	if err != nil && !os.IsNotExist(err) {
		return cfg, fs, err
	}
	if *levelPath != "" {
		cfg.LevelPath = *levelPath
	}
	if *dbPath != "" {
		cfg.DatabasePath = *dbPath
	}
	if *addr != "" {
		cfg.ListenAddr = *addr
	}
	if *profile != "" {
		cfg.PrecisionProfile = *profile
	}
	if *parallel > 0 {
		cfg.MaxParallelism = *parallel
	}
	if *buffer > 0 {
		cfg.SampleBufferLimit = *buffer
	}
	return cfg, fs, nil
}

func runGenerate(args []string) error {
	cfg, fs, err := loadConfig(args)
	if err != nil {
		return err
	}
	out := fs.Lookup("input").Value.String()
	if out == "" {
		out = cfg.LevelPath
	}
	seedFlag := fs.Lookup("seed")
	seed := cfg.Seed
	if seedFlag != nil {
		seed = cfg.Seed
	}
	level := sample.Generate(seed)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(level, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("generated %s: %d rooms, %d walls, %d doors, %d sources, %d listeners, %d truth paths\n",
		out, len(level.Rooms), len(level.Walls), len(level.Doors), len(level.Sources), len(level.Listeners), len(level.Truth.Paths))
	return nil
}

func runValidate(args []string) error {
	cfg, _, err := loadConfig(args)
	if err != nil {
		return err
	}
	level, err := levelio.LoadFile(cfg.LevelPath)
	if err != nil {
		return err
	}
	fmt.Printf("OK %s: seed=%d rooms=%d doors=%d truth_paths=%d\n", cfg.LevelPath, level.Seed, len(level.Rooms), len(level.Doors), len(level.Truth.Paths))
	return nil
}

func runSolve(args []string) error {
	cfg, _, err := loadConfig(args)
	if err != nil {
		return err
	}
	level, err := levelio.LoadFile(cfg.LevelPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()
	eng := engine.New(cfg, level, st)
	recovered, invalid, rerr := eng.Recover()
	if rerr != nil {
		return rerr
	}
	if invalid {
		fmt.Println("checkpoint hash/version mismatch: snapshot discarded, running full recompute")
	} else if recovered > 0 {
		fmt.Printf("recovered %d committed rooms from %s\n", recovered, cfg.DatabasePath)
	}
	result, merr := eng.RunBaseline()
	if merr != nil {
		return merr
	}
	fmt.Printf("solved %d paths; room commit order %v\n", len(result.Paths), result.Order)
	for _, path := range result.Paths {
		fmt.Printf("%s chain=%v length=%.3f spl=%.3fdB curve0=%.3fdB\n", path.Key, path.RoomChain, path.Length, path.SPLDB, path.CurveDB[0])
	}
	return nil
}

func runServe(args []string) error {
	cfg, _, err := loadConfig(args)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(cfg.LevelPath); os.IsNotExist(statErr) {
		level := sample.Generate(cfg.Seed)
		data, _ := json.MarshalIndent(level, "", "  ")
		_ = os.MkdirAll(filepath.Dir(cfg.LevelPath), 0o755)
		if err := os.WriteFile(cfg.LevelPath, append(data, '\n'), 0o644); err != nil {
			return err
		}
	}
	level, err := levelio.LoadFile(cfg.LevelPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	eng := engine.New(cfg, level, st)
	recovered, invalid, rerr := eng.Recover()
	if rerr != nil {
		return rerr
	}
	if invalid {
		fmt.Println("checkpoint hash/version mismatch: snapshot discarded")
	}
	snapshot := eng.Snapshot()
	if recovered < snapshot.Total {
		if _, merr := eng.RunBaseline(); merr != nil {
			fmt.Fprintln(os.Stderr, "baseline solve returned an error; page will show room failure:", merr)
		}
	} else {
		fmt.Printf("recovered all %d rooms from checkpoint\n", recovered)
	}
	server := &web.Server{Engine: eng, Addr: cfg.ListenAddr}
	fmt.Printf("room reverb workbench: http://%s\n", cfg.ListenAddr)
	return server.ListenAndServe()
}

func runVerify(_ []string) error {
	report, merr := acceptance.Run()
	data, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(data))
	if merr != nil {
		return merr
	}
	fmt.Fprintln(os.Stderr, "ACCEPTANCE OK: geometry, precision monotonicity, incremental/full/recovery equality, scope and scheduler checks passed")
	return nil
}

func runExampleConfig(args []string) error {
	fs := flag.NewFlagSet("example-config", flag.ContinueOnError)
	out := fs.String("out", "configs/example.json", "output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := config.Default().SaveExample(*out); err != nil {
		return err
	}
	fmt.Println("wrote", *out)
	return nil
}
