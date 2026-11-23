package workbench

import (
	"context"
	"fmt"
	"path/filepath"

	"storyworkbench/engine"
	"storyworkbench/story"
	"storyworkbench/worker"
)

type CompareRequest struct {
	ProjectID    string            `json:"project_id"`
	Edit         engine.NodeEdit   `json:"edit"`
	Edits        []engine.NodeEdit `json:"edits,omitempty"`
	SaveBoundary *int              `json:"save_boundary,omitempty"`
}

type PathComparison struct {
	Name     string       `json:"name"`
	Expected string       `json:"expected"`
	Actual   string       `json:"actual"`
	OK       bool         `json:"ok"`
	Error    *story.Error `json:"error,omitempty"`
}

type CompareResponse struct {
	OK            bool                       `json:"ok"`
	ProjectID     string                     `json:"project_id"`
	OriginalHash  string                     `json:"original_content_hash"`
	EditedHash    string                     `json:"edited_content_hash"`
	SaveBoundary  int                        `json:"save_boundary"`
	Continuous    *engine.RunResult          `json:"continuous,omitempty"`
	SaveResume    *engine.RunResult          `json:"save_resume,omitempty"`
	EditedReplay  *engine.RunResult          `json:"edited_replay,omitempty"`
	EditedProject *story.Project             `json:"edited_project,omitempty"`
	EditedGraph   *story.DependencyGraph     `json:"edited_dependency_graph,omitempty"`
	Invalidation  *engine.InvalidationReport `json:"invalidation,omitempty"`
	Comparisons   []PathComparison           `json:"comparisons"`
	ArtifactDir   string                     `json:"artifact_dir,omitempty"`
	ArtifactError string                     `json:"artifact_error,omitempty"`
	Error         *story.Error               `json:"error,omitempty"`
}

type pathResult struct {
	Name     string
	Run      *engine.RunResult
	Save     *engine.Save
	Outcome  *engine.ReplayOutcome
	Boundary int
	Err      error
}

