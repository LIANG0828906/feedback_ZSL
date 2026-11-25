package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"math"
	"sort"
	"strconv"
	"time"

	"iconseg/internal/core"
	"iconseg/internal/seg"
)

type RegionRecord = core.RegionRecord

type ExactResult struct {
	Snapshot         core.Snapshot   `json:"snapshot"`
	Mask             []byte          `json:"-"`
	RawMask          []byte          `json:"-"`
	Labels           []int32         `json:"-"`
	InitialMarkers   []int32         `json:"-"`
	Gradient         []float64       `json:"-"`
	RegionTable      []RegionRecord  `json:"region_table"`
	Components       []seg.Component `json:"components"`
	Match            seg.MatchReport `json:"match"`
	FloodSamples     int             `json:"flood_samples"`
	Reached          int             `json:"reached_pixels"`
	MergeCount       int             `json:"merge_count"`
	Elapsed          time.Duration   `json:"elapsed_ns"`
	MaskHash         string          `json:"mask_hash"`
	StatsHash        string          `json:"stats_hash"`
	TruthFingerprint string          `json:"truth_fingerprint"`
}

type PreviewResult struct {
	Snapshot          core.Snapshot   `json:"snapshot"`
	Mask              []byte          `json:"-"`
	Components        []seg.Component `json:"components"`
	Downsample        int             `json:"downsample"`
	FloodSamples      int             `json:"flood_samples"`
	EquivalentSamples int             `json:"equivalent_samples"`
	Elapsed           time.Duration   `json:"elapsed_ns"`
	WithinLatency     bool            `json:"within_latency"`
	Placeholder       bool            `json:"placeholder"`
	Rationale         string          `json:"rationale"`
	MaskHash          string          `json:"mask_hash"`
}

type ConsistencyReport struct {
	Consistent        bool                  `json:"consistent"`
	Measured          core.DivergenceReport `json:"measured"`
	PreviewVersion    string                `json:"preview_version"`
	ExactVersion      string                `json:"exact_version"`
	SnapshotSeed      int64                 `json:"snapshot_seed"`
	SameSeedSet       bool                  `json:"same_seed_set"`
	SameAlgorithm     bool                  `json:"same_algorithm"`
	PreviewDownsample int                   `json:"preview_downsample"`
}

type segmentationResult struct {
	gradient     seg.Gradient
	markers      []int32
	labels       []int32
	rawMask      []byte
	cleanMask    []byte
	regionTable  []RegionRecord
	floodSamples int
	reached      int
	mergeCount   int
}

// Exact runs the full-resolution path. It never reads preview data and is
// deterministic for a given icon and snapshot.
func Exact(icon core.Icon, snap core.Snapshot) (*ExactResult, error) {
	start := time.Now()
	if err := snap.ValidateFor(icon.Manifest.ImageID, icon.Manifest.Seed); err != nil {
		return nil, err
	}
	if err := icon.Manifest.ReferenceFingerprintCheck(); err != nil {
		return nil, err
	}
	run, err := runSegmentation(icon.Image, icon.Manifest.Seeds, snap.Params, snap.Params.CleanupRadius)
	if err != nil {
		return nil, err
	}
	components := seg.AnalyzeMask(run.cleanMask, icon.Manifest.Width, icon.Manifest.Height)
	match, err := seg.MatchTruth(run.cleanMask, components, icon.Manifest)
	if err != nil {
		return nil, err
	}
	statsHash := hashStats(run.cleanMask, components)
	return &ExactResult{
		Snapshot: snap, Mask: run.cleanMask, RawMask: run.rawMask, Labels: run.labels,
		InitialMarkers: run.markers, Gradient: run.gradient.V, RegionTable: run.regionTable,
		Components: components, Match: match, FloodSamples: run.floodSamples, Reached: run.reached,
		MergeCount: run.mergeCount, Elapsed: time.Since(start), MaskHash: core.MaskHash(run.cleanMask),
		StatsHash: statsHash, TruthFingerprint: icon.Manifest.ReferenceFingerprint,
	}, nil
}

