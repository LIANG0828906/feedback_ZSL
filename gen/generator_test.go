package gen

import (
	"context"
	"testing"

	"quotaengine/quota"
)

func TestReferenceMatchesEngine(t *testing.T) {
	p, err := buildProject(42)
	if err != nil {
		t.Fatal(err)
	}
	run, err := quota.Run(context.Background(), p, quota.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	truth, err := buildTruth(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := truth.Check(run); err != nil {
		t.Fatal(err)
	}
}
