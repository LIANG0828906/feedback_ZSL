package generator

import (
	"fmt"
	"math"
	"sort"

	"volumekernel/internal/camera"
	"volumekernel/internal/geom"
	"volumekernel/internal/model"
	"volumekernel/internal/physics"
)

type truthCell struct {
	MediumID string
	Density  float64
	SigmaA   float64
	SigmaS   float64
	G        float64
}

func BuildTruth(s model.Scene, cameras []int) (model.TruthManifest, error) {
	if len(cameras) == 0 {
		for i := range s.CameraTrack.Samples {
			cameras = append(cameras, i)
		}
	}
	h, err := model.HashScene(s)
	if err != nil {
		return model.TruthManifest{}, err
	}
	ch, err := model.HashValue(s.CameraTrack)
	if err != nil {
		return model.TruthManifest{}, err
	}
	rh, err := model.HashValue(s.Recipe)
	if err != nil {
		return model.TruthManifest{}, err
	}
	m := model.TruthManifest{
		SceneHash:        h,
		SceneVersion:     s.Version,
		CameraVersion:    ch,
		RecipeVersion:    rh,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	for _, ci := range cameras {
		if ci < 0 || ci >= len(s.CameraTrack.Samples) {
			return model.TruthManifest{}, model.ParamError("", "camera_index", "camera index %d out of range", ci)
		}
		sample := s.CameraTrack.Samples[ci]
		for y := 0; y < s.Viewport.Height; y++ {
			for x := 0; x < s.Viewport.Width; x++ {
				ray, err := camera.RayForPixel(sample, s.Viewport, x, y)
				if err != nil {
					return model.TruthManifest{}, err
				}
				tr := analyticRay(s, ray, ci, x, y)
				m.Rays = append(m.Rays, tr)
			}
		}
	}
	return m, nil
}

func analyticRay(s model.Scene, ray camera.Ray, cameraIndex, x, y int) model.TruthRay {
	var items []geom.SweepItem
	for _, medium := range s.Media {
		mi, ok := geom.RayAABB(ray.Origin, ray.Direction, medium.Bounds)
		if !ok {
			continue
		}
		for k := 0; k < medium.Density.NZ; k++ {
			for j := 0; j < medium.Density.NY; j++ {
				for i := 0; i < medium.Density.NX; i++ {
					density := medium.Density.At(i, j, k)
					if density <= 0 {
						continue
					}
					vi, hit := geom.RayVoxel(ray.Origin, ray.Direction, medium, i, j, k)
					if !hit {
						continue
					}
					t0 := math.Max(mi.T0, vi.T0)
					t1 := math.Min(mi.T1, vi.T1)
					if t1 <= t0 {
						continue
					}
					id := fmt.Sprintf("%s/%d/%d/%d", medium.ID, i, j, k)
					items = append(items, geom.SweepItem{
						ID: id,
						T0: t0,
						T1: t1,
						Value: truthCell{
							MediumID: medium.ID,
							Density:  density,
							SigmaA:   medium.Absorption,
							SigmaS:   medium.Scattering,
							G:        medium.Anisotropy,
						},
					})
				}
			}
		}
	}
	spans := geom.Sweep(items)
	transmittance := 1.0
	scattered := 0.0
	out := model.TruthRay{
		CameraIndex:       cameraIndex,
		PixelX:            x,
		PixelY:            y,
		RayID:             ray.ID,
		Transmittance:     1,
		ScatteredRadiance: 0,
	}
	shadow := physics.ShadowReference(s.Light)
	cosTheta := physics.CosTheta(ray.Direction, s.Light.Direction)
	for order, span := range spans {
		sigmaT := 0.0
		sigmaS := 0.0
		scatterG := 0.0
		mediaSet := make(map[string]struct{})
		for _, item := range span.Items {
			c := item.Value.(truthCell)
			sT := c.Density * (c.SigmaA + c.SigmaS)
			sS := c.Density * c.SigmaS
			sigmaT += sT
			sigmaS += sS
			scatterG += sS * c.G
			mediaSet[c.MediumID] = struct{}{}
		}
		if sigmaT <= 0 {
			continue
		}
		mediaIDs := make([]string, 0, len(mediaSet))
		for id := range mediaSet {
			mediaIDs = append(mediaIDs, id)
		}
		sort.Strings(mediaIDs)
		g := 0.0
		if sigmaS > 0 {
			g = scatterG / sigmaS
		}
		length := span.T1 - span.T0
		segT := math.Exp(-sigmaT * length)
		phase := physics.Phase(s.Recipe.Phase, g, cosTheta)
		segScatter := 0.0
		contribution := 0.0
		if sigmaS > 0 {
			segScatter = phase * s.Light.Radiance * shadow * sigmaS / sigmaT * (1 - segT)
			contribution = transmittance * segScatter
		}
		scattered += contribution
		transmittance *= segT
		q0 := model.QuantizeFloor(span.T0, s.FixedScale)
		q1 := model.QuantizeCeil(span.T1, s.FixedScale)
		out.Segments = append(out.Segments, model.SegmentTruth{
			Order:             order,
			T0Ticks:           q0.Ticks,
			T1Ticks:           q1.Ticks,
			T0ResidualTicks:   q0.Residual,
			T1ResidualTicks:   q1.Residual,
			MediaIDs:          mediaIDs,
			Transmittance:     segT,
			ScatteredRadiance: contribution,
		})
	}
	out.Transmittance = transmittance
	out.ScatteredRadiance = scattered
	return out
}
