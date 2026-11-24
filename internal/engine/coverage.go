package engine

import (
	"volumediag/internal/diag"
	"volumediag/internal/model"
	"volumediag/internal/volume"
)

func computeLightCoverage(scene *model.Scene, cameraIndex int, p volume.Params) (map[string]map[volume.BlockKey]struct{}, error) {
	if cameraIndex < 0 || cameraIndex >= len(scene.CameraPath) {
		return nil, diag.Dependency("", 0, 0, "cannot compute light coverage without a valid camera")
	}
	camera := &scene.CameraPath[cameraIndex]
	frame, ok := volume.NewCameraFrame(camera, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return nil, diag.Dependency(camera.ID, 0, 0, "cannot construct camera basis for coverage")
	}
	coverage := make(map[string]map[volume.BlockKey]struct{}, len(scene.Lights))
	for i := range scene.Lights {
		coverage[scene.Lights[i].ID] = make(map[volume.BlockKey]struct{})
	}
	for by := 0; by < scene.BlockRows(); by++ {
		for bx := 0; bx < scene.BlockCols(); bx++ {
			key := volume.BlockKey{
				CameraIndex: cameraIndex,
				CameraID:    camera.ID,
				X:           bx,
				Y:           by,
			}
			x0 := bx * scene.BlockSize.Width
			y0 := by * scene.BlockSize.Height
			x1 := min(x0+scene.BlockSize.Width, scene.Viewport.Width)
			y1 := min(y0+scene.BlockSize.Height, scene.Viewport.Height)
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					origin, dir := frame.Ray(camera, scene.Viewport.Width, scene.Viewport.Height, x, y)
					for li := range scene.Lights {
						light := &scene.Lights[li]
						if volume.LightCanInfluenceRay(light, origin, dir, camera.Far) {
							coverage[light.ID][key] = struct{}{}
						}
					}
				}
			}
		}
	}
	return coverage, nil
}

func computeBlockGeometry(scene *model.Scene, cameraIndex int, coverage map[string]map[volume.BlockKey]struct{}) map[volume.BlockKey]map[string]struct{} {
	out := make(map[volume.BlockKey]map[string]struct{})
	for i := range scene.Lights {
		light := &scene.Lights[i]
		for key := range coverage[light.ID] {
			deps := out[key]
			if deps == nil {
				deps = make(map[string]struct{})
				out[key] = deps
			}
			for _, id := range light.OccluderIDs {
				deps[id] = struct{}{}
			}
		}
	}
	if cameraIndex < 0 || cameraIndex >= len(scene.CameraPath) {
		return out
	}
	camera := &scene.CameraPath[cameraIndex]
	frame, ok := volume.NewCameraFrame(camera, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return out
	}
	for by := 0; by < scene.BlockRows(); by++ {
		for bx := 0; bx < scene.BlockCols(); bx++ {
			key := volume.BlockKey{CameraIndex: cameraIndex, CameraID: camera.ID, X: bx, Y: by}
			x0 := bx * scene.BlockSize.Width
			y0 := by * scene.BlockSize.Height
			x1 := min(x0+scene.BlockSize.Width, scene.Viewport.Width)
			y1 := min(y0+scene.BlockSize.Height, scene.Viewport.Height)
			for mi := range scene.Geometry {
				mesh := &scene.Geometry[mi]
				hit := false
				for y := y0; y < y1 && !hit; y++ {
					for x := x0; x < x1; x++ {
						origin, dir := frame.Ray(camera, scene.Viewport.Width, scene.Viewport.Height, x, y)
						if _, ok := volume.MeshIntersect(mesh, origin, dir, camera.Far); ok {
							hit = true
							break
						}
					}
				}
				if hit {
					deps := out[key]
					if deps == nil {
						deps = make(map[string]struct{})
						out[key] = deps
					}
					deps[mesh.ID] = struct{}{}
				}
			}
		}
	}
	return out
}

func geometryHitBlocks(scene *model.Scene, cameraIndex int, meshID string) map[volume.BlockKey]struct{} {
	out := make(map[volume.BlockKey]struct{})
	if cameraIndex < 0 || cameraIndex >= len(scene.CameraPath) {
		return out
	}
	mesh, idx := scene.FindMesh(meshID)
	if idx < 0 {
		return out
	}
	camera := &scene.CameraPath[cameraIndex]
	frame, ok := volume.NewCameraFrame(camera, scene.Viewport.Width, scene.Viewport.Height)
	if !ok {
		return out
	}
	for by := 0; by < scene.BlockRows(); by++ {
		for bx := 0; bx < scene.BlockCols(); bx++ {
			key := volume.BlockKey{CameraIndex: cameraIndex, CameraID: camera.ID, X: bx, Y: by}
			x0 := bx * scene.BlockSize.Width
			y0 := by * scene.BlockSize.Height
			x1 := min(x0+scene.BlockSize.Width, scene.Viewport.Width)
			y1 := min(y0+scene.BlockSize.Height, scene.Viewport.Height)
			hit := false
			for y := y0; y < y1 && !hit; y++ {
				for x := x0; x < x1; x++ {
					origin, dir := frame.Ray(camera, scene.Viewport.Width, scene.Viewport.Height, x, y)
					if _, ok := volume.MeshIntersect(mesh, origin, dir, camera.Far); ok {
						hit = true
						break
					}
				}
			}
			if hit {
				out[key] = struct{}{}
			}
		}
	}
	return out
}

func lightsReferencingMesh(scene *model.Scene, meshID string) []string {
	var out []string
	for i := range scene.Lights {
		for _, id := range scene.Lights[i].OccluderIDs {
			if id == meshID {
				out = append(out, scene.Lights[i].ID)
				break
			}
		}
	}
	return out
}

func sortedCoverageKeys(m map[volume.BlockKey]struct{}) []volume.BlockKey {
	out := make([]volume.BlockKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortBlockKeys(out)
	return out
}

func sortBlockKeys(keys []volume.BlockKey) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j].Less(keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
