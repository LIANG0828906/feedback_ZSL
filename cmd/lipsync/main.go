package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lipsync.local/prototype/pkg/checkpoint"
	"lipsync.local/prototype/pkg/dependency"
	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/lip"
	"lipsync.local/prototype/pkg/merge"
	"lipsync.local/prototype/pkg/pipeline"
	"lipsync.local/prototype/pkg/quant"
	"lipsync.local/prototype/pkg/sample"
	"lipsync.local/prototype/pkg/schedule"
	"lipsync.local/prototype/pkg/verify"
	"lipsync.local/prototype/pkg/workpool"
)

type summary struct {
	Seed            int64               `json:"seed"`
	Algorithm       string              `json:"algorithm"`
	DialogueCount   int                 `json:"dialogue_count"`
	PackageCount    int                 `json:"package_count"`
	FrameCount      int                 `json:"frame_count"`
	TransitionCount int                 `json:"transition_count"`
	Accepted        int                 `json:"accepted"`
	Deferred        int                 `json:"deferred"`
	Rejected        int                 `json:"rejected"`
	TruthVerified   bool                `json:"truth_verified"`
	PathParity      bool                `json:"path_parity"`
	CommitSequences map[string][]string `json:"commit_sequences"`
	ErrorChecks     map[string]string   `json:"error_checks"`
	Invalidation    invalidationSummary `json:"invalidation"`
}

type invalidationSummary struct {
	InvalidatedPackages []string `json:"invalidated_packages"`
	ReusedPackageCount  int      `json:"reused_package_count"`
	SQLiteCacheHits     int      `json:"sqlite_cache_hits"`
}

