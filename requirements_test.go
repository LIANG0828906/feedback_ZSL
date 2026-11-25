package audiobus_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"audiobus/domain"
	aerr "audiobus/errs"
	"audiobus/internal/graph"
	"audiobus/internal/loader"
	"audiobus/internal/pipeline"
	"audiobus/internal/review"
	"audiobus/internal/store"
)

func loadManifest(t *testing.T) domain.Manifest {
	t.Helper()
	set, err := loader.LoadSampleSet("artifacts/sample")
	if err != nil {
		t.Fatal(err)
	}
	return set.Manifest
}

func requireCode(t *testing.T, err error, code aerr.Code) *aerr.Error {
	t.Helper()
	var ae *aerr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *Error code %s, got %T %v", code, err, err)
	}
	if ae.Code != code {
		t.Fatalf("code=%s want=%s err=%v", ae.Code, code, ae)
	}
	return ae
}

func TestMappingErrorCodes(t *testing.T) {
	m := loadManifest(t)

	_, _, err := loader.LoadBranchBytes("", []byte(`{bad json`), &m)
	requireCode(t, err, aerr.EDecode)

	unknown := []byte(`{"branch":"delta","branchVersion":"x","header":{"channels":3,"totalSamples":4800,"sampleRate":48000},"events":[{"id":1}]}`)
	_, _, err = loader.LoadBranchBytes("delta", unknown, &m)
	requireCode(t, err, aerr.EVersion)

	missing := []byte(`{"branch":"alpha","branchVersion":"2026.11-local","header":{"channels":3,"totalSamples":4800,"sampleRate":48000},"events":[{"id":1,"ch":0,"ld":1,"tg":2,"pr":1,"ni":0,"cd":0,"fl":0,"lst":[0]}]}`)
	_, _, err = loader.LoadBranchBytes("alpha", missing, &m)
	ae := requireCode(t, err, aerr.ESchema)
	if ae.Path != "$.events[0].ts" {
		t.Fatalf("path=%s", ae.Path)
	}

	badCap := m
	badCap.Channels = 9
	alpha, err := readBranch("alpha")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = loader.LoadBranchBytes("alpha", alpha, &badCap)
	requireCode(t, err, aerr.ECap)

	overflow := []byte(`{"branch":"beta","branchVersion":"2026.11-local","header":{"channels":3,"totalSamples":4800,"sampleRate":48000},"events":[{"id":1,"ts":` + itoa(math.MaxInt64) + `,"ld":1,"tg":2,"pr":1,"ni":0,"cd":0,"fl":0,"lst":[0]}]}`)
	_, _, err = loader.LoadBranchBytes("beta", overflow, &m)
	ae = requireCode(t, err, aerr.EOvf)
	if ae.Branch != "beta" || ae.RawValue != math.MaxInt64 {
		t.Fatalf("overflow context=%+v", ae)
	}
}

