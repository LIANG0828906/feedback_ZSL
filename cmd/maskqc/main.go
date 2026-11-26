package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"maskqc/checkpoint"
	"maskqc/engine"
	"maskqc/model"
	"maskqc/sample"
	"maskqc/schema"
	"maskqc/web"
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
	case "verify":
		err = runVerify(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "version":
		fmt.Println("maskqc", model.AlgorithmVersion, model.ConversionVersion)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`maskqc - deterministic stroke topology and direction verifier

usage:
  maskqc generate [-seed N] [-sizes 96,128,160] [-out samples]
  maskqc verify   [-data samples] [-report artifacts/validation-report.json] [-seed N]
  maskqc serve    [-data samples] [-db state/maskqc.db] [-addr 127.0.0.1:8080]

verification modes exercised by "verify":
  - full recompute
  - incremental rerun
  - resume from SQLite checkpoints
  - edited incremental versus resume and full
  - per-stroke failure isolation and retry
`)
}

func runGenerate(args []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := flags.Int64("seed", 20261127, "deterministic sample seed")
	sizesText := flags.String("sizes", "96,128,160", "comma-separated square resolutions")
	output := flags.String("out", "samples", "sample output directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	sizes, err := parseSizes(*sizesText)
	if err != nil {
		return err
	}
	bundle, err := sample.Generate(*seed, sizes)
	if err != nil {
		return err
	}
	manifest, err := sample.Write(*output, bundle)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(data))
	return nil
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataRoot := flags.String("data", "samples", "deterministic sample directory")
	dbPath := flags.String("db", "state/maskqc.db", "SQLite checkpoint path")
	address := flags.String("addr", "127.0.0.1:8080", "local net/http listen address")
	seed := flags.Int64("seed", 20261127, "seed used if samples must be generated")
	blockSize := flags.Int("block", 16, "block edge length")
	parallel := flags.Int("parallel", 4, "maximum worker parallel degree")
	pixelBudget := flags.Int("pixel-budget", 16*16*8, "pixel buffer budget per worker")
	queueBudget := flags.Int("queue", 8, "pending block queue capacity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := ensureSamples(*dataRoot, *seed)
	if err != nil {
		return err
	}
	inputs, err := loadWebInputs(*dataRoot, manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		return err
	}
	store, err := checkpoint.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	config := engine.DefaultConfig()
	config.BlockSize = *blockSize
	config.MaxParallel = *parallel
	config.PixelBufferPerWorker = *pixelBudget
	config.QueueCapacity = *queueBudget
	stores := make([]*checkpoint.Store, len(inputs))
	for index := range stores {
		stores[index] = store
	}
	dashboard, err := web.New(inputs, config, stores)
	if err != nil {
		return err
	}
	httpServer := &http.Server{
		Addr:              *address,
		Handler:           dashboard.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Printf("MaskQC dashboard listening on http://%s\n", *address)
	return httpServer.ListenAndServe()
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dataRoot := flags.String("data", "samples", "deterministic sample directory")
	reportPath := flags.String("report", "artifacts/validation-report.json", "JSON validation report path")
	seed := flags.Int64("seed", 20261127, "seed used if samples must be generated")
	blockSize := flags.Int("block", 16, "block edge length")
	parallel := flags.Int("parallel", 4, "maximum worker parallel degree")
	pixelBudget := flags.Int("pixel-budget", 16*16*8, "pixel buffer budget per worker")
	queueBudget := flags.Int("queue", 8, "pending block queue capacity")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := ensureSamples(*dataRoot, *seed)
	if err != nil {
		return err
	}
	config := engine.DefaultConfig()
	config.BlockSize = *blockSize
	config.MaxParallel = *parallel
	config.PixelBufferPerWorker = *pixelBudget
	config.QueueCapacity = *queueBudget
	workRoot, err := os.MkdirTemp("", "maskqc-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workRoot)
	report := validationReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Seed:        manifest.Seed, BlockSize: *blockSize, MaxParallel: *parallel,
		PixelBufferPerWorker: *pixelBudget, QueueCapacity: *queueBudget,
	}
	pass := true
	for _, manifestProject := range manifest.Projects {
		baselineHash := ""
		projectReport := projectValidation{ID: manifestProject.ID, Width: manifestProject.Width, Height: manifestProject.Height}
		for _, version := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
			check, hash, err := verifyVersion(*dataRoot, manifestProject, version, config, filepath.Join(workRoot, manifestProject.ID+"-"+string(version)+".db"))
			if err != nil {
				check.Error = err.Error()
				pass = false
			}
			if baselineHash == "" && hash != "" {
				baselineHash = hash
			}
			check.CrossVersionMaskEqual = baselineHash != "" && hash == baselineHash
			if !check.CrossVersionMaskEqual {
				pass = false
			}
			projectReport.Versions = append(projectReport.Versions, check)
		}
		roundtrip, err := verifyRoundTrips(*dataRoot, manifestProject)
		if err != nil {
			pass = false
			projectReport.RoundTripError = err.Error()
		} else {
			projectReport.RoundTrips = roundtrip
			for _, check := range roundtrip {
				if !check.Exact {
					pass = false
				}
			}
		}
		editCheck, err := verifyEditAndFailure(*dataRoot, manifestProject, config, filepath.Join(workRoot, manifestProject.ID+"-edit.db"))
		if err != nil {
			pass = false
			projectReport.EditError = err.Error()
		} else {
			projectReport.Edit = editCheck
			if !editCheck.ThreePathsExact || !editCheck.FailureIsolated || !editCheck.RetryExact {
				pass = false
			}
		}
		report.Projects = append(report.Projects, projectReport)
	}
	report.Pass = pass
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*reportPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*reportPath, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(string(data))
	if !pass {
		return errors.New("one or more validation checks failed")
	}
	return nil
}

type validationReport struct {
	GeneratedAt          string              `json:"generatedAt"`
	Seed                 int64               `json:"seed"`
	BlockSize            int                 `json:"blockSize"`
	MaxParallel          int                 `json:"maxParallel"`
	PixelBufferPerWorker int                 `json:"pixelBufferPerWorker"`
	QueueCapacity        int                 `json:"queueCapacity"`
	Pass                 bool                `json:"pass"`
	Projects             []projectValidation `json:"projects"`
}

type projectValidation struct {
	ID             string                `json:"id"`
	Width          int                   `json:"width"`
	Height         int                   `json:"height"`
	Versions       []versionValidation   `json:"versions"`
	RoundTrips     []roundTripValidation `json:"roundTrips,omitempty"`
	RoundTripError string                `json:"roundTripError,omitempty"`
	Edit           *editValidation       `json:"edit,omitempty"`
	EditError      string                `json:"editError,omitempty"`
}

type versionValidation struct {
	Version               model.SchemaVersion `json:"version"`
	LoadOK                bool                `json:"loadOK"`
	DefaultFills          int                 `json:"defaultFills"`
	FullHash              string              `json:"fullHash"`
	IncrementalHash       string              `json:"incrementalHash"`
	ResumeHash            string              `json:"resumeHash"`
	ThreePathsExact       bool                `json:"threePathsExact"`
	Domains               int                 `json:"domains"`
	Directions            []string            `json:"directions"`
	Issues                int                 `json:"issues"`
	ReusedOnResume        int                 `json:"reusedOnResume"`
	CrossVersionMaskEqual bool                `json:"crossVersionMaskEqual"`
	Error                 string              `json:"error,omitempty"`
}

type roundTripValidation struct {
	Target           model.SchemaVersion `json:"target"`
	LossyIssues      int                 `json:"lossyIssues"`
	LossyCodes       []string            `json:"lossyCodes"`
	CoordinatesExact bool                `json:"coordinatesExact"`
	SequenceExact    bool                `json:"sequenceExact"`
	WeightExact      bool                `json:"weightExact"`
	Exact            bool                `json:"exact"`
}

type editValidation struct {
	InvalidatedBlocks []int  `json:"invalidatedBlocks"`
	ReusedBlocks      int    `json:"reusedBlocks"`
	ThreePathsExact   bool   `json:"threePathsExact"`
	FailureIsolated   bool   `json:"failureIsolated"`
	FailedStroke      string `json:"failedStroke,omitempty"`
	RetryExact        bool   `json:"retryExact"`
	Mismatch          string `json:"mismatch,omitempty"`
}

func verifyVersion(root string, manifestProject sample.ManifestProject, version model.SchemaVersion, config engine.Config, dbPath string) (versionValidation, string, error) {
	check := versionValidation{Version: version}
	project, conversion, err := loadVersion(root, manifestProject, version)
	if err != nil {
		return check, "", err
	}
	check.LoadOK = true
	for _, trace := range conversion.Traces {
		if trace.DefaultSource != "" {
			check.DefaultFills++
		}
	}
	config.ResidualDigest = conversion.ResidualDigest
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return check, "", err
	}
	defer store.Close()
	first, err := engine.New(project, config, store)
	if err != nil {
		return check, "", err
	}
	full, err := first.Run(context.Background(), engine.ModeFull)
	if err != nil {
		if full != nil {
			check.Issues = len(full.Issues)
		}
		return check, "", err
	}
	incremental, err := first.Run(context.Background(), engine.ModeIncremental)
	if err != nil {
		return check, "", err
	}
	second, err := engine.New(project, config, store)
	if err != nil {
		return check, "", err
	}
	resume, err := second.Run(context.Background(), engine.ModeResume)
	if err != nil {
		return check, "", err
	}
	check.FullHash = full.AccumulatedHash
	check.IncrementalHash = incremental.AccumulatedHash
	check.ResumeHash = resume.AccumulatedHash
	check.ThreePathsExact = engine.CompareRuns(full, incremental) == nil && engine.CompareRuns(full, resume) == nil
	check.Domains = len(full.Analysis.Domains)
	for _, domain := range full.Analysis.Domains {
		check.Directions = append(check.Directions, string(domain.Direction))
	}
	check.Issues = len(full.Issues)
	check.ReusedOnResume = len(resume.ReusedBlocks)
	return check, full.AccumulatedHash, nil
}

