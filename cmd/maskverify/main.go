package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"example.com/iconmask"
)

type imageReport struct {
	ImageID               string                    `json:"imageId"`
	Width                 int                       `json:"width"`
	Height                int                       `json:"height"`
	InitialRegions        int                       `json:"initialRegions"`
	TruthRegions          int                       `json:"truthRegions"`
	RepairedRegions       int                       `json:"repairedRegions"`
	InitialForeground     int                       `json:"initialForeground"`
	TruthForeground       int                       `json:"truthForeground"`
	RepairedForeground    int                       `json:"repairedForeground"`
	TruthHoles            int                       `json:"truthHoles"`
	RepairedHoles         int                       `json:"repairedHoles"`
	IoU                   float64                   `json:"iou"`
	PixelAccuracy         float64                   `json:"pixelAccuracy"`
	TruthCentroidDelta    float64                   `json:"maxTruthCentroidDelta"`
	TruthAreaRelError     float64                   `json:"maxTruthAreaRelativeError"`
	MissingForeground     int                       `json:"missingForeground"`
	SurplusForeground     int                       `json:"surplusForeground"`
	Rounds                []iconmask.RepairRoundLog `json:"rounds"`
	Converged             bool                      `json:"converged"`
	Success               bool                      `json:"success"`
	EdgePixels            int                       `json:"edgePixels"`
	OutsideBandChanged    int                       `json:"outsideBandChanged"`
	RadiusMin             int                       `json:"radiusMin"`
	RadiusMax             int                       `json:"radiusMax"`
	BlockwiseMatches      bool                      `json:"blockwiseMatchesFrame"`
	BlockPeakInFlight     int                       `json:"blockPeakInFlight"`
	BlockWaitEvents       int                       `json:"blockWaitEvents"`
	PreviewRegions        int                       `json:"previewRegions"`
	PreviewDownsample     int                       `json:"previewDownsample"`
	PreviewRounds         int                       `json:"previewRoundsUsed"`
	PreviewElapsed        time.Duration             `json:"previewElapsedNanos"`
	PreviewWithinBudget   bool                      `json:"previewWithinLatency"`
	MaxCentroidDelta      float64                   `json:"maxCentroidDelta"`
	MaxAreaRelativeError  float64                   `json:"maxAreaRelativeError"`
	PreviewExactPassed    bool                      `json:"previewExactPassed"`
	ThreePathMasksEqual   bool                      `json:"threePathMasksEqual"`
	ThreePathStatsEqual   bool                      `json:"threePathStatsEqual"`
	ThreePathCommitsEqual bool                      `json:"threePathCommitSequencesEqual"`
	StageCommits          int                       `json:"stageCommits"`
	FirstInconsistent     *iconmask.Coord           `json:"firstInconsistentBlock,omitempty"`
}

type verificationReport struct {
	GeneratedAt       string            `json:"generatedAt"`
	Seed              int64             `json:"seed"`
	GoVersion         string            `json:"goVersion"`
	AlgorithmVersion  string            `json:"algorithmVersion"`
	ProjectValidation string            `json:"projectValidation"`
	ErrorCodes        map[string]string `json:"errorCodes"`
	Images            []imageReport     `json:"images"`
	AllChecksPassed   bool              `json:"allChecksPassed"`
	Database          string            `json:"database"`
}

