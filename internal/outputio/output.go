package outputio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"fixedsampler/internal/engine"
	"fixedsampler/internal/model"
)

func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func WriteRunOutputs(dir string, p *model.Project, res *engine.RunResult, report *engine.ErrorReport) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(dir, "levels"), 0o755); err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(dir, "report.json"), report); err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(dir, "dependency_graph.json"), engine.BuildGraph(p, res)); err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(dir, "invalidation.json"), res.Invalidations); err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(dir, "commit_sequence.json"), res.CommitSequence); err != nil {
		return err
	}
	final := struct {
		ProjectID string                  `json:"project_id"`
		Planes    map[string]*model.Plane `json:"planes"`
	}{p.ProjectID, res.FinalPlanes}
	if err := WriteJSON(filepath.Join(dir, "final_texture.json"), final); err != nil {
		return err
	}
	for seq, planes := range res.LevelPlanes {
		if err := WriteJSON(filepath.Join(dir, "levels", "level-"+pad(seq)+".json"), planes); err != nil {
			return err
		}
	}
	return nil
}

func pad(seq int) string {
	return fmt.Sprintf("%02d", seq)
}