func main() {
	command := "demo"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "generate":
		if err := runGenerate(args); err != nil {
			fail(err)
		}
	case "demo", "diagnose":
		if err := runDemo(args); err != nil {
			fail(err)
		}
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		fail(fmt.Errorf("unknown command %q", command))
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `lipsync diagnostic entry

Usage:
  go run ./cmd/lipsync generate [-out artifacts] [-seed 20261126]
  go run ./cmd/lipsync demo     [-out artifacts] [-seed 20261126] [-workers 2] [-worker-buffer 1] [-queue-budget 1]

All generated samples, truth and SQLite checkpoints stay under -out.`)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "artifacts", "artifact directory")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, truth, err := sample.WriteFiles(*out, *seed)
	if err != nil {
		return err
	}
	fmt.Printf("generated %d dialogues, %d packages, %d frames, %d transitions in %s\n",
		len(project.Dialogues), countPackages(truth), countFrames(truth), countTransitions(truth), *out)
	return nil
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	out := fs.String("out", "artifacts", "artifact directory")
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	workers := fs.Int("workers", 2, "worker parallelism")
	workerBuffer := fs.Int("worker-buffer", 1, "per-worker buffer")
	queueBudget := fs.Int("queue-budget", 1, "pending queue budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, truth, err := sample.WriteFiles(*out, *seed)
	if err != nil {
		return err
	}
	for _, name := range []string{"realtime.sqlite", "full.sqlite", "resume.sqlite", "stage.sqlite", "edit.sqlite", "cache.sqlite"} {
		if err := removeDB(filepath.Join(*out, name)); err != nil {
			return err
		}
	}
	pool := workpool.Pool{Workers: *workers, PerWorkerBuffer: *workerBuffer, QueueBudget: *queueBudget}
	reports := make(map[pipeline.Path]*pipeline.Report)
	for _, path := range []pipeline.Path{pipeline.PathRealtime, pipeline.PathFull} {
		store, err := checkpoint.Open(filepath.Join(*out, string(path)+".sqlite"))
		if err != nil {
			return err
		}
		report, err := pipeline.Run(context.Background(), project, pipeline.Options{Path: path, Store: store, Pool: pool})
		if err != nil {
			store.Close()
			return err
		}
		if err := verify.Truth(project, report, truth); err != nil {
			store.Close()
			return fmt.Errorf("%s truth verification: %w", path, err)
		}
		store.Close()
		reports[path] = report
	}
	fullStore, err := checkpoint.Open(filepath.Join(*out, "full.sqlite"))
	if err != nil {
		return err
	}
	fullCheckpoints, err := fullStore.Committed(context.Background(), project.Recipe.Version, project.Recipe.AlgorithmVersion)
	fullStore.Close()
	if err != nil {
		return err
	}
	if len(fullCheckpoints) == 0 {
		return fmt.Errorf("full path produced no checkpoints")
	}
	resumeStore, err := checkpoint.Open(filepath.Join(*out, "resume.sqlite"))
	if err != nil {
		return err
	}
	seedCP := fullCheckpoints[0]
	seedCP.CommitSeq = 0
	if err := resumeStore.CommitCheckpoint(context.Background(), seedCP); err != nil {
		resumeStore.Close()
		return err
	}
	resumeReport, err := pipeline.Run(context.Background(), project, pipeline.Options{
		Path: pipeline.PathResume, Store: resumeStore, Pool: pool,
	})
	if err != nil {
		resumeStore.Close()
		return err
	}
	if err := verify.Truth(project, resumeReport, truth); err != nil {
		resumeStore.Close()
		return fmt.Errorf("resume truth verification: %w", err)
	}
	if len(resumeReport.Recovered) == 0 {
		resumeStore.Close()
		return fmt.Errorf("resume path did not recover the seeded committed shard")
	}
	resumeStore.Close()
	reports[pipeline.PathResume] = resumeReport

	if err := pipeline.CompareReports(reports[pipeline.PathFull], reports[pipeline.PathRealtime]); err != nil {
		return fmt.Errorf("realtime/full parity: %w", err)
	}
	if err := pipeline.CompareReports(reports[pipeline.PathFull], reports[pipeline.PathResume]); err != nil {
		return fmt.Errorf("resume/full parity: %w", err)
	}
	if err := verify.ValidateDispatch(project, reports[pipeline.PathFull]); err != nil {
		return fmt.Errorf("dispatch budget verification: %w", err)
	}
	invalidation, err := runInvalidationCheck(project, reports[pipeline.PathFull], *out)
	if err != nil {
		return err
	}

	errorChecks, err := runErrorChecks(project, reports[pipeline.PathFull], *out)
	if err != nil {
		return err
	}
	s := summary{
		Seed: *seed, Algorithm: project.Recipe.AlgorithmVersion,
		DialogueCount: len(project.Dialogues), PackageCount: countPackages(truth),
		FrameCount: countFrames(truth), TransitionCount: countTransitions(truth),
		Accepted:      len(reports[pipeline.PathFull].GlobalDispatch.Accepted),
		Deferred:      len(reports[pipeline.PathFull].GlobalDispatch.Deferred),
		Rejected:      len(reports[pipeline.PathFull].GlobalDispatch.Rejected),
		TruthVerified: true, PathParity: true,
		CommitSequences: map[string][]string{
			string(pipeline.PathRealtime): reports[pipeline.PathRealtime].CommitSequence,
			string(pipeline.PathFull):     reports[pipeline.PathFull].CommitSequence,
			string(pipeline.PathResume):   reports[pipeline.PathResume].CommitSequence,
		},
		ErrorChecks: errorChecks, Invalidation: invalidation,
	}
	if err := writeSummary(*out, s); err != nil {
		return err
	}
	fmt.Printf("truth_verified=true path_parity=true dialogues=%d packages=%d frames=%d transitions=%d\n",
		s.DialogueCount, s.PackageCount, s.FrameCount, s.TransitionCount)
	fmt.Printf("dispatch accepted=%d deferred=%d rejected=%d commit_sequence=%v\n",
		s.Accepted, s.Deferred, s.Rejected, reports[pipeline.PathFull].CommitSequence)
	fmt.Printf("recovered_shards=%v\n", resumeReport.Recovered)
	fmt.Printf("invalidation packages=%v reused=%d sqlite_cache_hits=%d\n",
		invalidation.InvalidatedPackages, invalidation.ReusedPackageCount, invalidation.SQLiteCacheHits)
	keys := make([]string, 0, len(errorChecks))
	for key := range errorChecks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Printf("error_check %-12s %s\n", key, errorChecks[key])
	}
	fmt.Printf("summary=%s\n", filepath.Join(*out, "demo_summary.json"))
	return nil
}

