package panel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"retargetlab/internal/engine"
	"retargetlab/internal/fsm"
	"retargetlab/internal/generator"
	"retargetlab/internal/panel"
	"retargetlab/internal/solver"
)

func TestPanelStateMatchesEngineState(t *testing.T) {
	ctx := context.Background()
	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := ws.TargetByName("reference")
	truth, _ := ws.TruthByTarget("reference")
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), MaxWorkers: 4, WindowSize: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SolveRange(ctx, 0, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetScale("gesture", 1.05); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(panel.New(e, &truth).Handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var snap engine.StateSnapshot
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	want := e.StateSnapshot()
	if snap.CommittedFrames != want.CommittedFrames || snap.CacheHits != want.CacheHits {
		t.Fatalf("panel counters differ: got %d/%d want %d/%d", snap.CommittedFrames, snap.CacheHits, want.CommittedFrames, want.CacheHits)
	}
	if len(snap.Invalidations) != 8 {
		t.Fatalf("panel invalidations = %d, want 8", len(snap.Invalidations))
	}
	flagged := 0
	for _, f := range snap.Frames {
		if f.Invalidated {
			flagged++
		}
	}
	if flagged != 8 {
		t.Fatalf("state matrix invalid markers = %d, want 8", flagged)
	}
	if snap.CacheHits != 8 {
		t.Fatalf("cache hit count = %d, want 8", snap.CacheHits)
	}

	body, _ := json.Marshal(map[string]any{"key": "gesture", "value": 1.07})
	res, err = http.Post(ts.URL+"/api/scale", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("scale edit status = %d", res.StatusCode)
	}

	res, err = http.Post(ts.URL+"/api/solve", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("solve status = %d", res.StatusCode)
	}
	if committed, total := e.Progress(); committed != total {
		t.Fatalf("engine progress after solve = %d/%d", committed, total)
	}
}

func TestPanelServesDiagnosticSections(t *testing.T) {
	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := ws.TargetByName("reference")
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(panel.New(e, nil).Handler())
	defer ts.Close()
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(res.Body)
	for _, needle := range []string{"帧窗口状态矩阵", "骨架层级", "关节映射关系", "约束违背曲线", "足部接触与根轨迹", "依赖失效传播"} {
		if !bytes.Contains(buf.Bytes(), []byte(needle)) {
			t.Fatalf("panel is missing section %q", needle)
		}
	}
}

func TestPanelSolveRetryFlow(t *testing.T) {
	ws, err := generator.Generate(generator.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	target, _ := ws.TargetByName("reference")
	var fail atomic.Bool
	fail.Store(true)
	e, err := engine.New(engine.Config{
		Source: ws.Source, Clip: ws.Clip, Target: target, Mapping: ws.Mapping,
		Params: solver.DefaultParams(), MaxWorkers: 2, WindowSize: 4,
		FrameFault: func(frame int) error {
			if frame == 5 && fail.Load() {
				return errors.New("panel injected fault")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(panel.New(e, nil).Handler())
	defer ts.Close()

	res, err := http.Post(ts.URL+"/api/solve", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if state, _ := e.State(5); state != fsm.Failed {
		t.Fatalf("frame 5 state after API solve = %s", state)
	}
	fail.Store(false)
	res, err = http.Post(ts.URL+"/api/retry", "application/json", bytes.NewReader([]byte(`{"frame":5}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d", res.StatusCode)
	}
	if state, _ := e.State(5); state != fsm.Committed {
		t.Fatalf("frame 5 state after retry = %s", state)
	}
}
