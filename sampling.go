package silhouette

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"sync"
)

type SampleVertex struct {
	Index   int   `json:"index"`
	X       int64 `json:"x"`
	Y       int64 `json:"y"`
	TNum    int64 `json:"t_num"`
	TDen    int64 `json:"t_den"`
	Rounded bool  `json:"rounded,omitempty"`
}

type SegmentSample struct {
	SegmentID      string         `json:"segment_id"`
	Tolerance      int64          `json:"tolerance"`
	Vertices       []SampleVertex `json:"vertices"`
	MaxChordHeight int64          `json:"max_chord_height"`
	FlatnessBound  int64          `json:"flatness_bound"`
	DepthReached   int            `json:"depth_reached"`
	Degraded       bool           `json:"degraded"`
}

type SamplingOptions struct {
	Tolerance         int64
	Budget            int
	MaxDepth          int
	Workers           int
	VerticesPerWorker int
}

type Degradation struct {
	SegmentID      string `json:"segment_id"`
	PriorityRank   int    `json:"priority_rank"`
	Basis          string `json:"basis"`
	CurvatureScore string `json:"curvature_score"`
	VisualWeight   int64  `json:"visual_weight"`
	FromTolerance  int64  `json:"from_tolerance"`
	ToTolerance    int64  `json:"to_tolerance"`
	BeforeVertices int    `json:"before_vertices"`
	AfterVertices  int    `json:"after_vertices"`
	BeforeMaxChord int64  `json:"before_max_chord"`
	AfterMaxChord  int64  `json:"after_max_chord"`
	BudgetBefore   int    `json:"budget_before"`
	BudgetAfter    int    `json:"budget_after"`
}

type BudgetReport struct {
	RequestedBudget int           `json:"requested_budget"`
	InitialVertices int           `json:"initial_vertices"`
	FinalVertices   int           `json:"final_vertices"`
	Remaining       int           `json:"remaining"`
	Degraded        bool          `json:"degraded"`
	Degradations    []Degradation `json:"degradations"`
}

type SamplingResult struct {
	BaseTolerance  int64            `json:"base_tolerance"`
	ToleranceBySeg map[string]int64 `json:"tolerance_by_segment"`
	Segments       []SegmentSample  `json:"segments"`
	Budget         BudgetReport     `json:"budget"`
	WorkerEvents   []WorkerEvent    `json:"worker_events,omitempty"`
}

type segmentPriority struct {
	id       string
	score    *big.Int
	weight   int64
	priority *big.Int
}

func absoluteBig(v *big.Int) *big.Int {
	out := new(big.Int).Set(v)
	return out.Abs(out)
}

func segmentPriorityFor(seg CurveSegment) segmentPriority {
	d1x := big.NewInt(seg.Controls[1].X - seg.Controls[0].X)
	d1y := big.NewInt(seg.Controls[1].Y - seg.Controls[0].Y)
	d2x := big.NewInt(seg.Controls[2].X - seg.Controls[1].X)
	d2y := big.NewInt(seg.Controls[2].Y - seg.Controls[1].Y)
	d3x := big.NewInt(seg.Controls[3].X - seg.Controls[2].X)
	d3y := big.NewInt(seg.Controls[3].Y - seg.Controls[2].Y)
	c1 := new(big.Int).Sub(new(big.Int).Mul(d1x, d2y), new(big.Int).Mul(d1y, d2x))
	c2 := new(big.Int).Sub(new(big.Int).Mul(d2x, d3y), new(big.Int).Mul(d2y, d3x))
	score := new(big.Int).Add(absoluteBig(c1), absoluteBig(c2))
	priority := new(big.Int).Mul(score, big.NewInt(seg.Weight))
	return segmentPriority{id: seg.ID, score: score, weight: seg.Weight, priority: priority}
}

