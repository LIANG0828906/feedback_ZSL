package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"resource-workbench/internal/store"
	"resource-workbench/internal/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "sample":
		cmdSample(os.Args[2:])
	case "validate":
		cmdValidate(os.Args[2:])
	case "run":
		cmdRun(ctx, os.Args[2:], "run")
	case "report":
		cmdRun(ctx, os.Args[2:], "report")
	case "compare":
		cmdCompare(ctx, os.Args[2:])
	case "truth-check":
		cmdTruthCheck(ctx, os.Args[2:])
	case "edit-zone":
		cmdEditZone(ctx, os.Args[2:])
	case "edit-nocollect":
		cmdEditNoCollect(ctx, os.Args[2:])
	case "retry-zone":
		cmdRetryZone(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `resource-workbench commands:
  sample        -seed 42 -out samples/verdant-reach.json
  validate      -region samples/verdant-reach.json
  run           -region samples/verdant-reach.json -db workbench.sqlite [-out report.json]
  report        -region samples/verdant-reach.json -db workbench.sqlite -out report.json
  compare       -region samples/verdant-reach.json -db workbench.sqlite
  truth-check   -region samples/verdant-reach.json -db workbench.sqlite
  edit-zone     -region ... -db ... -zone zone-1-west -density-limit 0.09
  edit-nocollect -region ... -db ... -id nc-grove -polygon "45,35;60,35;60,50;45,50;45,35"
  retry-zone    -region ... -db ... -zone zone-1-west`)
}

func cmdSample(args []string) {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	seed := fs.Uint64("seed", 42, "deterministic sample seed")
	out := fs.String("out", "samples/verdant-reach.json", "output path")
	_ = fs.Parse(args)
	region, err := workbench.GenerateSample(*seed)
	if err != nil {
		fail(err)
	}
	data, encodeErr := workbench.EncodeResult(region)
	if encodeErr != nil {
		fail(encodeErr)
	}
	if mkErr := os.MkdirAll(filepath.Dir(*out), 0o755); mkErr != nil {
		fail(&workbench.Error{Code: workbench.CodeIO, Message: mkErr.Error()})
	}
	if writeErr := os.WriteFile(*out, append(data, '\n'), 0o644); writeErr != nil {
		fail(&workbench.Error{Code: workbench.CodeIO, Message: writeErr.Error()})
	}
	printJSON(map[string]any{"ok": true, "path": *out, "seed": *seed, "zones": len(region.Zones), "sample_sites": len(region.SampleSites), "truth_points": len(region.Truth.Points)})
}

func cmdValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	regionPath := fs.String("region", "samples/verdant-reach.json", "region JSON path")
	_ = fs.Parse(args)
	region, err := loadRegion(*regionPath)
	if err != nil {
		fail(err)
	}
	printJSON(map[string]any{
		"ok": true, "region_id": region.RegionID, "zones": len(region.Zones),
		"sample_sites": len(region.SampleSites), "truth_points": len(region.Truth.Points),
		"raster": map[string]any{"rows": region.TerrainRaster.Rows, "cols": region.TerrainRaster.Cols},
	})
}

func cmdRun(ctx context.Context, args []string, mode string) {
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	regionPath := fs.String("region", "samples/verdant-reach.json", "region JSON path")
	dbPath := fs.String("db", "", "SQLite checkpoint path; empty disables persistence")
	out := fs.String("out", "", "JSON result output path")
	serial := fs.Bool("serial", false, "force a single worker")
	parallel := fs.Int("parallel", 0, "worker limit (0 uses region config)")
	quiet := fs.Bool("quiet", false, "only print errors and output path")
	_ = fs.Parse(args)
	engine, cleanup, err := openEngine(*regionPath, *dbPath, *serial, *parallel)
	if err != nil {
		fail(err)
	}
	defer cleanup()
	run, runErr := engine.Run(ctx)
	if run == nil {
		fail(runErr)
	}
	if *out != "" {
		if writeErr := writeResult(*out, run); writeErr != nil {
			fail(writeErr)
		}
	}
	if !*quiet {
		printJSON(runSummary(run))
	}
	if runErr != nil {
		fail(runErr)
	}
}

