package chaosdiag_test

import (
	"math/rand"
	"reflect"
	"testing"

	"chaosdiag/engine"
	"chaosdiag/gen"
	"chaosdiag/model"
	"chaosdiag/reconcile"
)

func TestSchedulerAndRepeatedSimulationAreOrderIndependent(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	want := engine.Run(bundle.Canonical)

	for iteration := 0; iteration < 50; iteration++ {
		p, err := cloneProject(bundle.Canonical)
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewSource(int64(iteration + 1)))
		rng.Shuffle(len(p.Instructions), func(i, j int) {
			p.Instructions[i], p.Instructions[j] = p.Instructions[j], p.Instructions[i]
		})
		got := engine.Run(p)
		if !reflect.DeepEqual(got.Schedules, want.Schedules) {
			t.Fatalf("iteration %d schedules changed after declaration slice shuffle", iteration)
		}
		if !reflect.DeepEqual(got.Results, want.Results) {
			t.Fatalf("iteration %d results changed after declaration slice shuffle", iteration)
		}
	}

	first := engine.Run(bundle.Canonical)
	firstVerdicts := reconcile.ReconcileAll(bundle.Canonical, first.Results)
	for i := 0; i < 20; i++ {
		next := engine.Run(bundle.Canonical)
		if !reflect.DeepEqual(next.Results, first.Results) {
			t.Fatal("repeated replay drifted")
		}
		nextVerdicts := reconcile.ReconcileAll(bundle.Canonical, next.Results)
		if !reflect.DeepEqual(nextVerdicts, firstVerdicts) {
			t.Fatal("repeated reconciliation drifted")
		}
	}
}

func cloneProject(p *model.Project) (*model.Project, error) {
	return model.CloneProject(p)
}