func sortedSegmentsByPriority(segments []CurveSegment) []segmentPriority {
	items := make([]segmentPriority, 0, len(segments))
	for _, seg := range segments {
		items = append(items, segmentPriorityFor(seg))
	}
	sort.SliceStable(items, func(i, j int) bool {
		cmp := items[i].priority.Cmp(items[j].priority)
		if cmp == 0 {
			return items[i].id < items[j].id
		}
		return cmp < 0
	})
	return items
}

// SampleSegment runs an exact-rational adaptive cubic subdivision. The final
// vertices are emitted in increasing parameter order and are a pure function of
// control points and tolerance.
func SampleSegment(seg CurveSegment, tolerance int64, maxDepth int) (*SegmentSample, error) {
	if tolerance < MinTolerance {
		return nil, toleranceError(seg.ID, tolerance)
	}
	if maxDepth <= 0 {
		maxDepth = MaxSubdivisionDepth
	}
	root := [4]rpoint{
		newRPoint(seg.Controls[0]), newRPoint(seg.Controls[1]),
		newRPoint(seg.Controls[2]), newRPoint(seg.Controls[3]),
	}
	result := &SegmentSample{SegmentID: seg.ID, Tolerance: tolerance}
	first, overflow := ratToFixedNearest(root[0].x)
	firstY, overflowY := ratToFixedNearest(root[0].y)
	if overflow || overflowY {
		return nil, overflowError("$.segments["+seg.ID+"].controls[0]", "vertex_round", 0, 0)
	}
	result.Vertices = append(result.Vertices, SampleVertex{Index: 0, X: first, Y: firstY, TNum: 0, TDen: 1})
	ctx := sampleContext{segID: seg.ID, tolerance: tolerance, maxDepth: maxDepth, result: result}
	zero := big.NewRat(0, 1)
	one := big.NewRat(1, 1)
	if err := ctx.recurse(root, zero, one, 0); err != nil {
		return nil, err
	}
	for i := range result.Vertices {
		result.Vertices[i].Index = i
	}
	return result, nil
}

type sampleContext struct {
	segID     string
	tolerance int64
	maxDepth  int
	result    *SegmentSample
}

func (c *sampleContext) recurse(cp [4]rpoint, t0, t1 *big.Rat, depth int) error {
	flatSq := curveFlatnessSq(cp)
	flatFloored, overflow := floorSqrtRat(flatSq)
	if overflow {
		flatFloored = math.MaxInt64
	}
	if compareDistanceSquaredToTolerance(flatSq, c.tolerance) <= 0 {
		if depth > c.result.DepthReached {
			c.result.DepthReached = depth
		}
		if flatFloored > c.result.FlatnessBound && !overflow {
			c.result.FlatnessBound = flatFloored
		}
		measured := curveMeasuredChordHeight(cp)
		if measured > c.result.MaxChordHeight {
			c.result.MaxChordHeight = measured
		}
		return c.appendEndpoint(cp[3], t1)
	}
	if depth >= c.maxDepth {
		measured, _ := floorSqrtRat(flatSq)
		return (&DomainError{Code: CodeTolerance, Path: "$.segments[" + c.segID + "]", Message: "tolerance precision floor cannot be met at maximum subdivision depth"}).
			WithDetail("segment_id", c.segID).
			WithDetail("measured", strconv.FormatInt(measured, 10)).
			WithDetail("tolerance", strconv.FormatInt(c.tolerance, 10)).
			WithDetail("depth", strconv.Itoa(depth))
	}
	left, right := splitCubic(cp)
	tm := new(big.Rat).Add(t0, t1)
	tm.Quo(tm, big.NewRat(2, 1))
	if err := c.recurse(left, t0, tm, depth+1); err != nil {
		return err
	}
	if err := c.recurse(right, tm, t1, depth+1); err != nil {
		return err
	}
	return nil
}

