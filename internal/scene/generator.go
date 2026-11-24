package scene

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"volumediag/internal/model"
	"volumediag/internal/volume"
)

type GenerateOptions struct {
	Seed           int64
	Width          int
	Height         int
	BlockWidth     int
	BlockHeight    int
	ReferenceSteps int
}

func DefaultGenerateOptions() GenerateOptions {
	return GenerateOptions{
		Seed:           20261124,
		Width:          64,
		Height:         48,
		BlockWidth:     8,
		BlockHeight:    8,
		ReferenceSteps: 2048,
	}
}

func Build(opts GenerateOptions) *model.Scene {
	if opts.Seed == 0 {
		opts.Seed = 20261124
	}
	if opts.Width <= 0 {
		opts.Width = 64
	}
	if opts.Height <= 0 {
		opts.Height = 48
	}
	if opts.BlockWidth <= 0 {
		opts.BlockWidth = 8
	}
	if opts.BlockHeight <= 0 {
		opts.BlockHeight = 8
	}
	backDepths := [][]float64{
		{0.00, 0.00, -0.04, 0.00},
		{0.00, 0.05, 0.00, -0.03},
		{0.00, 0.00, 0.02, 0.00},
	}
	leftDepths := [][]float64{
		{0.00, 0.00, 0.00},
		{0.02, 0.00, -0.02},
		{0.00, 0.00, 0.00},
	}
	barDepths := [][]float64{
		{0.00, 0.00},
		{0.00, 0.00},
	}
	return &model.Scene{
		FormatVersion:  model.SceneFormat,
		Seed:           opts.Seed,
		Viewport:       model.Viewport{Width: opts.Width, Height: opts.Height},
		BlockSize:      model.BlockSize{Width: opts.BlockWidth, Height: opts.BlockHeight},
		ActiveCameraID: "cam0",
		CameraPath: []model.Camera{
			{
				ID:       "cam0",
				Position: model.V3(0, 1.8, -16),
				Target:   model.V3(0, -0.2, 0),
				Up:       model.V3(0, 1, 0),
				FovYDeg:  52,
				Near:     0.1,
				Far:      20,
			},
			{
				ID:       "cam1",
				Position: model.V3(-2.2, 1.6, -15),
				Target:   model.V3(0.4, -0.1, 0),
				Up:       model.V3(0, 1, 0),
				FovYDeg:  50,
				Near:     0.1,
				Far:      20,
			},
			{
				ID:       "cam2",
				Position: model.V3(2.0, 2.2, -15.5),
				Target:   model.V3(-0.3, 0.2, 0),
				Up:       model.V3(0, 1, 0),
				FovYDeg:  55,
				Near:     0.1,
				Far:      20,
			},
		},
		Geometry: []model.DepthMesh{
			{
				ID:      "occluder_back",
				Origin:  model.V3(-6, -4, 5),
				AxisU:   model.V3(12, 0, 0),
				AxisV:   model.V3(0, 8, 0),
				Normal:  model.V3(0, 0, -1),
				Columns: 4,
				Rows:    3,
				Depths:  backDepths,
				Color:   model.C3(0.12, 0.16, 0.20),
			},
			{
				ID:      "occluder_left",
				Origin:  model.V3(-6, -4, -2),
				AxisU:   model.V3(2.2, 0, 0),
				AxisV:   model.V3(0, 7, 0),
				Normal:  model.V3(0, 0, -1),
				Columns: 3,
				Rows:    3,
				Depths:  leftDepths,
				Color:   model.C3(0.18, 0.12, 0.08),
			},
			{
				ID:      "occluder_bar",
				Origin:  model.V3(2.2, -1.2, 1.5),
				AxisU:   model.V3(3.6, 0, 0),
				AxisV:   model.V3(0, 2.2, 0),
				Normal:  model.V3(0, 0, -1),
				Columns: 2,
				Rows:    2,
				Depths:  barDepths,
				Color:   model.C3(0.09, 0.11, 0.15),
			},
		},
		Lights: []model.Light{
			{
				ID:          "point_key",
				Type:        "point",
				Position:    model.V3(-3.4, 4.0, -3.2),
				Direction:   model.V3(0.35, -1, 0.25),
				Intensity:   model.C3(4.5, 3.5, 2.5),
				Radius:      1.45,
				Anisotropy:  0.35,
				OccluderIDs: []string{"occluder_back", "occluder_left", "occluder_bar"},
			},
			{
				ID:           "spot_rim",
				Type:         "spot",
				Position:     model.V3(4.5, 4.4, 0.0),
				Direction:    model.V3(-1.0, -0.9, -0.35),
				Intensity:    model.C3(2.0, 3.0, 5.0),
				Radius:       1.15,
				Anisotropy:   0.10,
				OccluderIDs:  []string{"occluder_back", "occluder_bar"},
				ConeInnerDeg: 10,
				ConeOuterDeg: 34,
			},
			{
				ID:          "area_fill",
				Type:        "area",
				Position:    model.V3(-0.5, 4.6, -2.2),
				Direction:   model.V3(0, -1, 0.15),
				Intensity:   model.C3(1.6, 2.1, 2.8),
				Radius:      1.55,
				Anisotropy:  -0.18,
				OccluderIDs: []string{"occluder_left"},
				HalfExtents: [2]float64{2.0, 1.0},
			},
		},
		Medium: model.Medium{
			Extinction: model.C3(0.11, 0.10, 0.09),
			Scattering: model.C3(0.070, 0.065, 0.060),
			PhaseModel: "hg",
			PhaseG:     0.20,
		},
	}
}

