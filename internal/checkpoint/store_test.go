package checkpoint

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"iconbench/internal/model"
)

func TestStateMachineIllegalTransitionKeepsState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p := model.DefaultParams()
	if err := s.InitImage("img", 1, 64, 64, 64, p); err != nil {
		t.Fatal(err)
	}
	err = s.Transition(ctx, "img", 0, LayerGradient, StatePending, StateCommitted)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	state, err := s.GetState("img", 0, LayerGradient)
	if err != nil || state != StatePending {
		t.Fatalf("state should remain pending, got %q err=%v", state, err)
	}

	if err := s.Transition(ctx, "img", 0, LayerGradient, StatePending, StateProcessing); err != nil {
		t.Fatal(err)
	}
	if err := s.FailLayer(ctx, "img", 0, LayerGradient, "injected"); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "img", 0, LayerGradient, StateFailed, StatePending); err != nil {
		t.Fatal(err)
	}
	state, _ = s.GetState("img", 0, LayerGradient)
	if state != StatePending {
		t.Fatalf("retry should return pending, got %s", state)
	}
}

func TestSnapshotHashMismatchDetected(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p := model.DefaultParams()
	if err := s.InitImage("img", 1, 64, 64, 64, p); err != nil {
		t.Fatal(err)
	}
	if err := s.Transition(ctx, "img", 0, LayerMarkers, StatePending, StateProcessing); err != nil {
		t.Fatal(err)
	}
	rec := LayerRecord{
		ImageID: "img", BlockIndex: 0, BlockX: 0, BlockY: 0, Layer: LayerMarkers,
		ArtifactJSON: `{"ok":true}`, ContentHash: "deliberately-wrong",
		ParamVersion: p.ParamVersion, ParamHash: p.CanonicalHash(),
		AlgorithmVersion: model.AlgorithmVersion, SchemaVersion: model.SchemaVersion,
	}
	if err := s.CommitLayer(ctx, rec); err != nil {
		t.Fatal(err)
	}
	v, err := s.ValidateSnapshot(ctx, "img", p)
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || v.Field != "content_hash" || v.Layer != LayerMarkers {
		t.Fatalf("expected content_hash violation, got %+v", v)
	}
}
