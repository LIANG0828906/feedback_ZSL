package fogkernel

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	CacheFresh       = "fresh"
	CacheRecomputed  = "recomputed"
	CacheReused      = "reused"
	CachePropagated  = "propagated"
	CacheGeometryNew = "geometry_recomputed"
)

type EditRequest struct {
	Kind               string    `json:"kind"`
	MediumID           string    `json:"medium_id,omitempty"`
	X                  int       `json:"x,omitempty"`
	Y                  int       `json:"y,omitempty"`
	Z                  int       `json:"z,omitempty"`
	Value              int       `json:"value,omitempty"`
	SigmaAPermille     *int64    `json:"sigma_a_permille,omitempty"`
	SigmaSPermille     *int64    `json:"sigma_s_permille,omitempty"`
	AnisotropyPermille *int64    `json:"anisotropy_permille,omitempty"`
	CameraIndex        int       `json:"camera_index,omitempty"`
	PositionMM         *[3]int64 `json:"position_mm,omitempty"`
	YawMilliDeg        *int64    `json:"yaw_milli_deg,omitempty"`
	PitchMilliDeg      *int64    `json:"pitch_milli_deg,omitempty"`
}

type EditDiff struct {
	Kind          string         `json:"kind"`
	Before        map[string]any `json:"before,omitempty"`
	After         map[string]any `json:"after,omitempty"`
	ChangedRays   []int          `json:"changed_rays"`
	ChangedBlocks []string       `json:"changed_blocks"`
}

type InvalidationReport struct {
	Source         string   `json:"source"`
	Kind           string   `json:"kind"`
	DirectKeys     []string `json:"direct_keys"`
	ClosureKeys    []string `json:"closure_keys"`
	RecomputedKeys []string `json:"recomputed_keys"`
	PropagatedKeys []string `json:"propagated_keys"`
	ReusedKeys     []string `json:"reused_keys"`
	AffectedRays   []int    `json:"affected_rays"`
	Reason         string   `json:"reason"`
}

type IncrementalResult struct {
	Report       *SolveReport        `json:"report"`
	Invalidation *InvalidationReport `json:"invalidation"`
	Diff         *EditDiff           `json:"diff"`
	FullCheck    *FullCheckReport    `json:"full_check,omitempty"`
}

type FullCheckReport struct {
	OK           bool     `json:"ok"`
	ComparedRays int      `json:"compared_rays"`
	Mismatches   []string `json:"mismatches,omitempty"`
}

type IncrementalSession struct {
	scene      *Scene
	plans      map[int]RayPlan
	cache      map[string]BlockPhysics
	voxelDeps  map[string]map[string]struct{}
	volumeDeps map[string]map[string]struct{}
	rayBlocks  map[int][]string
	lastReport *SolveReport
	editCount  int
}

func NewIncrementalSession(scene *Scene) *IncrementalSession {
	s := &IncrementalSession{
		scene: scene.Clone(), plans: map[int]RayPlan{}, cache: map[string]BlockPhysics{},
		voxelDeps: map[string]map[string]struct{}{}, volumeDeps: map[string]map[string]struct{}{},
		rayBlocks: map[int][]string{},
	}
	s.rebuildDependencies(nil)
	report := s.accumulateAll(nil)
	s.lastReport = report
	return s
}

func blockKey(rayIndex, blockIndex int) string {
	return fmt.Sprintf("ray:%d:block:%d", rayIndex, blockIndex)
}

func parseBlockKey(key string) (int, int, bool) {
	var ri, bi int
	_, err := fmt.Sscanf(key, "ray:%d:block:%d", &ri, &bi)
	return ri, bi, err == nil
}

func voxelDepKey(volumeID string, x, y, z int) string {
	return fmt.Sprintf("voxel:%s:%d:%d:%d", volumeID, x, y, z)
}

func (s *IncrementalSession) addDep(m map[string]map[string]struct{}, dep, key string) {
	set, ok := m[dep]
	if !ok {
		set = map[string]struct{}{}
		m[dep] = set
	}
	set[key] = struct{}{}
}

func (s *IncrementalSession) rebuildDependencies(onlyRays map[int]bool) {
	for ri, ray := range s.scene.Rays {
		if onlyRays != nil && !onlyRays[ri] {
			continue
		}
		plan := PlanRay(s.scene, ray)
		s.plans[ri] = plan
		keys := make([]string, 0, len(plan.Blocks))
		for _, bp := range plan.Blocks {
			key := blockKey(ri, bp.BlockIndex)
			keys = append(keys, key)
			s.cache[key] = ComputeBlockPhysics(s.scene, ray, bp)
			for _, seg := range bp.Segments {
				s.addDep(s.voxelDeps, voxelDepKey(seg.Ref.VolumeID, seg.Ref.X, seg.Ref.Y, seg.Ref.Z), key)
				s.addDep(s.volumeDeps, "volume:"+seg.Ref.VolumeID, key)
			}
		}
		s.rayBlocks[ri] = keys
	}
}

