package audit_test

import (
	"testing"

	"replayaudit/internal/audit"
)

func TestRunAllAcceptance(t *testing.T) {
	rep, err := audit.RunAll(t.TempDir()+"/samples", t.TempDir()+"/dbs")
	if err != nil {
		t.Fatal(err)
	}
	var failed []string
	for _, c := range rep.Checks {
		if !c.OK {
			failed = append(failed, c.Name+": "+c.Detail)
			if c.Mismatch != nil {
				failed = append(failed, "  "+c.Mismatch.Error())
			}
		}
	}
	if len(failed) > 0 {
		for _, f := range failed {
			t.Error(f)
		}
		t.Fatalf("%d acceptance checks failed", len(failed))
	}
	if !rep.AllOK || len(rep.Replays) != 6 {
		t.Fatalf("unexpected report: allOK=%v replays=%d", rep.AllOK, len(rep.Replays))
	}
}
