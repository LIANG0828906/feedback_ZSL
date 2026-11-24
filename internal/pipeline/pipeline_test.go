package pipeline

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"mcapture/internal/dsp"
	"mcapture/internal/session"
	"mcapture/internal/store"
)

func loadSess(t *testing.T, mutate func(*session.GenConfig)) *session.Session {
	t.Helper()
	dir := t.TempDir()
	cfg := session.DefaultGenConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	if _, _, err := session.Generate(dir, cfg); err != nil {
		t.Fatal(err)
	}
	sess, err := session.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func rateVals(k []session.Knot) []float64 {
	out := make([]float64, len(k))
	for i, x := range k {
		out[i] = x.Rate
	}
	return out
}

func TestDriftEstimateAccuracyAndConvergence(t *testing.T) {
	sess := loadSess(t, nil)
	m := sess.Manifest
	tpl := dsp.MarkerTemplate(m.MarkerShape, m.MarkerWidth)
	refMarks, err := detectAllMarkers(sess.Channels[0].Data, tpl, m.Margin, m.CorePoints)
	if err != nil || len(refMarks) < 10 {
		t.Fatalf("ref markers: %v n=%d", err, len(refMarks))
	}
	segLen := int64(m.SampleRate * m.SegmentSec)
	for _, ch := range sess.Channels[1:] {
		obs, err := detectChannelMarkers(ch.Data, tpl, m.Margin, refMarks, ch.Spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		p, err := EstimateDrift(obs, m.CorePoints, m.Margin, segLen)
		if err != nil {
			t.Fatalf("estimate %s: %v", ch.Spec.ID, err)
		}
		if p.MarkersUsed != len(obs) {
			t.Fatalf("markers used mismatch")
		}
		// 速率节点与真值误差应在 1e-4 量级内。
		for k, knot := range p.Knots {
			trueRate := ch.Spec.DriftBasis[k].Rate
			if math.Abs(knot.Rate-trueRate) > 1.5e-4 {
				t.Fatalf("%s knot %d rate=%.6f true=%.6f", ch.Spec.ID, k, knot.Rate, trueRate)
			}
		}
		if math.Abs(p.Phi0-ch.Spec.DriftPhi0) > 1.0 {
			t.Fatalf("%s phi0=%.4f true=%.4f", ch.Spec.ID, p.Phi0, ch.Spec.DriftPhi0)
		}
		// 置信区间必须为有限正数。
		if p.Phi0CI <= 0 || len(p.RateCI) != len(p.Knots) {
			t.Fatalf("bad confidence interval")
		}
	}

	// 收敛性：标记数增加，速率误差下降。用前 6 个 vs 全部 12 个标记对比。
	ch := sess.Channels[1]
	obs, _ := detectChannelMarkers(ch.Data, tpl, m.Margin, refMarks, ch.Spec.ID)
	measure := func(n int) float64 {
		p, err := EstimateDrift(obs[:n], m.CorePoints, m.Margin, segLen)
		if err != nil {
			t.Fatal(err)
		}
		var maxErr float64
		for k, knot := range p.Knots {
			if e := math.Abs(knot.Rate - ch.Spec.DriftBasis[k].Rate); e > maxErr {
				maxErr = e
			}
		}
		return maxErr
	}
	// 节点数为 5，至少需 6 个标记。
	lo := measure(7)
	hi := measure(len(obs))
	if !(hi <= lo) {
		t.Fatalf("drift rate error did not improve with more markers: %.6f -> %.6f", lo, hi)
	}
	t.Logf("convergence max rate err: %.6f (7 markers) -> %.6f (%d markers)", lo, hi, len(obs))
}

func TestDriftInsufficientMarkers(t *testing.T) {
	sess := loadSess(t, nil)
	m := sess.Manifest
	segLen := int64(m.SampleRate * m.SegmentSec)
	obs := []markerObs{{index: 0, coreRef: 100, arrival: 100}, {index: 1, coreRef: 200, arrival: 201}}
	_, err := EstimateDrift(obs, m.CorePoints, m.Margin, segLen)
	if session.CodeOf(err) != session.EDrift {
		t.Fatalf("want E_DRIFT, got %v", err)
	}
}

func TestParameterValidation(t *testing.T) {
	sess := loadSess(t, nil)
	cfg := DefaultConfig(sess.Manifest)
	cfg.SincOrder = 5.0
	if session.CodeOf(cfg.Validate()) != session.EPara {
		t.Fatal("want E_PARA for sinc order")
	}
	cfg = DefaultConfig(sess.Manifest)
	cfg.HalfWindow = 2
	if session.CodeOf(cfg.Validate()) != session.EPara {
		t.Fatal("want E_PARA for half window")
	}
	cfg = DefaultConfig(sess.Manifest)
	cfg.MaxDriftRate = 0.5
	if session.CodeOf(cfg.Validate()) != session.EPara {
		t.Fatal("want E_PARA for drift rate")
	}
}

func TestStateMachine(t *testing.T) {
	g := &Segment{ChannelID: "ch1", Index: 0, State: StatePending}
	if err := g.Transition(StateCommitted); session.CodeOf(err) != session.EState {
		t.Fatal("pending -> committed must be illegal")
	}
	if g.State != StatePending {
		t.Fatal("state must be preserved after illegal transition")
	}
	if err := g.Transition(StateAligned); err != nil {
		t.Fatal(err)
	}
	if err := g.Transition(StateCommitted); err != nil {
		t.Fatal(err)
	}
	if err := g.Transition(StateAligned); session.CodeOf(err) != session.EState {
		t.Fatal("committed is frozen, any transition illegal")
	}
	if !g.IsFrozen() {
		t.Fatal("segment should be frozen")
	}
}

func runOnce(t *testing.T, sess *session.Session, workers int, db string, mode Mode) *Result {
	t.Helper()
	cfg := DefaultConfig(sess.Manifest)
	res, st, err := RunPath(context.Background(), sess, cfg,
		filepath.Join(t.TempDir(), db), mode, workers)
	if err != nil {
		t.Fatalf("run %s: %v", mode, err)
	}
	t.Cleanup(func() { st.Close() })
	return res
}

func TestThreePathDeterminism(t *testing.T) {
	sess := loadSess(t, nil)
	cfg := DefaultConfig(sess.Manifest)
	dir := t.TempDir()

	full, st1, err := RunPath(context.Background(), sess, cfg, filepath.Join(dir, "full.db"), ModeFull, 3)
	if err != nil {
		t.Fatal(err)
	}
	st1.Close()
	rt, st2, err := RunPath(context.Background(), sess, cfg, filepath.Join(dir, "rt.db"), ModeRealtime, 1)
	if err != nil {
		t.Fatal(err)
	}
	st2.Close()
	resume, st3, err := RunPath(context.Background(), sess, cfg, filepath.Join(dir, "resume.db"), ModeResume, 3)
	if err != nil {
		t.Fatal(err)
	}
	st3.Close()

	if err := CompareResults(sess, cfg, full, rt); err != nil {
		t.Fatalf("full vs realtime: %v", err)
	}
	if err := CompareResults(sess, cfg, full, resume); err != nil {
		t.Fatalf("full vs resume: %v", err)
	}
	if err := CompareCommitSeq(full, rt); err != nil {
		t.Fatalf("full vs realtime commit seq: %v", err)
	}
	if err := CompareCommitSeq(full, resume); err != nil {
		t.Fatalf("full vs resume commit seq: %v", err)
	}
	if err := CompareResiduals(full, rt); err != nil {
		t.Fatalf("full vs realtime residuals: %v", err)
	}
	if err := CompareResiduals(full, resume); err != nil {
		t.Fatalf("full vs resume residuals: %v", err)
	}
	if len(full.CommitSeq) == 0 || !equalStrings(full.CommitSeq, rt.CommitSeq) ||
		!equalStrings(full.CommitSeq, resume.CommitSeq) {
		t.Fatal("commit sequences must be identical")
	}
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

func TestCheckpointResumeReuse(t *testing.T) {
	sess := loadSess(t, nil)
	cfg := DefaultConfig(sess.Manifest)
	dir := t.TempDir()
	db := filepath.Join(dir, "cp.db")

	first, st, err := RunPath(context.Background(), sess, cfg, db, ModeFull, 2)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// 重新打开并以 resume 模式运行：全部已提交分段应被复用。
	res, st2, err := RunPath(context.Background(), sess, cfg, db, ModeResume, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	reused := 0
	for _, ev := range res.Events {
		if ev.Kind == "restored" {
			reused++
		}
	}
	if reused == 0 {
		t.Fatal("expected checkpoint segments to be restored and reused")
	}
	if err := CompareResults(sess, cfg, first, res); err != nil {
		t.Fatalf("resumed output diverges: %v", err)
	}
	// 恢复模式只提交未持久化的新段，因此新提交序列更短，但已恢复段逐样本一致。
	if len(res.CommitSeq) >= len(first.CommitSeq) {
		t.Fatalf("resume should only commit newly computed segments, got %d vs %d",
			len(res.CommitSeq), len(first.CommitSeq))
	}
	t.Logf("restored segments=%d", reused)
}

func TestSnapshotInvalidation(t *testing.T) {
	sess := loadSess(t, nil)
	cfg := DefaultConfig(sess.Manifest)
	dir := t.TempDir()
	db := filepath.Join(dir, "cp.db")
	_, st, err := RunPath(context.Background(), sess, cfg, db, ModeFull, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 篡改参数版本，重启应整体作废。
	if err := st.PutMeta("param_version", "params-TAMPERED"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(sess, cfg, st2)
	if err != nil {
		t.Fatal(err)
	}
	eng.buildSegments()
	if err := eng.restore(); err != nil {
		t.Fatal(err)
	}
	invalidated := false
	for _, ev := range eng.Events() {
		if ev.Kind == "snapshot_invalidated" {
			invalidated = true
		}
	}
	if !invalidated {
		t.Fatal("version mismatch must invalidate the whole snapshot")
	}
	recs, _, _ := st2.LoadAll()
	if len(recs) != 0 {
		t.Fatal("snapshot must be fully reset, no partial reuse")
	}
	st2.Close()
}

func TestBudgetBlockingRecorded(t *testing.T) {
	sess := loadSess(t, nil)
	cfg := DefaultConfig(sess.Manifest)
	// 极小缓存预算，强制任务排队阻塞。
	cfg.ResampleBudget = int(cfg.SegmentSamples)
	cfg.BufferCap = int(cfg.SegmentSamples)
	dir := t.TempDir()
	res, st, err := RunPath(context.Background(), sess, cfg, filepath.Join(dir, "b.db"), ModeFull, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	blocked := false
	for _, ev := range res.Events {
		if ev.Kind == "budget_blocked" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("expected budget_blocked events under tight cache budget")
	}
}

func TestFaultChains(t *testing.T) {
	// E_MARK：标记缺失。
	sessMark := loadSess(t, func(c *session.GenConfig) { c.DropMarker = 3 })
	_, err := NewEngine(sessMark, DefaultConfig(sessMark.Manifest), nil)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(sessMark, DefaultConfig(sessMark.Manifest), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = eng.Run(context.Background(), ModeFull, 1)
	if session.CodeOf(err) != session.EMark {
		t.Fatalf("want E_MARK, got %v", err)
	}

	// E_CLIP：增益溢出。
	sessClip := loadSess(t, func(c *session.GenConfig) { c.ClipGain = 4.0 })
	err = Preflight(sessClip)
	if session.CodeOf(err) != session.EClip {
		t.Fatalf("want E_CLIP, got %v", err)
	}
}

func TestEditDriftFrozenRejection(t *testing.T) {
	sess := loadSess(t, nil)
	cfg := DefaultConfig(sess.Manifest)
	dir := t.TempDir()
	res, st, err := RunPath(context.Background(), sess, cfg, filepath.Join(dir, "e.db"), ModeFull, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if res == nil {
		t.Fatal("nil result")
	}
	// 全部段已提交，编辑任意节点都应被拒绝（E_STATE）。
	eng, _ := NewEngine(sess, cfg, nil)
	_ = eng.estimate()
	eng.buildSegments()
	for _, list := range eng.segments {
		for _, seg := range list {
			seg.State = StateCommitted
		}
	}
	if err := eng.EditDrift("ch1", 1, 0.0001); session.CodeOf(err) != session.EState {
		t.Fatalf("want E_STATE editing frozen segment, got %v", err)
	}
}
