// Package sample generates deterministic local reverse-imaging tasks.
package sample

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"diffrender/render"
	"diffrender/scene"
)

// Task contains the canonical truth, a perturbed initial scene, all schema
// renditions, and target images for two cameras.
type Task struct {
	Seed            int64
	Name            string
	Truth           *scene.Scene
	Initial         *scene.Scene
	TruthValues     scene.Truth
	VersionFiles    map[string][]byte
	InitialVersions map[string][]byte
	Targets         []render.Image
	TargetNames     []string
}

// GenerateTask builds one deterministic task from a fixed seed.
func GenerateTask(seed int64) (*Task, error) {
	rng := rand.New(rand.NewSource(seed))
	radius := 0.58 + rng.Float64()*0.10
	center := scene.Vec3{
		X: (rng.Float64()*2 - 1) * 0.34,
		Y: radius,
		Z: (rng.Float64()*2 - 1) * 0.14,
	}
	albedo := scene.Vec3{
		X: 0.58 + rng.Float64()*0.30,
		Y: 0.16 + rng.Float64()*0.22,
		Z: 0.18 + rng.Float64()*0.24,
	}
	planeAlbedo := scene.Vec3{
		X: 0.55 + rng.Float64()*0.10,
		Y: 0.58 + rng.Float64()*0.10,
		Z: 0.62 + rng.Float64()*0.10,
	}
	lightCenter := scene.Vec3{X: 0.8 + rng.Float64()*0.5, Y: 3.8 + rng.Float64()*0.3, Z: 1.1 + rng.Float64()*0.5}
	aim := scene.Vec3{X: center.X * 0.3, Y: 0.45, Z: center.Z}
	lightNormal, _ := aim.Sub(lightCenter).Normalized()
	lightPower := 68 + rng.Float64()*22
	cameraJitter := func() float64 { return (rng.Float64()*2 - 1) * 0.08 }
	cam0 := makeCamera("camera_0", scene.Vec3{X: center.X - 2.25 + cameraJitter(), Y: 2.05 + cameraJitter(), Z: 3.75 + cameraJitter()}, center, 28, 28)
	cam1 := makeCamera("camera_1", scene.Vec3{X: center.X + 1.55 + cameraJitter(), Y: 2.00 + cameraJitter(), Z: 3.55 + cameraJitter()}, center, 28, 28)
	truth := &scene.Scene{
		ID:             fmt.Sprintf("task-%d", seed),
		SourceVersion:  scene.Version3,
		WorkingVersion: scene.WorkingVersion,
		Spheres: []scene.Sphere{{
			ID: "sphere_main", Center: center, Radius: radius, Albedo: albedo,
		}},
		Planes: []scene.Plane{{
			ID: "plane_ground", Normal: scene.Vec3{Y: 1}, Offset: 0, Albedo: planeAlbedo,
		}},
		Lights: []scene.AreaLight{{
			ID: "light_key", Center: lightCenter, Normal: lightNormal, Up: scene.Vec3{Z: 1},
			HalfWidth: 1.1, HalfHeight: 0.85, RadiantPowerW: lightPower,
			Color: scene.Vec3{X: 1, Y: 0.94, Z: 0.86},
		}},
		Cameras: []scene.Camera{cam0, cam1},
		Settings: scene.Settings{
			ShadowSoftness: scene.DefaultShadowSoftness,
			ToneMapping:    scene.DefaultToneMapping,
		},
	}
	if err := scene.Validate(truth, "<sample>"); err != nil {
		return nil, err
	}
	initial := truth.Clone()
	initial.SourceVersion = scene.Version3
	initial.ID = truth.ID + "-initial"
	sx := initial.Spheres[0]
	sx.Center.X += (rng.Float64()*2 - 1) * 0.05
	sx.Center.Y += (rng.Float64()*2 - 1) * 0.04
	sx.Center.Z += (rng.Float64()*2 - 1) * 0.04
	sx.Radius *= 0.95 + rng.Float64()*0.10
	sx.Albedo.X = clamp01(sx.Albedo.X + (rng.Float64()*2-1)*0.05)
	sx.Albedo.Y = clamp01(sx.Albedo.Y + (rng.Float64()*2-1)*0.05)
	sx.Albedo.Z = clamp01(sx.Albedo.Z + (rng.Float64()*2-1)*0.05)
	initial.Spheres[0] = sx
	initial.Planes[0].Albedo.X = clamp01(initial.Planes[0].Albedo.X + (rng.Float64()*2-1)*0.02)
	initial.Planes[0].Albedo.Y = clamp01(initial.Planes[0].Albedo.Y + (rng.Float64()*2-1)*0.02)
	initial.Planes[0].Albedo.Z = clamp01(initial.Planes[0].Albedo.Z + (rng.Float64()*2-1)*0.02)
	// Light intensity is calibrated by the task provider. Known exposure keeps
	// the photometric problem from having an unobservable global scale factor.
	if err := scene.Validate(initial, "<sample-initial>"); err != nil {
		return nil, err
	}
	task := &Task{
		Seed: seed, Name: truth.ID, Truth: truth, Initial: initial,
		VersionFiles: make(map[string][]byte), InitialVersions: make(map[string][]byte),
		TruthValues: scene.Truth{
			Spheres: []scene.TruthSphere{{
				ID: "sphere_main", Center: center, Radius: radius, Albedo: albedo,
			}},
			Lights: []scene.TruthLight{{ID: "light_key", RadiantPowerW: lightPower}},
		},
	}
	for _, version := range scene.SupportedVersions {
		res, err := scene.Export(truth, scene.ExportOptions{Version: version, AllowLossy: true})
		if err != nil {
			return nil, err
		}
		task.VersionFiles[version] = res.Data
		ires, err := scene.Export(initial, scene.ExportOptions{Version: version, AllowLossy: true})
		if err != nil {
			return nil, err
		}
		task.InitialVersions[version] = ires.Data
	}
	images, err := render.Render(truth, render.Config{MaxWorkers: 1, BlockPixels: 128, QueueCapacity: 2, Retries: 0})
	if err != nil {
		return nil, err
	}
	task.Targets = images
	for _, cam := range truth.Cameras {
		task.TargetNames = append(task.TargetNames, cam.ID+".drtgt")
	}
	return task, nil
}

