package morph

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestDiagnosticHandlers(t *testing.T) {
	opts := DefaultGenerateOptions()
	opts.ID, opts.Paths, opts.Keyframes, opts.Segments, opts.Samples = "ui", 2, 2, 4, 3
	scene, truth, err := GenerateScene(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfigForScene(scene)
	cfg.Checkpoint = t.TempDir()
	cfg.Adaptive.ForceFine = true
	eng, err := NewEngine(scene, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Run(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	s := &uiServer{scene: scene, truth: truth, cfg: cfg, engine: eng, workDir: t.TempDir()}
	rr := httptest.NewRecorder()
	s.handleState(rr, httptest.NewRequest("GET", "/api/state", nil))
	if rr.Code != 200 {
		t.Fatalf("state status=%d body=%s", rr.Code, rr.Body.String())
	}
	var st uiState
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Progress[0] != st.Progress[1] || len(st.Timeline.Samples) != len(scene.SampleTimes) {
		t.Fatalf("state=%+v", st)
	}

	rr = httptest.NewRecorder()
	s.handleFrame(rr, httptest.NewRequest("GET", "/api/frame?index=0", nil))
	if rr.Code != 200 {
		t.Fatalf("frame status=%d body=%s", rr.Code, rr.Body.String())
	}
	var fr uiFrame
	if err := json.Unmarshal(rr.Body.Bytes(), &fr); err != nil {
		t.Fatal(err)
	}
	if fr.Composite == "" || len(fr.Polylines) != len(scene.Paths) {
		t.Fatalf("frame=%+v", fr)
	}

	rr = httptest.NewRecorder()
	s.handleControl(rr, httptest.NewRequest("POST", "/api/control?action=pause", nil))
	if rr.Code != 200 || !eng.Paused() {
		t.Fatalf("pause status=%d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.handleControl(rr, httptest.NewRequest("POST", "/api/control?action=resume", nil))
	if rr.Code != 200 || eng.Paused() {
		t.Fatalf("resume status=%d", rr.Code)
	}
}
