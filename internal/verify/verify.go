package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"editorpick"
	"editorpick/internal/samplegen"
)

const (
	matrixTolerance = 5e-4
	boundsTolerance = 1.5e-3
	pointTolerance  = 8e-4
)

type Mismatch struct {
	Scope    string `json:"scope,omitempty"`
	PathA    string `json:"path_a,omitempty"`
	PathB    string `json:"path_b,omitempty"`
	StepID   string `json:"step_id,omitempty"`
	QueryID  string `json:"query_id,omitempty"`
	Field    string `json:"field,omitempty"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Message  string `json:"message,omitempty"`
}

type Outputs struct {
	RayStatus        map[string]string      `json:"ray_status"`
	RayNode          map[string]string      `json:"ray_node,omitempty"`
	RayPoint         map[string][3]float64  `json:"ray_point,omitempty"`
	RayAmbiguityNode map[string]string      `json:"ray_ambiguity_node,omitempty"`
	BoxStatus        map[string]string      `json:"box_status"`
	BoxNodes         map[string][]string    `json:"box_nodes"`
	PointStatus      map[string]string      `json:"point_status"`
	PointNodes       map[string][]string    `json:"point_nodes"`
	Matrices         map[string][16]float64 `json:"matrices"`
	AABBs            map[string][6]float64  `json:"aabbs"`
}

type PathConsistency struct {
	IncrementalVsResume *Mismatch `json:"incremental_vs_resume,omitempty"`
	IncrementalVsFull   *Mismatch `json:"incremental_vs_full,omitempty"`
	ResumeSnapshotPath  string    `json:"resume_snapshot_path,omitempty"`
}

type TruthReport struct {
	RayCount          int       `json:"ray_count"`
	BoxCount          int       `json:"box_count"`
	PointCount        int       `json:"point_count"`
	OperationCount    int       `json:"operation_count"`
	FirstMismatch     *Mismatch `json:"first_mismatch,omitempty"`
	OperationMismatch *Mismatch `json:"operation_mismatch,omitempty"`
	MaxPointDiff      float64   `json:"max_point_diff"`
	MaxMatrixDiff     float64   `json:"max_matrix_diff"`
	MaxBoundsDiff     float64   `json:"max_bounds_diff"`
}

type CodeCheck struct {
	Code   string `json:"code"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type ConcurrencyReport struct {
	RayQueries    int       `json:"ray_queries"`
	BoxQueries    int       `json:"box_queries"`
	SerialSame    bool      `json:"serial_same"`
	DerivedSame   bool      `json:"derived_same"`
	FirstMismatch *Mismatch `json:"first_mismatch,omitempty"`
}

type InvalidationReport struct {
	CacheReuseObserved bool     `json:"cache_reuse_observed"`
	UnaffectedReused   bool     `json:"unaffected_derived_reused"`
	ExpectedObjects    []string `json:"expected_objects"`
	FoundObjects       []string `json:"found_objects"`
	OK                 bool     `json:"ok"`
}

type Report struct {
	OK                    bool               `json:"ok"`
	Directory             string             `json:"directory"`
	Seed                  int64              `json:"seed"`
	ManifestOK            bool               `json:"manifest_ok"`
	DeterministicGenerate bool               `json:"deterministic_generate"`
	FileHashesEqual       bool               `json:"file_hashes_equal"`
	Tolerances            map[string]float64 `json:"tolerances"`
	Truth                 TruthReport        `json:"truth"`
	Paths                 PathConsistency    `json:"paths"`
	ErrorCodes            []CodeCheck        `json:"error_codes"`
	Concurrency           ConcurrencyReport  `json:"concurrency"`
	Invalidation          InvalidationReport `json:"invalidation"`
	SnapshotTamper        []CodeCheck        `json:"snapshot_tamper"`
	FirstFailure          string             `json:"first_failure,omitempty"`
}

func Run(dir, out string) (*Report, error) {
	report := &Report{
		OK:        true,
		Directory: dir,
		Seed:      samplegen.DefaultSeed,
		Tolerances: map[string]float64{
			"matrix": matrixTolerance, "bounds": boundsTolerance, "point": pointTolerance,
		},
	}
	fail := func(m *Mismatch) {
		if m == nil {
			return
		}
		report.OK = false
		if report.FirstFailure == "" {
			report.FirstFailure = mismatchString(m)
		}
	}

	deterministic, hashesEqual, err := checkDeterministicGeneration(dir)
	if err != nil {
		return nil, err
	}
	report.DeterministicGenerate = deterministic
	report.FileHashesEqual = hashesEqual
	if !deterministic || !hashesEqual {
		fail(&Mismatch{Scope: "generate", Message: "generation is not byte-for-byte deterministic or sample is stale"})
	}

	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		return nil, err
	}
	qf, err := editorpick.LoadQueryFile(filepath.Join(dir, "queries.json"))
	if err != nil {
		return nil, err
	}
	tf, err := editorpick.LoadTruthFile(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, err
	}
	manifest, err := editorpick.LoadManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if err := manifest.VerifyFileHashes(dir); err != nil {
		return nil, err
	}
	report.ManifestOK = true

	if m := checkDeclaredMatrices(scene, tf); m != nil {
		fail(m)
	}

	_, truthEngine, truthLoadErr := loadEngine(dir)
	if truthLoadErr != nil {
		return nil, truthLoadErr
	}
	initialOutputs := collectOutputs(truthEngine, qf)

	incEngine, incOut, m := runIncremental(dir, qf)
	if m != nil {
		fail(m)
	}
	resumeOut, snapshotPath, m := runResume(dir, qf)
	report.Paths.ResumeSnapshotPath = snapshotPath
	if m != nil {
		fail(m)
	}
	fullOut, m := runFullRebuild(dir, qf)
	if m != nil {
		fail(m)
	}

	if incOut != nil && resumeOut != nil {
		mm := compareOutputs("incremental", "resume", *incOut, *resumeOut)
		report.Paths.IncrementalVsResume = mm
		fail(mm)
	}
	if incOut != nil && fullOut != nil {
		mm := compareOutputs("incremental", "full-rebuild", *incOut, *fullOut)
		report.Paths.IncrementalVsFull = mm
		fail(mm)
	}

	truthReport := checkTruth(qf, tf, &initialOutputs)
	report.Truth = truthReport
	if truthReport.FirstMismatch != nil {
		fail(truthReport.FirstMismatch)
	}

	opMismatch, maxMat, maxBounds := checkOperationTruths(dir, qf, tf)
	report.Truth.OperationMismatch = opMismatch
	report.Truth.MaxMatrixDiff = maxFloat(report.Truth.MaxMatrixDiff, maxMat)
	report.Truth.MaxBoundsDiff = maxBounds
	if opMismatch != nil {
		fail(opMismatch)
	}

	report.ErrorCodes = checkErrorCodes()
	for _, c := range report.ErrorCodes {
		if !c.OK {
			fail(&Mismatch{Scope: "error-code", Field: c.Code, Message: c.Detail})
		}
	}
	report.SnapshotTamper = checkSnapshotTamper(dir)
	for _, c := range report.SnapshotTamper {
		if !c.OK {
			fail(&Mismatch{Scope: "snapshot", Field: c.Code, Message: c.Detail})
		}
	}

	report.Concurrency = checkConcurrency(incEngine, qf, incOut)
	if !report.Concurrency.SerialSame || !report.Concurrency.DerivedSame {
		if report.Concurrency.FirstMismatch != nil {
			fail(report.Concurrency.FirstMismatch)
		}
		fail(&Mismatch{Scope: "concurrency", Message: "parallel query outputs differ from serial outputs"})
	}
	report.Invalidation = checkInvalidation(incEngine, qf)
	if !report.Invalidation.OK {
		fail(&Mismatch{Scope: "invalidation", Message: "incremental cache/reuse contract failed"})
	}
	if inv := incEngine.LastInvalidation(); inv != nil {
		if data, err := json.MarshalIndent(inv, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "invalidation-report.json"), append(data, '\n'), 0o644)
		}
	}

	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil || filepath.Dir(out) == "." {
			data, _ := json.MarshalIndent(report, "", "  ")
			data = append(data, '\n')
			if werr := os.WriteFile(out, data, 0o644); werr != nil {
				return report, werr
			}
		}
	}
	return report, nil
}

