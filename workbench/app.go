package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"storyworkbench/checkpoint"
	"storyworkbench/config"
	"storyworkbench/engine"
	"storyworkbench/samplegen"
	"storyworkbench/story"
	"storyworkbench/worker"
)

type ProjectSummary struct {
	ID              string              `json:"id"`
	Title           string              `json:"title"`
	Seed            int64               `json:"seed"`
	ContentHash     string              `json:"content_hash"`
	NodeCount       int                 `json:"node_count"`
	ChoiceCount     int                 `json:"choice_count"`
	ShotCount       int                 `json:"shot_count"`
	TotalDurationMS int64               `json:"total_duration_ms"`
	ReachableCount  int                 `json:"reachable_count"`
	Checkpoint      *checkpoint.Summary `json:"checkpoint,omitempty"`
}

type ProjectDetail struct {
	Project    *story.Project         `json:"project"`
	Graph      *story.DependencyGraph `json:"dependency_graph"`
	Baseline   *engine.RunResult      `json:"baseline"`
	Checkpoint *checkpoint.Summary    `json:"checkpoint,omitempty"`
	Summary    ProjectSummary         `json:"summary"`
}

type App struct {
	cfg      config.Config
	pool     *worker.Pool
	store    *checkpoint.Store
	projects map[string]*story.Project
	order    []string
	baseline map[string]*engine.RunResult

	saveMu sync.Mutex
	saves  []SaveRecord
	runMu  sync.Mutex
	runSeq int
}

func NewApp(cfg config.Config) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.SavesDir, 0o755); err != nil {
		return nil, fmt.Errorf("create saves directory: %w", err)
	}
	if err := os.MkdirAll(cfg.ArtifactsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create artifacts directory: %w", err)
	}
	pool, err := worker.New(cfg.Workers, cfg.BufferPerWorker)
	if err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(cfg.CheckpointDB)
	if err != nil {
		pool.Close()
		return nil, err
	}
	app := &App{
		cfg:      cfg,
		pool:     pool,
		store:    store,
		projects: map[string]*story.Project{},
		baseline: map[string]*engine.RunResult{},
	}
	if err := app.loadProjects(cfg.SamplesDir); err != nil {
		_ = store.Close()
		pool.Close()
		return nil, err
	}
	if err := app.loadSaveIndex(); err != nil {
		_ = store.Close()
		pool.Close()
		return nil, err
	}
	return app, nil
}

func (a *App) Close() error {
	a.pool.Close()
	return a.store.Close()
}

func (a *App) Config() config.Config    { return a.cfg }
func (a *App) Store() *checkpoint.Store { return a.store }

func (a *App) loadProjects(dir string) error {
	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read sample manifest: %w", err)
	}
	var manifest samplegen.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse sample manifest: %w", err)
	}
	if len(manifest.Projects) == 0 {
		return fmt.Errorf("sample manifest has no projects")
	}
	for _, entry := range manifest.Projects {
		path := filepath.Join(dir, entry.File)
		project, err := story.LoadProject(path)
		if err != nil {
			return fmt.Errorf("load %s: %w", entry.File, err)
		}
		if project.ID != entry.ID {
			return fmt.Errorf("sample %s id mismatch: manifest=%s project=%s", entry.File, entry.ID, project.ID)
		}
		run, err := engine.Play(project, project.Choices)
		if err != nil {
			return fmt.Errorf("play %s: %w", project.ID, err)
		}
		if err := VerifyTruth(project, run); err != nil {
			return fmt.Errorf("truth check %s: %w", project.ID, err)
		}
		a.projects[project.ID] = project
		a.baseline[project.ID] = run
		a.order = append(a.order, project.ID)
	}
	sort.Strings(a.order)
	return nil
}

