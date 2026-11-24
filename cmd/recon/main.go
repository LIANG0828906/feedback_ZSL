// Command recon is the runnable end-to-end example of the collection-point
// reconstruction pipeline. It works fully offline on deterministic samples.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"collectionrecon/internal/checkpoint"
	"collectionrecon/internal/errors"
	"collectionrecon/internal/generator"
	"collectionrecon/internal/loader"
	"collectionrecon/internal/model"
	"collectionrecon/internal/pipeline"
	"collectionrecon/internal/validate"
	"collectionrecon/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var err error
	switch cmd {
	case "sample":
		err = runSample(fs, os.Args[2:])
	case "run":
		err = runRun(fs, os.Args[2:])
	case "incremental":
		err = runIncremental(fs, os.Args[2:])
	case "recover":
		err = runRecover(fs, os.Args[2:])
	case "serve":
		err = runServe(fs, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		if code := errors.Code(err); code != "" {
			os.Exit(1)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `recon - offline collection point reconstruction pipeline

usage:
  recon sample        [-out sample.json]
  recon run           [-sample sample.json] [-db recon.db] [-out report.json]
  recon incremental   [-sample sample.json] [-db recon.db] [-zone 2] [-nocollect]
  recon recover       [-sample sample.json] [-db recon.db]
  recon serve         [-sample sample.json] [-db recon.db] [-addr 127.0.0.1:8080]
`)
}

func generateOrLoad(samplePath string) (*model.Region, error) {
	if samplePath == "" {
		return generator.Generate(generator.DefaultConfig()), nil
	}
	raw, err := os.ReadFile(samplePath)
	if err != nil {
		return nil, err
	}
	return loader.Load(raw)
}

func runSample(fs *flag.FlagSet, args []string) error {
	out := fs.String("out", "sample.json", "output path for generated sample")
	if err := fs.Parse(args); err != nil {
		return err
	}
	region := generator.Generate(generator.DefaultConfig())
	data, err := json.MarshalIndent(region, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("generated region %s: zones=%d truthPoints=%d sampleCells=%d observations=%d -> %s\n",
		region.Name, len(region.Zones), len(region.Truth), len(region.SampleGrid),
		len(region.Observations), *out)
	return nil
}

func fullPipeline(region *model.Region, dbPath string) (pipeline.RunOutcome, *pipeline.Engine, []model.BoundRecord, []validate.ZoneCheck, error) {
	engine := pipeline.NewEngine(region, pipeline.DefaultLimits())
	cache := pipeline.NewMemoryCache()
	outcome, err := engine.FullRebuild(cache)
	if err != nil {
		return outcome, engine, nil, nil, err
	}
	bounds, err := pipeline.BuildBoundsReport(region, outcome.Results)
	if err != nil {
		return outcome, engine, nil, nil, err
	}
	checks := validate.CheckRegion(region, outcome.Results, validate.DefaultTolerances())
	if dbPath != "" {
		if err := persistCheckpoints(dbPath, region, outcome.Results); err != nil {
			return outcome, engine, nil, nil, err
		}
	}
	return outcome, engine, bounds, checks, nil
}

func persistCheckpoints(dbPath string, region *model.Region, results []model.ZoneResult) error {
	store, err := checkpoint.Open(dbPath, pipeline.AlgoVersion)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Reset(); err != nil {
		return err
	}
	for seq, result := range results {
		if err := store.Save(seq, result); err != nil {
			return err
		}
	}
	return nil
}

func runRun(fs *flag.FlagSet, args []string) error {
	sample := fs.String("sample", "", "path to sample json (default: regenerate)")
	db := fs.String("db", "recon.db", "sqlite checkpoint path")
	out := fs.String("out", "report.json", "report output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	region, err := generateOrLoad(*sample)
	if err != nil {
		return err
	}
	outcome, engine, bounds, checks, err := fullPipeline(region, *db)
	if err != nil {
		return err
	}
	report := model.PipelineReport{
		RegionID:      region.ID,
		Results:       outcome.Results,
		Invalidations: outcome.Invalidations,
		HitSequence:   outcome.HitSequence,
		Bounds:        bounds,
		AlgoVersion:   pipeline.AlgoVersion,
	}
	if err := writeJSON(*out, report); err != nil {
		return err
	}
	fmt.Print(validate.Summary(checks))
	fmt.Printf("all tolerances passed: %v\n", validate.AllPassed(checks, validate.DefaultTolerances()))
	fmt.Printf("queue events: %d hitSequence=%v -> report %s, db %s\n",
		len(engine.QueueLog()), outcome.HitSequence, *out, *db)
	return nil
}

func runIncremental(fs *flag.FlagSet, args []string) error {
	sample := fs.String("sample", "", "path to sample json")
	db := fs.String("db", "recon.db", "sqlite checkpoint path")
	zone := fs.Int("zone", 2, "zone id whose parameter is edited")
	moveNoCollect := fs.Bool("nocollect", false, "drag the no-collect boundary instead of editing density")
	if err := fs.Parse(args); err != nil {
		return err
	}
	region, err := generateOrLoad(*sample)
	if err != nil {
		return err
	}

	// Baseline full build plus persisted checkpoints.
	baseOutcome, _, _, _, err := fullPipeline(region, *db)
	if err != nil {
		return err
	}

	edited := cloneRegion(region)
	changes := make([]pipeline.Change, 0, 1)
	if *moveNoCollect {
		for i := range edited.NoCollect {
			if edited.NoCollect[i].ZoneID == *zone {
				edited.NoCollect[i].Bounds.MinX += 4
				edited.NoCollect[i].Bounds.MaxX += 4
			}
		}
		changes = append(changes, pipeline.Change{
			ZoneID: *zone, Node: pipeline.NodeNoCollect,
			Reason: "no-collect boundary dragged +4 on x",
		})
	} else {
		for i := range edited.Zones {
			if edited.Zones[i].ID == *zone {
				edited.Zones[i].TrueDensity *= 1.15
				edited.Zones[i].ParamVersion++
			}
		}
		changes = append(changes, pipeline.Change{
			ZoneID: *zone, Node: pipeline.NodeZoneParams,
			Reason: "zone density parameter edited",
		})
	}

	engine := pipeline.NewEngine(edited, pipeline.DefaultLimits())
	cache := pipeline.NewMemoryCache()
	cache.Seed(baseOutcome.Results)
	inc, err := engine.IncrementalRebuild(cache, changes)
	if err != nil {
		return err
	}

	// Fresh full rebuild from an empty cache for the edited content.
	fullEngine := pipeline.NewEngine(edited, pipeline.DefaultLimits())
	full, err := fullEngine.FullRebuild(pipeline.NewMemoryCache())
	if err != nil {
		return err
	}
	if _, err := pipeline.CompareOutcomes(inc, full); err != nil {
		return err
	}
	// Repeating the incremental path must reproduce the same hit sequence.
	cache2 := pipeline.NewMemoryCache()
	cache2.Seed(baseOutcome.Results)
	inc2, err := pipeline.NewEngine(edited, pipeline.DefaultLimits()).
		IncrementalRebuild(cache2, changes)
	if err != nil {
		return err
	}
	if err := pipeline.CompareHitSequence(inc, inc2); err != nil {
		return err
	}
	fmt.Println("incremental vs full rebuild: point sets and heat pixels identical; hit sequence stable across repeated incremental runs")
	for _, entry := range inc.Invalidations {
		fmt.Printf("zone %d reused=%v reason=%s\n", entry.ZoneID, entry.Reused, entry.Reason)
	}
	fmt.Printf("incremental hit sequence: %v\n", inc.HitSequence)

	// Persist the new edited checkpoints atomically.
	if err := persistCheckpoints(*db, edited, inc.Results); err != nil {
		return err
	}
	bounds, err := pipeline.BuildBoundsReport(edited, inc.Results)
	if err != nil {
		return err
	}
	report := model.PipelineReport{
		RegionID: edited.ID, Results: inc.Results,
		Invalidations: inc.Invalidations, HitSequence: inc.HitSequence,
		Bounds: bounds, AlgoVersion: pipeline.AlgoVersion,
	}
	return writeJSON("incremental-report.json", report)
}

func runRecover(fs *flag.FlagSet, args []string) error {
	sample := fs.String("sample", "", "path to sample json")
	db := fs.String("db", "recon.db", "sqlite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	region, err := generateOrLoad(*sample)
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*db, pipeline.AlgoVersion)
	if err != nil {
		return err
	}
	defer store.Close()

	expected := make(map[int]checkpoint.ExpectedVersion)
	for _, zone := range region.Zones {
		expected[zone.ID] = checkpoint.ExpectedVersion{
			ContentHash:  pipeline.ZoneContentHash(region, zone.ID),
			ParamVersion: zone.ParamVersion,
		}
	}
	loaded, err := store.Verify(expected)
	if err != nil {
		if errors.Code(err) == errors.EVer {
			fmt.Printf("version mismatch detected, invalidating all checkpoints: %v\n", err)
			if err := store.Reset(); err != nil {
				return err
			}
			_, _, _, _, err = fullPipeline(region, *db)
			return err
		}
		return err
	}
	fmt.Printf("recovered %d zone checkpoints; no recomputation required\n", len(loaded))
	return nil
}

func runServe(fs *flag.FlagSet, args []string) error {
	sample := fs.String("sample", "", "path to sample json")
	db := fs.String("db", "recon.db", "sqlite checkpoint path")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	region, err := generateOrLoad(*sample)
	if err != nil {
		return err
	}
	outcome, engine, bounds, checks, err := fullPipeline(region, *db)
	if err != nil {
		return err
	}
	report := model.PipelineReport{
		RegionID: region.ID, Results: outcome.Results,
		Invalidations: outcome.Invalidations, HitSequence: outcome.HitSequence,
		Bounds: bounds, AlgoVersion: pipeline.AlgoVersion,
	}
	server, err := web.NewServer(web.Dashboard{
		Region: region, Report: report, Checks: checks, QueueLog: engine.QueueLog(),
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("dashboard on http://%s (offline, stdlib only)\n", *addr)
	return srv.ListenAndServe()
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func cloneRegion(region *model.Region) *model.Region {
	data, err := json.Marshal(region)
	if err != nil {
		panic(err)
	}
	var copyRegion model.Region
	if err := json.Unmarshal(data, &copyRegion); err != nil {
		panic(err)
	}
	return &copyRegion
}