func (m *Mismatch) Error() string { return mismatchString(m) }

func mismatchString(m *Mismatch) string {
	if m.QueryID != "" {
		return fmt.Sprintf("%s query=%s field=%s: %s", m.Scope, m.QueryID, m.Field, m.Message)
	}
	if m.StepID != "" {
		return fmt.Sprintf("%s step=%s field=%s: %s", m.Scope, m.StepID, m.Field, m.Message)
	}
	return fmt.Sprintf("%s field=%s: %s", m.Scope, m.Field, m.Message)
}

func checkDeterministicGeneration(dir string) (bool, bool, error) {
	d1, err := os.MkdirTemp("", "editorpick-gen-a-*")
	if err != nil {
		return false, false, err
	}
	d2, err := os.MkdirTemp("", "editorpick-gen-b-*")
	if err != nil {
		return false, false, err
	}
	defer os.RemoveAll(d1)
	defer os.RemoveAll(d2)
	if err := samplegen.Generate(d1, samplegen.DefaultSeed); err != nil {
		return false, false, err
	}
	if err := samplegen.Generate(d2, samplegen.DefaultSeed); err != nil {
		return false, false, err
	}
	names := []string{"scene.json", "queries.json", "truth.json", "manifest.json"}
	deterministic := true
	matchesSource := true
	for _, name := range names {
		a, errA := os.ReadFile(filepath.Join(d1, name))
		b, errB := os.ReadFile(filepath.Join(d2, name))
		if errA != nil || errB != nil || string(a) != string(b) {
			deterministic = false
		}
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(src) != string(a) {
			matchesSource = false
		}
	}
	return deterministic, matchesSource, nil
}

