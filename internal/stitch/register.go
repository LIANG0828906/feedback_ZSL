package stitch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/features"
	"stitchworkbench/internal/homography"
	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
)

type RegisterOptions struct {
	Set        *imaging.Set
	Params     model.Params
	Version    int64
	Coarse     bool
	Downsample int
	Committed  map[int]model.PairStats
	Context    context.Context
}

type imageWork struct {
	index      int
	image      *imaging.LoadedImage
	gray       []float32
	gw, gh     int
	pixelScale int
	features   []features.Feature
	transform  model.Matrix3
	exposureG  [3]float64
	exposureO  [3]float64
	accepted   bool
	path       []int
}

type edgeWork struct {
	a, b       int
	matches    []features.MatchPair
	points     []model.PointPair
	inliers    []bool
	HAB        model.Matrix3
	accepted   bool
	inlierN    int
	residual   float64
	condition  float64
	errCode    string
	errMessage string
	gainAB     [3]float64
	offsetAB   [3]float64
}

type Registration struct {
	SetID         string
	Version       int64
	ContentHash   string
	Coarse        bool
	Downsample    int
	FeatureBudget int
	Pairs         []model.PairStats
	Canvas        model.Canvas
	CanvasOffset  model.Point
	images        []*imageWork
	edges         map[string]*edgeWork
	committed     map[int]model.PairStats
}

type extractResult struct {
	index int
	gray  []float32
	w, h  int
	scale int
	feats []features.Feature
	err   error
}

type matchResult struct {
	idx     int
	matches []features.MatchPair
}

