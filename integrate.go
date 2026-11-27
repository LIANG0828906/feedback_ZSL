package volumefog

import (
	"fmt"
	"math"
	"sort"
)

type RaySegmentResult struct {
	Index              int             `json:"index"`
	T0Q                int64           `json:"t0_q"`
	T1Q                int64           `json:"t1_q"`
	Active             []string        `json:"active"`
	ResidualQ          float64         `json:"residual_q"`
	Transmittance      float64         `json:"transmittance"`
	CumulativeRadiance float64         `json:"cumulative_radiance"`
	Dependencies       []DependencyRef `json:"dependencies,omitempty"`
}

type RayResult struct {
	RayID               string             `json:"ray_id"`
	Segments            []RaySegmentResult `json:"segments"`
	Dependencies        []DependencyRef    `json:"dependencies"`
	FinalTransmittance  float64            `json:"final_transmittance"`
	FinalRadiance       float64            `json:"final_radiance"`
	Color               [3]float64         `json:"color"`
	StepsBudget         int                `json:"steps_budget"`
	StepsUsed           int                `json:"steps_used"`
	ShadowSamplesBudget int                `json:"shadow_samples_budget"`
	ShadowSamplesUsed   int                `json:"shadow_samples_used"`
	RayCacheKey         string             `json:"ray_cache_key"`
	RayContentHash      string             `json:"ray_content_hash"`
}

type IntegrateOptions struct {
	Steps         int
	ShadowSamples int
	Budget        *Budget
	Chunk         ChunkCoord
	Worker        int
}

type primaryCell struct {
	T0Q    int64
	T1Q    int64
	SigmaT float64
	SigmaS float64
	Phase  float64
}

type integrationRefs struct {
	seen map[string]DependencyRef
	out  []DependencyRef
}

func newIntegrationRefs(capacity int) *integrationRefs {
	return &integrationRefs{seen: make(map[string]DependencyRef, capacity)}
}

func (r *integrationRefs) add(d DependencyRef) {
	k := dependencyKey(d)
	if _, ok := r.seen[k]; ok {
		return
	}
	r.seen[k] = d
	r.out = append(r.out, d)
}

func (r *integrationRefs) list() []DependencyRef {
	return dedupeDependencies(r.out)
}

func compositeDetailed(scene *Scene, active []string, p Vec3i, rayDir [3]float64,
	includeAniso bool, refs *integrationRefs) (sigmaT, sigmaS, phase float64) {
	var gWeighted, gSum float64
	for _, id := range active {
		m, _ := scene.MediumByID(id)
		if m == nil {
			continue
		}
		d, idx, ok := m.DensityAt(p)
		if !ok {
			continue
		}
		a := m.Absorption() * d
		s := m.Scattering() * d
		sigmaT += a + s
		sigmaS += s
		if d > 0 && refs != nil {
			refs.add(DependencyRef{Kind: "density", MediaID: id, Voxel: idx})
			refs.add(DependencyRef{Kind: "absorption", MediaID: id})
			refs.add(DependencyRef{Kind: "scattering", MediaID: id})
			if includeAniso {
				refs.add(DependencyRef{Kind: "anisotropy", MediaID: id})
			}
		}
		gWeighted += s * m.Anisotropy()
		gSum += s
	}
	g := 0.0
	if gSum > 0 {
		g = gWeighted / gSum
	}
	phase = phaseValue(scene.Recipe, g, rayDir)
	return sigmaT, sigmaS, phase
}