func checkDeclaredMatrices(scene *editorpick.Scene, tf *editorpick.TruthFile) *Mismatch {
	for id, expected := range tf.DeclaredLocalMatrices {
		n, ok := scene.NodeByID[id]
		if !ok {
			return &Mismatch{Scope: "schema", QueryID: id, Field: "node", Message: "declared matrix node missing"}
		}
		for i := 0; i < 16; i++ {
			actual := float64(n.Local[i]) / float64(editorpick.FixedScale)
			if math.Abs(actual-expected[i]) > 2e-6 {
				return &Mismatch{Scope: "declared-matrix", QueryID: id, Field: fmt.Sprintf("matrix[%d]", i), Expected: expected[i], Actual: actual}
			}
		}
	}
	return nil
}

func loadEngine(dir string) (*editorpick.Scene, *editorpick.Engine, error) {
	scene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		return nil, nil, err
	}
	e, err := editorpick.NewEngine(scene)
	if err != nil {
		return nil, nil, err
	}
	return scene, e, nil
}

func applyStep(e *editorpick.Engine, step editorpick.EditStep) error {
	batch := editorpick.PivotBatch{Selection: step.Selection, Pivot: step.Pivot, Ops: step.Ops}
	_, err := e.ApplyPivotBatch(batch)
	if step.ExpectCode != "" {
		if err == nil {
			return &Mismatch{Scope: "transaction", StepID: step.ID, Field: "code", Expected: step.ExpectCode, Actual: "nil"}
		}
		var de *editorpick.DomainError
		if !errors.As(err, &de) || de.Code != step.ExpectCode {
			return &Mismatch{Scope: "transaction", StepID: step.ID, Field: "code", Expected: step.ExpectCode, Actual: err.Error()}
		}
		return nil
	}
	if err != nil {
		return &Mismatch{Scope: "transaction", StepID: step.ID, Field: "apply", Actual: err.Error()}
	}
	return nil
}

func runIncremental(dir string, qf *editorpick.QueryFile) (*editorpick.Engine, *Outputs, *Mismatch) {
	_, e, err := loadEngine(dir)
	if err != nil {
		return nil, nil, &Mismatch{Scope: "incremental", Message: err.Error()}
	}
	for _, step := range qf.EditSequence {
		if err := applyStep(e, step); err != nil {
			return e, nil, err.(*Mismatch)
		}
	}
	out := collectOutputs(e, qf)
	return e, &out, nil
}

func runFullRebuild(dir string, qf *editorpick.QueryFile) (*Outputs, *Mismatch) {
	_, e, err := loadEngine(dir)
	if err != nil {
		return nil, &Mismatch{Scope: "full", Message: err.Error()}
	}
	for _, step := range qf.EditSequence {
		if err := applyStep(e, step); err != nil {
			return nil, err.(*Mismatch)
		}
	}
	if err := e.RebuildAll(); err != nil {
		return nil, &Mismatch{Scope: "full", Field: "RebuildAll", Message: err.Error()}
	}
	out := collectOutputs(e, qf)
	return &out, nil
}