func TestCrossBranchEquivalenceAndRoundTrip(t *testing.T) {
	set, err := loader.LoadSampleSet("artifacts/sample")
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.CrossBranchEquivalent(set); err != nil {
		t.Fatal(err)
	}
	for _, e := range loader.CanonicalEvents(set) {
		if _, err := loader.RoundTripConversion(e, set.Manifest); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrderFlagPackStageChains(t *testing.T) {
	set, err := loader.LoadSampleSet("artifacts/sample")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readBranch("alpha")
	if err != nil {
		t.Fatal(err)
	}
	var bf domain.BranchFile
	if err := json.Unmarshal(raw, &bf); err != nil {
		t.Fatal(err)
	}
	*bf.Events[1].Ts = 1
	mutated, _ := json.Marshal(bf)
	_, events, err := loader.LoadBranchBytes("alpha", mutated, &set.Manifest)
	ae := requireCode(t, err, aerr.EOrder)
	if ae.Channel != 0 || ae.Frame != 1 {
		t.Fatalf("order location ch=%d frame=%d", ae.Channel, ae.Frame)
	}

	flagEvents := loader.CanonicalEvents(set)
	for i := range flagEvents {
		if flagEvents[i].ID == 1 {
			flagEvents[i].Flag = 3
		}
	}
	err = loader.ValidateFlags(flagEvents, &set.Manifest)
	ae = requireCode(t, err, aerr.EFlag)
	if ae.Channel != 0 || ae.EventID != 1 || ae.Frame == 0 {
		t.Fatalf("flag location=%+v", ae)
	}

	events = loader.CanonicalEvents(set)
	engine := pipeline.NewEngine(set.Manifest, events, nil)
	engine.Fault = &pipeline.Fault{Window: 0, Channel: 1, Seq: 2}
	_, err = engine.RunRealtime(context.Background())
	ae = requireCode(t, err, aerr.EStage)
	if ae.Channel != 1 || ae.Packet != 2 {
		t.Fatalf("stage location=%+v", ae)
	}

	smallWindow := set.Manifest
	smallWindow.TimingWindowMs = 1
	engine = pipeline.NewEngine(smallWindow, events, nil)
	_, err = engine.RunRealtime(context.Background())
	ae = requireCode(t, err, aerr.EPack)
	if ae.Channel < 0 || ae.Packet < 1 {
		t.Fatalf("pack location=%+v", ae)
	}
}

func TestThreePathsAndSQLiteRecovery(t *testing.T) {
	set, err := loader.LoadSampleSet("artifacts/sample")
	if err != nil {
		t.Fatal(err)
	}
	events := loader.CanonicalEvents(set)
	ctx := context.Background()
	full, err := pipeline.NewEngine(set.Manifest, events, nil).RunRealtime(ctx)
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stop := pipeline.NewEngine(set.Manifest, events, st)
	stop.StopAfterWindow = 1
	if _, err := stop.RunRealtime(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	resume, err := pipeline.Resume(ctx, set.Manifest, events, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := store.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := pipeline.ReplayFromStore(ctx, ro)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ro.DB().Query(`SELECT window FROM window_batches ORDER BY window`)
	if err != nil {
		t.Fatal(err)
	}
	committedWindows := []int{}
	for rows.Next() {
		var window int
		if err := rows.Scan(&window); err != nil {
			t.Fatal(err)
		}
		committedWindows = append(committedWindows, window)
	}
	rows.Close()
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.CompareReports(full, resume, replay, set.Manifest.MaxSamplesPerPack); err != nil {
		t.Fatal(err)
	}
	if len(committedWindows) != 3 || committedWindows[0] != 0 || committedWindows[1] != 1 || committedWindows[2] != 2 {
		t.Fatalf("commit windows=%v want [0 1 2]", committedWindows)
	}
}

func TestSnapshotHashAndVersionInvalidation(t *testing.T) {
	set, err := loader.LoadSampleSet("artifacts/sample")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "snapshot.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine := pipeline.NewEngine(set.Manifest, loader.CanonicalEvents(set), st)
	engine.StopAfterWindow = 0
	if _, err := engine.RunRealtime(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE packets SET hash='bad' WHERE rowid=(SELECT rowid FROM packets LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, recovered, err := store.OpenValidated(ctx, dbPath, set.Manifest.BranchVersion, domain.AlgoVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatal("expected corrupt snapshot to be reset")
	}
	packets, err := reopened.LoadPackets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 0 {
		t.Fatalf("corrupt snapshot retained %d packets", len(packets))
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGraphPreciseInvalidationReusesCache(t *testing.T) {
	set, err := loader.LoadSampleSet("artifacts/sample")
	if err != nil {
		t.Fatal(err)
	}
	engine := pipeline.NewEngine(set.Manifest, loader.CanonicalEvents(set), nil)
	first, err := engine.RunRealtime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invalidations := graph.EventEdit(first.Report.Packets, 6)
	if len(invalidations) == 0 {
		t.Fatal("expected invalidations")
	}
	engine.Invalidate(invalidations)
	second, err := engine.RunRealtime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Reused) != 36-len(invalidations) {
		t.Fatalf("reused=%d want=%d invalidations=%d", len(second.Reused), 36-len(invalidations), len(invalidations))
	}
	for _, inv := range invalidations {
		for _, key := range second.Reused {
			if key.Window == inv.Window && key.Channel == inv.Channel && key.Seq == inv.Seq {
				t.Fatalf("invalidated packet reused: %+v", inv)
			}
		}
	}

	editedEvents := loader.CanonicalEvents(set)
	for i := range editedEvents {
		if editedEvents[i].ID == 10 {
			editedEvents[i].Priority = 6
		}
	}
	editInvalidations := graph.EventEdit(first.Report.Packets, 10)
	if len(editInvalidations) != 8 {
		t.Fatalf("event 10 invalidations=%d want 8", len(editInvalidations))
	}
	engine.Invalidate(editInvalidations)
	engine.Events = editedEvents
	edited, err := engine.RunRealtime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.Reused) != 28 {
		t.Fatalf("priority edit reused=%d want 28", len(edited.Reused))
	}
	if edited.Report.Frames[0][1920] == first.Report.Frames[0][1920] {
		t.Fatal("priority edit did not affect expected downstream frame")
	}

	added := graph.AddedEvent(first.Report.Packets, 2, 2208)
	if len(added) != 7 {
		t.Fatalf("added event invalidations=%d want 7", len(added))
	}
	if got := graph.QuantUnitChange(first.Report.Packets, 64, 128); len(got) != 36 {
		t.Fatalf("quant invalidations=%d want 36", len(got))
	}
	if got := graph.PacketCapChange(first.Report.Packets, 480, 240); len(got) != 36 {
		t.Fatalf("cap invalidations=%d want 36", len(got))
	}
}

func TestResourcePoolBlocksAndRecordsWait(t *testing.T) {
	pool := pipeline.NewResourcePool(1, 64)
	first, err := pool.Acquire(0, 32)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		second, err := pool.Acquire(1, 32)
		if err != nil {
			t.Error(err)
			return
		}
		second()
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	first()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued work did not resume")
	}
	records := pool.QueueRecords()
	if len(records) != 1 || records[0].Reason != "buffer_exhausted" || records[0].WaitNanos <= 0 {
		t.Fatalf("queue records=%+v", records)
	}
}

func TestIndependentReviewLegacyPersistentDB(t *testing.T) {
	result, err := review.ReviewSQLite("artifacts/db/resume.db", "artifacts/sample/truth.json")
	if err != nil {
		t.Fatal(err)
	}
	if result.PacketsChecked != 36 || result.ChannelsChecked != 3 {
		t.Fatalf("review=%+v", result)
	}
}

func readBranch(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("artifacts", "sample", "branch_"+name+".json"))
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