func cmdEditZone(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("edit-zone", flag.ExitOnError)
	regionPath := fs.String("region", "samples/verdant-reach.json", "region JSON path")
	dbPath := fs.String("db", "workbench.sqlite", "SQLite checkpoint path")
	zoneID := fs.String("zone", "", "zone id")
	density := fs.Float64("density-limit", -1, "new density limit")
	rarity := fs.String("rarity", "", "comma-separated rarity=weight (e.g. common=0.6,rare=0.4)")
	clear := fs.Bool("clear-rarity", false, "clear rarity override")
	out := fs.String("out", "", "JSON result output path")
	_ = fs.Parse(args)
	if *zoneID == "" {
		fail(&workbench.Error{Code: workbench.CodeSchema, Field: "zone", Message: "zone is required"})
	}
	req := workbench.ZoneEditRequest{ZoneID: *zoneID, ClearRarityOverride: *clear}
	if *density >= 0 {
		req.DensityLimit = density
	}
	if *rarity != "" {
		mix, err := parseMix(*rarity)
		if err != nil {
			fail(err)
		}
		req.RarityMix = mix
	}
	engine, cleanup, err := openEngine(*regionPath, *dbPath, false, 0)
	if err != nil {
		fail(err)
	}
	defer cleanup()
	run, runErr := engine.EditZone(ctx, req)
	if run == nil {
		fail(runErr)
	}
	if *out != "" {
		if writeErr := writeResult(*out, run); writeErr != nil {
			fail(writeErr)
		}
	}
	printJSON(runSummary(run))
	if runErr != nil {
		fail(runErr)
	}
}

func cmdEditNoCollect(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("edit-nocollect", flag.ExitOnError)
	regionPath := fs.String("region", "samples/verdant-reach.json", "region JSON path")
	dbPath := fs.String("db", "workbench.sqlite", "SQLite checkpoint path")
	id := fs.String("id", "", "no-collect id")
	polygon := fs.String("polygon", "", "x,y;x,y;... polygon; empty keeps existing or removes")
	remove := fs.Bool("remove", false, "remove the no-collect area")
	out := fs.String("out", "", "JSON result output path")
	_ = fs.Parse(args)
	req := workbench.NoCollectEditRequest{ID: *id, Remove: *remove}
	if *polygon != "" {
		points, err := parsePolygon(*polygon)
		if err != nil {
			fail(err)
		}
		req.Polygon = points
	}
	engine, cleanup, err := openEngine(*regionPath, *dbPath, false, 0)
	if err != nil {
		fail(err)
	}
	defer cleanup()
	run, runErr := engine.EditNoCollect(ctx, req)
	if run == nil {
		fail(runErr)
	}
	if *out != "" {
		if writeErr := writeResult(*out, run); writeErr != nil {
			fail(writeErr)
		}
	}
	printJSON(runSummary(run))
	if runErr != nil {
		fail(runErr)
	}
}

func cmdRetryZone(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("retry-zone", flag.ExitOnError)
	regionPath := fs.String("region", "samples/verdant-reach.json", "region JSON path")
	dbPath := fs.String("db", "workbench.sqlite", "SQLite checkpoint path")
	zoneID := fs.String("zone", "", "zone id")
	_ = fs.Parse(args)
	engine, cleanup, err := openEngine(*regionPath, *dbPath, false, 0)
	if err != nil {
		fail(err)
	}
	defer cleanup()
	run, runErr := engine.RetryZone(ctx, *zoneID)
	if run == nil {
		fail(runErr)
	}
	printJSON(runSummary(run))
	if runErr != nil {
		fail(runErr)
	}
}

func cmdCompare(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	regionPath := fs.String("region", "samples/verdant-reach.json", "region JSON path")
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	serial := fs.Bool("serial", false, "force a single worker")
	parallel := fs.Int("parallel", 0, "worker limit")
	_ = fs.Parse(args)
	engine, cleanup, err := openEngine(*regionPath, *dbPath, *serial, *parallel)
	if err != nil {
		fail(err)
	}
	defer cleanup()
	report, compareErr := engine.CompareFull(ctx)
	if report == nil {
		fail(compareErr)
	}
	printJSON(map[string]any{"ok": compareErr == nil, "comparison": report})
	if compareErr != nil {
		fail(compareErr)
	}
}