func (s *IncrementalSession) removeRayDependencies(rayIndex int) {
	prefix := fmt.Sprintf("ray:%d:block:", rayIndex)
	for _, deps := range s.voxelDeps {
		for key := range deps {
			if strings.HasPrefix(key, prefix) {
				delete(deps, key)
			}
		}
	}
	for _, deps := range s.volumeDeps {
		for key := range deps {
			if strings.HasPrefix(key, prefix) {
				delete(deps, key)
			}
		}
	}
	delete(s.rayBlocks, rayIndex)
}

func (s *IncrementalSession) Scene() *Scene { return s.scene.Clone() }

func (s *IncrementalSession) Report() *SolveReport { return s.lastReport }

func (s *IncrementalSession) accumulateAll(status map[string]string) *SolveReport {
	report := &SolveReport{
		OK: true, Path: PathExact, SceneVersion: s.scene.Version(),
		Budget: DefaultBudget(s.scene), Order: "incremental",
		Rays: make([]RayResult, len(s.scene.Rays)),
	}
	for ri := range s.scene.Rays {
		report.Rays[ri] = s.accumulateRay(ri, status, nil, nil)
	}
	s.lastReport = report
	return report
}

func (s *IncrementalSession) accumulateRay(rayIndex int, statusByKey map[string]string, sources, closure []string) RayResult {
	start := time.Now()
	ray := s.scene.Rays[rayIndex]
	plan, ok := s.plans[rayIndex]
	if !ok {
		plan = PlanRay(s.scene, ray)
		s.plans[rayIndex] = plan
	}
	result := RayResult{
		RayIndex: rayIndex, RayID: ray.ID, CameraIndex: ray.CameraIndex,
		Path: PathExact, TerminalBlock: -1,
	}
	if !plan.Hit || len(plan.Blocks) == 0 {
		result.Hit = false
		result.Transmittance = 1
		result.StopReason = StopNoIntersection
		result.DurationNS = time.Since(start).Nanoseconds()
		return result
	}
	result.Hit = true
	result.PlannedBlocks = len(plan.Blocks)
	t := 1.0
	threshold := s.scene.Recipe.TerminationThreshold()
	limit := s.scene.Recipe.BlockSampleLimit
	for _, bp := range plan.Blocks {
		if len(result.Blocks) >= limit {
			result.StopReason = StopBlockLimit
			break
		}
		key := blockKey(rayIndex, bp.BlockIndex)
		ph, ok := s.cache[key]
		if !ok {
			ph = ComputeBlockPhysics(s.scene, ray, bp)
			s.cache[key] = ph
			if statusByKey == nil {
				statusByKey = map[string]string{}
			}
			statusByKey[key] = CacheFresh
		}
		cacheStatus := CacheReused
		if statusByKey != nil {
			if st, ok := statusByKey[key]; ok {
				cacheStatus = st
			}
		}
		tBlock := expNeg(ph.Tau)
		energy := blockEnergy(t, ph)
		tOut := t * tBlock
		br := BlockResult{
			BlockIndex: bp.BlockIndex, ChunkBin: bp.Bin, T0: bp.T0, T1: bp.T1,
			TauA: ph.TauA, TauS: ph.TauS, Tau: ph.Tau, Albedo: ph.Albedo, Phase: ph.Phase,
			TBlock: tBlock, TIn: t, TOut: tOut, Energy: energy, VolumeIDs: ph.VolumeIDs,
			CacheStatus: cacheStatus,
		}
		if cacheStatus != CacheReused && sources != nil {
			br.InvalidationSources = append([]string(nil), sources...)
			br.PropagationClosure = append([]string(nil), closure...)
		} else if cacheStatus == CachePropagated && sources != nil {
			br.InvalidationSources = append([]string(nil), sources...)
			br.PropagationClosure = append([]string(nil), closure...)
		}
		t = tOut
		result.Transmittance = t
		result.TerminalBlock = bp.BlockIndex
		result.Blocks = append(result.Blocks, br)
		if t < threshold {
			result.StopReason = StopThreshold
			result.Blocks[len(result.Blocks)-1].Terminal = true
			break
		}
	}
	if result.StopReason == "" {
		if len(plan.Blocks) > limit && len(result.Blocks) == limit {
			result.StopReason = StopBlockLimit
		} else {
			result.StopReason = StopCompleted
		}
	}
	result.DurationNS = time.Since(start).Nanoseconds()
	return result
}

func sortedSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func raysFromKeys(keys []string) []int {
	set := map[int]struct{}{}
	for _, key := range keys {
		if ri, _, ok := parseBlockKey(key); ok {
			set[ri] = struct{}{}
		}
	}
	out := make([]int, 0, len(set))
	for ri := range set {
		out = append(out, ri)
	}
	sort.Ints(out)
	return out
}

func minBlockIndex(keys []string, rayIndex int) int {
	min := -1
	prefix := fmt.Sprintf("ray:%d:block:", rayIndex)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		_, bi, ok := parseBlockKey(key)
		if ok && (min < 0 || bi < min) {
			min = bi
		}
	}
	return min
}

func (s *IncrementalSession) ApplyEdit(req EditRequest) (*IncrementalResult, error) {
	beforeReport := s.lastReport
	diff := &EditDiff{Kind: req.Kind, ChangedRays: []int{}, ChangedBlocks: []string{}}
	var invalidation *InvalidationReport

	switch req.Kind {
	case "voxel":
		m, _, err := s.scene.Medium(req.MediumID)
		if err != nil {
			return nil, err
		}
		if req.X < 0 || req.X >= int(m.Density.Size.X) || req.Y < 0 || req.Y >= int(m.Density.Size.Y) ||
			req.Z < 0 || req.Z >= int(m.Density.Size.Z) {
			return nil, NewBizError(CodeEditTarget, "$.edit.voxel", "voxel coordinate (%d,%d,%d) outside %v", req.X, req.Y, req.Z, m.Density.Size).
				WithContext("medium_id", req.MediumID, "x", req.X, "y", req.Y, "z", req.Z)
		}
		if req.Value < 0 || req.Value > m.Density.Max {
			return nil, NewBizError(CodeDensityRange, "$.edit.value", "voxel value %d outside [0,%d]", req.Value, m.Density.Max).
				WithContext("value", req.Value, "max", m.Density.Max)
		}
		dep := voxelDepKey(req.MediumID, req.X, req.Y, req.Z)
		direct := sortedSet(s.voxelDeps[dep])
		old := m.Density.Get(req.X, req.Y, req.Z)
		diff.Before = map[string]any{"value": old}
		diff.After = map[string]any{"value": req.Value}
		m.Density.Values[m.Density.Index(req.X, req.Y, req.Z)] = req.Value
		invalidation = s.recomputeKeys("voxel-edit:"+dep, "voxel", direct, fmt.Sprintf("density value changed %d -> %d", old, req.Value))

	case "coefficients":
		m, _, err := s.scene.Medium(req.MediumID)
		if err != nil {
			return nil, err
		}
		if req.SigmaAPermille == nil && req.SigmaSPermille == nil && req.AnisotropyPermille == nil {
			return nil, NewBizError(CodeEditTarget, "$.edit", "coefficient edit requires at least one field")
		}
		before := map[string]any{"sigma_a_q": m.SigmaAQ, "sigma_s_q": m.SigmaSQ, "g_q": m.Phase.GQ}
		if req.SigmaAPermille != nil {
			q, err := CoefficientToQ(*req.SigmaAPermille, ScalePermille, "$.edit.sigma_a_permille")
			if err != nil {
				return nil, err
			}
			m.SigmaAQ = q
		}
		if req.SigmaSPermille != nil {
			q, err := CoefficientToQ(*req.SigmaSPermille, ScalePermille, "$.edit.sigma_s_permille")
			if err != nil {
				return nil, err
			}
			m.SigmaSQ = q
		}
		if req.AnisotropyPermille != nil {
			q, err := signedScaleToQ(*req.AnisotropyPermille, ScalePermille, "$.edit.anisotropy_permille")
			if err != nil {
				return nil, err
			}
			m.Phase.GQ = q
		}
		direct := sortedSet(s.volumeDeps["volume:"+req.MediumID])
		diff.Before = before
		diff.After = map[string]any{"sigma_a_q": m.SigmaAQ, "sigma_s_q": m.SigmaSQ, "g_q": m.Phase.GQ}
		invalidation = s.recomputeKeys("coefficient-edit:volume:"+req.MediumID, "coefficients", direct, "absorption/scattering/phase coefficient changed")

	case "camera":
		if req.CameraIndex < 0 || req.CameraIndex >= len(s.scene.Cameras) {
			return nil, NewBizError(CodeEditTarget, "$.edit.camera_index", "camera index %d outside 0..%d", req.CameraIndex, len(s.scene.Cameras)-1)
		}
		cam := s.scene.Cameras[req.CameraIndex]
		before := map[string]any{"position_micro": cam.PositionMicro, "yaw_milli_deg": cam.YawMilliDeg, "pitch_milli_deg": cam.PitchMilliDeg}
		if req.PositionMM != nil {
			pos, err := vecMM(*req.PositionMM, "$.edit.position_mm")
			if err != nil {
				return nil, err
			}
			cam.PositionMicro = pos
		}
		if req.YawMilliDeg != nil {
			cam.YawMilliDeg = *req.YawMilliDeg
		}
		if req.PitchMilliDeg != nil {
			cam.PitchMilliDeg = *req.PitchMilliDeg
		}
		s.scene.Cameras[req.CameraIndex] = cam
		s.scene.Rays = BuildRays(s.scene)
		affectedRays := map[int]bool{}
		for ri, ray := range s.scene.Rays {
			if ray.CameraIndex == req.CameraIndex {
				affectedRays[ri] = true
			}
		}
		var direct []string
		for ri := range affectedRays {
			for _, key := range s.rayBlocks[ri] {
				delete(s.cache, key)
			}
			s.removeRayDependencies(ri)
			delete(s.plans, ri)
			direct = append(direct, fmt.Sprintf("ray:%d:*", ri))
		}
		s.rebuildDependencies(affectedRays)
		for ri := range affectedRays {
			plan := s.plans[ri]
			for _, bp := range plan.Blocks {
				key := blockKey(ri, bp.BlockIndex)
				s.cache[key] = ComputeBlockPhysics(s.scene, s.scene.Rays[ri], bp)
			}
		}
		diff.Before = before
		diff.After = map[string]any{"position_micro": cam.PositionMicro, "yaw_milli_deg": cam.YawMilliDeg, "pitch_milli_deg": cam.PitchMilliDeg}
		keys := []string{}
		for ri := range affectedRays {
			keys = append(keys, s.rayBlocks[ri]...)
		}
		sort.Strings(keys)
		invalidation = &InvalidationReport{
			Source: "camera-edit:camera:" + intToString(int64(req.CameraIndex)), Kind: "camera",
			DirectKeys: direct, ClosureKeys: keys, RecomputedKeys: keys,
			AffectedRays: sortedIntKeys(affectedRays), Reason: "camera ray geometry changed; all ray intervals replanned",
		}

	default:
		return nil, NewBizError(CodeEditTarget, "$.edit.kind", "unknown edit kind %q", req.Kind)
	}

	newReport := s.rebuildReportWithInvalidation(invalidation)
	diff.ChangedRays, diff.ChangedBlocks = diffReports(beforeReport, newReport)
	result := &IncrementalResult{Report: newReport, Invalidation: invalidation, Diff: diff}
	check, err := s.FullCheck(newReport)
	if err != nil {
		return nil, err
	}
	result.FullCheck = check
	s.editCount++
	return result, nil
}

