package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"replaykernel/internal/engine"
	"replaykernel/internal/gen"
	"replaykernel/internal/model"
	"replaykernel/internal/store"
)

func TestVerifyDetectsMismatch(t *testing.T) {
	dir := t.TempDir()
	_, err := gen.Generate(gen.Spec{
		OutDir: dir, Seed: 7,
		MatchSpecs: []gen.MatchSpec{{MatchID: "v1", Turns: 80, Units: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "replays", "v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	truthRaw, err := os.ReadFile(filepath.Join(dir, "truth", "v1.truth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var truth model.MatchTruth
	if err := json.Unmarshal(truthRaw, &truth); err != nil {
		t.Fatal(err)
	}

	replay, err := engine.LoadReplay(raw)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "v1.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := engine.NewRunner(raw, engine.HashTruth(&truth), model.DefaultParams())
	if _, err := runner.RunLive(replay, st); err != nil {
		t.Fatal(err)
	}
	st.Close()

	rep, err := Verify(dbPath, &truth)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Checked != 80 {
		t.Fatalf("expected full pass, got %+v", rep)
	}

	// 篡改真值，复核必须只读 DB 并定位首个违规回合与字段。
	truth.Turns[9].Units[0].HP += 555
	rep, err = Verify(dbPath, &truth)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.First == nil {
		t.Fatal("expected violation finding")
	}
	if rep.First.TurnNo != 10 || rep.First.Field != "hp_milli" {
		t.Fatalf("finding wrong: %+v", rep.First)
	}
}
