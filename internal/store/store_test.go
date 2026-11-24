package store

import (
	"path/filepath"
	"testing"

	"replaykernel/internal/model"
)

func TestCommitOrderAndHashChain(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	meta := MetaRecord{
		MatchID: "m", ReplayHash: "rh", TruthHash: "th",
		ParamVersion: model.ParamVersion, AlgoVersion: model.AlgoVersion,
		Seed: 1, WindowSize: 1, Algorithm: model.AlgoFine,
	}
	decision := model.Decision{MatchID: "m", Algorithm: model.AlgoFine, WindowSize: 1}
	if err := st.InitMatch(meta, decision); err != nil {
		t.Fatal(err)
	}

	report := func(no int) *model.WindowReport {
		return &model.WindowReport{
			MatchID: "m", WindowNo: no, FromTurn: no, ToTurn: no + 1,
			Status: model.WindowRunning,
			Snapshots: []model.TurnState{{
				TurnNo: no + 1, Winner: -1,
				Units: []model.UnitState{{
					UnitID: "u1", HP: 1000, MP: 2000, Alive: true,
					Cooldowns: map[string]int{"s": 0},
				}},
			}},
		}
	}
	chain, err := st.CommitWindow(meta, report(0), RootChain)
	if err != nil {
		t.Fatal(err)
	}

	// 乱序提交必须被拒绝。
	if _, err := st.CommitWindow(meta, report(2), chain); err == nil {
		t.Fatal("out-of-order commit must fail")
	}

	if _, err := st.CommitWindow(meta, report(1), chain); err != nil {
		t.Fatal(err)
	}
	windows, err := st.LoadWindows("m")
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].ChainHash == "" || windows[1].ChainHash == windows[0].ChainHash {
		t.Fatalf("hash chain wrong: %+v", windows)
	}
	snaps, err := st.LoadSnapshots("m")
	if err != nil || len(snaps) != 2 || snaps[0].Units[0].HP != 1000 {
		t.Fatalf("snapshots wrong: %v %+v", err, snaps)
	}
}

func TestInvalidateClearsAll(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	meta := MetaRecord{MatchID: "m", ParamVersion: model.ParamVersion,
		AlgoVersion: model.AlgoVersion, Algorithm: model.AlgoFine}
	if err := st.InitMatch(meta, model.Decision{MatchID: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Invalidate("m"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadMeta("m"); err == nil {
		t.Fatal("meta must be removed after invalidation")
	}
	if wins, err := st.CommittedWindows("m"); err != nil || len(wins) != 0 {
		t.Fatalf("windows must be empty: %v %v", wins, err)
	}
}