func (s *IncrementalSession) recomputeKeys(source, kind string, direct []string, reason string) *InvalidationReport {
	status := map[string]string{}
	for _, key := range direct {
		ri, bi, ok := parseBlockKey(key)
		if !ok {
			continue
		}
		plan := s.plans[ri]
		if bi < 0 || bi >= len(plan.Blocks) {
			continue
		}
		s.cache[key] = ComputeBlockPhysics(s.scene, s.scene.Rays[ri], plan.Blocks[bi])
		status[key] = CacheRecomputed
	}
	affected := raysFromKeys(direct)
	closure := append([]string(nil), direct...)
	propagated := []string{}
	reused := []string{}
	for _, ri := range affected {
		from := minBlockIndex(direct, ri)
		for _, key := range s.rayBlocks[ri] {
			_, bi, ok := parseBlockKey(key)
			if !ok {
				continue
			}
			if bi < from {
				reused = append(reused, key)
			} else if status[key] != CacheRecomputed {
				status[key] = CachePropagated
				propagated = append(propagated, key)
				closure = append(closure, key)
			}
		}
	}
	sort.Strings(closure)
	closure = uniqueStrings(closure)
	sort.Strings(propagated)
	sort.Strings(reused)
	return &InvalidationReport{
		Source: source, Kind: kind, DirectKeys: direct, ClosureKeys: closure,
		RecomputedKeys: direct, PropagatedKeys: propagated, ReusedKeys: reused,
		AffectedRays: affected, Reason: reason + "; downstream blocks replay cached intrinsic optics under changed incoming transmittance",
	}
}