func (c *sampleContext) appendEndpoint(p rpoint, t *big.Rat) error {
	x, overflowX := ratToFixedNearest(p.x)
	y, overflowY := ratToFixedNearest(p.y)
	if overflowX || overflowY {
		return overflowError("$.segments["+c.segID+"].vertex", "vertex_round", 0, 0)
	}
	if !t.Num().IsInt64() || !t.Denom().IsInt64() {
		return overflowError("$.segments["+c.segID+"].parameter", "parameter_rational", 0, 0)
	}
	c.result.Vertices = append(c.result.Vertices, SampleVertex{
		X: x, Y: y, TNum: t.Num().Int64(), TDen: t.Denom().Int64(),
		Rounded: !p.x.IsInt() || !p.y.IsInt(),
	})
	return nil
}

func toleranceError(segmentID string, measured int64) *DomainError {
	return (&DomainError{Code: CodeTolerance, Path: "$.tolerance", Message: "tolerance is below the precision floor"}).
		WithDetail("segment_id", segmentID).
		WithDetail("measured", strconv.FormatInt(measured, 10)).
		WithDetail("minimum", strconv.FormatInt(MinTolerance, 10))
}

// SampleScene applies the base tolerance to every segment and then performs
// deterministic, recorded budget degradation. Segments with lower
// curvature*weight are degraded first.
func SampleScene(scene *Scene, opts SamplingOptions) (*SamplingResult, error) {
	return SampleSceneContext(context.Background(), scene, opts)
}

func SampleSceneContext(ctx context.Context, scene *Scene, opts SamplingOptions) (*SamplingResult, error) {
	if scene == nil {
		return nil, NewDomainError(CodeSchema, "$", "scene is nil")
	}
	tolerance := opts.Tolerance
	if tolerance == 0 {
		tolerance = scene.Tolerance.Value
	}
	budget := opts.Budget
	if budget == 0 {
		budget = scene.Budget.MaxVertices
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = MaxSubdivisionDepth
	}
	if tolerance < MinTolerance {
		return nil, toleranceError("scene", tolerance)
	}
	if budget <= 0 {
		return nil, NewDomainError(CodeBudget, "$.budget.max_vertices", "sample budget must be positive")
	}
	result := &SamplingResult{
		BaseTolerance:  tolerance,
		ToleranceBySeg: make(map[string]int64, len(scene.Segments)),
		Budget:         BudgetReport{RequestedBudget: budget},
	}
	current := make(map[string]*SegmentSample, len(scene.Segments))
	total := 0
	sampled, events, err := sampleSegments(ctx, scene.Segments, tolerance, maxDepth, opts.Workers, opts.VerticesPerWorker, nil)
	if err != nil {
		return nil, err
	}
	result.WorkerEvents = append(result.WorkerEvents, events...)
	for _, sample := range sampled {
		id := sample.SegmentID
		result.ToleranceBySeg[id] = tolerance
		current[id] = &sample
		total += len(sample.Vertices)
	}
	result.Budget.InitialVertices = total

	priority := sortedSegmentsByPriority(scene.Segments)
	minTotal := 2 * len(scene.Segments)
	if budget < minTotal {
		tail := ""
		if len(priority) > 0 {
			tail = priority[len(priority)-1].id
		}
		return result, budgetError(minTotal, budget, tail, "budget is below two endpoints per segment")
	}
	if total > budget {
		rankByID := make(map[string]int, len(priority))
		for i, item := range priority {
			rankByID[item.id] = i
		}
		for total > budget {
			progress := false
			for _, item := range priority {
				seg, _ := scene.Segment(item.id)
				before := current[item.id]
				if len(before.Vertices) <= 2 {
					continue
				}
				fromTol := before.Tolerance
				toTol := fromTol
				if toTol > math.MaxInt64/2 {
					toTol = math.MaxInt64
				} else {
					toTol *= 2
				}
				after, err := SampleSegment(*seg, toTol, maxDepth)
				if err != nil {
					return result, err
				}
				if len(after.Vertices) >= len(before.Vertices) {
					if toTol == math.MaxInt64 {
						continue
					}
					// A doubling that did not reduce the vertex count is not a
					// useful degradation. Continue with a larger tolerance.
					toTol = math.MaxInt64
					after, err = SampleSegment(*seg, toTol, maxDepth)
					if err != nil {
						return result, err
					}
					if len(after.Vertices) >= len(before.Vertices) {
						continue
					}
				}
				beforeTotal := total
				total = total - len(before.Vertices) + len(after.Vertices)
				current[item.id] = after
				after.Degraded = true
				result.ToleranceBySeg[item.id] = toTol
				result.Budget.Degradations = append(result.Budget.Degradations, Degradation{
					SegmentID:      item.id,
					PriorityRank:   rankByID[item.id],
					Basis:          fmt.Sprintf("curvature_score=%s visual_weight=%d priority=%s", item.score.String(), item.weight, item.priority.String()),
					CurvatureScore: item.score.String(),
					VisualWeight:   item.weight,
					FromTolerance:  fromTol,
					ToTolerance:    toTol,
					BeforeVertices: len(before.Vertices),
					AfterVertices:  len(after.Vertices),
					BeforeMaxChord: before.MaxChordHeight,
					AfterMaxChord:  after.MaxChordHeight,
					BudgetBefore:   beforeTotal,
					BudgetAfter:    total,
				})
				progress = true
				if total <= budget {
					break
				}
			}
			if !progress {
				tail := ""
				if len(priority) > 0 {
					tail = priority[len(priority)-1].id
				}
				return result, budgetError(total, budget, tail, "all segments reached the minimum endpoint count")
			}
		}
	}
	result.Budget.FinalVertices = total
	result.Budget.Remaining = budget - total
	result.Budget.Degraded = len(result.Budget.Degradations) > 0
	ids := make([]string, 0, len(current))
	for id := range current {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		result.Segments = append(result.Segments, *current[id])
	}
	return result, nil
}