func verifyRoundTrips(root string, manifestProject sample.ManifestProject) ([]roundTripValidation, error) {
	base, _, err := loadVersion(root, manifestProject, model.SchemaV3)
	if err != nil {
		return nil, err
	}
	results := make([]roundTripValidation, 0, 2)
	for _, target := range []model.SchemaVersion{model.SchemaV2, model.SchemaV1} {
		exported, err := schema.Export(base, target)
		if err != nil {
			return nil, err
		}
		low, _, err := schema.Load(exported.Artifact)
		if err != nil {
			return nil, err
		}
		back, err := schema.Export(low, model.SchemaV3)
		if err != nil {
			return nil, err
		}
		high, _, err := schema.Load(back.Artifact)
		if err != nil {
			return nil, err
		}
		check := roundTripValidation{Target: target, LossyIssues: len(exported.Issues)}
		for _, issue := range exported.Issues {
			check.LossyCodes = append(check.LossyCodes, issue.Code)
		}
		check.CoordinatesExact = true
		check.SequenceExact = true
		check.WeightExact = true
		if len(base.Strokes) != len(high.Strokes) {
			check.CoordinatesExact, check.SequenceExact, check.WeightExact = false, false, false
		} else {
			for index := range base.Strokes {
				left, right := base.Strokes[index], high.Strokes[index]
				if left.X0 != right.X0 || left.Y0 != right.Y0 || left.X1 != right.X1 || left.Y1 != right.Y1 {
					check.CoordinatesExact = false
				}
				if left.Seq != right.Seq || left.ID != right.ID {
					check.SequenceExact = false
				}
				if left.Weight != right.Weight {
					check.WeightExact = false
				}
			}
		}
		check.Exact = check.CoordinatesExact && check.SequenceExact && check.WeightExact
		results = append(results, check)
	}
	return results, nil
}

