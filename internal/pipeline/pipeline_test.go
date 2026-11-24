package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"spatialir/internal/checkpoint"
	"spatialir/internal/compare"
	"spatialir/internal/generator"
	"spatialir/internal/loader"
	"spatialir/internal/model"
)

func testParams() model.FrameParams {
	return model.FrameParams{
		FrameLength: 1024, FrameHop: 512, WindowLen: 768, ConvWindow: 64,
		RenderBlock: 512, InterpOrder: 1, BoundScale: 1, MaxWorkers: 3, BufferCap: 2048,
	}
}

func setup(t *testing.T, faults model.FaultSpec) (*loader.Dataset, model.FrameParams, string) {
	t.Helper()
	dir := t.TempDir()
	spec := generator.BuiltinSpecs()[0]
	spec.Faults = faults
	m, _, _, err := generator.Generate(spec, dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = m
	ds, err := loader.Load(dir, spec.Name+"_manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return ds, testParams(), filepath.Join(dir, "cp.db")
}

func runThreePaths(t *testing.T, ds *loader.Dataset, params model.FrameParams, dbPath string,
	opts func(*Options)) (*Result, *Result, *Result) {
	t.Helper()
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(ds, params)
	o1 := Options{Dataset: ds, Params: params, Store: store, Mode: ModeRealtime}
	if opts != nil {
		opts(&o1)
	}
	r1, err := eng.Run(context.Background(), o1)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	// 模拟进程重启：新引擎、新连接，走 resume。
	store2, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	eng2 := NewEngine(ds, params)
	r2, err := eng2.Run(context.Background(), Options{
		Dataset: ds, Params: params, Store: store2, Mode: ModeResume})
	if err != nil {
		t.Fatal(err)
	}

	eng3 := NewEngine(ds, params)
	r3, err := eng3.Run(context.Background(), Options{
		Dataset: ds, Params: params, Store: store2, Mode: ModeReplay})
	if err != nil {
		t.Fatal(err)
	}
	store2.Close()
	return r1, r2, r3
}

func TestThreePathsSampleIdentical(t *testing.T) {
	ds, params, db := setup(t, model.FaultSpec{})
	r1, r2, r3 := runThreePaths(t, ds, params, db, nil)
	eng := NewEngine(ds, params)
	if d := compare.Compare(r1.Report, r2.Report, eng.Frames()); d != nil {
		t.Fatalf("realtime vs resume diverge: %+v", d)
	}
	if d := compare.Compare(r1.Report, r3.Report, eng.Frames()); d != nil {
		t.Fatalf("realtime vs replay diverge: %+v", d)
	}
	if r1.Report.Committed != r1.Report.Frames {
		t.Fatalf("not all frames committed: %d/%d", r1.Report.Committed, r1.Report.Frames)
	}
	for ch := range r1.Report.Rendered {
		if len(r1.Report.Rendered[ch]) != ds.Manifest.LengthSamples {
			t.Fatal("rendered length must strictly match original")
		}
		orig, rend := 0.0, 0.0
		for _, v := range r1.Report.Original[ch] {
			orig += v * v
		}
		for _, v := range r1.Report.Rendered[ch] {
			rend += v * v
		}
		if orig <= 0 || rend <= 0 {
			t.Fatalf("non-positive energy ch=%d orig=%f rend=%f", ch, orig, rend)
		}
	}
}

func TestCheckpointPauseAndResume(t *testing.T) {
	ds, params, db := setup(t, model.FaultSpec{})
	store, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(ds, params)
	half := len(eng.Frames()) / 2
	r1, err := eng.Run(context.Background(), Options{
		Dataset: ds, Params: params, Store: store, Mode: ModeRealtime, PauseAt: half})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Report.Committed != half {
		t.Fatalf("pause commit=%d want %d", r1.Report.Committed, half)
	}
	store.Close()

	store2, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	eng2 := NewEngine(ds, params)
	r2, err := eng2.Run(context.Background(), Options{
		Dataset: ds, Params: params, Store: store2, Mode: ModeResume})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Report.Committed != len(eng2.Frames()) {
		t.Fatalf("resume did not finish: %d/%d", r2.Report.Committed, len(eng2.Frames()))
	}
	// 提交序列必须帧序稳定。
	for i, idx := range r2.Report.CommitSeq {
		if idx != i {
			t.Fatalf("commit sequence not frame-order at %d: %d", i, idx)
		}
	}
	store2.Close()
}

func TestSnapshotInvalidatedOnHashOrVersion(t *testing.T) {
	ds, params, db := setup(t, model.FaultSpec{})
	store, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(ds, params)
	if _, err := eng.Run(context.Background(), Options{
		Dataset: ds, Params: params, Store: store, Mode: ModeRealtime}); err != nil {
		t.Fatal(err)
	}
	snaps, _ := store.LoadFrames()
	if len(snaps) != len(eng.Frames()) {
		t.Fatal("checkpoint missing frames")
	}
	// 输入变化：哈希不匹配，快照整体作废，禁止混用旧帧。
	ds2 := *ds
	manifest2 := *ds.Manifest
	manifest2.Seed++
	ds2.Manifest = &manifest2
	resumed, err := store.InitSnapshot(mustHash(t, &ds2), params, len(eng.Frames()))
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Fatal("changed input must invalidate snapshot")
	}
	snaps2, _ := store.LoadFrames()
	if len(snaps2) != 0 {
		t.Fatal("old frames must be discarded wholesale")
	}
	store.Close()
}

func TestFaultInjectionFrameIsolation(t *testing.T) {
	cases := []struct {
		name   string
		faults model.FaultSpec
		code   model.ErrorCode
	}{
		{"order", model.FaultSpec{OrderFrame: 1, OrderChannel: "L"}, model.CodeOrder},
		{"clip", model.FaultSpec{ClipFrame: 1, ClipChannel: "L", ClipGain: 5}, model.CodeClip},
		{"frame", model.FaultSpec{BadFrames: []int{2}}, model.CodeFrame},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds, params, db := setup(t, tc.faults)
			store, err := checkpoint.Open(db)
			if err != nil {
				t.Fatal(err)
			}
			eng := NewEngine(ds, params)
			res, err := eng.Run(context.Background(), Options{
				Dataset: ds, Params: params, Store: store, Mode: ModeRealtime})
			if err != nil {
				t.Fatalf("frame fault must be isolated, got abort: %v", err)
			}
			failed := 0
			for _, f := range res.Report.FrameReports {
				if f.Status == model.StatusFailed {
					failed++
					if f.FailCode != tc.code {
						t.Fatalf("code=%s want %s", f.FailCode, tc.code)
					}
				}
			}
			if failed != 1 {
				t.Fatalf("want exactly one failed frame, got %d", failed)
			}
			if res.Report.Committed != res.Report.Frames-1 {
				t.Fatal("other frames must remain unaffected and committed")
			}
			// 失败帧可重试：清除注入后回到 pending 并重新估计成功。
			var failIdx int
			for i, f := range res.Report.FrameReports {
				if f.Status == model.StatusFailed {
					failIdx = i
				}
			}
			if err := eng.RetryFailed(failIdx); err != nil {
				t.Fatal(err)
			}
			ds.Manifest.Faults = model.FaultSpec{}
			res2, err := eng.Run(context.Background(), Options{
				Dataset: ds, Params: params, Store: store, Mode: ModeResume})
			if err != nil {
				t.Fatal(err)
			}
			if res2.Report.Committed != res2.Report.Frames {
				t.Fatal("retried frame must eventually commit")
			}
			store.Close()
		})
	}
}