func baseCellsForSegment(scene *Scene, ray Ray, seg Segment) []analyticCell {
	if ray.DirQ[0] == 0 && ray.DirQ[1] == 0 && ray.DirQ[2] == FixedScale {
		return axisAlignedCells(scene.Media, seg.Active, ray, seg.T0Q, seg.T1Q)
	}
	lengthUnits := float64(seg.T1Q-seg.T0Q) / float64(FixedScale)
	n := int(math.Ceil(lengthUnits * 4))
	if n < 8 {
		n = 8
	}
	if n > 64 {
		n = 64
	}
	out := make([]analyticCell, 0, n)
	for i := 0; i < n; i++ {
		t0 := seg.T0Q + int64(i)*(seg.T1Q-seg.T0Q)/int64(n)
		t1 := seg.T0Q + int64(i+1)*(seg.T1Q-seg.T0Q)/int64(n)
		if t1 > t0 {
			out = append(out, analyticCell{T0Q: t0, T1Q: t1})
		}
	}
	return out
}

func allocateGroups(counts []int, budget int) ([]int, error) {
	if budget < len(counts) {
		return nil, fogErr(E_INT, "step budget is smaller than the number of atomic segments")
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return make([]int, len(counts)), nil
	}
	if total <= budget {
		return append([]int(nil), counts...), nil
	}
	out := make([]int, len(counts))
	for i := range out {
		out[i] = 1
	}
	left := budget - len(counts)
	for left > 0 {
		best := -1
		bestDeficit := 0
		for i, c := range counts {
			if out[i] >= c {
				continue
			}
			if d := c - out[i]; d > bestDeficit {
				best, bestDeficit = i, d
			}
		}
		if best < 0 {
			break
		}
		out[best]++
		left--
	}
	return out, nil
}

func buildShadowCells(scene *Scene, p Vec3i, farQ int64) []analyticCell {
	if farQ <= p[2] || p[2] < 0 {
		return nil
	}
	start, end := p[2], farQ
	set := map[int64]struct{}{start: {}, end: {}}
	x, y := p[0], p[1]
	for i := range scene.Media {
		m := &scene.Media[i]
		if x < m.MinQ[0] || x >= m.MaxQ[0] || y < m.MinQ[1] || y >= m.MaxQ[1] {
			continue
		}
		ext := m.ExtentQ()[2]
		res := int64(m.DensityRes[2])
		if ext <= 0 || res <= 0 {
			continue
		}
		voxel := ext / res
		first := ceilDiv(m.MinQ[2]-start, voxel)
		last := floorDiv(m.MaxQ[2]-start, voxel)
		for k := first; k <= last; k++ {
			z := m.MinQ[2] + k*voxel
			if z > start && z < end {
				set[z] = struct{}{}
			}
		}
	}
	bounds := make([]int64, 0, len(set))
	for z := range set {
		bounds = append(bounds, z)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i] < bounds[j] })
	out := make([]analyticCell, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		if bounds[i+1] > bounds[i] {
			out = append(out, analyticCell{T0Q: bounds[i], T1Q: bounds[i+1]})
		}
	}
	return out
}

func activeAt(scene *Scene, p Vec3i) []string {
	var active []string
	for i := range scene.Media {
		if scene.Media[i].Contains(p) {
			active = append(active, scene.Media[i].ID)
		}
	}
	sort.Strings(active)
	return active
}

func shadowTransmittance(scene *Scene, p Vec3i, farQ int64, wantSamples int,
	refs *integrationRefs, opts IntegrateOptions) (float64, int) {
	if wantSamples <= 0 {
		return 1, 0
	}
	if opts.Budget != nil && wantSamples > opts.Budget.Capacity() {
		wantSamples = opts.Budget.Capacity()
	}
	cells := buildShadowCells(scene, p, farQ)
	if len(cells) == 0 {
		return 1, 0
	}
	used := len(cells)
	if wantSamples < used {
		used = wantSamples
	}
	if used <= 0 {
		return 1, 0
	}
	release := func() {}
	if opts.Budget != nil {
		release = opts.Budget.AcquireShadow(wantSamples, opts.Chunk, opts.Worker)
	}
	defer release()
	tau := 0.0
	if used >= len(cells) {
		for _, c := range cells {
			midZ := (c.T0Q + c.T1Q) / 2
			mid := Vec3i{p[0], p[1], midZ}
			active := activeAt(scene, mid)
			st, _, _ := compositeDetailed(scene, active, mid, [3]float64{0, 0, 1}, false, refs)
			dt := float64(c.T1Q-c.T0Q) / float64(FixedScale)
			tau += st * dt
		}
	} else {
		for g := 0; g < used; g++ {
			lo := g * len(cells) / used
			hi := (g + 1) * len(cells) / used
			if hi <= lo {
				continue
			}
			midZ := (cells[lo].T0Q + cells[hi-1].T1Q) / 2
			mid := Vec3i{p[0], p[1], midZ}
			active := activeAt(scene, mid)
			st, _, _ := compositeDetailed(scene, active, mid, [3]float64{0, 0, 1}, false, refs)
			dt := float64(cells[hi-1].T1Q-cells[lo].T0Q) / float64(FixedScale)
			tau += st * dt
		}
	}
	return math.Exp(-tau), used
}