func runSegmentation(img *image.NRGBA, seeds []core.Seed, params core.SegmentParams, cleanupRadius int) (*segmentationResult, error) {
	gradient := seg.ComputeGradient(img, params)
	markers, _ := seg.MakeMarkers(gradient.W, gradient.H, seeds)
	watershed := seg.Flood(gradient, seeds, params.FloodThreshold)
	watershed.FillUnlabeled(gradient)
	merged := seg.MergeRegions(gradient.W, gradient.H, watershed.Labels, watershed.RegionKeys, gradient, params)
	if len(merged.Isolated) > 0 {
		briefs := make([]core.RegionBrief, 0, len(merged.Isolated))
		for _, region := range merged.Isolated {
			briefs = append(briefs, core.RegionBrief{ID: int(region.ID), Area: region.Area, Reason: "isolated_below_min_region_size"})
		}
		return nil, core.RegionError("isolated watershed regions remain after merging", briefs)
	}
	rawMask := make([]byte, len(merged.Labels))
	for i, label := range merged.Labels {
		region, ok := merged.Regions[label]
		if label != 0 && ok && region.Key.Kind == core.SeedForeground {
			rawMask[i] = 1
		}
	}
	cleanMask := seg.Clean(rawMask, gradient.W, gradient.H, cleanupRadius)
	ids := make([]int, 0, len(merged.Regions))
	for id := range merged.Regions {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	table := make([]RegionRecord, 0, len(ids))
	for _, id := range ids {
		region := merged.Regions[int32(id)]
		table = append(table, RegionRecord{
			ID: int32(id), Kind: region.Key.Kind, Component: region.Key.Component, Area: region.Area,
		})
	}
	return &segmentationResult{
		gradient: gradient, markers: markers, labels: merged.Labels,
		rawMask: rawMask, cleanMask: cleanMask, regionTable: table, floodSamples: watershed.FloodSamples,
		reached: watershed.Reached, mergeCount: merged.MergeCount,
	}, nil
}

func hashStats(mask []byte, components []seg.Component) string {
	body, _ := json.Marshal(struct {
		Mask       string          `json:"mask"`
		Components []seg.Component `json:"components"`
	}{core.MaskHash(mask), components})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func downsampleImage(src *image.NRGBA, factor int) *image.NRGBA {
	b := src.Bounds()
	w := (b.Dx() + factor - 1) / factor
	h := (b.Dy() + factor - 1) / factor
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sr, sg, sb, sa, count int
			for yy := y * factor; yy < min((y+1)*factor, b.Dy()); yy++ {
				for xx := x * factor; xx < min((x+1)*factor, b.Dx()); xx++ {
					pixel := src.NRGBAAt(b.Min.X+xx, b.Min.Y+yy)
					sr += int(pixel.R)
					sg += int(pixel.G)
					sb += int(pixel.B)
					sa += int(pixel.A)
					count++
				}
			}
			if count == 0 {
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8(sr / count), G: uint8(sg / count),
				B: uint8(sb / count), A: uint8(sa / count),
			})
		}
	}
	return dst
}

func scaleSeeds(seeds []core.Seed, factor, w, h int) []core.Seed {
	out := make([]core.Seed, 0, len(seeds))
	for _, seed := range seeds {
		scaled := seed
		scaled.X = seed.X / factor
		scaled.Y = seed.Y / factor
		if scaled.X >= w {
			scaled.X = w - 1
		}
		if scaled.Y >= h {
			scaled.Y = h - 1
		}
		out = append(out, scaled)
	}
	return out
}

func upscaleMask(mask []byte, sw, sh, dw, dh, factor int) []byte {
	out := make([]byte, dw*dh)
	for y := 0; y < dh; y++ {
		sy := y / factor
		if sy >= sh {
			sy = sh - 1
		}
		for x := 0; x < dw; x++ {
			sx := x / factor
			if sx >= sw {
				sx = sw - 1
			}
			out[y*dw+x] = mask[sy*sw+sx]
		}
	}
	return out
}

