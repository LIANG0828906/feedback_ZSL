package registry_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

func incrementalFixture(t *testing.T) (*registry.Project, *registry.RunResult, []registry.Snapshot) {
	t.Helper()
	bundle, err := samplegen.BuildBundle(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	document, err := registry.LoadDocument(bundle.Files[registry.SchemaV1])
	if err != nil {
		t.Fatal(err)
	}
	run, snapshots, err := registry.ComputeProject(document.Project, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	return document.Project, run, snapshots
}

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	base, previous, snapshots := incrementalFixture(t)
	beforeHash, _ := base.ModelHash()

	eventIndex := -1
	for index, event := range base.Events {
		if event.Kind == registry.EventProbeResult && event.AtMS == 100 {
			eventIndex = index
			break
		}
	}
	if eventIndex < 0 {
		t.Fatal("probe event not found")
	}

	tests := []struct {
		name     string
		edit     registry.Edit
		from     int64
		extended int
		check    func(*testing.T, *registry.IncrementalResult)
	}{
		{
			name: "probe period",
			edit: registry.Edit{Kind: registry.EditProbePeriod, InstanceID: "api-a", EffectiveTick: 10, Value: 200},
			from: 10,
		},
		{
			name: "probe timeout",
			edit: registry.Edit{Kind: registry.EditProbeTimeout, InstanceID: "api-b", EffectiveTick: 8, Value: 500},
			from: 8,
		},
		{
			name: "renewal period",
			edit: registry.Edit{Kind: registry.EditRenewalPeriod, InstanceID: "api-a", EffectiveTick: 11, Value: 300},
			from: 11,
		},
		{
			name: "health thresholds",
			edit: registry.Edit{Kind: registry.EditHealthThreshold, InstanceID: "api-b", EffectiveTick: 10, Value: 1, Value2: 1},
			from: 10,
		},
		{
			name: "weight scale preserves candidate order",
			edit: registry.Edit{Kind: registry.EditWeightScale, InstanceID: "api-a", EffectiveTick: 10, Scale: registry.ScalePer10K},
			from: 10,
			check: func(t *testing.T, result *registry.IncrementalResult) {
				t.Helper()
				before := indexes(previous.Steps[10].Catalogs[0].Candidates)
				after := indexes(result.Run.Steps[10].Catalogs[0].Candidates)
				if !equalStrings(before, after) {
					t.Fatalf("candidate order changed: %v -> %v", before, after)
				}
			},
		},
		{
			name: "append event",
			edit: registry.Edit{
				Kind:          registry.EditAppendEvent,
				EffectiveTick: 12,
				Event: &registry.Event{
					AtMS:       1200,
					Kind:       registry.EventHeartbeat,
					InstanceID: "api-a",
				},
			},
			from: 12,
		},
		{
			name: "delete event",
			edit: registry.Edit{Kind: registry.EditDeleteEvent, EffectiveTick: 1, EventIndex: eventIndex},
			from: 1,
		},
		{
			name: "append beyond cached horizon",
			edit: registry.Edit{
				Kind:          registry.EditAppendEvent,
				EffectiveTick: 25,
				Event: &registry.Event{
					AtMS:       2500,
					Kind:       registry.EventHeartbeat,
					InstanceID: "api-a",
				},
			},
			from:     25,
			extended: 4,
			check: func(t *testing.T, result *registry.IncrementalResult) {
				if result.Invalidation.Extended != 4 {
					t.Fatalf("extended steps = %d, want 4", result.Invalidation.Extended)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := registry.IncrementalRecompute(previous, snapshots, base, test.edit, test.from-1)
			if err != nil {
				t.Fatal(err)
			}
			if result.FromTick != test.from {
				t.Fatalf("from tick = %d, want %d", result.FromTick, test.from)
			}
			wantReused := int(test.from) - test.extended
			if result.Invalidation.Reused != wantReused {
				t.Fatalf("reused = %d, want %d", result.Invalidation.Reused, wantReused)
			}
			if result.Invalidation.Extended != test.extended {
				t.Fatalf("extended = %d, want %d", result.Invalidation.Extended, test.extended)
			}
			if result.Invalidation.Recomputed != len(result.Run.Steps)-int(test.from) {
				t.Fatalf("recomputed = %d", result.Invalidation.Recomputed)
			}
			if len(result.Invalidation.Closure) == 0 || result.Invalidation.Source == "" {
				t.Fatalf("empty invalidation metadata: %+v", result.Invalidation)
			}
			for index := int(test.from); index < len(result.Run.Steps); index++ {
				if result.Run.Steps[index].Invalidation == nil {
					t.Fatalf("step %d has no invalidation source", index)
				}
			}
			for index := 0; index < int(test.from); index++ {
				if index >= len(previous.Steps) {
					continue
				}
				left, _ := json.Marshal(previous.Steps[index])
				right, _ := json.Marshal(result.Run.Steps[index])
				if !bytes.Equal(left, right) {
					t.Fatalf("reused prefix changed at step %d", index)
				}
			}
			full, err := registry.RunProject(result.Project, registry.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			stripped := stripInvalidation(result.Run.Steps)
			left, _ := json.Marshal(stripped)
			right, _ := json.Marshal(full.Steps)
			if !bytes.Equal(left, right) {
				t.Fatalf("incremental result differs from full recompute")
			}
			if test.check != nil {
				test.check(t, result)
			}
		})
	}
	if afterHash, _ := base.ModelHash(); afterHash != beforeHash {
		t.Fatal("incremental editing mutated the source project")
	}
}

func TestIncrementalRejectsCommittedHistoryEdit(t *testing.T) {
	base, previous, snapshots := incrementalFixture(t)
	_, err := registry.IncrementalRecompute(previous, snapshots, base, registry.Edit{
		Kind:          registry.EditProbePeriod,
		InstanceID:    "api-a",
		EffectiveTick: 5,
		Value:         100,
	}, 5)
	if registry.ErrorCode(err) != registry.CodeEditCommittedHistory {
		t.Fatalf("error = %v (%s)", err, registry.ErrorCode(err))
	}
}

func TestIncrementalDeleteLastEventShrinksHorizon(t *testing.T) {
	base, _, _ := incrementalFixture(t)
	extended, _, err := registry.ApplyEdit(base, registry.Edit{
		Kind:          registry.EditAppendEvent,
		EffectiveTick: 25,
		Event: &registry.Event{
			AtMS:       2500,
			Kind:       registry.EventHeartbeat,
			InstanceID: "api-a",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	previous, snapshots, err := registry.ComputeProject(extended, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	deleteIndex := -1
	for index, event := range extended.Events {
		if event.AtMS == 2500 && event.Kind == registry.EventHeartbeat {
			deleteIndex = index
			break
		}
	}
	if deleteIndex < 0 {
		t.Fatal("appended event not found")
	}
	result, err := registry.IncrementalRecompute(previous, snapshots, extended, registry.Edit{
		Kind:          registry.EditDeleteEvent,
		EffectiveTick: 25,
		EventIndex:    deleteIndex,
	}, 24)
	if err != nil {
		t.Fatal(err)
	}
	full, err := registry.RunProject(result.Project, registry.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(stripInvalidation(result.Run.Steps))
	expected, _ := json.Marshal(full.Steps)
	if !bytes.Equal(actual, expected) {
		t.Fatal("shrunk incremental timeline differs from full recompute")
	}
	if result.Invalidation.Recomputed != 0 || result.Invalidation.Reused != 21 {
		t.Fatalf("unexpected invalidation: %+v", result.Invalidation)
	}
}

func stripInvalidation(steps []registry.StepResult) []registry.StepResult {
	result := append([]registry.StepResult(nil), steps...)
	for index := range result {
		result[index].Invalidation = nil
	}
	return result
}

func indexes(candidates []registry.Candidate) []string {
	values := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		values = append(values, candidate.InstanceID)
	}
	return values
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