func VerifyTruth(project *story.Project, run *engine.RunResult) error {
	if project.Truth == nil {
		return nil
	}
	if run.ShotCount != project.Truth.ShotCount {
		return story.DivergeError(-1, "", "shot_count", fmt.Sprintf("expected %d, got %d", project.Truth.ShotCount, run.ShotCount))
	}
	if run.TotalDurationMS != project.Truth.TotalDurationMS {
		return story.DivergeError(-1, "", "total_duration_ms", fmt.Sprintf("expected %d, got %d", project.Truth.TotalDurationMS, run.TotalDurationMS))
	}
	if fmt.Sprint(run.EventOrder) != fmt.Sprint(project.Truth.EventOrder) {
		return story.DivergeError(-1, "", "event_order", "event arrival order differs from truth")
	}
	if fmt.Sprint(run.ReachableNodes) != fmt.Sprint(project.Truth.ReachableNodes) {
		return story.DivergeError(-1, "", "reachable_nodes", "reachable node set differs from truth")
	}
	for key, expected := range project.Truth.FinalFlags {
		actual, ok := run.FinalFlags[key]
		if !ok || !story.ScalarEqual(expected, actual) {
			return story.DivergeError(-1, "", "final_flags."+key, fmt.Sprintf("expected %v, got %v", expected, actual))
		}
	}
	for characterID, expectedState := range project.Truth.FinalCharacters {
		actualState, ok := run.FinalCharacters[characterID]
		if !ok {
			return story.DivergeError(-1, characterID, "final_characters", "character is missing from final state")
		}
		for key, expected := range expectedState {
			actual, ok := actualState[key]
			if !ok || !story.ScalarEqual(expected, actual) {
				return story.DivergeError(-1, characterID, "final_characters."+characterID+"."+key,
					fmt.Sprintf("expected %v, got %v", expected, actual))
			}
		}
	}
	return nil
}

func (a *App) Project(id string) (*story.Project, error) {
	project := a.projects[id]
	if project == nil {
		return nil, story.NewError(story.CodeNotFound, "", "project_id", "project not found")
	}
	return project, nil
}

func (a *App) Baseline(id string) (*engine.RunResult, error) {
	run := a.baseline[id]
	if run == nil {
		return nil, story.NewError(story.CodeNotFound, "", "project_id", "project not found")
	}
	return run, nil
}

func (a *App) Summaries() []ProjectSummary {
	result := make([]ProjectSummary, 0, len(a.order))
	for _, id := range a.order {
		project := a.projects[id]
		run := a.baseline[id]
		summary := ProjectSummary{
			ID:              project.ID,
			Title:           project.Title,
			Seed:            project.Seed,
			ContentHash:     story.ContentHash(project),
			NodeCount:       len(project.Nodes),
			ChoiceCount:     len(project.Choices),
			ShotCount:       run.ShotCount,
			TotalDurationMS: run.TotalDurationMS,
			ReachableCount:  len(run.ReachableNodes),
		}
		if cp, err := a.store.Summary(project.ID); err == nil {
			summary.Checkpoint = cp
		}
		result = append(result, summary)
	}
	return result
}

func (a *App) Detail(id string) (*ProjectDetail, error) {
	project, err := a.Project(id)
	if err != nil {
		return nil, err
	}
	graph, err := story.BuildDependencyGraph(project)
	if err != nil {
		return nil, err
	}
	run, _ := a.Baseline(id)
	summary := ProjectSummary{
		ID:              project.ID,
		Title:           project.Title,
		Seed:            project.Seed,
		ContentHash:     story.ContentHash(project),
		NodeCount:       len(project.Nodes),
		ChoiceCount:     len(project.Choices),
		ShotCount:       run.ShotCount,
		TotalDurationMS: run.TotalDurationMS,
		ReachableCount:  len(run.ReachableNodes),
	}
	detail := &ProjectDetail{
		Project:  project,
		Graph:    graph,
		Baseline: run,
		Summary:  summary,
	}
	if cp, err := a.store.Summary(project.ID); err == nil {
		detail.Checkpoint = cp
		detail.Summary.Checkpoint = cp
	}
	return detail, nil
}

func (a *App) StartCheckpoints(ctx context.Context, projectID string) (*engine.RunResult, *checkpoint.Summary, error) {
	project, err := a.Project(projectID)
	if err != nil {
		return nil, nil, err
	}
	if err := a.store.Reset(project.ID); err != nil {
		return nil, nil, err
	}
	session, err := engine.NewSession(project, project.Choices)
	if err != nil {
		return nil, nil, err
	}
	run, err := checkpoint.RecordSession(ctx, a.store, session)
	if err != nil {
		return nil, nil, err
	}
	summary, err := a.store.Summary(project.ID)
	if err != nil {
		return nil, nil, err
	}
	return run, summary, nil
}

func (a *App) RestoreCheckpoints(projectID string) (*engine.Save, *engine.RunResult, error) {
	project, err := a.Project(projectID)
	if err != nil {
		return nil, nil, err
	}
	save, err := a.store.LoadLatest(project)
	if err != nil {
		return nil, nil, err
	}
	run, err := engine.Resume(project, save, project.Choices)
	if err != nil {
		return nil, nil, err
	}
	return save, run, nil
}

func (a *App) nextRunSeq() int {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.runSeq++
	return a.runSeq
}

func (a *App) artifactDir() string {
	seq := a.nextRunSeq()
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	return filepath.Join(a.cfg.ArtifactsDir, fmt.Sprintf("compare-%s-%03d", stamp, seq))
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