// IntegrateRay runs one primary ray. Transmittance is accumulated from exact
// per-cell optical depth, so tightening the step budget never increases T.
// The step budget controls radiance quadrature groups and shadow sampling.
func IntegrateRay(scene *Scene, ray Ray, opts IntegrateOptions) (RayResult, error) {
	if opts.Steps <= 0 {
		return RayResult{}, &FogError{Code: E_INT, Message: "non-positive step budget", RayID: ray.ID}
	}
	if opts.ShadowSamples < 0 {
		return RayResult{}, &FogError{Code: E_INT, Message: "negative shadow budget", RayID: ray.ID}
	}
	segments := RaySegments(scene.Media, ray)
	if opts.Steps < len(segments) {
		e := &FogError{Code: E_INT, Message: "step budget cannot cover atomic segments", RayID: ray.ID,
			Tolerance: floatPtr(float64(len(segments)))}
		return RayResult{}, e
	}
	refs := newIntegrationRefs(64)
	refs.add(DependencyRef{Kind: "camera", CameraIndex: ray.CameraIndex})
	refs.add(DependencyRef{Kind: "recipe"})
	segmentCells := make([][]primaryCell, len(segments))
	segmentRefs := make([]*integrationRefs, len(segments))
	counts := make([]int, len(segments))
	rayDir := ray.DirectionFloat()
	for si, seg := range segments {
		segRefs := newIntegrationRefs(32)
		segRefs.add(DependencyRef{Kind: "camera", CameraIndex: ray.CameraIndex})
		segRefs.add(DependencyRef{Kind: "recipe"})
		cells := baseCellsForSegment(scene, ray, seg)
		store := make([]primaryCell, 0, len(cells))
		for _, c := range cells {
			mid := ray.PointAtQ((c.T0Q + c.T1Q) / 2)
			sigmaT, sigmaS, phase := compositeDetailed(scene, seg.Active, mid, rayDir, true, segRefs)
			store = append(store, primaryCell{T0Q: c.T0Q, T1Q: c.T1Q,
				SigmaT: sigmaT, SigmaS: sigmaS, Phase: phase})
		}
		segmentCells[si] = store
		counts[si] = len(store)
		segmentRefs[si] = segRefs
	}
	groupCounts, err := allocateGroups(counts, opts.Steps)
	if err != nil {
		if fe, ok := err.(*FogError); ok {
			fe.RayID = ray.ID
		}
		return RayResult{}, err
	}
	out := RayResult{
		RayID: ray.ID, FinalTransmittance: 1, StepsBudget: opts.Steps,
		ShadowSamplesBudget: opts.ShadowSamples,
	}
	T, L := 1.0, 0.0
	stepsUsed, shadowUsed := 0, 0
	broken := false
	for si, seg := range segments {
		groups := groupCellsFromPrimary(segmentCells[si], groupCounts[si])
		for _, group := range groups {
			if len(group) == 0 {
				continue
			}
			t0, t1 := group[0].T0Q, group[len(group)-1].T1Q
			dt := float64(t1-t0) / float64(FixedScale)
			if dt <= 0 {
				continue
			}
			tau := 0.0
			for _, c := range group {
				tau += c.SigmaT * (float64(c.T1Q-c.T0Q) / float64(FixedScale))
			}
			midT := (t0 + t1) / 2
			mid := ray.PointAtQ(midT)
			sigmaS, phase := 0.0, 0.0
			if ray.DirQ[0] == 0 && ray.DirQ[1] == 0 && ray.DirQ[2] == FixedScale {
				sigmaS, phase = segmentSampleSigmaS(scene, seg.Active, mid, rayDir)
			} else {
				_, sigmaS, phase = compositeAt(scene, seg.Active, mid, rayDir)
			}
			shadow, samples := shadowTransmittance(scene, mid, ray.FarQ, opts.ShadowSamples,
				segmentRefs[si], opts)
			shadowUsed += samples
			source := sigmaS * phase * shadow
			effSigma := tau / dt
			if effSigma > 1e-15 {
				L += T * source * (1 - math.Exp(-tau)) / effSigma
			} else {
				L += T * source * dt
			}
			T *= math.Exp(-tau)
			stepsUsed++
			if T <= scene.Recipe.TransmittanceCutoff {
				T = 0
				broken = true
				break
			}
		}
		out.Segments = append(out.Segments, RaySegmentResult{
			Index: seg.Index, T0Q: seg.T0Q, T1Q: seg.T1Q,
			Active: append([]string(nil), seg.Active...), ResidualQ: seg.ResidualQ,
			Transmittance: T, CumulativeRadiance: L,
			Dependencies: segmentRefs[si].list(),
		})
		for _, d := range segmentRefs[si].list() {
			refs.add(d)
		}
		if broken {
			break
		}
	}
	out.FinalTransmittance = T
	out.FinalRadiance = L
	out.Color = [3]float64{
		L * scene.Recipe.AmbientColor[0],
		L * scene.Recipe.AmbientColor[1],
		L * scene.Recipe.AmbientColor[2],
	}
	out.StepsUsed = stepsUsed
	out.ShadowSamplesUsed = shadowUsed
	deps := refs.list()
	fp, key := RayCacheKey(scene, ray.ID, deps, "")
	out.Dependencies = deps
	out.RayContentHash = fp
	out.RayCacheKey = key
	return out, nil
}

