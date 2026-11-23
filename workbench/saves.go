package workbench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"storyworkbench/engine"
	"storyworkbench/story"
)

type SaveRecord struct {
	ID               string `json:"id"`
	ProjectID        string `json:"project_id"`
	Version          int    `json:"version"`
	Label            string `json:"label"`
	File             string `json:"file"`
	ShotProgress     int    `json:"shot_progress"`
	ChoiceOffset     int    `json:"choice_offset"`
	ContentHash      string `json:"content_hash"`
	Fingerprint      string `json:"fingerprint"`
	SchemaVersion    int    `json:"schema_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	CreatedAt        string `json:"created_at"`
}

type SaveCreateResponse struct {
	Record SaveRecord        `json:"record"`
	Save   engine.Save       `json:"save"`
	Run    *engine.RunResult `json:"run,omitempty"`
}

func (a *App) loadSaveIndex() error {
	path := filepath.Join(a.cfg.SavesDir, "index.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		a.saves = []SaveRecord{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read save index: %w", err)
	}
	if err := json.Unmarshal(data, &a.saves); err != nil {
		return fmt.Errorf("parse save index: %w", err)
	}
	sort.Slice(a.saves, func(i, j int) bool {
		if a.saves[i].ProjectID != a.saves[j].ProjectID {
			return a.saves[i].ProjectID < a.saves[j].ProjectID
		}
		if a.saves[i].Version != a.saves[j].Version {
			return a.saves[i].Version < a.saves[j].Version
		}
		return a.saves[i].ID < a.saves[j].ID
	})
	return nil
}

func (a *App) persistSaveIndexLocked() error {
	path := filepath.Join(a.cfg.SavesDir, "index.json")
	return writeJSONAtomic(path, a.saves)
}

func (a *App) CreateSave(projectID string, boundary int, label string) (*SaveCreateResponse, error) {
	project, err := a.Project(projectID)
	if err != nil {
		return nil, err
	}
	session, err := engine.PlayToBoundary(project, project.Choices, boundary)
	if err != nil {
		return nil, err
	}
	save := session.Snapshot()
	if err := engine.ValidateSave(project, &save, project.Choices); err != nil {
		return nil, err
	}
	if strings.TrimSpace(label) == "" {
		label = fmt.Sprintf("Shot boundary %d", boundary)
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	version := 1
	for _, record := range a.saves {
		if record.ProjectID == projectID && record.ShotProgress == boundary {
			if record.Version >= version {
				version = record.Version + 1
			}
		}
	}
	id := fmt.Sprintf("%s-shot-%05d-v%03d", projectID, boundary, version)
	fileName := id + ".json"
	path := filepath.Join(a.cfg.SavesDir, fileName)
	if err := engine.WriteSaveFile(path, save); err != nil {
		return nil, err
	}
	record := SaveRecord{
		ID:               id,
		ProjectID:        projectID,
		Version:          version,
		Label:            label,
		File:             fileName,
		ShotProgress:     save.State.ShotProgress,
		ChoiceOffset:     save.State.ChoiceOffset,
		ContentHash:      save.ContentHash,
		Fingerprint:      save.Fingerprint,
		SchemaVersion:    save.SchemaVersion,
		AlgorithmVersion: save.AlgorithmVersion,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	a.saves = append(a.saves, record)
	if err := a.persistSaveIndexLocked(); err != nil {
		return nil, err
	}
	return &SaveCreateResponse{Record: record, Save: save}, nil
}

func (a *App) Saves(projectID string) []SaveRecord {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	result := make([]SaveRecord, 0)
	for _, record := range a.saves {
		if projectID == "" || record.ProjectID == projectID {
			result = append(result, record)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Version != result[j].Version {
			return result[i].Version > result[j].Version
		}
		return result[i].CreatedAt > result[j].CreatedAt
	})
	return result
}

func (a *App) saveRecord(projectID, saveID string) (*SaveRecord, error) {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	for _, record := range a.saves {
		if record.ProjectID == projectID && record.ID == saveID {
			copy := record
			return &copy, nil
		}
	}
	return nil, story.NewError(story.CodeNotFound, "", "save_id", "save not found")
}

func (a *App) LoadSave(projectID, saveID string) (*engine.Save, error) {
	project, err := a.Project(projectID)
	if err != nil {
		return nil, err
	}
	record, err := a.saveRecord(projectID, saveID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(a.cfg.SavesDir, record.File)
	save, err := engine.LoadSaveFile(path)
	if err != nil {
		return nil, err
	}
	if err := engine.ValidateSave(project, save, project.Choices); err != nil {
		return nil, err
	}
	return save, nil
}

func (a *App) ResumeSave(projectID, saveID string) (*engine.Save, *engine.RunResult, error) {
	project, err := a.Project(projectID)
	if err != nil {
		return nil, nil, err
	}
	save, err := a.LoadSave(projectID, saveID)
	if err != nil {
		return nil, nil, err
	}
	run, err := engine.Resume(project, save, project.Choices)
	if err != nil {
		return nil, nil, err
	}
	return save, run, nil
}

func (a *App) SavePath(record SaveRecord) string {
	return filepath.Join(a.cfg.SavesDir, record.File)
}