func verifyEditAndFailure(root string, manifestProject sample.ManifestProject, config engine.Config, dbPath string) (*editValidation, error) {
	project, conversion, err := loadVersion(root, manifestProject, model.SchemaV3)
	if err != nil {
		return nil, err
	}
	config.ResidualDigest = conversion.ResidualDigest
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	first, err := engine.New(project, config, store)
	if err != nil {
		return nil, err
	}
	if _, err := first.Run(context.Background(), engine.ModeFull); err != nil {
		return nil, err
	}
	editedStroke := project.Strokes[0]
	editedStroke.X1 += 3
	editedStroke.Hardness = 82
	edited, err := first.ApplyStrokeEdit(context.Background(), editedStroke)
	if err != nil && !model.IsVerificationError(err) {
		return nil, err
	}
	check := &editValidation{}
	if edited.Invalidation != nil {
		check.InvalidatedBlocks = edited.Invalidation.InvalidatedBlocks
	}
	check.ReusedBlocks = len(edited.ReusedBlocks)
	resumeEngine, err := engine.New(project, config, store)
	if err != nil {
		return nil, err
	}
	resumed, err := resumeEngine.Run(context.Background(), engine.ModeResume)
	if err != nil && !model.IsVerificationError(err) {
		return nil, err
	}
	fullEngine, err := engine.New(project, config, store)
	if err != nil {
		return nil, err
	}
	full, err := fullEngine.Run(context.Background(), engine.ModeFull)
	if err != nil && !model.IsVerificationError(err) {
		return nil, err
	}
	check.ThreePathsExact = engine.CompareRuns(edited, resumed) == nil && engine.CompareRuns(edited, full) == nil
	if mismatch := engine.CompareRuns(edited, resumed); mismatch != nil {
		check.Mismatch = fmt.Sprintf("%+v", *mismatch)
	}
	failingID := project.Strokes[1].ID
	check.FailedStroke = failingID
	resumeEngine.SetStrokeFailureHook(func(stroke model.Stroke) error {
		if stroke.ID == failingID {
			return errors.New("injected deterministic single-stroke failure")
		}
		return nil
	})
	failureStroke := project.Strokes[1]
	failureStroke.Hardness = 73
	failedResult, failureErr := resumeEngine.ApplyStrokeEdit(context.Background(), failureStroke)
	check.FailureIsolated = failureErr != nil && failedResult != nil && len(failedResult.StrokeFailures) == 1 &&
		failedResult.StrokeFailures[0] == failingID
	strokeCount, blockCount, countsErr := store.Counts(project.ID)
	if countsErr != nil {
		return nil, countsErr
	}
	check.FailureIsolated = check.FailureIsolated && strokeCount >= len(project.Strokes) && blockCount > 0
	resumeEngine.SetStrokeFailureHook(nil)
	retry, retryErr := resumeEngine.Run(context.Background(), engine.ModeIncremental)
	if retryErr != nil && !model.IsVerificationError(retryErr) {
		return nil, retryErr
	}
	retryFullEngine, err := engine.New(project, config, store)
	if err != nil {
		return nil, err
	}
	retryFull, err := retryFullEngine.Run(context.Background(), engine.ModeFull)
	if err != nil && !model.IsVerificationError(err) {
		return nil, err
	}
	check.RetryExact = engine.CompareRuns(retry, retryFull) == nil
	return check, nil
}

