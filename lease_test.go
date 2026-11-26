package leasekit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func generatedBundle(t *testing.T) (*Project, *Truth, map[string]*Project) {
	t.Helper()
	p := GenerateProject(FixedSeed)
	truth, err := GenerateTruth(p)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := ExportV1(p)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := ExportV2(p)
	if err != nil {
		t.Fatal(err)
	}
	v3, err := ExportV3(p)
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[string]*Project{}
	for name, body := range map[string][]byte{"v1": v1, "v2": v2, "v3": v3} {
		lp, err := Load(body)
		if err != nil {
			t.Fatalf("Load(%s): %v", name, err)
		}
		if !CoreEqual(p, lp) {
			t.Fatalf("schema %s project mismatch\ncanonical=%s\nloaded=%s", name, mustJSON(p), mustJSON(lp))
		}
		loaded[name] = lp
	}
	return p, truth, loaded
}

func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

func TestThreeSchemaLoadingAndSampleFiles(t *testing.T) {
	p, truth, loaded := generatedBundle(t)
	for name, lp := range loaded {
		report, err := Run(lp)
		if err != nil {
			t.Fatalf("Run(%s): %v", name, err)
		}
		if string(report.CanonicalJSON()) != string(truth.Report.CanonicalJSON()) {
			t.Fatalf("%s report differs from truth", name)
		}
	}

	for _, file := range []string{
		"lease-v1-tick-flat.json",
		"lease-v2-ms-nested.json",
		"lease-v3-seconds-absolute.json",
		"truth.json",
	} {
		path := filepath.Join("samples", "generated", file)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sample %s missing; run go run ./cmd/leasegen: %v", path, err)
		}
	}

	v1Body, err := os.ReadFile(filepath.Join("samples", "generated", "lease-v1-tick-flat.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Load(v1Body); err != nil {
		t.Fatalf("committed v1 sample no longer loads: %v", err)
	}

	for _, op := range p.Operations {
		if op.ExpectedReason == "" {
			continue
		}
		got := truth.Report.ResultBySeq(op.Seq)
		if got.Code != op.ExpectedReason {
			t.Fatalf("seq=%d reason=%s want %s", op.Seq, got.Code, op.ExpectedReason)
		}
	}
}

func TestWindowBoundariesAndScale(t *testing.T) {
	p := minimalWindowProject()
	report, err := Run(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.ResultBySeq(2).Code; got != CodeRenewTooEarly {
		t.Fatalf("early code=%s", got)
	}
	if got := report.ResultBySeq(3).Code; got != CodeOK {
		t.Fatalf("lower equality should be accepted, got %s", got)
	}
	if got := report.ResultBySeq(4).Code; got != CodeRenewWindowClosed {
		t.Fatalf("upper equality should be closed, got %s", got)
	}
	if got := report.ResultBySeq(5).Code; got != CodeRenewExpired {
		t.Fatalf("after deadline code=%s", got)
	}

	correct, err := TicksToMS(20, 100)
	if err != nil || correct != 2000 {
		t.Fatalf("correct tick conversion: %d %v", correct, err)
	}
	wrongScale, _ := TicksToMS(20, 99)
	if wrongScale == correct {
		t.Fatal("test premise broken: wrong integer scale unexpectedly matches")
	}
	if wrongScale >= 2000 {
		t.Fatalf("wrong scale should move boundary before 2000, got %d", wrongScale)
	}
}

func minimalWindowProject() *Project {
	return &Project{
		ProjectID: "window-boundary",
		Boundary:  BoundaryLowerInclusiveUpperExclusive,
		Resources: []Resource{{ID: "r", Mode: ModeExclusive, LeaseTTLMS: 2000, RenewWindowMS: 500, MaxRenewals: 2, MaxRenewDurationMS: 2000, RenewAttemptBudget: 10}},
		Holders:   []Holder{{ID: "h", ResourceID: "r", Priority: 1}},
		Operations: []Operation{
			{Seq: 1, HolderID: "h", Kind: OpAcquire, AtMS: 0},
			{Seq: 2, HolderID: "h", Kind: OpRenew, AtMS: 1499, RenewForMS: 2000},
			{Seq: 3, HolderID: "h", Kind: OpRenew, AtMS: 1500, RenewForMS: 2000},
			{Seq: 4, HolderID: "h", Kind: OpRenew, AtMS: 3500, RenewForMS: 2000},
			{Seq: 5, HolderID: "h", Kind: OpRenew, AtMS: 3501, RenewForMS: 2000},
		},
	}
}

func TestCompetitionArbitration(t *testing.T) {
	_, truth, _ := generatedBundle(t)
	low := truth.Report.ResultBySeq(14)
	high := truth.Report.ResultBySeq(17)
	if low.Code != CodeCompetitionLost || low.ExpiresAtMS != 3000 {
		t.Fatalf("priority loser wrong: %+v", low)
	}
	if high.Code != CodeOK || high.ExpiresAtMS != 5100 {
		t.Fatalf("priority winner wrong: %+v", high)
	}
	if low.Arbitration == nil || low.Arbitration.Winner != "holder-prio-high" {
		t.Fatalf("priority arbitration missing: %+v", low.Arbitration)
	}

	tieLoser := truth.Report.ResultBySeq(16)
	tieWinner := truth.Report.ResultBySeq(13)
	if tieLoser.Code != CodeCompetitionLost || tieLoser.ExpiresAtMS != 3000 {
		t.Fatalf("tie loser wrong: %+v", tieLoser)
	}
	if tieWinner.Code != CodeOK || tieWinner.ExpiresAtMS != 5000 {
		t.Fatalf("tie winner wrong: %+v", tieWinner)
	}
	if tieLoser.Arbitration == nil || tieLoser.Arbitration.WinnerSeq != 13 {
		t.Fatalf("tie arbitration wrong: %+v", tieLoser.Arbitration)
	}
}

func TestChainInvalidationAndAcquireVisibility(t *testing.T) {
	_, truth, _ := generatedBundle(t)
	cases := map[int]struct {
		code   string
		source Milli
	}{
		9:  {CodeResourceHeld, 3000},
		22: {CodeResourceHeld, 5000},
		27: {CodeResourceHeld, 7000},
	}
	for seq, want := range cases {
		got := truth.Report.ResultBySeq(seq)
		if got.Code != want.code || got.Source.AtMS != want.source {
			t.Fatalf("seq=%d got code=%s source=%d want %s/%d", seq, got.Code, got.Source.AtMS, want.code, want.source)
		}
	}
	if got := truth.Report.ResultBySeq(28); !got.Granted || got.AtMS != 7100 {
		t.Fatalf("later acquire should see 7000ms release: %+v", got)
	}
	found := false
	for _, ev := range truth.Report.Events {
		if ev.HolderID == "holder-order-a" && ev.Type == "lease_invalidated" && ev.AtMS == 7000 {
			found = true
		}
	}
	if !found {
		t.Fatal("missing holder-order-a invalidation at 7000ms")
	}
}