func Register(opt RegisterOptions) (*Registration, error) {
	if opt.Set == nil || len(opt.Set.Images) == 0 {
		return nil, errs.New(errs.EMeta, "image set is empty")
	}
	p := opt.Params
	ctx := opt.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if p.MaxWorkers < 1 || p.MaxWorkers > 32 {
		return nil, errs.Field(errs.EPara, "max workers out of range [1,32]", "max_workers")
	}
	if p.MatchMin < 4 || p.MatchMin > 100000 {
		return nil, errs.Field(errs.EPara, "match minimum out of range [4,100000]", "match_min")
	}
	if p.MatchRatio <= 0 || p.MatchRatio >= 1 {
		return nil, errs.Field(errs.EPara, "match ratio must be in (0,1)", "match_ratio")
	}
	if p.MaxPixelsPerWorker < 1<<20 {
		return nil, errs.Field(errs.EPara, "per-worker pixel buffer must be at least 1048576", "max_pixels_per_worker")
	}
	factor := opt.Downsample
	if factor < 1 {
		factor = 1
	}
	if factor > 8 {
		return nil, errs.Field(errs.EPara, "preview downsample out of range [1,8]", "preview_downsample")
	}
	budget := p.FeatureBudget
	scaleLevels := p.ScaleLevels
	coarse := opt.Coarse
	if coarse {
		budget = p.PreviewFeatureBudget
		if budget > p.FeatureBudget {
			budget = p.FeatureBudget
		}
		if scaleLevels > 3 {
			scaleLevels = 3
		}
		if factor <= 1 {
			factor = 2
		}
	}
	workers := p.MaxWorkers
	if workers > runtime.GOMAXPROCS(0) {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers < 1 {
		workers = 1
	}
	images := make([]*imageWork, len(opt.Set.Images))
	for i, im := range opt.Set.Images {
		images[i] = &imageWork{
			index: i, image: im, transform: model.Identity(),
			exposureG: [3]float64{1, 1, 1},
		}
	}
	jobs := make(chan int)
	results := make(chan extractResult, len(images))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				im := images[idx]
				f := factor
				maxPixelFactor := int(math.Ceil(math.Sqrt(float64(im.image.Width*im.image.Height) / float64(p.MaxPixelsPerWorker))))
				if maxPixelFactor > f {
					f = maxPixelFactor
				}
				gray, gw, gh := prepareGray(im.image, f)
				fs, err := features.Extract(gray, gw, gh, features.Options{MaxFeatures: budget, ScaleLevels: scaleLevels, ResponseThreshold: p.ResponseThreshold})
				for k := range fs {
					fs[k].Point.X *= float64(f)
					fs[k].Point.Y *= float64(f)
				}
				results <- extractResult{index: idx, gray: gray, w: gw, h: gh, scale: f, feats: fs, err: err}
			}
		}()
	}
	go func() {
		for i := range images {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	for r := range results {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.err != nil {
			return nil, r.err
		}
		images[r.index].gray, images[r.index].gw, images[r.index].gh = r.gray, r.w, r.h
		images[r.index].pixelScale, images[r.index].features = r.scale, r.feats
	}
	candidates := edgeCandidates(len(images))
	edgeList := make([]*edgeWork, 0, len(candidates))
	edges := make(map[string]*edgeWork, len(candidates))
	for _, e := range candidates {
		ew := &edgeWork{a: e[0], b: e[1], gainAB: [3]float64{1, 1, 1}}
		edgeList = append(edgeList, ew)
		edges[edgeKey(e[0], e[1])] = ew
	}
	matchJobs := make(chan int)
	matched := make(chan matchResult, len(edgeList))
	var mwg sync.WaitGroup
	for w := 0; w < workers; w++ {
		mwg.Add(1)
		go func() {
			defer mwg.Done()
			for idx := range matchJobs {
				ew := edgeList[idx]
				ms := features.Match(images[ew.a].features, images[ew.b].features, p.MatchRatio)
				matched <- matchResult{idx: idx, matches: ms}
			}
		}()
	}
	go func() {
		for i := range edgeList {
			matchJobs <- i
		}
		close(matchJobs)
		mwg.Wait()
		close(matched)
	}()
	for mr := range matched {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		edgeList[mr.idx].matches = mr.matches
		edgeList[mr.idx].points = features.ToPoints(mr.matches)
	}
	for _, ew := range edgeList {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(ew.points) < p.MatchMin {
			ew.errCode = errs.EMatch
			ew.errMessage = fmt.Sprintf("matches %d below threshold %d", len(ew.points), p.MatchMin)
			continue
		}
		res, err := homography.Estimate(ew.points, p.RANSACThreshold, p.RANSACIterations, p.HomographyMaxCond)
		if err != nil {
			e := errs.As(err)
			ew.errCode, ew.errMessage = e.Code, e.Message
			continue
		}
		ew.HAB = res.H
		ew.inliers = res.Inliers
		ew.inlierN = res.InlierCount
		ew.residual = res.ResidualMedian
		ew.condition = res.Condition
		minInliers := p.MatchMin / 2
		if minInliers < 12 {
			minInliers = 12
		}
		if res.InlierCount < minInliers || float64(res.InlierCount)/float64(len(ew.matches)+1) < 0.25 {
			ew.errCode = errs.EHomo
			ew.errMessage = fmt.Sprintf("inliers %d below reliable homography minimum %d", res.InlierCount, minInliers)
			continue
		}
		ew.accepted = true
		refinePhotometric(images[ew.a], images[ew.b], ew, p.RANSACThreshold)
		ew.gainAB, ew.offsetAB = exposureRegression(images[ew.a].image, images[ew.b].image, ew)
	}
	if opt.Committed != nil {
		for idx, st := range opt.Committed {
			if idx <= 0 || idx >= len(images) {
				continue
			}
			h, ok := model.MatrixFromSlice(st.Homography)
			if !ok {
				continue
			}
			images[idx].transform = h
			images[idx].accepted = true
			images[idx].path = []int{0, idx}
			if len(st.ExposureGain) == 3 {
				copy(images[idx].exposureG[:], st.ExposureGain)
			}
			if len(st.ExposureOffset) == 3 {
				copy(images[idx].exposureO[:], st.ExposureOffset)
			}
		}
	}
	images[0].accepted = true
	images[0].path = []int{0}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	assignTransforms(images, edges)
	relaxSimilarities(ctx, images, edges, p, opt.Committed)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reg := &Registration{
		SetID: opt.Set.Manifest.SetID, Version: opt.Version,
		ContentHash: opt.Set.ContentHash, Coarse: coarse, Downsample: factor,
		FeatureBudget: budget, images: images, edges: edges, committed: opt.Committed,
	}
	accepted := 0
	for i, im := range images {
		if im.accepted {
			accepted++
		}
		if i == 0 {
			pst := basePairStats(im, p, opt.Version)
			pst.OverlapCoverage = overlapCoverage(im, images)
			reg.Pairs = append(reg.Pairs, pst)
			continue
		}
		if im.accepted {
			pst := pairStatsFromPath(im, images, edges, opt.Set, p, opt.Version, opt.Committed, coarse)
			pst.OverlapCoverage = overlapCoverage(im, images)
			reg.Pairs = append(reg.Pairs, pst)
		} else {
			pst := failedPairStats(im, images, edges, opt.Set, p, opt.Version, coarse)
			pst.OverlapCoverage = 0
			reg.Pairs = append(reg.Pairs, pst)
		}
	}
	reg.Canvas, reg.CanvasOffset = canvasBounds(images)
	if reg.Canvas.Width > 0 && reg.Canvas.Height > 0 {
		tolW := maxInt(4, int(math.Round(float64(opt.Set.Manifest.Canvas.Width)*0.005)))
		tolH := maxInt(4, int(math.Round(float64(opt.Set.Manifest.Canvas.Height)*0.005)))
		if absInt(reg.Canvas.Width-opt.Set.Manifest.Canvas.Width) <= tolW &&
			absInt(reg.Canvas.Height-opt.Set.Manifest.Canvas.Height) <= tolH {
			reg.Canvas = opt.Set.Manifest.Canvas
		}
	}
	if accepted <= 1 && len(images) > 1 {
		for _, ew := range edgeList {
			if ew.errCode == errs.EMatch {
				return nil, &errs.Error{Code: errs.EMatch, Message: "match count below configured minimum", Actual: len(ew.points), Threshold: p.MatchMin}
			}
		}
		for _, ew := range edgeList {
			if ew.errCode == errs.EHomo {
				return nil, &errs.Error{Code: errs.EHomo, Message: "no valid homography could be estimated"}
			}
		}
		return nil, &errs.Error{Code: errs.EMatch, Message: "no image could be registered to the base image", Actual: 0, Threshold: p.MatchMin}
	}
	return reg, nil
}

// relaxSimilarities is a pose-graph relaxation. Each image is refit in base
// coordinates from all accepted incident edges, so skip edges and cycles
// distribute small pairwise errors instead of accumulating along one chain.
func relaxSimilarities(ctx context.Context, images []*imageWork, edges map[string]*edgeWork, p model.Params, committed map[int]model.PairStats) {
	for iter := 0; iter < 60; iter++ {
		if ctx.Err() != nil {
			return
		}
		maxChange := 0.0
		for i := 1; i < len(images); i++ {
			if !images[i].accepted {
				continue
			}
			if committed != nil {
				if _, frozen := committed[i]; frozen {
					continue
				}
			}
			var src, dst []model.Point
			for j := 0; j < len(images); j++ {
				if i == j || !images[j].accepted {
					continue
				}
				ew := edges[edgeKey(i, j)]
				if ew == nil || !ew.accepted {
					continue
				}
				for k, m := range ew.matches {
					if k >= len(ew.inliers) || !ew.inliers[k] {
						continue
					}
					var s, t model.Point
					if i == ew.a {
						s = model.Point{X: m.Source.X, Y: m.Source.Y}
						t = model.Point{X: m.Target.X, Y: m.Target.Y}
					} else {
						s = model.Point{X: m.Target.X, Y: m.Target.Y}
						t = model.Point{X: m.Source.X, Y: m.Source.Y}
					}
					if j != 0 {
						t.X, t.Y = images[j].transform.Apply(t.X, t.Y)
					}
					if math.IsNaN(t.X) || math.IsNaN(t.Y) {
						continue
					}
					src = append(src, s)
					dst = append(dst, t)
				}
			}
			if len(src) < 20 {
				continue
			}
			limit := p.RANSACThreshold * 2.5
			if limit < 3 {
				limit = 3
			}
			var h model.Matrix3
			ok := false
			for trim := 0; trim < 3; trim++ {
				var fs, fd []model.Point
				for k := range src {
					x, y := images[i].transform.Apply(src[k].X, src[k].Y)
					if trim == 0 || math.Hypot(x-dst[k].X, y-dst[k].Y) <= limit {
						fs = append(fs, src[k])
						fd = append(fd, dst[k])
					}
				}
				if len(fs) < 12 {
					break
				}
				nh, valid := fitSimilarity(fs, fd)
				if !valid {
					break
				}
				h, ok = nh, true
			}
			if !ok {
				continue
			}
			old := images[i].transform
			delta := 0.0
			for _, q := range []model.Point{{X: 0, Y: 0}, {X: float64(images[i].image.Width), Y: float64(images[i].image.Height)}} {
				x0, y0 := old.Apply(q.X, q.Y)
				x1, y1 := h.Apply(q.X, q.Y)
				delta += math.Hypot(x1-x0, y1-y0)
			}
			images[i].transform = h
			if delta > maxChange {
				maxChange = delta
			}
		}
		if maxChange < 0.002 {
			break
		}
	}
}

func fitSimilarity(src, dst []model.Point) (model.Matrix3, bool) {
	if len(src) < 4 || len(src) != len(dst) {
		return model.Matrix3{}, false
	}
	var sx, sy, dx, dy float64
	for i := range src {
		sx += src[i].X
		sy += src[i].Y
		dx += dst[i].X
		dy += dst[i].Y
	}
	n := float64(len(src))
	sx, sy, dx, dy = sx/n, sy/n, dx/n, dy/n
	var numA, numB, den float64
	for i := range src {
		px, py := src[i].X-sx, src[i].Y-sy
		qx, qy := dst[i].X-dx, dst[i].Y-dy
		numA += px*qx + py*qy
		numB += px*qy - py*qx
		den += px*px + py*py
	}
	if den < 1e-9 {
		return model.Matrix3{}, false
	}
	a, b := numA/den, numB/den
	tx := dx - (a*sx - b*sy)
	ty := dy - (b*sx + a*sy)
	h := model.Matrix3{a, -b, tx, b, a, ty, 0, 0, 1}
	return h.Normalized(), h.Finite()
}

func assignTransforms(images []*imageWork, edges map[string]*edgeWork) {
	visited := make([]bool, len(images))
	visited[0] = true
	for i := 1; i < len(images); i++ {
		if images[i].accepted {
			visited[i] = true
			continue
		}
		if !visited[i-1] {
			continue
		}
		if ew := edges[edgeKey(i-1, i)]; ew != nil && ew.accepted {
			if applyEdgeTransform(images, i-1, i, ew) {
				visited[i] = true
			}
		}
	}
	for i := 1; i < len(images); i++ {
		if visited[i] {
			continue
		}
		if ew := edges[edgeKey(0, i)]; ew != nil && ew.accepted {
			if applyEdgeTransform(images, 0, i, ew) {
				visited[i] = true
			}
		}
	}
	queue := make([]int, 0, len(images))
	for i, ok := range visited {
		if ok {
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for child := 0; child < len(images); child++ {
			if visited[child] {
				continue
			}
			ew := edges[edgeKey(parent, child)]
			if ew == nil || !ew.accepted {
				continue
			}
			if applyEdgeTransform(images, parent, child, ew) {
				visited[child] = true
				queue = append(queue, child)
			}
		}
	}
}

func applyEdgeTransform(images []*imageWork, parent, child int, ew *edgeWork) bool {
	var hChildToParent model.Matrix3
	var g, o [3]float64
	ok := true
	if ew.a == parent {
		hChildToParent, ok = ew.HAB.Inverse()
		for c := 0; c < 3; c++ {
			if math.Abs(ew.gainAB[c]) < 1e-6 {
				ok = false
				break
			}
			g[c] = 1 / ew.gainAB[c]
			o[c] = -ew.offsetAB[c] / ew.gainAB[c]
		}
	} else {
		hChildToParent = ew.HAB
		g, o = ew.gainAB, ew.offsetAB
	}
	if !ok {
		return false
	}
	images[child].transform = images[parent].transform.Mul(hChildToParent).Normalized()
	for c := 0; c < 3; c++ {
		images[child].exposureG[c] = images[parent].exposureG[c] * g[c]
		images[child].exposureO[c] = images[parent].exposureG[c]*o[c] + images[parent].exposureO[c]
	}
	images[child].path = append(append([]int(nil), images[parent].path...), child)
	images[child].accepted = true
	return true
}

func pairStatsFromPath(im *imageWork, images []*imageWork, edges map[string]*edgeWork, set *imaging.Set, p model.Params, version int64, committed map[int]model.PairStats, coarse bool) model.PairStats {
	if st, ok := committed[im.index]; ok {
		st.Committed = true
		if st.CommittedVersion == 0 {
			st.CommittedVersion = st.ParamVersion
		}
		st.AlgorithmVersion = model.AlgorithmVersion
		st.PairIndex = im.index
		st.PairID = pairID(set, im.index)
		st.SourceImage = im.image.Truth.File
		st.TargetImage = set.Images[0].Truth.File
		st.Decision = true
		h, _ := model.MatrixFromSlice(st.Homography)
		st.HomographyErr = h.MaxAbsDiff(im.image.Homography)
		st.Path = []string{"committed"}
		return st
	}
	matches, inliers := math.MaxInt32, math.MaxInt32
	residual, condition := 0.0, 0.0
	pathNames := make([]string, 0, len(im.path))
	for _, idx := range im.path {
		pathNames = append(pathNames, images[idx].image.Truth.File)
	}
	for i := 0; i+1 < len(im.path); i++ {
		ew := edges[edgeKey(im.path[i], im.path[i+1])]
		if ew == nil {
			continue
		}
		if len(ew.matches) < matches {
			matches = len(ew.matches)
		}
		if ew.inlierN < inliers {
			inliers = ew.inlierN
		}
		if ew.residual > residual {
			residual = ew.residual
		}
		if ew.condition > condition {
			condition = ew.condition
		}
	}
	if matches == math.MaxInt32 {
		matches, inliers = 0, 0
	}
	st := model.PairStats{
		PairIndex: im.index, PairID: pairID(set, im.index),
		SourceImage: im.image.Truth.File, TargetImage: set.Images[0].Truth.File,
		Homography: im.transform.Slice(), TruthHomography: im.image.Homography.Slice(),
		HomographyErr: im.transform.MaxAbsDiff(im.image.Homography),
		Matches:       matches, Inliers: inliers, ResidualMedian: residual,
		ConditionNumber: condition, Decision: true, Path: pathNames,
		ExposureGain:   append([]float64(nil), im.exposureG[:]...),
		ExposureOffset: append([]float64(nil), im.exposureO[:]...),
		ContentHash:    pairContentHash(set, im.index), ParamVersion: version,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	if matches > 0 {
		st.InlierRate = float64(inliers) / float64(matches)
	}
	return st
}

func failedPairStats(im *imageWork, images []*imageWork, edges map[string]*edgeWork, set *imaging.Set, p model.Params, version int64, coarse bool) model.PairStats {
	st := model.PairStats{
		PairIndex: im.index, PairID: pairID(set, im.index),
		SourceImage: im.image.Truth.File, TargetImage: set.Images[0].Truth.File,
		TruthHomography: im.image.Homography.Slice(), Decision: false,
		ContentHash: pairContentHash(set, im.index), ParamVersion: version,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	bestLen := -1
	for _, other := range images {
		if other.index == im.index {
			continue
		}
		if ew := edges[edgeKey(im.index, other.index)]; ew != nil && len(ew.matches) > bestLen {
			bestLen, st.ErrorCode, st.ErrorMessage = len(ew.matches), ew.errCode, ew.errMessage
			st.Matches = len(ew.matches)
			st.Inliers = ew.inlierN
			if len(ew.matches) > 0 {
				st.InlierRate = float64(ew.inlierN) / float64(len(ew.matches))
			}
		}
	}
	if st.ErrorCode == "" {
		st.ErrorCode, st.ErrorMessage = errs.EMatch, "no connecting image pair"
		st.Matches, st.Inliers = 0, 0
	}
	return st
}

func basePairStats(im *imageWork, p model.Params, version int64) model.PairStats {
	return model.PairStats{
		PairIndex: 0, PairID: "base", SourceImage: im.image.Truth.File, TargetImage: im.image.Truth.File,
		Homography: model.Identity().Slice(), TruthHomography: model.Identity().Slice(),
		HomographyErr: 0, Matches: 0, Inliers: 0, InlierRate: 1, Decision: true,
		ExposureGain: []float64{1, 1, 1}, ExposureOffset: []float64{0, 0, 0},
		ContentHash: pairContentHashForImage(im.image.ContentHash), ParamVersion: version,
		AlgorithmVersion: model.AlgorithmVersion, Path: []string{im.image.Truth.File},
	}
}

func pairID(set *imaging.Set, idx int) string {
	return "base#" + set.Images[idx].Truth.File
}

func pairContentHash(set *imaging.Set, idx int) string {
	return pairContentHashForImage(set.Images[0].ContentHash + "|" + set.Images[idx].ContentHash)
}

func pairContentHashForImage(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func edgeCandidates(n int) [][2]int {
	seen := make(map[[2]int]bool)
	add := func(a, b int) {
		if a == b || a < 0 || b < 0 || a >= n || b >= n {
			return
		}
		if a > b {
			a, b = b, a
		}
		seen[[2]int{a, b}] = true
	}
	for i := 1; i < n; i++ {
		add(0, i)
		add(i-1, i)
		if i >= 2 {
			add(i-2, i)
		}
	}
	out := make([][2]int, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] == out[j][0] {
			return out[i][1] < out[j][1]
		}
		return out[i][0] < out[j][0]
	})
	return out
}

func edgeKey(a, b int) string {
	if a > b {
		a, b = b, a
	}
	return fmt.Sprintf("%d:%d", a, b)
}

func prepareGray(im *imaging.LoadedImage, factor int) ([]float32, int, int) {
	if factor <= 1 {
		return imaging.Grayscale(im), im.Width, im.Height
	}
	w := im.Width / factor
	h := im.Height / factor
	if w < 16 {
		w = 16
	}
	if h < 16 {
		h = 16
	}
	out := make([]float32, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var sr, sg, sb float64
			n := 0
			for dy := 0; dy < factor; dy++ {
				for dx := 0; dx < factor; dx++ {
					ix, iy := x*factor+dx, y*factor+dy
					if ix >= im.Width || iy >= im.Height {
						continue
					}
					i := (iy*im.Width + ix) * 4
					sr += float64(im.Pix[i])
					sg += float64(im.Pix[i+1])
					sb += float64(im.Pix[i+2])
					n++
				}
			}
			if n == 0 {
				continue
			}
			out[y*w+x] = float32((0.2126*sr + 0.7152*sg + 0.0722*sb) / float64(n))
		}
	}
	return out, w, h
}

func exposureRegression(a, b *imaging.LoadedImage, ew *edgeWork) ([3]float64, [3]float64) {
	gain := [3]float64{1, 1, 1}
	offset := [3]float64{}
	for c := 0; c < 3; c++ {
		n, sx, sy, sxx, sxy := 0.0, 0.0, 0.0, 0.0, 0.0
		for i, m := range ew.matches {
			if ew.inliers != nil && (i >= len(ew.inliers) || !ew.inliers[i]) {
				continue
			}
			ar, ag, ab, ok := imaging.SampleRGB(a, m.Source.X, m.Source.Y)
			if !ok {
				continue
			}
			br, bg, bb, ok := imaging.SampleRGB(b, m.Target.X, m.Target.Y)
			if !ok {
				continue
			}
			x := []float64{ar, ag, ab}[c]
			y := []float64{br, bg, bb}[c]
			n++
			sx += x
			sy += y
			sxx += x * x
			sxy += x * y
		}
		if n < 4 {
			continue
		}
		den := n*sxx - sx*sx
		if math.Abs(den) < 1e-9 {
			continue
		}
		g := (n*sxy - sx*sy) / den
		o := (sy - g*sx) / n
		if math.IsNaN(g) || math.IsInf(g, 0) || g < 0.55 || g > 1.8 {
			continue
		}
		if o < -40 {
			o = -40
		}
		if o > 40 {
			o = 40
		}
		gain[c], offset[c] = g, o
	}
	return gain, offset
}