func runResume(dir string, qf *editorpick.QueryFile) (*Outputs, string, *Mismatch) {
	_, e, err := loadEngine(dir)
	if err != nil {
		return nil, "", &Mismatch{Scope: "resume", Message: err.Error()}
	}
	checkpoint := -1
	for i, step := range qf.EditSequence {
		if err := applyStep(e, step); err != nil {
			return nil, "", err.(*Mismatch)
		}
		if step.Checkpoint {
			checkpoint = i
			break
		}
	}
	if checkpoint < 0 {
		return nil, "", &Mismatch{Scope: "resume", Field: "checkpoint", Message: "no checkpoint step"}
	}
	snapshotPath := filepath.Join(dir, "resume-snapshot.json")
	if err := e.SaveSnapshot(snapshotPath); err != nil {
		return nil, snapshotPath, &Mismatch{Scope: "resume", Field: "SaveSnapshot", Message: err.Error()}
	}

	restartScene, err := editorpick.LoadSceneFile(filepath.Join(dir, "scene.json"))
	if err != nil {
		return nil, snapshotPath, &Mismatch{Scope: "resume", Message: err.Error()}
	}
	restarted, err := editorpick.NewEngine(restartScene)
	if err != nil {
		return nil, snapshotPath, &Mismatch{Scope: "resume", Message: err.Error()}
	}
	snap, err := editorpick.LoadSnapshot(snapshotPath, restartScene)
	if err != nil {
		return nil, snapshotPath, &Mismatch{Scope: "resume", Field: "LoadSnapshot", Message: err.Error()}
	}
	if err := restarted.RestoreSnapshot(snap); err != nil {
		return nil, snapshotPath, &Mismatch{Scope: "resume", Field: "RestoreSnapshot", Message: err.Error()}
	}
	for i := checkpoint + 1; i < len(qf.EditSequence); i++ {
		if err := applyStep(restarted, qf.EditSequence[i]); err != nil {
			return nil, snapshotPath, err.(*Mismatch)
		}
	}
	out := collectOutputs(restarted, qf)
	return &out, snapshotPath, nil
}

func collectOutputs(e *editorpick.Engine, qf *editorpick.QueryFile) Outputs {
	out := Outputs{
		RayStatus:        map[string]string{},
		RayNode:          map[string]string{},
		RayPoint:         map[string][3]float64{},
		RayAmbiguityNode: map[string]string{},
		BoxStatus:        map[string]string{},
		BoxNodes:         map[string][]string{},
		PointStatus:      map[string]string{},
		PointNodes:       map[string][]string{},
		Matrices:         map[string][16]float64{},
		AABBs:            map[string][6]float64{},
	}
	for _, q := range qf.Rays {
		r, err := e.Raycast(q)
		if err != nil {
			out.RayStatus[q.ID] = "ERROR:" + err.Error()
			continue
		}
		out.RayStatus[q.ID] = r.Status
		if r.Hit != nil {
			out.RayNode[q.ID] = r.Hit.NodeID
			out.RayPoint[q.ID] = [3]float64{r.Hit.Point.X.Float64(), r.Hit.Point.Y.Float64(), r.Hit.Point.Z.Float64()}
		}
		if r.Ambiguity != nil {
			out.RayAmbiguityNode[q.ID] = r.Ambiguity.NodeID
		}
	}
	for _, q := range qf.Boxes {
		r, err := e.BoxSelect(q)
		if err != nil {
			out.BoxStatus[q.ID] = "ERROR:" + err.Error()
			continue
		}
		out.BoxStatus[q.ID] = r.Status
		out.BoxNodes[q.ID] = append([]string(nil), r.NodeIDs...)
	}
	for _, q := range qf.Points {
		r, err := e.QueryPoint(q)
		if err != nil {
			out.PointStatus[q.ID] = "ERROR:" + err.Error()
			continue
		}
		out.PointStatus[q.ID] = r.Status
		out.PointNodes[q.ID] = append([]string(nil), r.NodeIDs...)
	}
	for _, n := range e.Scene().Nodes {
		d, ok := e.Derived(n.ID)
		if !ok {
			continue
		}
		var mat [16]float64
		for i, v := range d.World {
			mat[i] = v.Float64()
		}
		out.Matrices[n.ID] = mat
		out.AABBs[n.ID] = [6]float64{
			d.AABB.Min.X.Float64(), d.AABB.Min.Y.Float64(), d.AABB.Min.Z.Float64(),
			d.AABB.Max.X.Float64(), d.AABB.Max.Y.Float64(), d.AABB.Max.Z.Float64(),
		}
	}
	return out
}

