package acousticfield

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestValidationErrorsCarryObjectAndPath(t *testing.T) {
	sample, err := Generate(606)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sample.Level)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "id")
	missing, _ := json.Marshal(doc)
	_, err = LoadLevelJSON(missing)
	if !IsCode(err, CodeSchema) {
		t.Fatalf("expected E_SCHEMA, got %v", err)
	}
	var fe *FieldError
	if !errors.As(err, &fe) || fe.ObjectID == "" || fe.FieldPath == "" {
		t.Fatalf("missing object/field context: %v", err)
	}
	raw, _ = json.Marshal(sample.Level)
	doc = nil
	_ = json.Unmarshal(raw, &doc)
	chunks := doc["chunks"].([]any)
	bounds := chunks[0].(map[string]any)["bounds"].(map[string]any)
	delete(bounds, "min_x")
	missing, _ = json.Marshal(doc)
	_, err = LoadLevelJSON(missing)
	if !IsCode(err, CodeSchema) {
		t.Fatalf("expected nested E_SCHEMA, got %v", err)
	}

	bad := CloneLevel(sample.Level)
	c, _ := chunkByID(bad, 2)
	c.Absorption[0].Coefficient = 0
	err = ValidateLevel(bad)
	if !IsCode(err, CodeValue) {
		t.Fatalf("expected E_VAL, got %v", err)
	}
	if !errors.As(err, &fe) || fe.ObjectID != "chunk-2" || fe.FieldPath == "" || fe.Measured == nil {
		t.Fatalf("bad numeric error context: %v", err)
	}

	bad = CloneLevel(sample.Level)
	c, _ = chunkByID(bad, 3)
	c.Reflectors[0].Normal = Vec{}
	if err := ValidateLevel(bad); !IsCode(err, CodeValue) {
		t.Fatalf("expected normal E_VAL, got %v", err)
	}

	bad = CloneLevel(sample.Level)
	bad.Sources[0].Position = Point{X: -1, Y: -1}
	if err := ValidateLevel(bad); !IsCode(err, CodeValue) {
		t.Fatalf("expected source position E_VAL, got %v", err)
	}

	bad = CloneLevel(sample.Level)
	bad.Chunks[1].Bounds = bad.Chunks[0].Bounds
	if err := ValidateLevel(bad); !IsCode(err, CodeValue) {
		t.Fatalf("expected tiling E_VAL, got %v", err)
	}
}

func TestStateMachineRejectsIllegalTransitionWithoutMutation(t *testing.T) {
	m := NewStateMachine([]int{0, 1, 2})
	if err := m.TransitionChunk(0, ChunkCommitted); !IsCode(err, CodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if m.ChunkState(0) != ChunkPending {
		t.Fatal("illegal transition changed chunk state")
	}
	if c, r := m.Progress(); c != 0 || r != 0 {
		t.Fatalf("illegal transition changed progress %d/%d", c, r)
	}
	if err := m.TransitionChunk(0, ChunkFiltering); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionChunk(0, ChunkTransferring); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionChunk(0, ChunkCommitted); err != nil {
		t.Fatal(err)
	}
	if c, _ := m.Progress(); c != 1 {
		t.Fatalf("committed progress=%d", c)
	}
	if err := m.InvalidateChunk(0); err != nil {
		t.Fatal(err)
	}
	if c, _ := m.Progress(); c != 0 {
		t.Fatalf("invalidation progress=%d", c)
	}

	if err := m.RegisterRound(1); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionRound(1, RoundCommitted); !IsCode(err, CodeState) {
		t.Fatalf("expected illegal round transition, got %v", err)
	}
	if m.RoundState(1) != RoundPending {
		t.Fatal("illegal round transition changed state")
	}
	if err := m.TransitionRound(1, RoundRebuilding); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionRound(1, RoundCommitted); err != nil {
		t.Fatal(err)
	}
	if _, r := m.Progress(); r != 1 {
		t.Fatalf("committed rounds=%d", r)
	}
}

func TestFailedChunkRetryTransition(t *testing.T) {
	m := NewStateMachine([]int{9})
	if err := m.TransitionChunk(9, ChunkFiltering); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionChunk(9, ChunkFailed); err != nil {
		t.Fatal(err)
	}
	if err := m.RetryChunk(9); err != nil {
		t.Fatal(err)
	}
	if m.ChunkState(9) != ChunkFiltering {
		t.Fatalf("retry state=%s", m.ChunkState(9))
	}
}
