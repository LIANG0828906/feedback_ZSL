package pipeline

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"volumekernel/internal/camera"
	"volumekernel/internal/kernel"
	"volumekernel/internal/model"
)

type PixelCoord struct {
	X int
	Y int
}

type BlockPlan struct {
	X      int
	Y      int
	Index  int
	Pixels []PixelCoord
}

func PlanBlocks(vp model.Viewport, r model.Recipe) []BlockPlan {
	cols := (vp.Width + r.BlockWidth - 1) / r.BlockWidth
	rows := (vp.Height + r.BlockHeight - 1) / r.BlockHeight
	out := make([]BlockPlan, 0, cols*rows)
	for by := 0; by < rows; by++ {
		for bx := 0; bx < cols; bx++ {
			p := BlockPlan{X: bx, Y: by, Index: by*cols + bx}
			x0 := bx * r.BlockWidth
			y0 := by * r.BlockHeight
			x1 := x0 + r.BlockWidth
			y1 := y0 + r.BlockHeight
			if x1 > vp.Width {
				x1 = vp.Width
			}
			if y1 > vp.Height {
				y1 = vp.Height
			}
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					p.Pixels = append(p.Pixels, PixelCoord{X: x, Y: y})
				}
			}
			out = append(out, p)
		}
	}
	return out
}

type BlockFingerprint struct {
	Deps             []string
	ContentHash      string
	RayDigest        string
	ParameterVersion string
	CacheKey         string
}

func ComputeFingerprint(s model.SceneSnapshot, cameraIndex int, block BlockPlan, mode string, steps, shadow int) (BlockFingerprint, error) {
	if cameraIndex < 0 || cameraIndex >= len(s.Scene.CameraTrack.Samples) {
		return BlockFingerprint{}, model.ParamError("", "camera_index", "camera index %d out of range", cameraIndex)
	}
	sample := s.Scene.CameraTrack.Samples[cameraIndex]
	depSet := make(map[string]struct{})
	rayIDs := make([]string, 0, len(block.Pixels))
	for _, p := range block.Pixels {
		ray, err := camera.RayForPixel(sample, s.Scene.Viewport, p.X, p.Y)
		if err != nil {
			return BlockFingerprint{}, err
		}
		rayIDs = append(rayIDs, ray.ID)
		for _, d := range kernel.Dependencies(s.Scene, ray) {
			depSet[d] = struct{}{}
		}
	}
	depSet["camera:"+s.Scene.CameraTrack.ID+":"+sample.ID] = struct{}{}
	depSet["recipe:"+s.Scene.Recipe.Version] = struct{}{}
	deps := make([]string, 0, len(depSet))
	for d := range depSet {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	sort.Strings(rayIDs)
	rayDigest, err := model.HashValue(rayIDs)
	if err != nil {
		return BlockFingerprint{}, err
	}
	var mediaParts, allParts []string
	for _, d := range deps {
		vh, isMedia, err := dependencyValueHash(s, d)
		if err != nil {
			return BlockFingerprint{}, err
		}
		allParts = append(allParts, d+"="+vh)
		if isMedia {
			mediaParts = append(mediaParts, d+"="+vh)
		}
	}
	contentHash, err := model.HashValue(mediaParts)
	if err != nil {
		return BlockFingerprint{}, err
	}
	upstreamHash, err := model.HashValue(allParts)
	if err != nil {
		return BlockFingerprint{}, err
	}
	paramVersion := fmt.Sprintf("%s|%s|%s|%d|%d", s.CameraVersion, s.RecipeVersion, mode, steps, shadow)
	cacheKey, err := model.HashValue(struct {
		Format    string
		Content   string
		RayDigest string
		Parameter string
		Upstream  string
		BlockX    int
		BlockY    int
	}{
		Format: model.CacheFormatVersion, Content: contentHash, RayDigest: rayDigest,
		Parameter: paramVersion, Upstream: upstreamHash, BlockX: block.X, BlockY: block.Y,
	})
	if err != nil {
		return BlockFingerprint{}, err
	}
	return BlockFingerprint{
		Deps:             deps,
		ContentHash:      contentHash,
		RayDigest:        rayDigest,
		ParameterVersion: paramVersion,
		CacheKey:         cacheKey,
	}, nil
}

func dependencyValueHash(s model.SceneSnapshot, dep string) (string, bool, error) {
	parts := strings.SplitN(dep, ":", 3)
	if len(parts) == 0 {
		return "", false, fmt.Errorf("malformed dependency %q", dep)
	}
	switch parts[0] {
	case "voxel":
		if len(parts) != 3 {
			return "", true, fmt.Errorf("malformed voxel dependency %q", dep)
		}
		coord := strings.Split(parts[2], ",")
		if len(coord) != 3 {
			return "", true, fmt.Errorf("malformed voxel coordinate %q", parts[2])
		}
		i, err1 := strconv.Atoi(coord[0])
		j, err2 := strconv.Atoi(coord[1])
		k, err3 := strconv.Atoi(coord[2])
		if err1 != nil || err2 != nil || err3 != nil {
			return "", true, fmt.Errorf("malformed voxel coordinate %q", parts[2])
		}
		for _, m := range s.Scene.Media {
			if m.ID == parts[1] {
				if i < 0 || j < 0 || k < 0 || i >= m.Density.NX || j >= m.Density.NY || k >= m.Density.NZ {
					return "", true, fmt.Errorf("voxel dependency out of range %q", dep)
				}
				h, err := model.HashValue(m.Density.Values[(k*m.Density.NY+j)*m.Density.NX+i])
				return h, true, err
			}
		}
		return "", true, fmt.Errorf("unknown medium in dependency %q", dep)
	case "coeff":
		if len(parts) != 3 {
			return "", true, fmt.Errorf("malformed coefficient dependency %q", dep)
		}
		for _, m := range s.Scene.Media {
			if m.ID != parts[1] {
				continue
			}
			switch parts[2] {
			case "absorption":
				h, err := model.HashValue(m.Absorption)
				return h, true, err
			case "scattering":
				h, err := model.HashValue(m.Scattering)
				return h, true, err
			default:
				return "", true, fmt.Errorf("unknown coefficient field %q", parts[2])
			}
		}
		return "", true, fmt.Errorf("unknown medium in dependency %q", dep)
	case "medium":
		if len(parts) < 2 {
			return "", true, fmt.Errorf("malformed medium dependency %q", dep)
		}
		for _, m := range s.Scene.Media {
			if m.ID == parts[1] {
				h, err := model.HashValue(m.MaterialVersion)
				return h, true, err
			}
		}
		return "", true, fmt.Errorf("unknown medium in dependency %q", dep)
	case "camera":
		h, err := model.HashValue(s.CameraVersion)
		return h, false, err
	case "recipe":
		h, err := model.HashValue(s.RecipeVersion)
		return h, false, err
	default:
		h, err := model.HashValue(dep)
		return h, false, err
	}
}
