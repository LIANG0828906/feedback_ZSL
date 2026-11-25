package acceptance_test

import (
	"testing"

	"coldkernel/internal/incremental"
	"coldkernel/internal/model"
	"coldkernel/internal/oracle"
	"coldkernel/internal/sample"
	"coldkernel/internal/testutil"
)

func TestIncrementalLimitEditMatchesFullRecompute(t *testing.T) {
	tr := loadTriple(t)

	inc := incremental.NewEngine(sample.CloneProject(tr.ProjectV1))
	base, err := inc.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.AssertRunsEqual("baseline", oracle.Simulate(tr.ProjectV1), base); err != nil {
		t.Fatal(err)
	}

	const fnID = "fn-b"
	fromTick := int64(12)
	inc.ApplyLimit(fnID, tr.ProjectV1.Functions[1].ConcurrencyLimit+2)
	edits := []incremental.Edit{{
		Kind: incremental.EditLimit, FunctionID: fnID, FromTick: fromTick,
	}}
	got, stats, err := inc.Run(edits)
	if err != nil {
		t.Fatal(err)
	}

	// Full recomputation reference using the same edited declaration.
	edited := sample.CloneProject(tr.ProjectV1)
	edited.Functions[1].ConcurrencyLimit += 2
	want := oracle.Simulate(edited)
	if err := testutil.AssertRunsEqual("incremental-limit", want, got); err != nil {
		t.Fatal(err)
	}

	// Only fn-b's cells from tick 12 may be recomputed; everything else
	// must be served from cache.
	fnCount := len(tr.ProjectV1.Functions)
	totalCells := fnCount * int(tr.ProjectV1.HorizonTicks)
	expectedRecomputed := int(tr.ProjectV1.HorizonTicks - fromTick)
	if stats.Recomputed != expectedRecomputed {
		t.Fatalf("recomputed=%d want %d", stats.Recomputed, expectedRecomputed)
	}
	if stats.Reused != totalCells-expectedRecomputed {
		t.Fatalf("reused=%d want %d", stats.Reused, totalCells-expectedRecomputed)
	}
}

func TestIncrementalClosureIsExplicit(t *testing.T) {
	tr := loadTriple(t)
	inc := incremental.NewEngine(sample.CloneProject(tr.ProjectV1))
	if _, err := inc.Baseline(); err != nil {
		t.Fatal(err)
	}
	edits := []incremental.Edit{
		{Kind: incremental.EditWarmPool, FunctionID: "fn-c", FromTick: 20},
		{Kind: incremental.EditReserved, FunctionID: "fn-a", FromTick: 8},
	}
	closure := incremental.Closure(tr.ProjectV1, edits)
	// Warm-pool membership is tick-0 initial state, so the fn-c warm-pool
	// edit closes from tick 0; the fn-a reserved edit starts at tick 8.
	if !closure["fn-c/0"] || !closure["fn-c/20"] {
		t.Fatal("fn-c warm-pool closure must start at tick 0")
	}
	if !closure["fn-a/8"] || closure["fn-a/7"] {
		t.Fatal("fn-a closure boundary wrong")
	}
	if closure["fn-b/0"] || closure["fn-d/30"] {
		t.Fatal("unaffected function must not enter closure")
	}
}

func TestIncrementalWarmPoolAndBudget(t *testing.T) {
	tr := loadTriple(t)
	cases := []struct {
		name string
		edit func(e *incremental.Engine)
		ref  func(p *model.Project)
		kind incremental.Kind
	}{
		{
			name: "warmpool",
			edit: func(e *incremental.Engine) { e.ApplyWarmPool("fn-a", 3) },
			ref:  func(p *model.Project) { p.Functions[0].WarmPoolSize = 3 },
			kind: incremental.EditWarmPool,
		},
		{
			name: "budget",
			edit: func(e *incremental.Engine) { e.ApplyBudget("fn-a", 2000) },
			ref:  func(p *model.Project) {
				fn := p.Functions[0]
				fn.ColdBudgetRatio = 2000
				fn.ColdBudgetAbsolute = (2000 * int64(len(p.Requests))) / 10000
			},
			kind: incremental.EditBudget,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc := incremental.NewEngine(sample.CloneProject(tr.ProjectV1))
			if _, err := inc.Baseline(); err != nil {
				t.Fatal(err)
			}
			tc.edit(inc)
			got, _, err := inc.Run([]incremental.Edit{{
				Kind: tc.kind, FunctionID: "fn-a", FromTick: 5,
			}})
			if err != nil {
				t.Fatal(err)
			}
			edited := sample.CloneProject(tr.ProjectV1)
			tc.ref(edited)
			want := oracle.Simulate(edited)
			if err := testutil.AssertRunsEqual(tc.name, want, got); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIncrementalRequestAddDelete(t *testing.T) {
	tr := loadTriple(t)
	inc := incremental.NewEngine(sample.CloneProject(tr.ProjectV1))
	if _, err := inc.Baseline(); err != nil {
		t.Fatal(err)
	}
	newReq := &model.Request{
		ID: "req-extra", FunctionID: "fn-c", ArriveTick: 10, Slots: 1,
		Priority: model.PriorityShared, Tenant: "tenant-a", HoldTicks: 3,
	}
	inc.AddRequest(newReq)
	got, _, err := inc.Run([]incremental.Edit{{
		Kind: incremental.EditRequestAdd, FunctionID: "fn-c", FromTick: 10,
	}})
	if err != nil {
		t.Fatal(err)
	}
	edited := sample.CloneProject(tr.ProjectV1)
	edited.Requests = append(edited.Requests, newReq)
	sortReq(edited)
	want := oracle.Simulate(edited)
	if err := testutil.AssertRunsEqual("request-add", want, got); err != nil {
		t.Fatal(err)
	}
}

func sortReq(p *model.Project) {
	// Keep arrival ordering consistent with the loader's stable order.
	for i := 1; i < len(p.Requests); i++ {
		for j := i; j > 0 && p.Requests[j].ArriveTick < p.Requests[j-1].ArriveTick; j-- {
			p.Requests[j], p.Requests[j-1] = p.Requests[j-1], p.Requests[j]
		}
	}
}
