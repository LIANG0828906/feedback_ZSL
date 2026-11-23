package morph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

type StressLevel struct {
	Name       string      `json:"name"`
	Paths      int         `json:"paths"`
	Keyframes  int         `json:"keyframes"`
	Samples    int         `json:"samples"`
	SceneFile  string      `json:"scene_file"`
	TruthFile  string      `json:"truth_file"`
	SceneBytes int64       `json:"scene_bytes"`
	TruthBytes int64       `json:"truth_bytes"`
	WorkUnits  int64       `json:"work_units"`
	Decision   Quality     `json:"decision"`
	Run        *RunSummary `json:"run,omitempty"`
}

type StressReport struct {
	Seed   int64         `json:"seed"`
	Levels []StressLevel `json:"levels"`
}

func GenerateStressScenarios(outDir string, seed int64, doRun bool) (*StressReport, error) {
	levels := []GenerateOptions{
		{ID: "stress-32", Paths: 32, Keyframes: 2, Segments: 6, Samples: 5},
		{ID: "stress-128", Paths: 128, Keyframes: 3, Segments: 6, Samples: 5},
		{ID: "stress-512", Paths: 512, Keyframes: 4, Segments: 6, Samples: 5},
		{ID: "stress-2048", Paths: 2048, Keyframes: 5, Segments: 5, Samples: 3},
	}
	if err := os.MkdirAll(filepath.Join(outDir, "scenes"), 0o755); err != nil {
		return nil, err
	}
	rep := &StressReport{Seed: seed}
	for _, base := range levels {
		base.Seed = seed
		base.Scale = 4
		base.Quantum = 0.002
		base.ReferenceGrid = 16
		base.CurveSamplesPerPath = 18
		if base.Paths >= 128 {
			base.CurveSamplesPerPath = 12
		}
		if base.Paths >= 512 {
			base.CurveSamplesPerPath = 6
		}
		base.CoordExtent = 48
		base.PaddingPixels = 3
		base.MaxFramePixels = 2048
		scene, truth, err := GenerateScene(base)
		if err != nil {
			return nil, err
		}
		scenePath, truthPath, err := WriteGeneratedCompressed(filepath.Join(outDir, "scenes"), scene, truth)
		if err != nil {
			return nil, err
		}
		sf, _ := os.Stat(scenePath)
		tf, _ := os.Stat(truthPath)
		cfg := DefaultConfigForScene(scene)
		cfg.Runtime.Workers = 4
		cfg.Adaptive.WorkThreshold = 100000
		q, err := DecideQuality(scene, cfg)
		if err != nil {
			return nil, err
		}
		lv := StressLevel{
			Name: base.ID, Paths: len(scene.Paths), Keyframes: base.Keyframes, Samples: len(scene.SampleTimes),
			SceneFile: scenePath, TruthFile: truthPath, SceneBytes: sf.Size(), TruthBytes: tf.Size(),
			WorkUnits: q.WorkUnits, Decision: q,
		}
		if doRun {
			cfg.Checkpoint = filepath.Join(outDir, "checkpoints", base.ID)
			_ = os.RemoveAll(cfg.Checkpoint)
			e, err := NewEngine(scene, cfg)
			if err != nil {
				return nil, err
			}
			sum, err := e.Run(context.Background(), RunOptions{})
			lv.Run = sum
			if err != nil {
				return rep, err
			}
			if err := ExportResults(filepath.Join(outDir, "frames", base.ID), e.Results()); err != nil {
				return rep, err
			}
		}
		rep.Levels = append(rep.Levels, lv)
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return rep, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "stress_report.json"), append(data, '\n'), 0o644); err != nil {
		return rep, err
	}
	return rep, nil
}