func budgetError(total, budget int, tail, reason string) *DomainError {
	overage := total - budget
	if overage < 0 {
		overage = 0
	}
	return (&DomainError{Code: CodeBudget, Path: "$.budget.max_vertices", Message: "sample budget cannot be met after deterministic degradation"}).
		WithDetail("overage_vertices", strconv.Itoa(overage)).
		WithDetail("last_priority_segment", tail).
		WithDetail("reason", reason).
		WithDetail("vertices", strconv.Itoa(total)).
		WithDetail("budget", strconv.Itoa(budget))
}

func sampleSegments(ctx context.Context, segments []CurveSegment, tolerance int64, maxDepth, workers, verticesPerWorker int, estimates map[string]int) ([]SegmentSample, []WorkerEvent, error) {
	ordered := append([]CurveSegment(nil), segments...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	results := make([]SegmentSample, len(ordered))
	if workers <= 1 || len(ordered) <= 1 {
		for i, seg := range ordered {
			sample, err := SampleSegment(seg, tolerance, maxDepth)
			if err != nil {
				return nil, nil, err
			}
			results[i] = *sample
		}
		return results, nil, nil
	}
	pool, err := NewWorkerPool(WorkerPoolConfig{Workers: workers, VerticesPerWorker: verticesPerWorker})
	if err != nil {
		return nil, nil, err
	}
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for i, seg := range ordered {
		i, seg := i, seg
		estimate := 0
		if estimates != nil {
			estimate = estimates[seg.ID]
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := pool.Do(ctx, "sample:"+seg.ID, estimate, func(workerID, bufferLimit int) (int, error) {
				sample, err := SampleSegment(seg, tolerance, maxDepth)
				if err != nil {
					return 0, err
				}
				if len(sample.Vertices) > bufferLimit {
					return len(sample.Vertices), (&DomainError{Code: CodeBuffer, Path: "$.worker.buffer", Message: "per-worker vertex buffer exhausted"}).
						WithDetail("worker_id", strconv.Itoa(workerID)).
						WithDetail("task", seg.ID).
						WithDetail("vertices", strconv.Itoa(len(sample.Vertices))).
						WithDetail("capacity", strconv.Itoa(bufferLimit))
				}
				results[i] = *sample
				return len(sample.Vertices), nil
			})
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	pool.Close()
	if firstErr != nil {
		return nil, pool.Events(), firstErr
	}
	return results, pool.Events(), nil
}
