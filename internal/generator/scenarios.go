package generator

import (
	"mvccworkbench/internal/domain"
)

type Batch struct {
	Project *domain.Project
	Truth   *domain.Truth
}

type Suite struct {
	Seed    int64             `json:"seed"`
	Batches map[string]*Batch `json:"batches"`
	Order   []string          `json:"order"`
}

func DefaultSuite(seed int64) (*Suite, error) {
	if seed == 0 {
		seed = 424242
	}
	suite := &Suite{Seed: seed, Batches: map[string]*Batch{}}

	visibility, truth, err := visibilityBatch(seed)
	if err != nil {
		return nil, err
	}
	suite.add("B1-visibility", visibility, truth)

	conflict, truth, err := conflictGCBatch(seed + 17)
	if err != nil {
		return nil, err
	}
	suite.add("B2-conflict-gc", conflict, truth)

	tie, truth, err := tieOrderBatch(seed + 31)
	if err != nil {
		return nil, err
	}
	suite.add("B3-tie-order", tie, truth)
	return suite, nil
}

func (s *Suite) add(id string, project *domain.Project, truth *domain.Truth) {
	s.Batches[id] = &Batch{Project: project, Truth: truth}
	s.Order = append(s.Order, id)
}

func visibilityBatch(seed int64) (*domain.Project, *domain.Truth, error) {
	b := NewBuilder("B1-visibility", seed, domain.TieGlobalTxnID, domain.ConflictFirstCommitterWins)
	b.AddItem("alpha", "primary visibility chain").
		AddItem("beta", "parallel diagnostic chain")

	t1 := b.BeginTxn(10, domain.ReadCommitted)
	b.Write(t1, "alpha", 20)
	b.Write(t1, "beta", 20)
	b.Commit(t1, 30)

	t2 := b.BeginTxn(40, domain.ReadCommitted)
	b.Read(t2, "alpha", 50)

	t3 := b.BeginTxn(60, domain.RepeatableRead)
	b.Read(t3, "alpha", 70)

	t4 := b.BeginTxn(80, domain.ReadCommitted)
	b.Write(t4, "alpha", 90)
	b.Commit(t4, 100)

	b.Read(t3, "alpha", 110)
	b.Read(t2, "alpha", 120)

	t5 := b.BeginTxn(130, domain.ReadCommitted)
	b.Write(t5, "alpha", 140)
	b.Rollback(t5, 150)

	t7 := b.BeginTxn(160, domain.ReadCommitted)
	b.Read(t7, "alpha", 170)

	t6 := b.BeginTxn(180, domain.ReadCommitted)
	b.Write(t6, "alpha", 190)

	t8 := b.BeginTxn(200, domain.ReadCommitted)
	b.Read(t8, "alpha", 210)

	b.GC(220)

	b.Commit(t2, 230)
	b.Commit(t7, 240)
	b.Commit(t8, 250)
	b.GC(260)

	b.Commit(t3, 270)
	b.GC(280)

	return b.Build()
}

func conflictGCBatch(seed int64) (*domain.Project, *domain.Truth, error) {
	b := NewBuilder("B2-conflict-gc", seed, domain.TieGlobalTxnID, domain.ConflictFirstCommitterWins)
	b.AddItem("counter", "write-write conflict item").
		AddItem("note", "snapshot protected item")

	t1 := b.BeginTxn(10, domain.ReadCommitted)
	b.Write(t1, "counter", 20)
	b.Write(t1, "note", 20)
	b.Commit(t1, 30)

	t2 := b.BeginTxn(40, domain.RepeatableRead)
	b.Read(t2, "counter", 50)

	t3 := b.BeginTxn(60, domain.ReadCommitted)
	b.Write(t3, "counter", 70)

	t4 := b.BeginTxn(80, domain.ReadCommitted)
	b.Write(t4, "counter", 90)

	t5 := b.BeginTxn(90, domain.ReadCommitted)
	b.Read(t5, "counter", 100)

	b.Commit(t3, 110)
	b.Rollback(t4, 120)

	b.Read(t2, "counter", 130)
	b.Read(t5, "counter", 140)

	b.GC(150)
	b.Commit(t5, 160)
	b.GC(170)

	b.Rollback(t2, 180)
	b.GC(190)

	return b.Build()
}

func tieOrderBatch(seed int64) (*domain.Project, *domain.Truth, error) {
	b := NewBuilder("B3-tie-order", seed, domain.TieGlobalTxnID, domain.ConflictFirstCommitterWins)
	b.AddItem("slot", "tie window item").
		AddItem("ledger", "independent tie commit item")

	t1 := b.BeginTxn(10, domain.ReadCommitted)
	b.Write(t1, "slot", 20)
	b.Commit(t1, 30)

	t2 := b.BeginTxn(40, domain.ReadCommitted)
	b.Write(t2, "ledger", 50)
	b.Commit(t2, 60)

	t5 := b.BeginTxn(80, domain.RepeatableRead)
	b.Read(t5, "slot", 90)

	t3 := b.BeginTxn(100, domain.ReadCommitted)
	b.Write(t3, "slot", 110)

	t4 := b.BeginTxn(120, domain.ReadCommitted)
	b.Write(t4, "ledger", 130)

	// Both commit at the same logical millisecond. GLOBAL_TXN_ID_ASC gives
	// t3 then t4 a unique adjacent commit order.
	b.Commit(t3, 200)
	b.Commit(t4, 200)

	b.Read(t5, "slot", 210)

	t6 := b.BeginTxn(210, domain.ReadCommitted)
	b.Read(t6, "slot", 220)
	b.Read(t6, "ledger", 230)

	b.GC(240)
	b.Commit(t6, 250)
	b.Commit(t5, 260)
	b.GC(270)

	return b.Build()
}