type truthZoneCheck struct {
	ZoneID       string  `json:"zone_id"`
	DensityError float64 `json:"density_error"`
	RarityL1     float64 `json:"rarity_l1"`
	CountError   float64 `json:"count_relative_error"`
	HeatmapError float64 `json:"heatmap_error"`
	Passed       bool    `json:"passed"`
}

func cmdTruthCheck(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("truth-check", flag.ExitOnError)
	regionPath := fs.String("region", "samples/verdant-reach.json", "region JSON path")
	dbPath := fs.String("db", "", "SQLite checkpoint path")
	_ = fs.Parse(args)
	region, err := loadRegion(*regionPath)
	if err != nil {
		fail(err)
	}
	engine, cleanup, openErr := openEngine(*regionPath, *dbPath, true, 1)
	if openErr != nil {
		fail(openErr)
	}
	defer cleanup()
	run, runErr := engine.Run(ctx)
	if run == nil {
		fail(runErr)
	}
	truthCounts := make(map[string]int)
	for _, point := range region.Truth.Points {
		truthCounts[point.ZoneID]++
	}
	checks := make([]truthZoneCheck, 0, len(run.Zones))
	passed := true
	for _, zone := range run.Zones {
		truth := region.Truth.ZoneTruth[zone.ZoneID]
		densityErr := math.Abs(zone.Estimate.Density - truth.Density)
		rarityL1 := 0.0
		keys := make(map[string]struct{})
		for key := range truth.RarityMix {
			keys[key] = struct{}{}
		}
		for key := range zone.Estimate.RarityMix {
			keys[key] = struct{}{}
		}
		for key := range keys {
			rarityL1 += math.Abs(zone.Estimate.RarityMix[key] - truth.RarityMix[key])
		}
		countBase := math.Max(1, float64(truthCounts[zone.ZoneID]))
		countErr := math.Abs(float64(zone.PointCount-truthCounts[zone.ZoneID])) / countBase
		heatErr := 0.0
		heatCount := 0
		for _, site := range region.SampleSites {
			if siteZone := zoneForPoint(region, site.Point()); siteZone == nil || siteZone.ID != zone.ZoneID {
				continue
			}
			heatErr += math.Abs(run.Heatmap.At(site.X, site.Y) - site.ObservedDensity)
			heatCount++
		}
		if heatCount > 0 {
			heatErr /= float64(heatCount)
		}
		ok := densityErr <= region.Sampling.DensityTolerance &&
			rarityL1 <= math.Max(0.05, 2*region.Sampling.RarityStep) &&
			countErr <= region.Sampling.CountTolerance &&
			heatErr <= region.Sampling.HeatmapTolerance
		passed = passed && ok
		checks = append(checks, truthZoneCheck{
			ZoneID: zone.ZoneID, DensityError: densityErr, RarityL1: rarityL1,
			CountError: countErr, HeatmapError: heatErr, Passed: ok,
		})
	}
	printJSON(map[string]any{
		"ok": passed, "density_tolerance": region.Sampling.DensityTolerance,
		"count_tolerance":   region.Sampling.CountTolerance,
		"heatmap_tolerance": region.Sampling.HeatmapTolerance,
		"zones":             checks, "bias_passed": run.Bias.Passed,
	})
	if runErr != nil {
		fail(runErr)
	}
	if !passed {
		os.Exit(1)
	}
}

func openEngine(regionPath, dbPath string, serial bool, parallel int) (*workbench.Engine, func(), *workbench.Error) {
	region, err := loadRegion(regionPath)
	if err != nil {
		return nil, func() {}, err
	}
	var persistence workbench.PersistentStore
	cleanup := func() {}
	if dbPath != "" {
		db, openErr := store.Open(dbPath)
		if openErr != nil {
			return nil, cleanup, &workbench.Error{Code: workbench.CodeStore, Message: openErr.Error()}
		}
		persistence = db
		cleanup = func() { _ = db.Close() }
	}
	engine, engineErr := workbench.NewEngine(region, workbench.RuntimeConfig{
		Serial: serial, MaxParallel: parallel,
	}, persistence)
	if engineErr != nil {
		cleanup()
		return nil, func() {}, engineErr
	}
	return engine, cleanup, nil
}