func runInvalidationCheck(project *domain.Project, report *pipeline.Report, out string) (invalidationSummary, error) {
	var result invalidationSummary
	edited := cloneProject(project)
	edited.Dialogues[0].Phonemes[1].WeightPermille += 13
	editStore, err := checkpoint.Open(filepath.Join(out, "edit.sqlite"))
	if err != nil {
		return result, err
	}
	editedReport, err := pipeline.Run(context.Background(), edited, pipeline.Options{
		Path: pipeline.PathFull, Store: editStore,
	})
	editStore.Close()
	if err != nil {
		return result, err
	}
	diff := dependency.Diff(report.Graph, editedReport.Graph)
	result.InvalidatedPackages = diff.InvalidatedPackages
	result.ReusedPackageCount = len(diff.ReusedPackages)
	if len(result.InvalidatedPackages) == 0 || result.ReusedPackageCount == 0 {
		return result, fmt.Errorf("invalidation check did not produce both invalidated and reused packages")
	}
	cacheStore, err := checkpoint.Open(filepath.Join(out, "cache.sqlite"))
	if err != nil {
		return result, err
	}
	defer cacheStore.Close()
	if err := dependency.PersistPackages(context.Background(), cacheStore, report.Graph, project.Recipe); err != nil {
		return result, err
	}
	reused, missing, err := dependency.LoadReusablePackages(
		context.Background(), cacheStore, editedReport.Graph, diff, edited.Recipe,
	)
	if err != nil {
		return result, err
	}
	if len(missing) != 0 {
		return result, fmt.Errorf("persistent package cache missing entries: %v", missing)
	}
	result.SQLiteCacheHits = len(reused)
	for _, id := range result.InvalidatedPackages {
		if _, bad := reused[id]; bad {
			return result, fmt.Errorf("invalidated package %s was loaded from cache", id)
		}
	}
	if result.SQLiteCacheHits != result.ReusedPackageCount {
		return result, fmt.Errorf("cache hit count %d != reused package count %d", result.SQLiteCacheHits, result.ReusedPackageCount)
	}
	return result, nil
}

