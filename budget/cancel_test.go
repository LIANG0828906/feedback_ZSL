package budget

import (
	"context"
	"errors"
	"testing"
)

func TestCancellationBoundaryAndResume(t *testing.T) {
	fixture := testFixture(t)
	params := DefaultParams()
	params.WindowTicks = 3
	ctx, cancel := context.WithCancel(context.Background())
	params.OnWindowCommitted = func(commit WindowCommit) {
		if commit.WindowIndex == 0 {
			cancel()
		}
	}

	dir := t.TempDir()
	partial, err := RunRealtimeContext(ctx, fixture.Project, params, dir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if len(partial.Commits) != 1 || len(partial.Ticks) != params.WindowTicks {
		t.Fatalf("partial boundary commits=%d ticks=%d", len(partial.Commits), len(partial.Ticks))
	}

	resumeParams := DefaultParams()
	resumeParams.WindowTicks = params.WindowTicks
	resumed, err := ResumeFromStore(fixture.Project, resumeParams, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareTickReports(fixture.Truth, resumed); err != nil {
		t.Fatalf("resumed report mismatch: %v", err)
	}
	if len(resumed.Commits) != 4 {
		t.Fatalf("resumed commits = %d, want 4", len(resumed.Commits))
	}
}
