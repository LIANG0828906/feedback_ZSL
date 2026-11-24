package engine_test

import (
	"context"
	"testing"

	"economycore/engine"
	"economycore/sample"
)

func TestRecipeAndWorkerEditsMatchFullSettlement(t *testing.T) {
	cases := []struct {
		name            string
		edit            func(*engine.Engine) (*engine.EditReport, error)
		wantInvalidated string
	}{
		{
			name: "recipe",
			edit: func(e *engine.Engine) (*engine.EditReport, error) {
				return e.EditRecipe("sawmill", "efficient")
			},
			wantInvalidated: "workshop",
		},
		{
			name: "workers",
			edit: func(e *engine.Engine) (*engine.EditReport, error) {
				return e.EditWorkers("workshop", 0)
			},
			wantInvalidated: "workshop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := engine.New(sample.Scenario(42), engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eng.RunFull(context.Background()); err != nil {
				t.Fatal(err)
			}
			report, err := tc.edit(eng)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, invalidated := range report.Invalidated {
				if invalidated.NodeID == tc.wantInvalidated {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s edit did not invalidate %s: %+v", tc.name, tc.wantInvalidated, report)
			}
			incremental, err := eng.RunIncremental(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := engine.New(eng.Model(), engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			full, err := fresh.RunFull(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.CompareRuns(incremental, full); err != nil {
				t.Fatalf("%s incremental differs from full: %v", tc.name, err)
			}
		})
	}
}