func previewParams(base core.SegmentParams, factor int) core.SegmentParams {
	params := base
	scale := factor * factor
	params.MinRegionSize = max(1, base.MinRegionSize/scale)
	params.MergeAreaDiff = max(1, base.MergeAreaDiff/scale)
	params.CleanupRadius = max(0, base.CleanupRadius-1)
	return params
}

// Preview runs an adaptive low-resolution path. The candidate factor is
// increased until the measured wall time fits the manifest latency budget.
func Preview(icon core.Icon, snap core.Snapshot) (*PreviewResult, error) {
	budget := time.Duration(icon.Manifest.PreviewLatencyMS) * time.Millisecond
	if err := snap.ValidateFor(icon.Manifest.ImageID, icon.Manifest.Seed); err != nil {
		return nil, err
	}
	candidates := []int{2, 3, 4, 6, 8, 1}
	var last *PreviewResult
	for _, factor := range candidates {
		if min(icon.Manifest.Width, icon.Manifest.Height)/factor < 12 {
			continue
		}
		attemptStart := time.Now()
		small := downsampleImage(icon.Image, factor)
		params := previewParams(snap.Params, factor)
		seeds := scaleSeeds(icon.Manifest.Seeds, factor, small.Bounds().Dx(), small.Bounds().Dy())
		run, err := runSegmentation(small, seeds, params, params.CleanupRadius)
		if err != nil {
			last = nil
			continue
		}
		fullMask := upscaleMask(run.cleanMask, small.Bounds().Dx(), small.Bounds().Dy(),
			icon.Manifest.Width, icon.Manifest.Height, factor)
		components := seg.AnalyzeMask(fullMask, icon.Manifest.Width, icon.Manifest.Height)
		elapsed := time.Since(attemptStart)
		within := elapsed <= budget
		result := &PreviewResult{
			Snapshot: snap, Mask: fullMask, Components: components, Downsample: factor,
			FloodSamples: run.floodSamples, EquivalentSamples: run.floodSamples * factor * factor,
			Elapsed: elapsed, WithinLatency: within, Placeholder: true,
			Rationale: fmt.Sprintf("factor=%d selected by adaptive probe; measured=%s budget=%s; queue pops=%d",
				factor, elapsed.Round(time.Microsecond), budget, run.floodSamples),
			MaskHash: core.MaskHash(fullMask),
		}
		last = result
		if within {
			return result, nil
		}
	}
	if last == nil {
		return nil, core.NewError(core.CodeRegion, "preview could not reach a valid low-resolution segmentation")
	}
	last.Rationale += "; latency budget exceeded at maximum safe downsampling"
	return last, nil
}