func segmentSampleSigmaS(scene *Scene, active []string, p Vec3i, rayDir [3]float64) (sigmaS, phase float64) {
	var gWeighted, gSum float64
	for _, id := range active {
		m, _ := scene.MediumByID(id)
		if m == nil {
			continue
		}
		d, _, ok := m.DensityAt(p)
		if !ok {
			continue
		}
		s := m.Scattering() * d
		sigmaS += s
		gWeighted += s * m.Anisotropy()
		gSum += s
	}
	g := 0.0
	if gSum > 0 {
		g = gWeighted / gSum
	}
	return sigmaS, phaseValue(scene.Recipe, g, rayDir)
}

func groupCellsFromPrimary(cells []primaryCell, groups int) [][]primaryCell {
	if groups <= 0 || len(cells) == 0 {
		return nil
	}
	if groups > len(cells) {
		groups = len(cells)
	}
	out := make([][]primaryCell, 0, groups)
	for g := 0; g < groups; g++ {
		lo := g * len(cells) / groups
		hi := (g + 1) * len(cells) / groups
		if hi <= lo {
			continue
		}
		out = append(out, cells[lo:hi])
	}
	return out
}

func floatPtr(v float64) *float64 { return &v }

func rayResultDigest(rays []RayResult) string {
	parts := make([]string, 0, len(rays))
	for _, r := range rays {
		parts = append(parts, fmt.Sprintf("%s|%.15g|%.15g|%d|%d", r.RayID,
			r.FinalTransmittance, r.FinalRadiance, r.StepsUsed, r.ShadowSamplesUsed))
	}
	return hashStrings(parts...)
}