func Generate(outDir string, opts GenerateOptions) (*model.Bundle, error) {
	scene := Build(opts)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	steps := opts.ReferenceSteps
	if steps <= 0 {
		steps = volume.ReferenceStepsForFar(scene.CameraPath[0].Far)
	}
	cameraIndex := 0
	active, idx := scene.ActiveCamera()
	if idx < 0 {
		return nil, fmt.Errorf("active camera is missing")
	}
	cameraIndex = idx
	_ = active

	truth := &model.Truth{
		FormatVersion: model.TruthFormat,
		Tolerance: model.Tolerance{
			TransmittanceAbs: 2e-5,
			ScatterAbs:       4e-3,
			PixelAbs:         4e-3,
			Relative:         2e-3,
		},
		Rays:   make([]model.RayTruth, 0, 12),
		Pixels: make([]model.PixelTruth, 0, scene.Viewport.Width*scene.Viewport.Height),
	}
	probePixels := [][2]int{
		{4, 4}, {16, 8}, {32, 8}, {52, 8},
		{8, 20}, {28, 20}, {44, 20}, {58, 20},
		{12, 38}, {32, 38}, {50, 38}, {60, 42},
	}
	probeSet := make(map[int]struct{}, len(probePixels))
	for _, xy := range probePixels {
		if xy[0] >= 0 && xy[0] < scene.Viewport.Width && xy[1] >= 0 && xy[1] < scene.Viewport.Height {
			probeSet[xy[1]*scene.Viewport.Width+xy[0]] = struct{}{}
		}
	}
	frame, ok := volume.NewCameraFrame(active, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return nil, fmt.Errorf("cannot construct active camera frame")
	}
	rayID := 0
	for y := 0; y < scene.Viewport.Height; y++ {
		for x := 0; x < scene.Viewport.Width; x++ {
			ref, ok := volume.ReferencePixel(scene, cameraIndex, x, y, steps)
			if !ok {
				return nil, fmt.Errorf("reference integration failed at (%d,%d)", x, y)
			}
			truth.Pixels = append(truth.Pixels, model.PixelTruth{
				CameraID: active.ID,
				X:        x,
				Y:        y,
				Volume:   ref.Scatter,
			})
			if _, wanted := probeSet[y*scene.Viewport.Width+x]; wanted {
				origin, dir := frame.Ray(active, scene.Viewport.Width, scene.Viewport.Height, x, y)
				truth.Rays = append(truth.Rays, model.RayTruth{
					ID:            fmt.Sprintf("probe-%02d", rayID),
					CameraID:      active.ID,
					PixelX:        x,
					PixelY:        y,
					Origin:        origin,
					Direction:     dir,
					TNear:         0,
					TFar:          active.Far,
					Transmittance: ref.Transmittance,
					Scatter:       ref.Scatter,
				})
				rayID++
			}
		}
	}
	if len(truth.Rays) == 0 {
		return nil, fmt.Errorf("no truth probe rays generated")
	}

	sceneBytes, err := json.MarshalIndent(scene, "", "  ")
	if err != nil {
		return nil, err
	}
	truthBytes, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	manifest := model.SourceManifest{
		GeneratorID: model.GeneratorID,
		Seed:        scene.Seed,
		SourceHash:  model.HashSources(sceneBytes, truthBytes),
		SceneHash:   scene.ComputeHash(),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(filepath.Join(outDir, "scene.json"), sceneBytes); err != nil {
		return nil, err
	}
	if err := atomicWrite(filepath.Join(outDir, "truth.json"), truthBytes); err != nil {
		return nil, err
	}
	if err := atomicWrite(filepath.Join(outDir, "manifest.json"), manifestBytes); err != nil {
		return nil, err
	}
	return &model.Bundle{
		Scene:      scene,
		Truth:      truth,
		SourceHash: manifest.SourceHash,
		SceneHash:  manifest.SceneHash,
		Dir:        outDir,
	}, nil
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}