func loadVersion(root string, manifestProject sample.ManifestProject, version model.SchemaVersion) (*model.Project, *model.ConversionReport, error) {
	relative := manifestProject.Files[string(version)]
	if relative == "" {
		return nil, nil, fmt.Errorf("%s has no %s artifact", manifestProject.ID, version)
	}
	return schema.LoadFile(filepath.Join(root, filepath.FromSlash(relative)))
}

func loadWebInputs(root string, manifest *sample.Manifest) ([]web.ProjectInput, error) {
	inputs := make([]web.ProjectInput, 0, len(manifest.Projects))
	for _, manifestProject := range manifest.Projects {
		project, conversion, err := loadVersion(root, manifestProject, model.SchemaV3)
		if err != nil {
			return nil, err
		}
		icon, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifestProject.Image)))
		if err != nil {
			return nil, err
		}
		versions := make([]string, 0, len(manifestProject.Files))
		for version := range manifestProject.Files {
			versions = append(versions, version)
		}
		sort.Strings(versions)
		inputs = append(inputs, web.ProjectInput{
			ID: project.ID, Project: project, IconPNG: icon, Versions: versions,
			Lossy: manifestProject.Lossy, ResidualDigest: conversion.ResidualDigest,
		})
	}
	return inputs, nil
}

func ensureSamples(root string, seed int64) (*sample.Manifest, error) {
	manifest, err := sample.LoadManifest(root)
	if err == nil {
		return manifest, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	bundle, err := sample.Generate(seed, sample.DefaultSizes())
	if err != nil {
		return nil, err
	}
	return sample.Write(root, bundle)
}

func parseSizes(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	sizes := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid size %q", part)
		}
		sizes = append(sizes, value)
	}
	return sizes, nil
}