func compareOutputs(pathA, pathB string, a, b Outputs) *Mismatch {
	rayIDs := sortedKeys(a.RayStatus)
	for _, id := range rayIDs {
		if a.RayStatus[id] != b.RayStatus[id] {
			return mismatch("path-consistency", pathA, pathB, id, "ray.status", a.RayStatus[id], b.RayStatus[id])
		}
		if a.RayNode[id] != b.RayNode[id] {
			return mismatch("path-consistency", pathA, pathB, id, "ray.node_id", a.RayNode[id], b.RayNode[id])
		}
		if a.RayAmbiguityNode[id] != b.RayAmbiguityNode[id] {
			return mismatch("path-consistency", pathA, pathB, id, "ray.ambiguity_node", a.RayAmbiguityNode[id], b.RayAmbiguityNode[id])
		}
		if pa, ok := a.RayPoint[id]; ok {
			pb := b.RayPoint[id]
			for i := 0; i < 3; i++ {
				if math.Abs(pa[i]-pb[i]) > pointTolerance {
					return mismatch("path-consistency", pathA, pathB, id, fmt.Sprintf("ray.point[%c]", 'x'+i), pa[i], pb[i])
				}
			}
		}
	}
	for _, id := range sortedKeys(a.BoxStatus) {
		if a.BoxStatus[id] != b.BoxStatus[id] {
			return mismatch("path-consistency", pathA, pathB, id, "box.status", a.BoxStatus[id], b.BoxStatus[id])
		}
		if !equalStrings(a.BoxNodes[id], b.BoxNodes[id]) {
			return mismatch("path-consistency", pathA, pathB, id, "box.node_ids", a.BoxNodes[id], b.BoxNodes[id])
		}
	}
	for _, id := range sortedKeys(a.PointStatus) {
		if a.PointStatus[id] != b.PointStatus[id] {
			return mismatch("path-consistency", pathA, pathB, id, "point.status", a.PointStatus[id], b.PointStatus[id])
		}
		if !equalStrings(a.PointNodes[id], b.PointNodes[id]) {
			return mismatch("path-consistency", pathA, pathB, id, "point.node_ids", a.PointNodes[id], b.PointNodes[id])
		}
	}
	for _, id := range sortedKeys(a.Matrices) {
		ma, mb := a.Matrices[id], b.Matrices[id]
		for i := 0; i < 16; i++ {
			if math.Abs(ma[i]-mb[i]) > matrixTolerance {
				return mismatch("path-consistency", pathA, pathB, id, fmt.Sprintf("world_matrix[%d]", i), ma[i], mb[i])
			}
		}
		ba, bb := a.AABBs[id], b.AABBs[id]
		for i := 0; i < 6; i++ {
			if math.Abs(ba[i]-bb[i]) > boundsTolerance {
				return mismatch("path-consistency", pathA, pathB, id, fmt.Sprintf("world_aabb[%d]", i), ba[i], bb[i])
			}
		}
	}
	return nil
}

func mismatch(scope, a, b, queryID, field string, expected, actual any) *Mismatch {
	return &Mismatch{Scope: scope, PathA: a, PathB: b, QueryID: queryID, Field: field, Expected: expected, Actual: actual}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func checkTruth(qf *editorpick.QueryFile, tf *editorpick.TruthFile, out *Outputs) TruthReport {
	rep := TruthReport{RayCount: len(qf.Rays), BoxCount: len(qf.Boxes), PointCount: len(qf.Points), OperationCount: len(tf.OperationTruths)}
	if out == nil {
		rep.FirstMismatch = &Mismatch{Scope: "truth", Message: "no outputs"}
		return rep
	}
	rayTruth := map[string]editorpick.RayTruth{}
	for _, rt := range tf.RayTruths {
		rayTruth[rt.QueryID] = rt
	}
	for _, q := range qf.Rays {
		rt := rayTruth[q.ID]
		status := out.RayStatus[q.ID]
		if status != rt.ExpectedStatus {
			rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "ray.status", Expected: rt.ExpectedStatus, Actual: status}
			return rep
		}
		if rt.ExpectedStatus == "hit" {
			if out.RayNode[q.ID] != rt.NodeID {
				rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "ray.node_id", Expected: rt.NodeID, Actual: out.RayNode[q.ID]}
				return rep
			}
			actual := out.RayPoint[q.ID]
			if rt.Point == nil {
				rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "ray.point", Message: "truth point missing"}
				return rep
			}
			for i := 0; i < 3; i++ {
				diff := math.Abs(actual[i] - rt.Point[i])
				rep.MaxPointDiff = math.Max(rep.MaxPointDiff, diff)
				if diff > pointTolerance {
					rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: fmt.Sprintf("ray.point[%c]", 'x'+i), Expected: rt.Point[i], Actual: actual[i]}
					return rep
				}
			}
		}
		if rt.ExpectedStatus == editorpick.EAmbiguous && rt.NodeID != "" && out.RayAmbiguityNode[q.ID] != rt.NodeID {
			rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "ray.ambiguity_node", Expected: rt.NodeID, Actual: out.RayAmbiguityNode[q.ID]}
			return rep
		}
	}
	boxTruth := map[string]editorpick.BoxTruth{}
	for _, bt := range tf.BoxTruths {
		boxTruth[bt.QueryID] = bt
	}
	for _, q := range qf.Boxes {
		bt := boxTruth[q.ID]
		if out.BoxStatus[q.ID] != bt.ExpectedStatus {
			rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "box.status", Expected: bt.ExpectedStatus, Actual: out.BoxStatus[q.ID]}
			return rep
		}
		if !equalStrings(out.BoxNodes[q.ID], bt.NodeIDs) {
			rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "box.node_ids", Expected: bt.NodeIDs, Actual: out.BoxNodes[q.ID]}
			return rep
		}
	}
	pointTruth := map[string]editorpick.PointTruth{}
	for _, pt := range tf.PointTruths {
		pointTruth[pt.QueryID] = pt
	}
	for _, q := range qf.Points {
		pt := pointTruth[q.ID]
		if out.PointStatus[q.ID] != pt.ExpectedStatus {
			rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "point.status", Expected: pt.ExpectedStatus, Actual: out.PointStatus[q.ID]}
			return rep
		}
		if pt.ExpectedStatus == "hit" && !equalStrings(out.PointNodes[q.ID], pt.NodeIDs) {
			rep.FirstMismatch = &Mismatch{Scope: "truth", QueryID: q.ID, Field: "point.node_ids", Expected: pt.NodeIDs, Actual: out.PointNodes[q.ID]}
			return rep
		}
	}
	return rep
}

