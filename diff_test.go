package replaybench_test

import (
	"context"
	"strings"
	"testing"

	"replaybench/internal/diff"
	"replaybench/internal/eerr"
	"replaybench/internal/engine"
	"replaybench/internal/model"
)

func TestDiffLocatesFirstMismatch(t *testing.T) {
	loaded := loadSample(t, model.FormatWeightCast)
	res, err := engine.Run(context.Background(), loaded.Replay)
	if err != nil {
		t.Fatal(err)
	}
	expected := res.Snapshots
	second, err := engine.Run(context.Background(), loaded.Replay)
	if err != nil {
		t.Fatal(err)
	}
	actual := second.Snapshots
	actual[1].States[0].HP++
	err = diff.Snapshots(expected, actual)
	if !eerr.IsCode(err, eerr.EDiff) {
		t.Fatalf("err=%v", err)
	}
	ee, _ := eerr.AsError(err)
	if ee.Round != 2 || !strings.Contains(ee.Field, ".hp") {
		t.Fatalf("diff location round=%d field=%s", ee.Round, ee.Field)
	}
}
