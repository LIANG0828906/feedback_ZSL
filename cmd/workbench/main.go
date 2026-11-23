package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"storyworkbench/checkpoint"
	"storyworkbench/config"
	"storyworkbench/engine"
	"storyworkbench/samplegen"
	"storyworkbench/story"
	"storyworkbench/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "demo":
		err = demo(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "workbench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Narrative Relay - deterministic interactive story workbench

Usage:
  workbench serve   [-config workbench.json] [-addr host:port] [-samples dir]
  workbench validate [-samples dir]
  workbench demo    [-samples dir] [-artifacts dir] [-db file] [-workers n] [-buffer n]

The serve command opens the local offline UI. The demo command verifies every
shot boundary, edited replay, three-path equality and SQLite restart behavior.`)
}

func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "optional JSON config path")
	addr := flags.String("addr", "", "listen address")
	samples := flags.String("samples", "", "sample project directory")
	dbPath := flags.String("db", "", "SQLite checkpoint database")
	saves := flags.String("saves", "", "versioned save directory")
	artifacts := flags.String("artifacts", "", "diagnostic artifact directory")
	workers := flags.Int("workers", 0, "maximum worker threads")
	buffer := flags.Int("buffer", 0, "state buffer per worker")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Address = *addr
	}
	if *samples != "" {
		cfg.SamplesDir = *samples
	}
	if *dbPath != "" {
		cfg.CheckpointDB = *dbPath
	}
	if *saves != "" {
		cfg.SavesDir = *saves
	}
	if *artifacts != "" {
		cfg.ArtifactsDir = *artifacts
	}
	if *workers > 0 {
		cfg.Workers = *workers
	}
	if *buffer > 0 {
		cfg.BufferPerWorker = *buffer
	}
	app, err := workbench.NewApp(cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	return workbench.RunServer(app, cfg.Address)
}

func loadManifest(dir string) (*samplegen.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest samplegen.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func validate(args []string) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	samples := flags.String("samples", "samples", "sample project directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	manifest, err := loadManifest(*samples)
	if err != nil {
		return err
	}
	failed := false
	for _, entry := range manifest.Projects {
		project, err := story.LoadProject(filepath.Join(*samples, entry.File))
		if err != nil {
			fmt.Printf("FAIL %-28s %v\n", entry.ID, err)
			failed = true
			continue
		}
		run, err := engine.Play(project, project.Choices)
		if err != nil {
			fmt.Printf("FAIL %-28s %v\n", entry.ID, err)
			failed = true
			continue
		}
		if err := workbench.VerifyTruth(project, run); err != nil {
			fmt.Printf("FAIL %-28s %v\n", entry.ID, err)
			failed = true
			continue
		}
		fmt.Printf("OK   %-28s shots=%-3d duration=%-6d reachable=%d events=%d\n",
			project.ID, run.ShotCount, run.TotalDurationMS, len(run.ReachableNodes), len(run.EventOrder))
	}
	if failed {
		return fmt.Errorf("one or more samples failed validation")
	}
	return nil
}

type demoReport struct {
	GeneratedAt      string         `json:"generated_at"`
	GoVersion        string         `json:"go_version"`
	BaseSeed         int64          `json:"base_seed"`
	Samples          []sampleReport `json:"samples"`
	OK               bool           `json:"ok"`
	TotalBoundaries  int            `json:"total_boundaries_checked"`
	TotalCheckpoints int            `json:"total_checkpoints_recovered"`
}

type sampleReport struct {
	ProjectID           string   `json:"project_id"`
	ContentHash         string   `json:"content_hash"`
	ShotCount           int      `json:"shot_count"`
	TotalDurationMS     int64    `json:"total_duration_ms"`
	TruthVerified       bool     `json:"truth_verified"`
	BoundariesChecked   int      `json:"boundaries_checked"`
	CheckpointShots     int      `json:"checkpoint_shots"`
	CheckpointRestartOK bool     `json:"checkpoint_restart_ok"`
	ThreePathsOK        bool     `json:"three_paths_ok"`
	EditedNode          string   `json:"edited_node"`
	InvalidatedNodes    int      `json:"invalidated_nodes"`
	InvalidatedShots    int      `json:"invalidated_shots"`
	ReusedShots         int      `json:"reused_shots"`
	SaveBoundary        int      `json:"save_boundary"`
	ArtifactDir         string   `json:"artifact_dir"`
	Errors              []string `json:"errors,omitempty"`
}

func demo(args []string) error {
	flags := flag.NewFlagSet("demo", flag.ContinueOnError)
	samples := flags.String("samples", "samples", "sample project directory")
	artifacts := flags.String("artifacts", filepath.Join("artifacts", "verification"), "verification report directory")
	dbPath := flags.String("db", filepath.Join("data", "verification.db"), "SQLite verification database")
	saves := flags.String("saves", filepath.Join("data", "verification-saves"), "verification save directory")
	workers := flags.Int("workers", 4, "maximum worker threads")
	buffer := flags.Int("buffer", 8, "state buffer per worker")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*artifacts, 0o755); err != nil {
		return err
	}
	cfg := config.Default()
	cfg.SamplesDir = *samples
	cfg.CheckpointDB = *dbPath
	cfg.SavesDir = *saves
	cfg.ArtifactsDir = filepath.Join(*artifacts, "runs")
	cfg.Workers = *workers
	cfg.BufferPerWorker = *buffer
	app, err := workbench.NewApp(cfg)
	if err != nil {
		return err
	}
	defer app.Close()

	manifest, err := loadManifest(*samples)
	if err != nil {
		return err
	}
	report := demoReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		GoVersion:   runtime.Version(),
		BaseSeed:    manifest.BaseSeed,
		Samples:     []sampleReport{},
	}
	for index, entry := range manifest.Projects {
		sample := verifySample(app, entry, index, *artifacts)
		report.Samples = append(report.Samples, sample)
		report.TotalBoundaries += sample.BoundariesChecked
		report.TotalCheckpoints += sample.CheckpointShots
	}
	report.OK = true
	for _, sample := range report.Samples {
		if len(sample.Errors) > 0 {
			report.OK = false
		}
	}
	sort.Slice(report.Samples, func(i, j int) bool { return report.Samples[i].ProjectID < report.Samples[j].ProjectID })
	if err := writeJSON(filepath.Join(*artifacts, "verification-report.json"), report); err != nil {
		return err
	}
	if err := writeMarkdown(filepath.Join(*artifacts, "verification-summary.md"), report); err != nil {
		return err
	}
	printReport(report)
	if !report.OK {
		return fmt.Errorf("verification failed")
	}
	return nil
}

func verifySample(app *workbench.App, entry samplegen.ManifestEntry, index int, artifactRoot string) sampleReport {
	project, err := app.Project(entry.ID)
	if err != nil {
		return sampleReport{ProjectID: entry.ID, Errors: []string{err.Error()}}
	}
	baseline, err := app.Baseline(entry.ID)
	if err != nil {
		return sampleReport{ProjectID: entry.ID, Errors: []string{err.Error()}}
	}
	sample := sampleReport{
		ProjectID:       project.ID,
		ContentHash:     story.ContentHash(project),
		ShotCount:       baseline.ShotCount,
		TotalDurationMS: baseline.TotalDurationMS,
		TruthVerified:   workbench.VerifyTruth(project, baseline) == nil,
		EditedNode:      "archive",
	}
	if !sample.TruthVerified {
		sample.Errors = append(sample.Errors, "generated truth mismatch")
	}
	for boundary := 0; boundary <= baseline.ShotCount; boundary++ {
		session, err := engine.PlayToBoundary(project, project.Choices, boundary)
		if err != nil {
			sample.Errors = append(sample.Errors, fmt.Sprintf("boundary %d prepare: %v", boundary, err))
			break
		}
		save := session.Snapshot()
		data, err := engine.MarshalSave(save)
		if err != nil {
			sample.Errors = append(sample.Errors, fmt.Sprintf("boundary %d marshal: %v", boundary, err))
			break
		}
		parsed, err := engine.ParseSave(data)
		if err != nil {
			sample.Errors = append(sample.Errors, fmt.Sprintf("boundary %d parse: %v", boundary, err))
			break
		}
		resumed, err := engine.Resume(project, parsed, project.Choices)
		if err != nil {
			sample.Errors = append(sample.Errors, fmt.Sprintf("boundary %d resume: %v", boundary, err))
			break
		}
		if err := engine.CompareRuns(baseline, resumed); err != nil {
			sample.Errors = append(sample.Errors, fmt.Sprintf("boundary %d divergence: %v", boundary, err))
			break
		}
		sample.BoundariesChecked++
	}

	checkpointShots, checkpointOK, checkpointErr := verifyCheckpointRestart(project, index, artifactRoot)
	sample.CheckpointShots = checkpointShots
	sample.CheckpointRestartOK = checkpointOK
	if checkpointErr != nil {
		sample.Errors = append(sample.Errors, "checkpoint restart: "+checkpointErr.Error())
	}

	node := findNode(project, "archive")
	if node == nil {
		sample.Errors = append(sample.Errors, "edit target archive missing")
		return sample
	}
	requires := append([]story.Condition(nil), node.Requires...)
	events := append([]story.Event(nil), node.Events...)
	events = append(events, story.Event{
		ID: "ev-verification-editorial", Kind: "set_flag", Key: "verification_note", Value: "atomic-replay",
	})
	request := workbench.CompareRequest{
		ProjectID: project.ID,
		Edit: engine.NodeEdit{
			NodeID:   "archive",
			Requires: &requires,
			Events:   &events,
		},
		SaveBoundary: intPtr(baseline.ShotCount / 2),
	}
	response := app.CompareThreePaths(context.Background(), request)
	sample.ThreePathsOK = response.OK
	sample.SaveBoundary = response.SaveBoundary
	sample.ArtifactDir = response.ArtifactDir
	if response.Invalidation != nil {
		sample.InvalidatedNodes = len(response.Invalidation.InvalidatedNodes)
		sample.InvalidatedShots = len(response.Invalidation.InvalidatedShots)
		sample.ReusedShots = len(response.Invalidation.ReusedShotSequences)
	}
	if !response.OK {
		sample.Errors = append(sample.Errors, "three-path compare: "+formatAnyError(response.Error))
	}
	return sample
}

func verifyCheckpointRestart(project *story.Project, index int, artifactRoot string) (int, bool, error) {
	dbPath := filepath.Join(artifactRoot, fmt.Sprintf("checkpoint-%02d.db", index))
	_ = os.Remove(dbPath)
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return 0, false, err
	}
	session, err := engine.NewSession(project, project.Choices)
	if err != nil {
		_ = store.Close()
		return 0, false, err
	}
	recorded, err := checkpoint.RecordSession(context.Background(), store, session)
	if err != nil {
		_ = store.Close()
		return 0, false, err
	}
	if err := store.Close(); err != nil {
		return 0, false, err
	}
	reopened, err := checkpoint.Open(dbPath)
	if err != nil {
		return 0, false, err
	}
	defer reopened.Close()
	latest, err := reopened.LoadLatest(project)
	if err != nil {
		return 0, false, err
	}
	resumed, err := engine.Resume(project, latest, project.Choices)
	if err != nil {
		return 0, false, err
	}
	if err := engine.CompareRuns(recorded, resumed); err != nil {
		return 0, false, err
	}
	return latest.State.ShotProgress, true, nil
}

func findNode(project *story.Project, id string) *story.Node {
	for i := range project.Nodes {
		if project.Nodes[i].ID == id {
			return &project.Nodes[i]
		}
	}
	return nil
}

func intPtr(value int) *int { return &value }

func formatAnyError(value any) string {
	if value == nil {
		return "unknown error"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func writeMarkdown(path string, report demoReport) error {
	var b strings.Builder
	b.WriteString("# 工作台实际验证结果\n\n")
	b.WriteString(fmt.Sprintf("- 生成时间：`%s`\n", report.GeneratedAt))
	b.WriteString(fmt.Sprintf("- Go：`%s`\n", report.GoVersion))
	b.WriteString(fmt.Sprintf("- 样本基准种子：`%d`\n", report.BaseSeed))
	b.WriteString(fmt.Sprintf("- 逐镜头边界存档续演检查：`%d` 次\n", report.TotalBoundaries))
	b.WriteString(fmt.Sprintf("- SQLite 重启恢复镜头：`%d` 个\n", report.TotalCheckpoints))
	b.WriteString(fmt.Sprintf("- 总体结果：**%s**\n\n", map[bool]string{true: "PASS", false: "FAIL"}[report.OK]))
	b.WriteString("| 工程 | 镜头 | 时长(ms) | 边界续演 | SQLite 重启 | 三路径 | 失效节点/镜头 | 复用镜头 |\n")
	b.WriteString("|---|---:|---:|---:|---|---:|---:|---:|\n")
	for _, sample := range report.Samples {
		checkpoint := "FAIL"
		if sample.CheckpointRestartOK {
			checkpoint = "PASS"
		}
		three := "FAIL"
		if sample.ThreePathsOK {
			three = "PASS"
		}
		b.WriteString(fmt.Sprintf("| `%s` | %d | %d | %d/%d | %s (%d) | %s | %d/%d | %d |\n",
			sample.ProjectID, sample.ShotCount, sample.TotalDurationMS,
			sample.BoundariesChecked, sample.ShotCount+1, checkpoint, sample.CheckpointShots,
			three, sample.InvalidatedNodes, sample.InvalidatedShots, sample.ReusedShots))
	}
	if len(report.Samples) > 0 {
		b.WriteString("\n## 诊断产物\n\n")
		for _, sample := range report.Samples {
			if sample.ArtifactDir != "" {
				b.WriteString(fmt.Sprintf("- `%s`: `%s`\n", sample.ProjectID, sample.ArtifactDir))
			}
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func printReport(report demoReport) {
	fmt.Printf("verification: %s\n", map[bool]string{true: "PASS", false: "FAIL"}[report.OK])
	for _, sample := range report.Samples {
		fmt.Printf("  %-28s boundaries=%d/%d checkpoint=%d three_paths=%v invalid=%d/%d reused=%d\n",
			sample.ProjectID, sample.BoundariesChecked, sample.ShotCount+1,
			sample.CheckpointShots, sample.ThreePathsOK,
			sample.InvalidatedNodes, sample.InvalidatedShots, sample.ReusedShots)
	}
	fmt.Printf("  boundary checks=%d checkpoint shots=%d\n", report.TotalBoundaries, report.TotalCheckpoints)
}