func loadRegion(path string) (*workbench.Region, *workbench.Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &workbench.Error{Code: workbench.CodeIO, Message: fmt.Sprintf("read region %s: %v", path, err)}
	}
	return workbench.LoadRegion(data)
}

func writeResult(path string, value any) *workbench.Error {
	data, err := workbench.EncodeResult(value)
	if err != nil {
		return err
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return &workbench.Error{Code: workbench.CodeIO, Message: mkErr.Error()}
	}
	if writeErr := os.WriteFile(path, append(data, '\n'), 0o644); writeErr != nil {
		return &workbench.Error{Code: workbench.CodeIO, Message: writeErr.Error()}
	}
	return nil
}

func runSummary(run *workbench.RunResult) map[string]any {
	return map[string]any{
		"ok": run.Bias.Passed, "region_id": run.RegionID, "mode": run.Mode,
		"commit_order": run.CommitOrder, "recomputed": run.Recomputed,
		"points": len(run.Points), "bias_passed": run.Bias.Passed,
		"total_bias_upper": run.Bias.TotalUpper,
		"snapshot":         run.Snapshot, "worker_stats": run.Worker,
	}
}

func parseMix(text string) (map[string]float64, *workbench.Error) {
	out := make(map[string]float64)
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pair := strings.SplitN(part, "=", 2)
		if len(pair) != 2 {
			return nil, &workbench.Error{Code: workbench.CodeSchema, Field: "rarity", Message: "expected rarity=weight"}
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(pair[1]), 64)
		if err != nil {
			return nil, &workbench.Error{Code: workbench.CodeSchema, Field: "rarity", Message: err.Error()}
		}
		out[strings.TrimSpace(pair[0])] = value
	}
	return out, nil
}

func parsePolygon(text string) ([]workbench.Vec2, *workbench.Error) {
	var points []workbench.Vec2
	for _, pair := range strings.Split(text, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		values := strings.Split(pair, ",")
		if len(values) != 2 {
			return nil, &workbench.Error{Code: workbench.CodeSchema, Field: "polygon", Message: "expected x,y pairs"}
		}
		x, errX := strconv.ParseFloat(strings.TrimSpace(values[0]), 64)
		y, errY := strconv.ParseFloat(strings.TrimSpace(values[1]), 64)
		if errX != nil || errY != nil {
			return nil, &workbench.Error{Code: workbench.CodeSchema, Field: "polygon", Message: "invalid coordinate"}
		}
		points = append(points, workbench.Vec2{X: x, Y: y})
	}
	if len(points) >= 3 && (points[0].X != points[len(points)-1].X || points[0].Y != points[len(points)-1].Y) {
		points = append(points, points[0])
	}
	return points, nil
}

func zoneForPoint(region *workbench.Region, p workbench.Vec2) *workbench.Zone {
	// The public sample API intentionally does not expose point/zone testing.
	for i := range region.Zones {
		if containsPoint(region.Zones[i].Polygon, p) {
			return &region.Zones[i]
		}
	}
	return nil
}

func containsPoint(polygon []workbench.Vec2, p workbench.Vec2) bool {
	inside := false
	points := polygon
	if len(points) > 1 && points[0] == points[len(points)-1] {
		points = points[:len(points)-1]
	}
	for i, j := 0, len(points)-1; i < len(points); j, i = i, i+1 {
		a, b := points[j], points[i]
		if (a.Y > p.Y) != (b.Y > p.Y) {
			x := (b.X-a.X)*(p.Y-a.Y)/(b.Y-a.Y) + a.X
			if p.X < x {
				inside = !inside
			}
		}
	}
	return inside
}

func printJSON(value any) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func fail(err *workbench.Error) {
	if err == nil {
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stderr)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(map[string]any{"ok": false, "error": err})
	os.Exit(1)
}
