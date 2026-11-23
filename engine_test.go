package spatialpick

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

func mustSample(t *testing.T) (*Scene, *Truth, *Engine) {
	t.Helper()
	scene, truth, err := GenerateSample()
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	return scene, truth, engine
}

func TestQueriesArePureAndDeterministicUnderConcurrency(t *testing.T) {
	scene, _, engine := mustSample(t)
	idx := engine.Index()
	ray := scene.RayQueries[0]
	box := scene.BoxQueries[0]
	point := scene.PointQueries[0]
	wantRay, _ := idx.PickRay(ray, scene.Camera)
	wantBox, _ := idx.SelectBox(box, scene.Camera)
	wantPoint, _ := idx.ContainsPoint(point, scene.Camera)
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan string, workers*3)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				gotRay, _ := idx.PickRay(ray, scene.Camera)
				gotBox, _ := idx.SelectBox(box, scene.Camera)
				gotPoint, _ := idx.ContainsPoint(point, scene.Camera)
				if !reflect.DeepEqual(gotRay, wantRay) {
					errs <- "ray result changed across calls"
					return
				}
				if !reflect.DeepEqual(gotBox, wantBox) {
					errs <- "box result changed across calls"
					return
				}
				if !reflect.DeepEqual(gotPoint, wantPoint) {
					errs <- "point result changed across calls"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

func TestQueryParameterAndAmbiguityErrors(t *testing.T) {
	scene, _, engine := mustSample(t)
	_, err := engine.PickRay(RayQuery{ID: "outside", Screen: Vec2{X: -1, Y: 0}})
	if !IsCode(err, CodeParameter) {
		t.Fatalf("ray E_PARA: %v", err)
	}
	var ce *CodedError
	IsCoded(err, &ce)
	if ce.QueryID != "outside" || ce.Field != "screen" {
		t.Fatalf("bad E_PARA context: %+v", ce)
	}
	_, err = engine.SelectBox(BoxQuery{ID: "outside-box", Rect: Rect{X0: 0, Y0: 0, X1: 5000, Y1: 10}})
	if !IsCode(err, CodeParameter) {
		t.Fatalf("box E_PARA: %v", err)
	}

	var ambigRay RayQuery
	for _, q := range scene.RayQueries {
		if q.ID == "ray-ambig-sphere" {
			ambigRay = q
		}
	}
	res, err := engine.PickRay(ambigRay)
	if !IsCode(err, CodeAmbiguous) {
		t.Fatalf("ray E_AMBIG: %v", err)
	}
	IsCoded(err, &ce)
	if ce.QueryID != ambigRay.ID || ce.GeometryID == "" || len(ce.Ambiguities) == 0 {
		t.Fatalf("bad E_AMBIG context: %+v", ce)
	}
	if res.ErrorCode != CodeAmbiguous {
		t.Fatalf("result error code = %q", res.ErrorCode)
	}

	var ambigPoint PointQuery
	for _, q := range scene.PointQueries {
		if q.ID == "point-ambig-sphere-surface" {
			ambigPoint = q
		}
	}
	_, err = engine.ContainsPoint(ambigPoint)
	if !IsCode(err, CodeAmbiguous) {
		t.Fatalf("point E_AMBIG: %v", err)
	}
}

func TestCacheReuseStaleVersionAndErrorTrace(t *testing.T) {
	scene, _, engine := mustSample(t)
	old, err := engine.GetDerived("n006", 0)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion := old.Version
	unaffected, _ := engine.GetDerived("n060", 0)
	beforeAncestor, _ := engine.GetDerived("n005", 0)
	if _, err := engine.ApplyPivotBatch(scene.EditSequence[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.GetDerived("n006", oldVersion); !IsCode(err, CodeStale) {
		t.Fatalf("want E_STALE, got %v", err)
	}
	var ce *CodedError
	_, _ = engine.GetDerived("n006", oldVersion)
	IsCoded(mustErrStale(t, engine, oldVersion), &ce)
	if ce.NodeID != "n006" || !ce.HasExpectedVersion || ce.ExpectedVersion != oldVersion {
		t.Fatalf("bad stale context: %+v", ce)
	}
	changed, err := engine.GetDerived("n006", 0)
	if err != nil {
		t.Fatal(err)
	}
	if changed == old {
		t.Fatal("affected cache pointer was reused")
	}
	afterUnaffected, _ := engine.GetDerived("n060", 0)
	afterAncestor, _ := engine.GetDerived("n005", 0)
	if afterUnaffected != unaffected || afterAncestor != beforeAncestor {
		t.Fatal("non-descendant cache was not reused")
	}
	if len(changed.ErrorTrace) != changed.Depth+1 {
		t.Fatalf("trace length=%d depth=%d", len(changed.ErrorTrace), changed.Depth)
	}
	last := 0.0
	for _, step := range changed.ErrorTrace {
		if step.Cumulative < last || step.QuantumError <= 0 {
			t.Fatalf("invalid non-monotonic trace: %+v", changed.ErrorTrace)
		}
		last = step.Cumulative
	}
}

func mustErrStale(t *testing.T, e *Engine, version uint64) error {
	t.Helper()
	_, err := e.GetDerived("n006", version)
	return err
}

func TestDependencyPlanDetectsMissedObjects(t *testing.T) {
	scene, _, engine := mustSample(t)
	plan, err := engine.PlanTransformInvalidation([]string{"n006"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(plan.AffectedNodes, "n006") || !containsString(plan.AffectedNodes, "n047") {
		t.Fatalf("descendants missing from plan: %v", plan.AffectedNodes)
	}
	if containsString(plan.AffectedNodes, "n060") {
		t.Fatal("unrelated branch was invalidated")
	}
	before := engine.Bundle()
	changes := map[string]FixedMatrix{"n006": shiftLocalX(t, engine.st.locals["n006"], scene.Precision, 0.01)}

	tampered := *plan
	tampered.IndexItems = append([]InvalidationItem(nil), plan.IndexItems...)
	tampered.CacheItems = append([]InvalidationItem(nil), plan.CacheItems...)
	if len(tampered.IndexItems) > 0 {
		tampered.IndexItems = tampered.IndexItems[:len(tampered.IndexItems)-1]
	}
	_, err = engine.ApplyLocalTransforms(&tampered, changes)
	if !IsCode(err, CodeDependency) {
		t.Fatalf("want E_DEP, got %v", err)
	}
	var ce *CodedError
	IsCoded(err, &ce)
	if len(ce.Missing) == 0 {
		t.Fatalf("E_DEP did not list missing objects: %+v", ce)
	}
	after := engine.Bundle()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed dependency validation mutated the scene")
	}
	if _, err := engine.ApplyLocalTransforms(plan, changes); err != nil {
		t.Fatal(err)
	}
	if engine.Revision() != 1 {
		t.Fatalf("revision = %d", engine.Revision())
	}
}

func shiftLocalX(t *testing.T, m FixedMatrix, precision int, dx float64) FixedMatrix {
	t.Helper()
	q, ok := quantizeFloat(dx, precision)
	if !ok {
		t.Fatal("bad quantization")
	}
	m[3] += q
	return m
}

func TestBatchCommitIsAtomicAndStalePlansRollBack(t *testing.T) {
	scene, _, engine := mustSample(t)
	plan, err := engine.PlanPivotBatch(scene.EditSequence[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ApplyPivotBatch(scene.EditSequence[1]); err != nil {
		t.Fatal(err)
	}
	before, _ := jsonCloneBundle(t, engine.Bundle())
	_, err = engine.CommitBatch(plan)
	if !IsCode(err, CodeTransaction) {
		t.Fatalf("want E_TXN, got %v", err)
	}
	var ce *CodedError
	IsCoded(err, &ce)
	if ce.BatchID != scene.EditSequence[0].ID || ce.ObjectID == "" {
		t.Fatalf("E_TXN lacks batch/object: %+v", ce)
	}
	after, _ := jsonCloneBundle(t, engine.Bundle())
	if !reflect.DeepEqual(before, after) {
		t.Fatal("stale batch modified state")
	}

	plan2, err := engine.PlanPivotBatch(scene.EditSequence[2])
	if err != nil {
		t.Fatal(err)
	}
	bad := *plan2
	bad.NewLocals = make(map[string]FixedMatrix, len(plan2.NewLocals))
	for id, m := range plan2.NewLocals {
		bad.NewLocals[id] = m
	}
	for id, m := range bad.NewLocals {
		m[15] = 0
		bad.NewLocals[id] = m
		break
	}
	_, err = engine.CommitBatch(&bad)
	if !IsCode(err, CodeTransaction) {
		t.Fatalf("want E_TXN for malformed batch, got %v", err)
	}
	after2, _ := jsonCloneBundle(t, engine.Bundle())
	if !reflect.DeepEqual(before, after2) {
		t.Fatal("failed batch did not roll back")
	}
}

func jsonCloneBundle(t *testing.T, b ResultBundle) (ResultBundle, error) {
	t.Helper()
	return b, nil
}

func TestFixedEditSequenceIsBitIdenticalAcrossSchedules(t *testing.T) {
	scene, _, _ := mustSample(t)
	const n = 8
	results := make([]map[string]FixedMatrix, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := NewEngine(scene)
			if err != nil {
				errs <- err
				return
			}
			for _, batch := range scene.EditSequence {
				if _, err := e.ApplyPivotBatch(batch); err != nil {
					errs <- err
					return
				}
			}
			m := make(map[string]FixedMatrix)
			for _, id := range e.NodeIDs() {
				d, err := e.GetDerived(id, 0)
				if err != nil {
					errs <- err
					return
				}
				m[id] = d.World
			}
			results[i] = m
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		if !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("schedule %d produced different fixed world matrices", i)
		}
	}
}

func TestCompareBundlesLocatesFirstDivergence(t *testing.T) {
	scene, _, e1 := mustSample(t)
	e2, err := NewEngine(scene)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e1.ApplyPivotBatch(scene.EditSequence[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.ApplyPivotBatch(scene.EditSequence[1]); err != nil {
		t.Fatal(err)
	}
	b1, b2 := e1.Bundle(), e2.Bundle()
	err = CompareBundles(&b1, &b2)
	if !IsCode(err, CodeDivergence) {
		t.Fatalf("want E_DIVERGE, got %v", err)
	}
	var ce *CodedError
	IsCoded(err, &ce)
	if ce.ObjectID == "" || ce.DataItem == "" || ce.Left == nil || ce.Right == nil {
		t.Fatalf("incomplete divergence error: %+v", ce)
	}
}

func TestRebuildFailureIsolatedAndRetryable(t *testing.T) {
	_, _, engine := mustSample(t)
	unrelated, _ := engine.GetDerived("n060", 0)
	report, err := engine.RebuildSubtree(context.Background(), []string{"n012"}, RebuildOptions{
		MaxParallel: 4, MaxScratchBytes: 1 << 20,
		Fault: func(id string) error {
			if id == "n012" {
				return context.DeadlineExceeded
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(report.Failed, "n012") {
		t.Fatalf("failed list = %v", report.Failed)
	}
	if _, err := engine.GetDerived("n012", 0); !IsCode(err, CodeStale) {
		t.Fatalf("failed entry did not report E_STALE: %v", err)
	}
	gotUnrelated, _ := engine.GetDerived("n060", 0)
	if gotUnrelated != unrelated {
		t.Fatal("failed rebuild polluted unrelated cache")
	}
	report2, err := engine.RebuildSubtree(context.Background(), []string{"n012"}, RebuildOptions{MaxParallel: 4, MaxScratchBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(report2.Failed) != 0 || !containsString(report2.Rebuilt, "n012") {
		t.Fatalf("retry report = %+v", report2)
	}
	if _, err := engine.GetDerived("n012", 0); err != nil {
		t.Fatalf("retry did not restore cache: %v", err)
	}
	if _, err := engine.RebuildSubtree(context.Background(), []string{"n012"}, RebuildOptions{MaxScratchBytes: 1}); !IsCode(err, CodeResource) {
		t.Fatalf("scratch limit error = %v", err)
	}
}

func TestQueriesRunWhileIncrementalRebuildComputes(t *testing.T) {
	scene, _, engine := mustSample(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := engine.RebuildSubtree(context.Background(), []string{"n012"}, RebuildOptions{
			MaxParallel: 2,
			Fault: func(id string) error {
				if id == "n012" {
					close(entered)
					<-release
				}
				return nil
			},
		})
		done <- err
	}()
	<-entered
	queryDone := make(chan error, 1)
	go func() {
		res, err := engine.PickRay(scene.RayQueries[0])
		if err == nil && res.QueryID != scene.RayQueries[0].ID {
			err = context.DeadlineExceeded
		}
		queryDone <- err
	}()
	select {
	case err := <-queryDone:
		if err != nil {
			t.Fatalf("query blocked/failed during rebuild: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("query blocked while rebuild was computing")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestGeometryAddRemoveInvalidatesOnlyNodeEntry(t *testing.T) {
	_, _, engine := mustSample(t)
	childBefore, _ := engine.GetDerived("n002", 0)
	g := &Geometry{
		ID:   "g-added-runtime",
		Kind: GeometryAABB,
		Min:  V3(-0.1, -0.1, -0.1),
		Max:  V3(0.1, 0.1, 0.1),
	}
	plan, err := engine.PlanGeometryMutations([]GeometryMutation{{NodeID: "n001", Geometry: g}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Invalidation.CacheItems) != 1 || len(plan.Invalidation.IndexItems) != 1 {
		t.Fatalf("geometry invalidation is not node-local: %+v", plan.Invalidation)
	}
	before := engine.Bundle()
	tampered := *plan
	tampered.Invalidation = &InvalidationPlan{
		BaseRevision:     plan.Invalidation.BaseRevision,
		BaseIndexVersion: plan.Invalidation.BaseIndexVersion,
		ChangedNodes:     plan.Invalidation.ChangedNodes,
		AffectedNodes:    plan.Invalidation.AffectedNodes,
		CacheItems:       append([]InvalidationItem(nil), plan.Invalidation.CacheItems...),
	}
	if _, err := engine.CommitGeometryPlan(&tampered); !IsCode(err, CodeDependency) {
		t.Fatalf("missing index invalidation: %v", err)
	}
	after, _ := jsonCloneBundle(t, engine.Bundle())
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed geometry plan mutated state")
	}
	if _, err := engine.ApplyGeometryMutations([]GeometryMutation{{NodeID: "n001", Geometry: g}}); err != nil {
		t.Fatal(err)
	}
	d, err := engine.GetDerived("n001", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.GeometryID != g.ID {
		t.Fatalf("geometry id = %q", d.GeometryID)
	}
	if _, ok := engine.Index().FindEntry("n001"); !ok {
		t.Fatal("added geometry missing from index")
	}
	childAfter, _ := engine.GetDerived("n002", 0)
	if childAfter != childBefore {
		t.Fatal("child cache should not be invalidated by geometry-only edit")
	}
	foundGeometryEdge := false
	for _, edge := range engine.Dependencies() {
		if edge.From == "geometry:"+g.ID && edge.To == indexObject("n001", g.ID) {
			foundGeometryEdge = true
		}
	}
	if !foundGeometryEdge {
		t.Fatal("dependency graph does not expose geometry -> index edge")
	}
	if _, err := engine.ApplyGeometryMutations([]GeometryMutation{{NodeID: "n001", Geometry: nil}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.Index().FindEntry("n001"); ok {
		t.Fatal("removed geometry remains in index")
	}
}