func main() {
	seed := flag.Int64("seed", 42, "deterministic sample seed")
	output := flag.String("out", "artifacts/verification.json", "verification report path")
	database := flag.String("db", "artifacts/verify/checkpoints.db", "temporary SQLite checkpoint path")
	flag.Parse()

	project := iconmask.GenerateProject(*seed)
	if err := iconmask.ValidateProject(project); err != nil {
		fmt.Fprintf(os.Stderr, "project validation failed: %v\n", err)
		os.Exit(1)
	}
	_ = os.MkdirAll(filepath.Dir(*database), 0o755)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(*database + suffix)
	}
	report := verificationReport{
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		Seed:              *seed,
		GoVersion:         runtime.Version(),
		AlgorithmVersion:  iconmask.AlgorithmVersion,
		ProjectValidation: "passed",
		ErrorCodes:        verifyErrorCodes(project),
		Database:          *database,
	}
	allPassed := true
	snapshot := iconmask.NewParameterSnapshot(iconmask.DefaultRepairConfig())
	for _, sample := range project.Images {
		frame, frameErr := iconmask.RefineSample(sample, iconmask.DefaultRepairConfig())
		if frameErr != nil {
			fmt.Fprintf(os.Stderr, "%s frame repair failed: %v\n", sample.ID, frameErr)
			os.Exit(1)
		}
		blockwise, blockErr := iconmask.RefineBlockwise(sample, iconmask.DefaultRepairConfig())
		if blockErr != nil {
			fmt.Fprintf(os.Stderr, "%s blockwise repair failed: %v\n", sample.ID, blockErr)
			os.Exit(1)
		}
		dual, dualErr := iconmask.RunDualPath(sample, snapshot, iconmask.DefaultPreviewConfig())
		if dualErr != nil {
			fmt.Fprintf(os.Stderr, "%s preview/exact failed: %v\n", sample.ID, dualErr)
			os.Exit(1)
		}
		consistencyCfg := iconmask.DefaultConsistencyConfig()
		consistencyCfg.DatabasePath = *database
		consistencyCfg.RunID = "verify-" + sample.ID
		threePath, threePathErr := iconmask.RunThreePathConsistency(context.Background(), sample, snapshot, consistencyCfg)
		if threePathErr != nil {
			fmt.Fprintf(os.Stderr, "%s three-path failed: %v\n", sample.ID, threePathErr)
			os.Exit(1)
		}
		entry := imageReport{
			ImageID:               sample.ID,
			Width:                 sample.Width,
			Height:                sample.Height,
			InitialRegions:        iconmask.AnalyzeMask(sample.InitialMask).Count,
			TruthRegions:          iconmask.AnalyzeMask(sample.Truth.ReferenceMask).Count,
			RepairedRegions:       frame.Stats.Count,
			InitialForeground:     sample.InitialMask.Count(),
			TruthForeground:       sample.Truth.ReferenceMask.Count(),
			RepairedForeground:    frame.Mask.Count(),
			IoU:                   frame.Accuracy.IoU,
			PixelAccuracy:         frame.Accuracy.PixelAccuracy,
			MissingForeground:     frame.Accuracy.MissingForeground,
			SurplusForeground:     frame.Accuracy.SurplusForeground,
			Rounds:                frame.Rounds,
			Converged:             frame.Converged,
			Success:               frame.Success,
			EdgePixels:            frame.EdgeBand.Count(),
			OutsideBandChanged:    frame.OutsideChanged,
			RadiusMin:             frame.RadiusTable.Config.Min,
			RadiusMax:             frame.RadiusTable.Config.Max,
			BlockwiseMatches:      frame.Mask.Equal(blockwise.Mask),
			PreviewRegions:        dual.Preview.Stats.Count,
			PreviewDownsample:     dual.Preview.ActualDownsample,
			PreviewRounds:         dual.Preview.RoundsUsed,
			PreviewElapsed:        dual.Preview.Elapsed,
			PreviewWithinBudget:   dual.Preview.WithinLatency,
			MaxCentroidDelta:      dual.Comparison.MaxCentroidDelta,
			MaxAreaRelativeError:  dual.Comparison.MaxAreaRelativeErr,
			PreviewExactPassed:    dual.Comparison.Passed,
			ThreePathMasksEqual:   threePath.MasksEqual,
			ThreePathStatsEqual:   threePath.StatsEqual,
			ThreePathCommitsEqual: threePath.CommitSequencesEqual,
			StageCommits:          len(threePath.Full.Commits),
			FirstInconsistent:     threePath.FirstInconsistentBlock,
		}
		if blockwise.BlockSchedule != nil {
			entry.BlockPeakInFlight = blockwise.BlockSchedule.PeakInFlight
			entry.BlockWaitEvents = len(blockwise.BlockSchedule.WaitEvents)
		}
		truthStats := iconmask.AnalyzeMask(sample.Truth.ReferenceMask)
		for _, component := range truthStats.Regions {
			entry.TruthHoles += component.Holes
		}
		for _, component := range frame.Stats.Regions {
			entry.RepairedHoles += component.Holes
		}
		for i := 0; i < minInt(len(truthStats.Regions), len(frame.Stats.Regions)); i++ {
			truthRegion := truthStats.Regions[i]
			repairedRegion := frame.Stats.Regions[i]
			delta := math.Hypot(
				truthRegion.Centroid.X-repairedRegion.Centroid.X,
				truthRegion.Centroid.Y-repairedRegion.Centroid.Y,
			)
			areaError := math.Abs(float64(truthRegion.Area-repairedRegion.Area)) / float64(maxInt(1, truthRegion.Area))
			entry.TruthCentroidDelta = math.Max(entry.TruthCentroidDelta, delta)
			entry.TruthAreaRelError = math.Max(entry.TruthAreaRelError, areaError)
		}
		passed := entry.Converged && entry.Success && entry.OutsideBandChanged == 0 && entry.BlockwiseMatches &&
			entry.PreviewExactPassed && entry.ThreePathMasksEqual && entry.ThreePathStatsEqual &&
			entry.ThreePathCommitsEqual && entry.FirstInconsistent == nil &&
			entry.RepairedRegions == entry.TruthRegions && entry.RepairedHoles == entry.TruthHoles
		allPassed = allPassed && passed
		report.Images = append(report.Images, entry)
		fmt.Printf("%s iou=%.4f regions=%d/%d preview=%.3f/%.3f 3path=%t commits=%d\n",
			sample.ID, entry.IoU, entry.InitialRegions, entry.RepairedRegions,
			entry.MaxCentroidDelta, entry.MaxAreaRelativeError,
			entry.ThreePathMasksEqual && entry.ThreePathStatsEqual && entry.ThreePathCommitsEqual,
			entry.StageCommits)
	}
	for code, status := range report.ErrorCodes {
		if status != "observed" {
			allPassed = false
			fmt.Printf("error code check failed: %s=%s\n", code, status)
		}
	}
	report.AllChecksPassed = allPassed
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal report: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create output directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*output, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("report=%s allChecksPassed=%t\n", *output, allPassed)
	if !allPassed {
		os.Exit(1)
	}
}

