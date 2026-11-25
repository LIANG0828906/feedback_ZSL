package replaybench_test

import (
	"context"
	"testing"

	"replaybench/internal/diff"
	"replaybench/internal/engine"
	"replaybench/internal/fixed"
	"replaybench/internal/generator"
	"replaybench/internal/loader"
	"replaybench/internal/model"
)

func TestThreeFormatsLoadAndGeneratedTruth(t *testing.T) {
	set, err := generator.DeterministicSet(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	var baseline []model.RoundSnapshot
	for _, format := range []model.FormatID{model.FormatWeightCast, model.FormatCooldownQueue, model.FormatTimestamp} {
		loaded, err := loader.LoadJSON(set.JSON[format])
		if err != nil {
			t.Fatalf("format %d load: %v", format, err)
		}
		if loaded.Replay.Format != format {
			t.Fatalf("source format = %d want %d", loaded.Replay.Format, format)
		}
		if loaded.Replay.SourceFormatVersion == "" {
			t.Fatal("source format version not retained")
		}
		res, err := engine.Run(context.Background(), loaded.Replay)
		if err != nil {
			t.Fatalf("format %d run: %v", format, err)
		}
		expected := truthAsExpectedSnapshots(t, loaded.Truth, res.Snapshots)
		if err := diff.Snapshots(expected, res.Snapshots); err != nil {
			t.Fatalf("format %d truth mismatch: %v", format, err)
		}
		if res.Verdict != loaded.Truth.Verdict || res.WinnerTeam != loaded.Truth.WinnerTeam {
			t.Fatalf("verdict = %s/%s want %s/%s", res.Verdict, res.WinnerTeam, loaded.Truth.Verdict, loaded.Truth.WinnerTeam)
		}
		if format == model.FormatWeightCast {
			baseline = res.Snapshots
		} else if err := diff.Snapshots(baseline, res.Snapshots); err != nil {
			t.Fatalf("format %d differs from format 1: %v", format, err)
		}
	}
}

func truthAsExpectedSnapshots(t *testing.T, truth *model.Truth, actual []model.RoundSnapshot) []model.RoundSnapshot {
	t.Helper()
	out := make([]model.RoundSnapshot, len(actual))
	copy(out, actual)
	for i := range out {
		rt := truth.Rounds[i]
		out[i].EffectiveSkills = append([]string(nil), rt.EffectiveSkills...)
		out[i].Adjudications = append([]model.Adjudication(nil), rt.Adjudications...)
		for j := range out[i].States {
			st := &out[i].States[j]
			if hp, ok := rt.HP[st.UnitID]; ok {
				st.HP = fixed.MustWhole(hp)
			}
			if resource, ok := rt.Resource[st.UnitID]; ok {
				st.Resource = fixed.MustWhole(resource)
			}
		}
	}
	return out
}