func TestBoundViolationReturnsEBound(t *testing.T) {
	ds, params, db := setup(t, model.FaultSpec{})
	// 收紧系数把截断能量上界压到实测之下，确定性触发 E_BOUND。
	params.BoundScale = 1e-9
	store, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(ds, params)
	res, err := eng.Run(context.Background(), Options{
		Dataset: ds, Params: params, Store: store, Mode: ModeRealtime})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range res.Report.FrameReports {
		if f.FailCode == model.CodeBound && f.FailReason != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected at least one E_BOUND frame with metric name")
	}
	store.Close()
}

func TestDivergenceDetection(t *testing.T) {
	ds, params, db := setup(t, model.FaultSpec{})
	r1, _, r3 := runThreePaths(t, ds, params, db, nil)
	eng := NewEngine(ds, params)
	// 人为篡改一个采样，比对器必须定位到首个不一致采样、帧与字段。
	r3.Report.Rendered[0][100] += 0.5
	d := compare.Compare(r1.Report, r3.Report, eng.Frames())
	if d == nil || d.Field != "rendered_sample" || d.SampleIndex != 100 || d.Channel != "L" {
		t.Fatalf("divergence not located: %+v", d)
	}
	if err := compare.AsError(d); model.AsAppError(err).Code != model.CodeDiverge {
		t.Fatalf("expect E_DIVERGE, got %v", err)
	}
}

func TestPoolBufferBlockingRecorded(t *testing.T) {
	ds, params, _ := setup(t, model.FaultSpec{})
	// 缓冲预算只够一帧，并行度高，必然出现阻塞排队事件且不丢帧。
	params.BufferCap = params.FrameLength
	params.MaxWorkers = 4
	db := filepath.Join(t.TempDir(), "cp.db")
	store, err := checkpoint.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(ds, params)
	res, err := eng.Run(context.Background(), Options{
		Dataset: ds, Params: params, Store: store, Mode: ModeRealtime})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PoolEvents) == 0 {
		t.Fatal("buffer exhaustion blocking must be recorded")
	}
	if res.Report.Committed != res.Report.Frames {
		t.Fatal("blocked tasks must not be dropped")
	}
	store.Close()
}

func mustHash(t *testing.T, ds *loader.Dataset) string {
	t.Helper()
	h, err := InputHash(ds.Manifest, ds.Signals, ds.Dry)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
