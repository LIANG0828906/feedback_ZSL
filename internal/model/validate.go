package model

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"qualitybench/internal/core"
)

func ValidateProject(p *Project) error {
	if p == nil {
		return core.Schema("project", "project is missing")
	}
	if strings.TrimSpace(p.SchemaVersion) == "" {
		return core.Schema("schema_version", "required field is missing")
	}
	if p.Width <= 0 {
		return core.Schema("width", "required positive field is missing or invalid")
	}
	if p.Height <= 0 {
		return core.Schema("height", "required positive field is missing or invalid")
	}
	if p.Channels != 3 {
		return core.Range("channels", "only 3-channel RGB benchmarks are supported, got %d", p.Channels)
	}
	if p.BlockSize <= 0 {
		return core.Schema("block_size", "required positive field is missing or invalid")
	}
	if p.Width%p.BlockSize != 0 || p.Height%p.BlockSize != 0 {
		candidates := CommonCandidates(p.Width, p.Height)
		e := core.BlockError("block_size %d does not divide %dx%d", p.BlockSize, p.Width, p.Height)
		e.Details = map[string]any{"block_size": p.BlockSize, "candidates": candidates}
		return e
	}
	if !IsFinite(p.AttributionShare) || p.AttributionShare <= 0 || p.AttributionShare > 1 {
		return core.Para(0, "attribution_share", "must be in (0,1], got %v", p.AttributionShare)
	}
	if !IsFinite(p.MetricTolerance) || p.MetricTolerance < 0 || p.MetricTolerance > 1 {
		return core.Para(0, "metric_tolerance", "must be in [0,1], got %v", p.MetricTolerance)
	}
	if strings.TrimSpace(p.AlgorithmVersion) == "" {
		return core.Schema("algorithm_version", "required field is missing")
	}
	if len(p.Images) == 0 {
		return core.Schema("images", "at least one image is required")
	}
	seenImages := make(map[string]bool, len(p.Images))
	for i, image := range p.Images {
		path := fmt.Sprintf("images[%d]", i)
		if strings.TrimSpace(image.ID) == "" {
			return core.Schema(path+".id", "required field is missing")
		}
		if seenImages[image.ID] {
			return core.Range(path+".id", "duplicate image id %q", image.ID)
		}
		seenImages[image.ID] = true
		if image.Frame < 0 {
			return core.Range(path+".frame", "frame must be non-negative")
		}
		if !image.Watermark.InFrame(p.Width, p.Height) {
			return core.Range(path+".watermark", "watermark %s is outside %dx%d", image.Watermark, p.Width, p.Height)
		}
		for j, shape := range image.Shapes {
			sp := fmt.Sprintf("%s.shapes[%d]", path, j)
			if strings.TrimSpace(shape.ID) == "" {
				return core.Schema(sp+".id", "required field is missing")
			}
			if shape.Kind == "" {
				return core.Schema(sp+".kind", "required field is missing")
			}
			if !shape.BBox.InFrame(p.Width, p.Height) {
				return core.Range(sp+".bbox", "shape bbox %s is outside %dx%d", shape.BBox, p.Width, p.Height)
			}
			if shape.BBox.Intersects(image.Watermark) {
				return core.Range(sp+".bbox", "watermark %s overlaps shape %q at %s",
					image.Watermark, shape.ID, shape.BBox)
			}
		}
		for j, detail := range image.Details {
			dp := fmt.Sprintf("%s.details[%d]", path, j)
			if !detail.BBox.InFrame(p.Width, p.Height) {
				return core.Range(dp+".bbox", "detail bbox %s is outside %dx%d", detail.BBox, p.Width, p.Height)
			}
			if detail.BBox.Intersects(image.Watermark) {
				return core.Range(dp+".bbox", "detail bbox overlaps watermark")
			}
			if detail.Period < 2 {
				return core.Range(dp+".period", "period must be >= 2")
			}
			if detail.Delta < 1 || detail.Delta > 32 {
				return core.Range(dp+".delta", "delta must be in [1,32]")
			}
		}
	}
	seenChains := make(map[string]bool, len(p.Chains))
	for i, chain := range p.Chains {
		path := fmt.Sprintf("chains[%d]", i)
		if strings.TrimSpace(chain.ID) == "" {
			return core.Schema(path+".id", "required field is missing")
		}
		if !seenImages[chain.ImageID] {
			return core.Schema(path+".image_id", "references unknown image %q", chain.ImageID)
		}
		if seenChains[chain.ImageID] {
			return core.Range(path+".image_id", "duplicate chain for image %q", chain.ImageID)
		}
		seenChains[chain.ImageID] = true
		if len(chain.Steps) == 0 {
			return core.Schema(path+".steps", "at least one step is required")
		}
		if err := ValidateSteps(chain.Steps, p.Width, p.Height); err != nil {
			return err
		}
	}
	for imageID := range seenImages {
		if !seenChains[imageID] {
			return core.Schema("chains", "image %q has no degradation chain", imageID)
		}
	}
	for i, edit := range p.Edits {
		if err := ValidateEdit(edit, p, fmt.Sprintf("edits[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

func ValidateSteps(steps []Step, width, height int) error {
	for i, step := range steps {
		path := fmt.Sprintf("steps[%d]", i)
		if !step.Type.Valid() {
			return core.Para(i+1, "type", "unknown step type %q", step.Type)
		}
		if !IsFinite(step.Strength) {
			return core.Para(i+1, "strength", "strength must be finite")
		}
		lo, hi := -1.0, 1.0
		if step.Type != StepChroma {
			lo = 0
		}
		if step.Strength < lo || step.Strength > hi {
			return core.Para(i+1, "strength", "must be in [%g,%g], got %g", lo, hi, step.Strength)
		}
		if step.Region != nil && !step.Region.InFrame(width, height) {
			return core.Para(i+1, "region", "region %s is outside %dx%d", *step.Region, width, height)
		}
		_ = path
	}
	return nil
}

func ValidateEdit(edit Edit, p *Project, path string) error {
	if !contains(p.Images, edit.ImageID) {
		return core.Schema(path+".image_id", "references unknown image %q", edit.ImageID)
	}
	switch edit.Op {
	case "set_strength":
		if edit.Step < 1 || edit.Step > len(chainFor(p, edit.ImageID).Steps) {
			return core.Range(path+".step", "step is outside chain")
		}
		if edit.Strength == nil || !IsFinite(*edit.Strength) {
			return core.Schema(path+".streength", "strength is required and finite")
		}
	case "set_region":
		if edit.Step < 1 || edit.Step > len(chainFor(p, edit.ImageID).Steps) {
			return core.Range(path+".step", "step is outside chain")
		}
		if edit.Region != nil && !edit.Region.InFrame(p.Width, p.Height) {
			return core.Range(path+".region", "region is outside frame")
		}
	case "move_step":
		n := len(chainFor(p, edit.ImageID).Steps)
		if edit.From < 1 || edit.From > n || edit.To < 1 || edit.To > n {
			return core.Range(path+".from", "step positions must be in [1,%d]", n)
		}
	case "rollback":
		if strings.TrimSpace(edit.Version) == "" {
			return core.Schema(path+".version", "version is required")
		}
	default:
		return core.Para(edit.Step, "op", "unknown edit op %q", edit.Op)
	}
	return nil
}

func ValidateTruth(t *Truth, p *Project) error {
	if t == nil {
		return core.Schema("truth", "truth is missing")
	}
	if strings.TrimSpace(t.SchemaVersion) == "" {
		return core.Schema("truth.schema_version", "required field is missing")
	}
	if p != nil {
		fp, err := Fingerprint(p)
		if err != nil {
			return core.Schema("project", "cannot fingerprint project: %v", err)
		}
		if t.ProjectFingerprint != fp {
			return core.Range("truth.project_fingerprint", "does not match project (%s != %s)", t.ProjectFingerprint, fp)
		}
		if t.Width != p.Width || t.Height != p.Height || t.Channels != p.Channels {
			return core.Range("truth", "truth geometry does not match project")
		}
		if t.BlockSize != p.BlockSize {
			return core.Range("truth.block_size", "truth block size %d != project %d", t.BlockSize, p.BlockSize)
		}
		if t.AlgorithmVersion != p.AlgorithmVersion {
			return core.Range("truth.algorithm_version", "algorithm versions differ")
		}
	}
	if t.Width <= 0 || t.Height <= 0 || t.Channels <= 0 || t.BlockSize <= 0 {
		return core.Schema("truth", "missing or invalid geometry")
	}
	if t.Width%t.BlockSize != 0 || t.Height%t.BlockSize != 0 {
		e := core.BlockError("truth block_size %d does not divide %dx%d", t.BlockSize, t.Width, t.Height)
		e.Details = map[string]any{"candidates": CommonCandidates(t.Width, t.Height)}
		return e
	}
	if !IsFinite(t.AttributionShare) || t.AttributionShare <= 0 || t.AttributionShare > 1 {
		return core.Range("truth.attribution_share", "must be in (0,1]")
	}
	if !IsFinite(t.AttributionTolerance) || t.AttributionTolerance < 0 || t.AttributionTolerance > 1 {
		return core.Range("truth.attribution_tolerance", "must be in [0,1]")
	}
	if len(t.Images) == 0 {
		return core.Schema("truth.images", "at least one image truth entry is required")
	}
	expectedBlocks := (t.Width / t.BlockSize) * (t.Height / t.BlockSize)
	for i, image := range t.Images {
		path := fmt.Sprintf("truth.images[%d]", i)
		if strings.TrimSpace(image.ID) == "" {
			return core.Schema(path+".id", "required field is missing")
		}
		if p != nil {
			spec, ok := p.Image(image.ID)
			if !ok {
				return core.Range(path+".id", "unknown image %q", image.ID)
			}
			if image.Frame != spec.Frame {
				return core.Range(path+".frame", "frame does not match project")
			}
			if !image.Watermark.InFrame(t.Width, t.Height) {
				return core.Range(path+".watermark", "watermark outside frame")
			}
			for j, shape := range image.Shapes {
				if !shape.BBox.InFrame(t.Width, t.Height) {
					return core.Range(fmt.Sprintf("%s.shapes[%d].bbox", path, j), "shape outside frame")
				}
				if shape.BBox.Intersects(image.Watermark) {
					return core.Range(fmt.Sprintf("%s.shapes[%d].bbox", path, j),
						"watermark overlaps declared shape")
				}
			}
			chain := chainFor(p, image.ID)
			if image.StepCount != len(chain.Steps) {
				return core.Range(path+".step_count", "step count %d != chain length %d",
					image.StepCount, len(chain.Steps))
			}
		}
		if len(image.Steps) != image.StepCount {
			return core.Schema(path+".steps", "steps length %d does not match step_count %d",
				len(image.Steps), image.StepCount)
		}
		for j := range image.Steps {
			if !image.Steps[j].Type.Valid() {
				return core.Para(j+1, "type", "unknown truth step type %q", image.Steps[j].Type)
			}
		}
		if len(image.Samples) < 3 {
			return core.Schema(path+".samples", "at least 3 samples are required")
		}
		uniq := make(map[string]bool)
		for j, s := range image.Samples {
			sp := fmt.Sprintf("%s.samples[%d]", path, j)
			if !IsFinite(s.X) || !IsFinite(s.Y) {
				return core.Range(sp, "sample coordinates must be finite")
			}
			if s.X < 0 || s.Y < 0 || s.X >= float64(t.Width) || s.Y >= float64(t.Height) {
				return core.Range(sp, "sample (%.3f,%.3f) is outside %dx%d", s.X, s.Y, t.Width, t.Height)
			}
			if s.Tolerance < 0 || s.Tolerance > 255 {
				return core.Range(sp+".tolerance", "tolerance must be in [0,255]")
			}
			key := fmt.Sprintf("%d,%d", int(math.Floor(s.X)), int(math.Floor(s.Y)))
			uniq[key] = true
		}
		if len(uniq) < 2 {
			return core.Range(path+".samples", "sampling coordinates are degenerate")
		}
		if len(image.Blocks) != expectedBlocks {
			return core.BlockError("truth block count %d != expected %d for image %s",
				len(image.Blocks), expectedBlocks, image.ID)
		}
		seenBlocks := make(map[string]bool)
		for j, block := range image.Blocks {
			bp := fmt.Sprintf("%s.blocks[%d]", path, j)
			if block.W <= 0 || block.H <= 0 {
				return core.Schema(bp, "block dimensions are missing or invalid")
			}
			if block.X%t.BlockSize != 0 || block.Y%t.BlockSize != 0 ||
				block.W != t.BlockSize || block.H != t.BlockSize ||
				!(core.Rect{X: block.X, Y: block.Y, W: block.W, H: block.H}).InFrame(t.Width, t.Height) {
				return core.Range(bp, "block geometry is not on the configured grid")
			}
			key := fmt.Sprintf("%d,%d", block.X, block.Y)
			if seenBlocks[key] {
				return core.Range(bp, "duplicate block coordinates")
			}
			seenBlocks[key] = true
			for _, pair := range [][2]float64{
				block.Metrics.NoiseVariance,
				block.Metrics.EdgeSharpness,
				block.Metrics.ChromaShift,
			} {
				if !IsFinite(pair[0]) || !IsFinite(pair[1]) || pair[0] > pair[1] {
					return core.Range(bp+".metrics", "metric interval must be finite and ordered")
				}
			}
			if block.Attribution.Step < 0 || block.Attribution.Step > image.StepCount {
				return core.Range(bp+".attribution.step", "attribution step is outside chain")
			}
			if !IsFinite(block.Attribution.Share) || block.Attribution.Share < 0 || block.Attribution.Share > 1 {
				return core.Range(bp+".attribution.share", "share must be in [0,1]")
			}
			if !IsFinite(block.Attribution.Tolerance) || block.Attribution.Tolerance < 0 || block.Attribution.Tolerance > 1 {
				return core.Range(bp+".attribution.tolerance", "tolerance must be in [0,1]")
			}
			if block.Attribution.Step == 0 && block.Attribution.Category != "none" {
				return core.Range(bp+".attribution.category", "step 0 must use category none")
			}
		}
	}
	return nil
}

func CommonCandidates(width, height int) []int {
	divisors := core.CommonDivisors(width, height)
	if len(divisors) <= 1 {
		return divisors
	}
	out := make([]int, 0, len(divisors))
	for _, d := range divisors {
		out = append(out, d)
	}
	sort.Ints(out)
	return out
}

func contains(images []ImageSpec, id string) bool {
	for _, image := range images {
		if image.ID == id {
			return true
		}
	}
	return false
}

func chainFor(p *Project, imageID string) Chain {
	if p == nil {
		return Chain{}
	}
	for _, chain := range p.Chains {
		if chain.ImageID == imageID {
			return chain
		}
	}
	return Chain{}
}