// CheckConsistency consumes only the two existing results. It introduces no
// additional processing stage.
func CheckConsistency(preview *PreviewResult, exact *ExactResult, manifest core.TruthManifest) (ConsistencyReport, error) {
	report := ConsistencyReport{}
	if preview.Snapshot.ParamVersion != exact.Snapshot.ParamVersion ||
		preview.Snapshot.AlgorithmVersion != exact.Snapshot.AlgorithmVersion ||
		preview.Snapshot.Seed != exact.Snapshot.Seed ||
		preview.Snapshot.ImageID != exact.Snapshot.ImageID {
		return report, core.VersionError(exact.Snapshot.ParamVersion, exact.Snapshot.ParamVersion, preview.Snapshot.ParamVersion)
	}
	if len(preview.Mask) != len(exact.Mask) {
		return report, core.VersionError(exact.Snapshot.ParamVersion,
			strconv.Itoa(len(exact.Mask)), strconv.Itoa(len(preview.Mask)))
	}
	report.PreviewVersion = preview.Snapshot.ParamVersion
	report.ExactVersion = exact.Snapshot.ParamVersion
	report.SnapshotSeed = exact.Snapshot.Seed
	report.SameSeedSet = true
	report.SameAlgorithm = true
	report.PreviewDownsample = preview.Downsample
	measured := core.DivergenceReport{
		PreviewRegionCount: len(preview.Components), ExactRegionCount: len(exact.Components),
		CentroidTolerance: manifest.PreviewCentroidTolerance,
		AreaTolerance:     manifest.PreviewAreaRelTolerance,
		PixelTolerance:    manifest.PreviewDiffPixelTolerance,
	}
	measured.RegionCount = len(preview.Components)
	for i := range exact.Mask {
		if (exact.Mask[i] != 0) != (preview.Mask[i] != 0) {
			measured.DifferentPixels++
		}
	}
	measured.DiffPixelRatio = float64(measured.DifferentPixels) / float64(len(exact.Mask))
	if len(preview.Components) != len(exact.Components) {
		measured.FirstRegionID = min(len(preview.Components), len(exact.Components)) + 1
		report.Measured = measured
		err := core.NewError(core.CodeDiverge, "preview and exact region counts differ")
		err.Divergence = &measured
		return report, err
	}
	n := len(exact.Components)
	cost := make([][]float64, n)
	for i := 0; i < n; i++ {
		cost[i] = make([]float64, n)
		for j := 0; j < n; j++ {
			dx := preview.Components[j].CentroidX - exact.Components[i].CentroidX
			dy := preview.Components[j].CentroidY - exact.Components[i].CentroidY
			areaRel := math.Abs(float64(preview.Components[j].Area-exact.Components[i].Area)) /
				math.Max(1, float64(exact.Components[i].Area))
			cost[i][j] = math.Hypot(dx, dy) + areaRel
		}
	}
	assignment := seg.AssignMinimum(cost)
	firstBad := 0
	for i := 0; i < n; i++ {
		j := assignment[i]
		dx := preview.Components[j].CentroidX - exact.Components[i].CentroidX
		dy := preview.Components[j].CentroidY - exact.Components[i].CentroidY
		delta := math.Hypot(dx, dy)
		areaRel := math.Abs(float64(preview.Components[j].Area-exact.Components[i].Area)) /
			math.Max(1, float64(exact.Components[i].Area))
		if delta > measured.MaxCentroidDelta {
			measured.MaxCentroidDelta = delta
		}
		if areaRel > measured.MaxAreaRelError {
			measured.MaxAreaRelError = areaRel
		}
		if firstBad == 0 && (delta > manifest.PreviewCentroidTolerance || areaRel > manifest.PreviewAreaRelTolerance) {
			firstBad = exact.Components[i].ID
		}
	}
	report.Measured = measured
	consistent := measured.MaxCentroidDelta <= manifest.PreviewCentroidTolerance &&
		measured.MaxAreaRelError <= manifest.PreviewAreaRelTolerance &&
		measured.DiffPixelRatio <= manifest.PreviewDiffPixelTolerance
	report.Consistent = consistent
	if !consistent {
		if firstBad == 0 {
			firstBad = firstDifferingRegion(preview.Mask, exact.Mask, manifest.Width, exact.Components)
		}
		measured.FirstRegionID = firstBad
		report.Measured = measured
		err := core.NewError(core.CodeDiverge, fmt.Sprintf(
			"preview differs: regions=%d centroid=%.4f area_rel=%.4f diff_pixels=%d",
			measured.PreviewRegionCount, measured.MaxCentroidDelta, measured.MaxAreaRelError,
			measured.DifferentPixels))
		err.Divergence = &measured
		return report, err
	}
	return report, nil
}

func firstDifferingRegion(previewMask, exactMask []byte, width int, components []seg.Component) int {
	for _, component := range components {
		for y := component.BBox.Y; y < component.BBox.Y+component.BBox.H; y++ {
			for x := component.BBox.X; x < component.BBox.X+component.BBox.W; x++ {
				i := y*width + x
				if i < 0 || i >= len(previewMask) || i >= len(exactMask) {
					continue
				}
				if (previewMask[i] != 0) != (exactMask[i] != 0) {
					return component.ID
				}
			}
		}
	}
	if len(components) > 0 {
		return components[0].ID
	}
	return 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
