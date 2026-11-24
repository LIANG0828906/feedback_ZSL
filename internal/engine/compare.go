package engine

import (
	"context"
	"fmt"
	"math"

	"volumediag/internal/diag"
	"volumediag/internal/model"
	"volumediag/internal/volume"
)

// VerifyAgainstFull recomputes every block in an empty in-memory engine and
// compares block buffers, per-pixel statistics and final additive pixels.
func (e *Engine) VerifyAgainstFull(ctx context.Context) (*CompareReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.RLock()
	current := make(map[volume.BlockKey]*volume.BlockResult, len(e.cache))
	for k, v := range e.cache {
		current[k] = v
	}
	scene := e.scene.Clone()
	params := e.params
	bundle := &model.Bundle{
		Scene:      scene,
		Truth:      e.bundle.Truth,
		SourceHash: e.bundle.SourceHash,
		SceneHash:  scene.ComputeHash(),
	}
	e.mu.RUnlock()
	if len(current) != scene.BlockRows()*scene.BlockCols() {
		report := &CompareReport{}
		return report, &diag.Error{
			Code:    diag.EDep,
			Message: "incremental cache is incomplete; cannot compare against full rebuild",
		}
	}
	full, err := Open(bundle, params, "")
	if err != nil {
		return nil, err
	}
	reportFull, err := full.Render(ctx, ModeFull, RenderOptions{})
	if err != nil {
		return nil, err
	}
	if !reportFull.Complete {
		return nil, &diag.Error{Code: diag.EBlock, Message: "full rebuild did not complete"}
	}
	return compareBlockCaches(current, full.Cache())
}

func compareBlockCaches(left, right map[volume.BlockKey]*volume.BlockResult) (*CompareReport, error) {
	report := &CompareReport{}
	keys := make([]volume.BlockKey, 0, len(left))
	for key := range left {
		keys = append(keys, key)
	}
	sortBlockKeys(keys)
	fail := func(key volume.BlockKey, px, py int, item, l, r string) (*CompareReport, error) {
		report.FirstMismatch = fmt.Sprintf("camera=%s block=(%d,%d) pixel=(%d,%d) item=%s",
			key.CameraID, key.X, key.Y, px, py, item)
		report.FirstBlockX = key.X
		report.FirstBlockY = key.Y
		report.PixelX = px
		report.PixelY = py
		report.Item = item
		report.Left = l
		report.Right = r
		report.Equal = false
		return report, &diag.Error{
			Code:     diag.EDiff,
			CameraID: key.CameraID,
			BlockX:   key.X,
			BlockY:   key.Y,
			PixelX:   px,
			PixelY:   py,
			Item:     item,
			Left:     l,
			Right:    r,
			Message:  "incremental and full rebuild differ",
		}
	}
	for _, key := range keys {
		lb := left[key]
		rb := right[key]
		if rb == nil {
			return fail(key, 0, 0, "block_set", "present", "missing")
		}
		report.BlocksCompared++
		if lb.Width != rb.Width || lb.Height != rb.Height || lb.Jitter != rb.Jitter ||
			lb.JitterSamples != rb.JitterSamples || lb.ParamsVersion != rb.ParamsVersion ||
			lb.AlgorithmVersion != rb.AlgorithmVersion || lb.MediumVersion != rb.MediumVersion ||
			lb.GeometryVersion != rb.GeometryVersion {
			return fail(key, 0, 0, "block_metadata", fmt.Sprintf("%+v", lb), fmt.Sprintf("%+v", rb))
		}
		if !stringMapsEqual(lb.LightVersions, rb.LightVersions) {
			return fail(key, 0, 0, "light_versions", fmt.Sprintf("%v", lb.LightVersions), fmt.Sprintf("%v", rb.LightVersions))
		}
		if lb.Digest != rb.Digest {
			return fail(key, 0, 0, "block_digest", lb.Digest, rb.Digest)
		}
		for i := range lb.Pixels {
			lp := lb.Pixels[i]
			rp := rb.Pixels[i]
			report.PixelsCompared++
			for c := 0; c < 3; c++ {
				d := math.Abs(lp.Volume[c] - rp.Volume[c])
				if d > report.MaxAbsVolumeDiff {
					report.MaxAbsVolumeDiff = d
				}
				d = math.Abs(lp.Transmittance[c] - rp.Transmittance[c])
				if d > report.MaxAbsStatDiff {
					report.MaxAbsStatDiff = d
				}
				if !sameFloat(lp.Volume[c], rp.Volume[c]) {
					return fail(key, lp.X, lp.Y, "volume_color", formatColor(lp.Volume), formatColor(rp.Volume))
				}
				if !sameFloat(lp.Transmittance[c], rp.Transmittance[c]) {
					return fail(key, lp.X, lp.Y, "transmittance_stat", formatColor(lp.Transmittance), formatColor(rp.Transmittance))
				}
				if !sameFloat(lp.Scatter[c], rp.Scatter[c]) {
					return fail(key, lp.X, lp.Y, "scatter_stat", formatColor(lp.Scatter), formatColor(rp.Scatter))
				}
			}
		}
		for i := range lb.Composite {
			report.PixelsCompared++
			for c := 0; c < 3; c++ {
				d := math.Abs(lb.Composite[i][c] - rb.Composite[i][c])
				if d > report.MaxAbsPixelDiff {
					report.MaxAbsPixelDiff = d
				}
				if !sameFloat(lb.Composite[i][c], rb.Composite[i][c]) {
					px := lb.Pixels[i].X
					py := lb.Pixels[i].Y
					return fail(key, px, py, "composite_pixel", formatColor(lb.Composite[i]), formatColor(rb.Composite[i]))
				}
			}
		}
		for c := 0; c < 3; c++ {
			if !sameFloat(lb.Stats.MeanTransmittance[c], rb.Stats.MeanTransmittance[c]) ||
				!sameFloat(lb.Stats.MeanScatter[c], rb.Stats.MeanScatter[c]) ||
				!sameFloat(lb.Stats.MaxScatter[c], rb.Stats.MaxScatter[c]) {
				return fail(key, 0, 0, "block_stats", fmt.Sprintf("%+v", lb.Stats), fmt.Sprintf("%+v", rb.Stats))
			}
		}
	}
	if len(left) != len(right) {
		return fail(volume.BlockKey{}, 0, 0, "block_count", fmt.Sprint(len(left)), fmt.Sprint(len(right)))
	}
	report.Equal = true
	return report, nil
}

func sameFloat(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}

func formatColor(c model.Color) string {
	return fmt.Sprintf("[%.17g,%.17g,%.17g]", c[0], c[1], c[2])
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