func runErrorChecks(project *domain.Project, report *pipeline.Report, out string) (map[string]string, error) {
	checks := make(map[string]string)
	check := func(name string, want domain.Code, err error) error {
		got := domain.CodeOf(err)
		checks[name] = string(got)
		if got != want {
			return fmt.Errorf("%s: want %s, got %s (%v)", name, want, got, err)
		}
		return nil
	}
	_, err := domain.LoadProject([]byte(`{"version":`))
	if err := check("E_DECODE", domain.ErrDecode, err); err != nil {
		return nil, err
	}
	brokenJSON, err := withoutWeight(project)
	if err != nil {
		return nil, err
	}
	_, err = domain.LoadProject(brokenJSON)
	if err := check("E_SCHEMA", domain.ErrSchema, err); err != nil {
		return nil, err
	}
	badOrder := cloneProject(project)
	badOrder.Dialogues[0].Phonemes[1].StartSample = badOrder.Dialogues[0].Phonemes[0].EndSample - 1
	_, err = domain.LoadProject(mustJSON(badOrder))
	if err := check("E_ORDER", domain.ErrOrder, err); err != nil {
		return nil, err
	}
	badRate := cloneProject(project)
	badRate.Recipe.FrameRate = 45
	_, err = domain.LoadProject(mustJSON(badRate))
	if err := check("E_RATE", domain.ErrRate, err); err != nil {
		return nil, err
	}
	_, _, err = quant.SampleToFrame("d", "p", "end", math.MaxInt64, 48000, 50, domain.QuantizeNearest)
	if err := check("E_OVF", domain.ErrOverflow, err); err != nil {
		return nil, err
	}
	badAffinity := []domain.QuantizedPhoneme{
		{PhonemeID: "a", StartFrame: 0, EndFrame: 1, Category: domain.CategoryVowel, AffinityToNext: 1001},
		{PhonemeID: "b", StartFrame: 1, EndFrame: 2, Category: domain.CategoryConsonant},
	}
	_, err = merge.Merge("d", "speaker-a", badAffinity, merge.Config{AffinityThreshold: 600, MaxPhonemesPerPackage: 2})
	if err := check("E_PARA", domain.ErrPara, err); err != nil {
		return nil, err
	}
	tolerancePhonemes := []domain.QuantizedPhoneme{
		{PhonemeID: "s", StartFrame: 0, EndFrame: 2, Category: domain.CategorySilence},
		{PhonemeID: "v", StartFrame: 2, EndFrame: 4, Category: domain.CategoryVowel},
	}
	tolerancePackages := []domain.LipPackage{
		{ID: "d/pkg/000", DialogueID: "d", StartFrame: 0, EndFrame: 2, PhonemeIDs: []string{"s"}},
		{ID: "d/pkg/001", DialogueID: "d", StartFrame: 2, EndFrame: 4, PhonemeIDs: []string{"v"}},
	}
	_, err = lip.RenderFrames("d", tolerancePhonemes, tolerancePackages, 0)
	if err := check("E_LIP", domain.ErrLip, err); err != nil {
		return nil, err
	}
	_, err = schedule.Dispatch([]domain.LipPackage{{
		ID: "d/pkg/000", DialogueID: "d", Channel: "missing", StartFrame: 0, EndFrame: 2,
	}}, schedule.Config{MaxPackagesPerFrame: 1, MaxPackagesPerChannelFrame: 1, Channels: map[string]int{"a": 1}})
	if err := check("E_CONFLICT", domain.ErrConflict, err); err != nil {
		return nil, err
	}
	machine := checkpoint.NewShardMachine("shard-000", "d")
	_ = machine.Transition(checkpoint.StateQuantizing)
	err = machine.Transition(checkpoint.StateCommitted)
	if err := check("E_STATE", domain.ErrState, err); err != nil {
		return nil, err
	}
	altered := cloneReport(report)
	if len(altered.Shards) > 0 && len(altered.Shards[0].Lip.Frames) > 0 {
		altered.Shards[0].Lip.Frames[0].Lip = "deliberate-diff"
	}
	err = pipeline.CompareReports(report, altered)
	if err := check("E_DIFF", domain.ErrDiff, err); err != nil {
		return nil, err
	}
	stagePath := filepath.Join(out, "stage.sqlite")
	if err := removeDB(stagePath); err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(stagePath)
	if err != nil {
		return nil, err
	}
	faultProject := cloneProject(project)
	stageReport, err := pipeline.Run(context.Background(), faultProject, pipeline.Options{
		Path: pipeline.PathRealtime, Store: store,
		Faults: map[string]pipeline.FaultKind{faultProject.Dialogues[1].ID: pipeline.FaultStage},
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	err = stageReport.FirstError()
	if err := check("E_STAGE", domain.ErrStage, err); err != nil {
		store.Close()
		return nil, err
	}
	if got := len(stageReport.CommitSequence); got != len(faultProject.Dialogues)-1 {
		store.Close()
		return nil, fmt.Errorf("E_STAGE isolation: committed %d shards, want %d", got, len(faultProject.Dialogues)-1)
	}
	store.Close()
	return checks, nil
}

func withoutWeight(project *domain.Project) ([]byte, error) {
	data := mustJSON(project)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	dialogues := raw["dialogues"].([]any)
	phonemes := dialogues[0].(map[string]any)["phonemes"].([]any)
	delete(phonemes[1].(map[string]any), "weight_permille")
	return json.Marshal(raw)
}

func cloneProject(project *domain.Project) *domain.Project {
	var clone domain.Project
	if err := json.Unmarshal(mustJSON(project), &clone); err != nil {
		panic(err)
	}
	return &clone
}

func cloneReport(report *pipeline.Report) *pipeline.Report {
	clone := *report
	clone.Shards = append([]pipeline.ShardResult(nil), report.Shards...)
	for i := range clone.Shards {
		clone.Shards[i].Lip = report.Shards[i].Lip
		clone.Shards[i].Lip.Frames = append([]domain.FrameLip(nil), report.Shards[i].Lip.Frames...)
	}
	return &clone
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func removeDB(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func writeSummary(dir string, s summary) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(dir, "demo_summary.json"), data, 0o644)
}

func countPackages(truth *sample.Truth) int {
	total := 0
	for _, d := range truth.Dialogues {
		total += len(d.Packages)
	}
	return total
}

func countFrames(truth *sample.Truth) int {
	total := 0
	for _, d := range truth.Dialogues {
		total += len(d.Frames)
	}
	return total
}

func countTransitions(truth *sample.Truth) int {
	total := 0
	for _, d := range truth.Dialogues {
		total += len(d.Transitions)
	}
	return total
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	if code := domain.CodeOf(err); code != "" {
		fmt.Fprintln(os.Stderr, "code:", code)
	}
	os.Exit(1)
}