func checkOperationTruths(dir string, qf *editorpick.QueryFile, tf *editorpick.TruthFile) (*Mismatch, float64, float64) {
	_, e, err := loadEngine(dir)
	if err != nil {
		return &Mismatch{Scope: "operation-truth", Message: err.Error()}, 0, 0
	}
	var maxMat, maxBounds float64
	for i, step := range qf.EditSequence {
		if err := applyStep(e, step); err != nil {
			return err.(*Mismatch), maxMat, maxBounds
		}
		if i >= len(tf.OperationTruths) {
			return &Mismatch{Scope: "operation-truth", StepID: step.ID, Field: "truth", Message: "missing operation truth"}, maxMat, maxBounds
		}
		expected := tf.OperationTruths[i]
		for id, mat := range expected.WorldMatrices {
			d, ok := e.Derived(id)
			if !ok {
				return &Mismatch{Scope: "operation-truth", StepID: step.ID, QueryID: id, Field: "derived", Message: "missing"}, maxMat, maxBounds
			}
			for j := 0; j < 16; j++ {
				actual := d.World[j].Float64()
				diff := math.Abs(actual - mat[j])
				maxMat = math.Max(maxMat, diff)
				if diff > matrixTolerance {
					return &Mismatch{Scope: "operation-truth", StepID: step.ID, QueryID: id, Field: fmt.Sprintf("world_matrix[%d]", j), Expected: mat[j], Actual: actual}, maxMat, maxBounds
				}
			}
			if bounds, ok := expected.WorldAABBs[id]; ok {
				actualBounds := [6]float64{d.AABB.Min.X.Float64(), d.AABB.Min.Y.Float64(), d.AABB.Min.Z.Float64(), d.AABB.Max.X.Float64(), d.AABB.Max.Y.Float64(), d.AABB.Max.Z.Float64()}
				for j := 0; j < 6; j++ {
					diff := math.Abs(actualBounds[j] - bounds[j])
					maxBounds = math.Max(maxBounds, diff)
					if diff > boundsTolerance {
						return &Mismatch{Scope: "operation-truth", StepID: step.ID, QueryID: id, Field: fmt.Sprintf("world_aabb[%d]", j), Expected: bounds[j], Actual: actualBounds[j]}, maxMat, maxBounds
					}
				}
			}
		}
	}
	return nil, maxMat, maxBounds
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func checkConcurrency(e *editorpick.Engine, qf *editorpick.QueryFile, serial *Outputs) ConcurrencyReport {
	rep := ConcurrencyReport{RayQueries: len(qf.Rays), BoxQueries: len(qf.Boxes)}
	if e == nil {
		return rep
	}
	rayOutcomes := editorpick.RaycastParallel(e, qf.Rays)
	for i, out := range rayOutcomes {
		if out.Err != nil || out.Result == nil || serial == nil {
			return rep
		}
		id := qf.Rays[i].ID
		if out.Result.Status != serial.RayStatus[id] {
			return rep
		}
		if out.Result.Hit != nil {
			p := out.Result.Hit.Point
			got := [3]float64{p.X.Float64(), p.Y.Float64(), p.Z.Float64()}
			want := serial.RayPoint[id]
			for j := 0; j < 3; j++ {
				if math.Abs(got[j]-want[j]) > pointTolerance {
					return rep
				}
			}
		}
	}
	boxOutcomes := editorpick.BoxSelectParallel(e, qf.Boxes)
	for i, out := range boxOutcomes {
		if out.Err != nil || out.Result == nil || serial == nil || out.Result.Status != serial.BoxStatus[qf.Boxes[i].ID] {
			return rep
		}
	}
	rep.SerialSame = true
	if err := e.RebuildAllParallel(); err != nil {
		rep.FirstMismatch = &Mismatch{Scope: "derived-parallel", Message: err.Error()}
		return rep
	}
	parallelOut := collectOutputs(e, qf)
	if mm := compareOutputs("serial", "parallel-derived", *serial, parallelOut); mm != nil {
		rep.FirstMismatch = mm
		return rep
	}
	rep.DerivedSame = true
	return rep
}

func checkInvalidation(e *editorpick.Engine, qf *editorpick.QueryFile) InvalidationReport {
	rep := InvalidationReport{ExpectedObjects: []string{"world:chain-00", "world:chain-23", "entry:chain-23", "entry:root"}}
	if e == nil || len(qf.Rays) < 2 {
		return rep
	}
	query := qf.Rays[1]
	_, firstHit, _ := e.RaycastCached(query)
	_, secondHit, _ := e.RaycastCached(query)
	rep.CacheReuseObserved = !firstHit && secondHit
	before, _ := e.Derived("sphere-node")
	beforeRevision := before.Revision
	_, err := e.ApplyPivotBatch(editorpick.PivotBatch{
		Selection: []string{"chain-00"}, Pivot: editorpick.Vec3FromFloat(0, 3, 0),
		Ops: []editorpick.PivotOp{{Type: "translate", Vector: editorpick.Vec3FromFloat(0.05, 0, 0)}},
	})
	if err != nil {
		return rep
	}
	after, ok := e.Derived("sphere-node")
	rep.UnaffectedReused = ok && after.Revision == beforeRevision
	inv := e.LastInvalidation()
	if inv != nil {
		set := map[string]bool{}
		for _, obj := range inv.Objects {
			set[obj.ObjectID] = true
			rep.FoundObjects = append(rep.FoundObjects, obj.ObjectID)
		}
		for _, expected := range rep.ExpectedObjects {
			if !set[expected] {
				rep.OK = false
				return rep
			}
		}
	}
	_, cacheHitAfter, _ := e.RaycastCached(query)
	rep.OK = rep.CacheReuseObserved && rep.UnaffectedReused && !cacheHitAfter
	return rep
}

func checkErrorCodes() []CodeCheck {
	precision := editorpick.DefaultPrecision()
	identity := []float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	base := editorpick.SceneFile{Schema: editorpick.SceneSchemaV1, StructureVersion: editorpick.StructureVersion, Precision: precision}
	var checks []CodeCheck

	missingID := base
	missingID.Nodes = []editorpick.NodeDTO{{Matrix: identity}}
	checks = append(checks, parseCodeCheck("E_SCHEMA-missing-id", mustJSON(missingID), editorpick.ESchema, "nodes[0]"))

	cycle := base
	idA, idB := "a", "b"
	cycle.Nodes = []editorpick.NodeDTO{
		{ID: &idA, ParentID: &idB, Matrix: identity},
		{ID: &idB, ParentID: &idA, Matrix: identity},
	}
	checks = append(checks, parseCodeCheck("E_CYCLE-parent-cycle", mustJSON(cycle), editorpick.ECycle, "a"))

	missingParent := base
	idC := "c"
	no := "missing"
	missingParent.Nodes = []editorpick.NodeDTO{{ID: &idC, ParentID: &no, Matrix: identity}}
	checks = append(checks, parseCodeCheck("E_REF-missing-parent", mustJSON(missingParent), editorpick.ERef, "c"))

	badGeom := base
	idD := "d"
	badIndex := 9
	badGeom.Nodes = []editorpick.NodeDTO{{ID: &idD, Matrix: identity, GeometryIndex: &badIndex}}
	checks = append(checks, parseCodeCheck("E_REF-geometry-index", mustJSON(badGeom), editorpick.ERef, "d"))

	_, engine, err := engineForErrorCode()
	if err == nil {
		_, txnErr := engine.ApplyPivotBatch(editorpick.PivotBatch{})
		checks = append(checks, domainCodeCheck("E_TXN-empty-batch", txnErr, editorpick.ETxn))
	} else {
		checks = append(checks, CodeCheck{Code: editorpick.ETxn, OK: false, Detail: err.Error()})
	}
	return checks
}

func parseCodeCheck(name string, data []byte, code, nodeID string) CodeCheck {
	_, err := editorpick.ParseScene(data)
	check := CodeCheck{Code: name}
	var de *editorpick.DomainError
	if errors.As(err, &de) && de.Code == code && (nodeID == "" || de.NodeID == nodeID) {
		check.OK = true
		check.Detail = err.Error()
	} else if err != nil {
		check.Detail = "unexpected error: " + err.Error()
	} else {
		check.Detail = "expected error but parse succeeded"
	}
	return check
}

func domainCodeCheck(name string, err error, code string) CodeCheck {
	check := CodeCheck{Code: name}
	var de *editorpick.DomainError
	if errors.As(err, &de) && de.Code == code {
		check.OK = true
		check.Detail = err.Error()
	} else if err != nil {
		check.Detail = "unexpected error: " + err.Error()
	} else {
		check.Detail = "expected error but operation succeeded"
	}
	return check
}

func engineForErrorCode() (*editorpick.Scene, *editorpick.Engine, error) {
	dir, err := os.MkdirTemp("", "editorpick-code-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	if err := samplegen.Generate(dir, samplegen.DefaultSeed); err != nil {
		return nil, nil, err
	}
	return loadEngine(dir)
}

func checkSnapshotTamper(dir string) []CodeCheck {
	_, e, err := loadEngine(dir)
	if err != nil {
		return []CodeCheck{{Code: editorpick.EHash, OK: false, Detail: err.Error()}}
	}
	path := filepath.Join(dir, ".verify-snapshot-tamper.json")
	defer os.Remove(path)
	if err := e.SaveSnapshot(path); err != nil {
		return []CodeCheck{{Code: editorpick.EHash, OK: false, Detail: err.Error()}}
	}
	valid, err := editorpick.LoadSnapshot(path, e.Scene())
	if err != nil {
		return []CodeCheck{{Code: editorpick.EHash, OK: false, Detail: err.Error()}}
	}
	var checks []CodeCheck
	hashTampered := *valid
	hashTampered.Revision += 1
	writeJSON(path, hashTampered)
	_, err = editorpick.LoadSnapshot(path, e.Scene())
	checks = append(checks, domainCodeCheck("E_HASH-content-tamper", err, editorpick.EHash))

	versionTampered := *valid
	versionTampered.StructureVersion = "bad-version"
	writeJSON(path, versionTampered)
	_, err = editorpick.LoadSnapshot(path, e.Scene())
	checks = append(checks, domainCodeCheck("E_VERSION-mismatch", err, editorpick.EVersion))
	return checks
}

func writeJSON(path string, v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func Demo(dir string) error {
	_, e, err := loadEngine(dir)
	if err != nil {
		return err
	}
	qf, err := editorpick.LoadQueryFile(filepath.Join(dir, "queries.json"))
	if err != nil {
		return err
	}
	fmt.Println("Ray picks:")
	for _, q := range qf.Rays {
		r, err := e.Raycast(q)
		if err != nil {
			return err
		}
		if r.Hit != nil {
			fmt.Printf("  %-24s hit  %-12s point=%s\n", q.ID, r.Hit.NodeID, formatPoint(r.Hit.Point))
		} else if r.Ambiguity != nil {
			fmt.Printf("  %-24s %s %-12s %s..%s\n", q.ID, editorpick.EAmbiguous, r.Ambiguity.NodeID, formatPoint(r.Ambiguity.Lower), formatPoint(r.Ambiguity.Upper))
		} else {
			fmt.Printf("  %-24s miss\n", q.ID)
		}
	}
	fmt.Println("Box selects:")
	for _, q := range qf.Boxes {
		r, _ := e.BoxSelect(q)
		fmt.Printf("  %-18s %-9s %s\n", q.ID, r.Status, strings.Join(r.NodeIDs, ","))
	}
	return nil
}

func formatPoint(p editorpick.Vec3) string {
	return fmt.Sprintf("(%.6f,%.6f,%.6f)", p.X.Float64(), p.Y.Float64(), p.Z.Float64())
}