func (a *App) CompareThreePaths(ctx context.Context, req CompareRequest) *CompareResponse {
	response := &CompareResponse{
		ProjectID:   req.ProjectID,
		Comparisons: []PathComparison{},
	}
	project, err := a.Project(req.ProjectID)
	if err != nil {
		response.Error = errorPayload(err)
		return a.finishCompare(response, req, nil)
	}
	baseline, err := a.Baseline(req.ProjectID)
	if err != nil {
		response.Error = errorPayload(err)
		return a.finishCompare(response, req, nil)
	}
	edits := req.Edits
	if len(edits) == 0 {
		edits = []engine.NodeEdit{req.Edit}
	}
	editedProject, err := engine.ApplyEditSequence(project, edits)
	if err != nil {
		response.Error = errorPayload(err)
		return a.finishCompare(response, req, nil)
	}
	response.OriginalHash = story.ContentHash(project)
	response.EditedHash = story.ContentHash(editedProject)
	boundary := baseline.ShotCount / 2
	if req.SaveBoundary != nil {
		boundary = *req.SaveBoundary
	}
	if boundary < 0 || boundary > baseline.ShotCount {
		response.Error = story.NewError(story.CodePara, "", "save_boundary",
			fmt.Sprintf("save boundary must be in [0,%d]", baseline.ShotCount))
		return a.finishCompare(response, req, nil)
	}
	response.SaveBoundary = boundary

	jobs := []worker.Job{
		{
			ID: 0,
			Fn: func(ctx context.Context) (any, error) {
				run, err := engine.Play(editedProject, editedProject.Choices)
				return &pathResult{Name: "continuous", Run: run, Err: err}, err
			},
		},
		{
			ID: 1,
			Fn: func(ctx context.Context) (any, error) {
				usedBoundary := boundary
				var session *engine.Session
				for usedBoundary >= 0 {
					var boundaryErr error
					session, boundaryErr = engine.PlayToBoundary(editedProject, editedProject.Choices, usedBoundary)
					if boundaryErr == nil {
						break
					}
					if appErr, ok := story.AsError(boundaryErr); ok && appErr.Code == story.CodePara && appErr.Field == "shot_boundary" {
						usedBoundary--
						continue
					}
					return &pathResult{Name: "save_resume", Err: boundaryErr}, boundaryErr
				}
				if session == nil {
					err := story.NewError(story.CodePara, "", "save_boundary", "no valid shot boundary available")
					return &pathResult{Name: "save_resume", Err: err}, err
				}
				save := session.Snapshot()
				data, err := engine.MarshalSave(save)
				if err != nil {
					return &pathResult{Name: "save_resume", Err: err}, err
				}
				parsed, err := engine.ParseSave(data)
				if err != nil {
					return &pathResult{Name: "save_resume", Err: err}, err
				}
				if err := engine.ValidateSave(editedProject, parsed, editedProject.Choices); err != nil {
					return &pathResult{Name: "save_resume", Err: err}, err
				}
				run, err := engine.Resume(editedProject, parsed, editedProject.Choices)
				return &pathResult{Name: "save_resume", Run: run, Save: parsed, Boundary: usedBoundary, Err: err}, err
			},
		},
		{
			ID: 2,
			Fn: func(ctx context.Context) (any, error) {
				outcome, err := engine.ReplayEditedSequence(project, project.Choices, baseline, edits)
				return &pathResult{Name: "edited_replay", Outcome: outcome, Run: func() *engine.RunResult {
					if outcome != nil {
						return outcome.Run
					}
					return nil
				}(), Err: err}, err
			},
		},
	}
	results, runErr := a.pool.Run(ctx, jobs)
	if runErr != nil {
		response.Error = errorPayload(runErr)
		return a.finishCompare(response, req, editedProject)
	}
	var outcomes [3]*pathResult
	for i, result := range results {
		outcome, ok := result.Value.(*pathResult)
		if !ok {
			response.Error = story.NewError(story.CodeDiverge, "", "worker", "unexpected worker result type")
			return a.finishCompare(response, req, editedProject)
		}
		if result.Err != nil && outcome.Err == nil {
			outcome.Err = result.Err
		}
		outcomes[i] = outcome
	}
	if outcomes[0] != nil {
		response.Continuous = outcomes[0].Run
	}
	if outcomes[1] != nil {
		response.SaveResume = outcomes[1].Run
		if outcomes[1].Err == nil {
			response.SaveBoundary = outcomes[1].Boundary
		}
	}
	if outcomes[2] != nil {
		response.EditedReplay = outcomes[2].Run
		if outcomes[2].Outcome != nil {
			response.Invalidation = &outcomes[2].Outcome.Invalidation
		}
	}
	for _, outcome := range outcomes {
		if outcome != nil && outcome.Err != nil {
			response.Error = errorPayload(outcome.Err)
			break
		}
	}
	if response.Error == nil {
		comparisons := []struct {
			name     string
			expected string
			actual   string
			a        *engine.RunResult
			b        *engine.RunResult
		}{
			{"continuous_vs_save_resume", "continuous", "save_resume", response.Continuous, response.SaveResume},
			{"continuous_vs_edited_replay", "continuous", "edited_replay", response.Continuous, response.EditedReplay},
			{"save_resume_vs_edited_replay", "save_resume", "edited_replay", response.SaveResume, response.EditedReplay},
		}
		for _, comparison := range comparisons {
			view := PathComparison{
				Name: comparison.name, Expected: comparison.expected,
				Actual: comparison.actual, OK: true,
			}
			if err := engine.CompareRuns(comparison.a, comparison.b); err != nil {
				view.OK = false
				view.Error = errorPayload(err)
				if response.Error == nil {
					response.Error = view.Error
				}
			}
			response.Comparisons = append(response.Comparisons, view)
		}
	}
	response.OK = response.Error == nil
	return a.finishCompare(response, req, editedProject)
}

func (a *App) finishCompare(response *CompareResponse, req CompareRequest, editedProject *story.Project) *CompareResponse {
	if editedProject != nil {
		response.EditedProject = &story.Project{
			SchemaVersion: editedProject.SchemaVersion,
			ID:            editedProject.ID,
			Title:         editedProject.Title,
			Seed:          editedProject.Seed,
			InitialFlags:  editedProject.InitialFlags,
			Characters:    editedProject.Characters,
			Nodes:         editedProject.Nodes,
			Choices:       editedProject.Choices,
		}
		if graph, err := story.BuildDependencyGraph(editedProject); err == nil {
			response.EditedGraph = graph
		}
	}
	dir := a.artifactDir()
	response.ArtifactDir = dir
	if err := writeJSONAtomic(filepath.Join(dir, "request.json"), req); err != nil {
		response.ArtifactError = err.Error()
		return response
	}
	artifacts := []struct {
		name  string
		value any
	}{
		{"response.json", response},
		{"continuous.json", response.Continuous},
		{"save-resume.json", response.SaveResume},
		{"edited-replay.json", response.EditedReplay},
		{"invalidation.json", response.Invalidation},
	}
	for _, artifact := range artifacts {
		if artifact.value == nil {
			continue
		}
		if err := writeJSONAtomic(filepath.Join(dir, artifact.name), artifact.value); err != nil {
			response.ArtifactError = err.Error()
			break
		}
	}
	return response
}

func errorPayload(err error) *story.Error {
	if err == nil {
		return nil
	}
	if appErr, ok := story.AsError(err); ok {
		return appErr
	}
	return story.NewError(story.CodeSave, "", "error", err.Error())
}