func (s *IncrementalSession) rebuildReportWithInvalidation(inv *InvalidationReport) *SolveReport {
	status := map[string]string{}
	for _, k := range inv.RecomputedKeys {
		status[k] = CacheRecomputed
	}
	for _, k := range inv.PropagatedKeys {
		status[k] = CachePropagated
	}
	affected := map[int]bool{}
	for _, ri := range inv.AffectedRays {
		affected[ri] = true
	}
	report := &SolveReport{
		OK: true, Path: PathExact, SceneVersion: s.scene.Version(),
		Budget: DefaultBudget(s.scene), Order: "incremental",
		Rays: make([]RayResult, len(s.scene.Rays)),
	}
	for ri := range s.scene.Rays {
		if affected[ri] || inv.Kind == "camera" {
			report.Rays[ri] = s.accumulateRay(ri, status, []string{inv.Source}, inv.ClosureKeys)
		} else if s.lastReport != nil && ri < len(s.lastReport.Rays) {
			report.Rays[ri] = s.lastReport.Rays[ri]
		} else {
			report.Rays[ri] = s.accumulateRay(ri, nil, nil, nil)
		}
	}
	s.lastReport = report
	return report
}

func (s *IncrementalSession) FullCheck(incremental *SolveReport) (*FullCheckReport, error) {
	full, err := SolveScene(s.scene, SolveOptions{Path: PathExact, Workers: 1, Buffer: 64})
	if err != nil {
		return nil, err
	}
	check := &FullCheckReport{OK: true, ComparedRays: len(full.Rays)}
	mismatches := []string{}
	if len(full.Rays) != len(incremental.Rays) {
		check.OK = false
		check.Mismatches = append(check.Mismatches, fmt.Sprintf("ray count %d != %d", len(incremental.Rays), len(full.Rays)))
		return check, nil
	}
	for ri := range full.Rays {
		a := full.Rays[ri]
		b := incremental.Rays[ri]
		if !near(a.Transmittance, b.Transmittance, 1e-12) || a.TerminalBlock != b.TerminalBlock || len(a.Blocks) != len(b.Blocks) {
			check.OK = false
			mismatches = append(mismatches, fmt.Sprintf("ray %d conclusion full T=%.12f term=%d blocks=%d vs inc T=%.12f term=%d blocks=%d",
				ri, a.Transmittance, a.TerminalBlock, len(a.Blocks), b.Transmittance, b.TerminalBlock, len(b.Blocks)))
			continue
		}
		for bi := range a.Blocks {
			x, y := a.Blocks[bi], b.Blocks[bi]
			if !near(x.Tau, y.Tau, 1e-12) || !near(x.TOut, y.TOut, 1e-12) || !near(x.Energy, y.Energy, 1e-12) {
				check.OK = false
				mismatches = append(mismatches, fmt.Sprintf("ray %d block %d mismatch full tau=%.12f t=%.12f e=%.12f vs inc tau=%.12f t=%.12f e=%.12f",
					ri, bi, x.Tau, x.TOut, x.Energy, y.Tau, y.TOut, y.Energy))
			}
		}
	}
	check.Mismatches = mismatches
	return check, nil
}

func diffReports(before, after *SolveReport) ([]int, []string) {
	changedRays := []int{}
	changedBlocks := []string{}
	if before == nil {
		return changedRays, changedBlocks
	}
	for ri := range after.Rays {
		if ri >= len(before.Rays) {
			continue
		}
		a, b := before.Rays[ri], after.Rays[ri]
		rayChanged := mathAbs(a.Transmittance-b.Transmittance) > 1e-12 || a.TerminalBlock != b.TerminalBlock || len(a.Blocks) != len(b.Blocks)
		maxBlocks := len(a.Blocks)
		if len(b.Blocks) > maxBlocks {
			maxBlocks = len(b.Blocks)
		}
		for bi := 0; bi < maxBlocks; bi++ {
			if bi >= len(a.Blocks) || bi >= len(b.Blocks) {
				rayChanged = true
				changedBlocks = append(changedBlocks, blockKey(ri, bi))
				continue
			}
			x, y := a.Blocks[bi], b.Blocks[bi]
			if mathAbs(x.Tau-y.Tau) > 1e-12 || mathAbs(x.TOut-y.TOut) > 1e-12 || mathAbs(x.Energy-y.Energy) > 1e-12 {
				changedBlocks = append(changedBlocks, blockKey(ri, bi))
				rayChanged = true
			}
		}
		if rayChanged {
			changedRays = append(changedRays, ri)
		}
	}
	sort.Ints(changedRays)
	sort.Strings(changedBlocks)
	return changedRays, uniqueStrings(changedBlocks)
}

func mathAbs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sortedIntKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