func makeCamera(id string, eye, focus scene.Vec3, width, height int) scene.Camera {
	forward, ok := focus.Sub(eye).Normalized()
	if !ok {
		forward = scene.Vec3{Z: -1}
	}
	return scene.Camera{
		ID: id, Eye: eye, LookAt: eye.Add(forward), Up: scene.Vec3{Y: 1},
		FovYRad: 38 * math.Pi / 180, Width: width, Height: height,
	}
}

// WriteTask materializes all scene versions, initial scenes, targets, and truth.
func WriteTask(dir string, task *Task) error {
	if task == nil {
		return fmt.Errorf("sample: nil task")
	}
	if err := os.MkdirAll(filepath.Join(dir, "scenes"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "initial"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "targets"), 0o755); err != nil {
		return err
	}
	for _, version := range scene.SupportedVersions {
		if err := os.WriteFile(filepath.Join(dir, "scenes", "scene-"+version+".json"), task.VersionFiles[version], 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "initial", "initial-"+version+".json"), task.InitialVersions[version], 0o644); err != nil {
			return err
		}
	}
	for i, im := range task.Targets {
		if err := render.WriteTargetFile(filepath.Join(dir, "targets", task.TargetNames[i]), im); err != nil {
			return err
		}
	}
	truthData, err := json.MarshalIndent(task.TruthValues, "", "  ")
	if err != nil {
		return err
	}
	truthData = append(truthData, '\n')
	return os.WriteFile(filepath.Join(dir, "truth.json"), truthData, 0o644)
}

// ScenePaths returns stable relative scene paths.
func (t *Task) ScenePaths(root string) map[string]string {
	out := make(map[string]string)
	for _, version := range scene.SupportedVersions {
		out[version] = filepath.Join(root, "scenes", "scene-"+version+".json")
	}
	return out
}

func (t *Task) InitialPaths(root string) map[string]string {
	out := make(map[string]string)
	for _, version := range scene.SupportedVersions {
		out[version] = filepath.Join(root, "initial", "initial-"+version+".json")
	}
	return out
}

func (t *Task) TargetPaths(root string) []string {
	out := make([]string, len(t.TargetNames))
	for i, name := range t.TargetNames {
		out[i] = filepath.Join(root, "targets", name)
	}
	return out
}

// SupportedVersions returns a stable copy for report iteration.
func SupportedVersions() []string {
	out := append([]string(nil), scene.SupportedVersions...)
	sort.Strings(out)
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