func verifyErrorCodes(project *iconmask.Project) map[string]string {
	statuses := map[string]string{
		iconmask.CodeSchema:     "not_observed",
		iconmask.CodeParameter:  "not_observed",
		iconmask.CodeRadius:     "not_observed",
		iconmask.CodeIteration:  "not_observed",
		iconmask.CodeState:      "not_observed",
		iconmask.CodeDivergence: "not_observed",
	}
	broken := *project
	broken.Images = append([]iconmask.ImageSample(nil), project.Images...)
	broken.Images[0].InitialMask.Width++
	if err := iconmask.ValidateProject(&broken); err != nil && err.Code == iconmask.CodeSchema {
		statuses[iconmask.CodeSchema] = "observed"
	}
	sample := project.Images[0]
	if _, err := iconmask.LocateEdgeBand(sample.ID, sample.InitialMask, sample.Truth.Segments, iconmask.EdgeConfig{
		Shape: "hexagon", RadiusBandMin: 2,
	}); err != nil && err.Code == iconmask.CodeParameter {
		statuses[iconmask.CodeParameter] = "observed"
	}
	if _, err := iconmask.ResolveRadii(sample.ID, sample.Truth.Segments, iconmask.RadiusConfig{
		Min: 0, Max: 4, CurvatureBands: 4,
	}); err != nil && err.Code == iconmask.CodeRadius {
		statuses[iconmask.CodeRadius] = "observed"
	}
	cfg := iconmask.DefaultRepairConfig()
	cfg.MaxRounds = 1
	if _, err := iconmask.RefineSample(sample, cfg); err != nil && err.Code == iconmask.CodeIteration {
		statuses[iconmask.CodeIteration] = "observed"
	}
	machine := iconmask.NewBlockStateMachine(iconmask.Coord{})
	if err := machine.Start(iconmask.StageClose); err != nil && err.Code == iconmask.CodeState {
		statuses[iconmask.CodeState] = "observed"
	}
	previewCfg := iconmask.DefaultPreviewConfig()
	previewCfg.AreaRelativeTolerance = -1
	if _, err := iconmask.RunDualPath(sample, iconmask.NewParameterSnapshot(iconmask.DefaultRepairConfig()), previewCfg); err != nil &&
		err.Code == iconmask.CodeDivergence {
		statuses[iconmask.CodeDivergence] = "observed"
	}
	return statuses
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
