package audit

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"

	"replayaudit/internal/engine"
	"replayaudit/internal/errs"
	"replayaudit/internal/formats"
	"replayaudit/internal/gen"
	"replayaudit/internal/model"
	"replayaudit/internal/store"
	"replayaudit/internal/timeconv"
)

func check(name string, fn func() (string, error)) CheckResult {
	detail, err := fn()
	if err != nil {
		code := errs.CodeOf(err)
		return CheckResult{Name: name, OK: false, Detail: err.Error(), Code: code}
	}
	return CheckResult{Name: name, OK: true, Detail: detail}
}

func runFormatErrorChecks(rep *VerificationReport, samplesDir string) {
	dir, err := os.MkdirTemp("", "audit-fmt-*")
	if err != nil {
		rep.add(CheckResult{Name: "format_errors", OK: false, Detail: err.Error()})
		return
	}
	defer os.RemoveAll(dir)
	p := model.DefaultParams()

	rep.add(check("E_FORMAT unknown format number", func() (string, error) {
		_, _, err := formats.Decode([]byte(`{"format":99,"replay_id":"x"}`), p)
		if errs.CodeOf(err) != errs.EFormat {
			return "", errWrongCode(err, errs.EFormat)
		}
		ae, _ := errs.AsAudit(err)
		if ae.Path != "format" {
			return "", errDetailf("expected path format, got %q", ae.Path)
		}
		return "code=E_FORMAT path=format", nil
	}))

	rep.add(check("E_SCHEMA missing format discriminator", func() (string, error) {
		_, _, err := formats.Decode([]byte(`{"replay_id":"x"}`), p)
		if errs.CodeOf(err) != errs.ESchema {
			return "", errWrongCode(err, errs.ESchema)
		}
		return "code=E_SCHEMA path=format", nil
	}))

	rep.add(check("E_SCHEMA missing events[0].at", func() (string, error) {
		doc := `{"format":1,"replay_id":"x","entities":[{"id":"a","side":"b","max_hp":10}],
			"events":[{"id":"e1","kind":"damage","amount":5,"chain":["a"],"targets":["a"]}]}`
		_, _, err := formats.Decode([]byte(doc), p)
		if errs.CodeOf(err) != errs.ESchema {
			return "", errWrongCode(err, errs.ESchema)
		}
		ae, _ := errs.AsAudit(err)
		if ae.Path != "events[0].at" || ae.EventID != "e1" {
			return "", errDetailf("unexpected path/event: %q %q", ae.Path, ae.EventID)
		}
		return "code=E_SCHEMA path=events[0].at event=e1", nil
	}))

	rep.add(check("E_FORMAT malformed JSON", func() (string, error) {
		_, _, err := formats.Decode([]byte(`{not json`), p)
		if errs.CodeOf(err) != errs.EFormat {
			return "", errWrongCode(err, errs.EFormat)
		}
		return "code=E_FORMAT", nil
	}))

	rep.add(check("sequence semantic swap f1.casters/f2.targets", func() (string, error) {
		f1doc := `{"format":1,"replay_id":"x","entities":[{"id":"c","side":"b","max_hp":10},{"id":"t","side":"r","max_hp":10}],
			"events":[{"id":"e1","at":1,"kind":"damage","amount":5,"chain":["c"],"targets":["t"]}]}`
		r1, _, err := formats.Decode([]byte(f1doc), p)
		if err != nil {
			return "", err
		}
		if !reflect.DeepEqual(r1.Events[0].Casters, []string{"c"}) {
			return "", errDetailf("f1 chain must map to casters, got %v", r1.Events[0].Casters)
		}
		f2doc := `{"format":2,"replay_id":"x","entities":[{"id":"c","side":"b","max_hp":10},{"id":"t","side":"r","max_hp":10}],
			"events":[{"id":"e1","at":0,"kind":"damage","amount":5,"chain":["t"],"casters":["c"]}]}`
		r2, _, err := formats.Decode([]byte(f2doc), p)
		if err != nil {
			return "", err
		}
		if !reflect.DeepEqual(r2.Events[0].Targets, []string{"t"}) || !reflect.DeepEqual(r2.Events[0].Casters, []string{"c"}) {
			return "", errDetailf("f2 chain must map to targets, got c=%v t=%v", r2.Events[0].Casters, r2.Events[0].Targets)
		}
		return "f1.chain->casters; f2.chain->targets", nil
	}))

	// Ensure the samples directory was actually materialized.
	rep.add(check("samples tree exists", func() (string, error) {
		if _, err := os.Stat(filepath.Join(samplesDir, "index.json")); err != nil {
			return "", err
		}
		return samplesDir, nil
	}))
}

func runTimeConversionChecks(rep *VerificationReport) {
	p := model.DefaultParams()
	rep.add(check("turn<->ms integer round trip for all samples", func() (string, error) {
		n := 0
		for _, b := range gen.BuildAll() {
			for _, e := range b.Replay.Events {
				turn, back, err := timeconv.RoundTripMS(e.At, p, e.ID)
				if err != nil {
					return "", err
				}
				if back != e.At || turn < 1 {
					return "", errDetailf("event %s: at=%d turn=%d back=%d", e.ID, e.At, turn, back)
				}
				n++
			}
		}
		return fmt.Sprintf("%d events exact identity", n), nil
	}))

	rep.add(check("E_OVF on turn->ms overflow with event id", func() (string, error) {
		_, err := timeconv.TurnToMS(math.MaxInt64, p.MsPerTurn, "start", "e_ovf_1")
		if errs.CodeOf(err) != errs.EOvf {
			return "", errWrongCode(err, errs.EOvf)
		}
		ae, ok := errs.AsAudit(err)
		if !ok || ae.EventID != "e_ovf_1" {
			return "", errDetailf("missing event tag: %+v", err)
		}
		return "code=E_OVF event=e_ovf_1", nil
	}))
}

func runConflictChecks(rep *VerificationReport) {
	p := model.DefaultParams()
	ents := []model.EntitySpec{
		{ID: "a", Side: "blue", MaxHP: 100, MaxMP: 100},
		{ID: "b", Side: "red", MaxHP: 100, MaxMP: 100},
	}

	rep.add(check("mutex adjudicated by priority", func() (string, error) {
		r := &model.Replay{ReplayID: "cf_mutex_ok", Entities: ents,
			Mutex: []model.MutexGroup{{ID: "g1", EventIDs: []string{"e1", "e2"}}},
			Events: []*model.Event{
				{ID: "e1", Kind: "damage", At: 0, Priority: 10, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 30},
				{ID: "e2", Kind: "damage", At: 0, Priority: 1, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 30},
			}}
		res, err := engine.SettleFull(r, p)
		if err != nil {
			return "", err
		}
		by := res.ResultByID()
		if by["e1"].Status != engine.StatusSettled || by["e2"].Status != engine.StatusSuppressed {
			return "", errDetailf("bad statuses e1=%s e2=%s", by["e1"].Status, by["e2"].Status)
		}
		if len(res.Conflicts) != 1 || res.Conflicts[0].Type != "mutex" || res.Conflicts[0].PrimaryEventID != "e2" {
			return "", errDetailf("missing mutex detail: %+v", res.Conflicts)
		}
		return "e1 winner, e2 suppressed; 1 detail record", nil
	}))

	rep.add(check("E_CONFLICT on mandatory mutex loser", func() (string, error) {
		r := &model.Replay{ReplayID: "cf_mutex_bad", Entities: ents,
			Mutex: []model.MutexGroup{{ID: "g1", EventIDs: []string{"e1", "e2"}}},
			Events: []*model.Event{
				{ID: "e1", Kind: "damage", At: 0, Priority: 10, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 30},
				{ID: "e2", Kind: "damage", At: 0, Priority: 1, Mandatory: true, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 30},
			}}
		res, err := engine.SettleFull(r, p)
		if errs.CodeOf(err) != errs.EConflict {
			return "", errWrongCode(err, errs.EConflict)
		}
		ae, _ := errs.AsAudit(err)
		if ae.EventID != "e2" {
			return "", errDetailf("expected conflict event e2, got %q", ae.EventID)
		}
		if res == nil || len(res.Conflicts) == 0 {
			return "", errDetailf("expected detail records on report")
		}
		return "code=E_CONFLICT event=e2", nil
	}))

	rep.add(check("E_CONFLICT on tied contradictory flags", func() (string, error) {
		r := &model.Replay{ReplayID: "cf_flag_bad", Entities: ents,
			Flags: []model.FlagDef{
				{ID: "d1", Name: "f", Value: true, Priority: 0},
				{ID: "d2", Name: "f", Value: false, Priority: 0},
			}}
		_, err := engine.SettleFull(r, p)
		if errs.CodeOf(err) != errs.EConflict {
			return "", errWrongCode(err, errs.EConflict)
		}
		ae, _ := errs.AsAudit(err)
		if ae.EventID != "d1" {
			return "", errDetailf("expected primary d1, got %q", ae.EventID)
		}
		return "code=E_CONFLICT event=d1", nil
	}))

	rep.add(check("E_CONFLICT sync with pending failed event", func() (string, error) {
		r := &model.Replay{ReplayID: "cf_sync_bad", Entities: ents,
			Sync: []model.SyncPoint{{ID: "s1", At: 100}},
			Events: []*model.Event{
				{ID: "e1", Kind: "damage", At: 0, Fault: true, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 10},
				{ID: "e2", Kind: "damage", At: 200, Casters: []string{"a"}, Targets: []string{"b"}, Amount: 10},
			}}
		res, err := engine.SettleFull(r, p)
		if errs.CodeOf(err) != errs.EConflict {
			return "", errWrongCode(err, errs.EConflict)
		}
		ae, _ := errs.AsAudit(err)
		if ae.EventID != "e1" {
			return "", errDetailf("expected pending event e1, got %q", ae.EventID)
		}
		by := res.ResultByID()
		if by["e1"].Status != engine.StatusFailed {
			return "", errDetailf("e1 should be failed, got %s", by["e1"].Status)
		}
		return "code=E_CONFLICT event=e1 status=failed", nil
	}))

	rep.add(check("fault isolated then recoverable by edit", func() (string, error) {
		r := precisionReplay()
		r.Events[0].Fault = true
		svc := engine.NewService(r, p)
		first, err := svc.RunFull()
		if err != nil {
			return "", err
		}
		if first.ResultByID()["e1"].Status != engine.StatusFailed {
			return "", errDetailf("e1 expected failed")
		}
		v := false
		res, ierr := svc.ApplyEdit(engine.Edit{Op: "fault", EventID: "e1", Bool: &v})
		if ierr != nil {
			return "", ierr
		}
		fresh, _ := engine.SettleFull(r, p)
		if mm := engine.CompareReports(fresh, res.Report); mm != nil {
			return "", mm
		}
		return "failed event isolated; clearing fault recomputes and matches full", nil
	}))
}

func runPrecisionCheck(rep *VerificationReport) {
	p := model.DefaultParams()
	rep.add(check("precise invalidation only touches true downstream", func() (string, error) {
		r := precisionReplay()
		svc := engine.NewService(r, p)
		if _, err := svc.RunFull(); err != nil {
			return "", err
		}
		res, err := svc.ApplyEdit(engine.Edit{Op: "amount", EventID: "e1", Amount: 99})
		if err != nil {
			return "", err
		}
		got := append([]string(nil), res.Manifest.RecomputedEvent...)
		want := []string{"e1", "e3"}
		if !reflect.DeepEqual(got, want) {
			return "", errDetailf("recomputed=%v want %v; reused=%v", got, want, res.Manifest.ReusedEvents)
		}
		reused := map[string]bool{}
		for _, id := range res.Manifest.ReusedEvents {
			reused[id] = true
		}
		for _, id := range []string{"e2", "e4", "e5", "e6"} {
			if !reused[id] {
				return "", errDetailf("event %s should be cache-reused, reused=%v", id, res.Manifest.ReusedEvents)
			}
		}
		fresh, _ := engine.SettleFull(r, p)
		if mm := engine.CompareReports(fresh, res.Report); mm != nil {
			return "", mm
		}
		return "recomputed=[e1 e3]; reused=[e2 e4 e5 e6]; parity exact", nil
	}))
}

func runCheckpointInvalidationCheck(rep *VerificationReport, dbDir string) {
	p := model.DefaultParams()
	r := precisionReplay()

	rep.add(check("checkpoint crash/resume parity", func() (string, error) {
		segP := p
		segP.SegmentUnits = 1000
		path := filepath.Join(dbDir, "resume.db")
		st, err := store.OpenFile(path)
		if err != nil {
			return "", err
		}
		if _, err := engine.RunCheckpointed(r, segP, st, engine.CheckpointOptions{InterruptAfter: 2}); err != engine.ErrInterrupted {
			_ = st.Close()
			return "", errDetailf("expected ErrInterrupted, got %v", err)
		}
		_ = st.Close()
		st2, err := store.OpenFile(path)
		if err != nil {
			return "", err
		}
		res, err := engine.RunCheckpointed(r, segP, st2, engine.CheckpointOptions{})
		_ = st2.Close()
		if err != nil {
			return "", err
		}
		full, _ := engine.SettleFull(r, segP)
		if mm := engine.CompareReports(full, res); mm != nil {
			return "", mm
		}
		return "interrupt after 2 commits; resumed report equals full", nil
	}))

	rep.add(check("version mismatch purges all segments", func() (string, error) {
		path := filepath.Join(dbDir, "version-mismatch.db")
		st, err := store.OpenFile(path)
		if err != nil {
			return "", err
		}
		if _, err := engine.RunCheckpointed(r, p, st, engine.CheckpointOptions{}); err != nil {
			_ = st.Close()
			return "", err
		}
		old, _ := st.LoadMeta(r.ReplayID)
		if old == nil || old.SegmentCount == 0 {
			_ = st.Close()
			return "", errDetailf("expected stored segments")
		}
		_ = st.Close()

		p2 := p
		p2.ParamsVersion = "params-v9"
		st2, err := store.OpenFile(path)
		if err != nil {
			return "", err
		}
		ok, err := st2.CheckCompatible(r.ReplayID, model.HashReplay(r), model.FormatVersion, engine.ParamsTag(p2), model.AlgoVersion)
		if err != nil {
			return "", err
		}
		if ok {
			return "", errDetailf("version mismatch must be incompatible")
		}
		recs, _, _ := st2.LoadSegments(r.ReplayID)
		if len(recs) != 0 {
			return "", errDetailf("old segments must be purged, found %d", len(recs))
		}
		res, err := engine.RunCheckpointed(r, p2, st2, engine.CheckpointOptions{})
		_ = st2.Close()
		if err != nil {
			return "", err
		}
		full, _ := engine.SettleFull(r, p2)
		if mm := engine.CompareReports(full, res); mm != nil {
			return "", mm
		}
		return "params version mismatch -> purge -> full recompute parity", nil
	}))

	rep.add(check("content hash mismatch purges snapshot", func() (string, error) {
		path := filepath.Join(dbDir, "content-mismatch.db")
		st, err := store.OpenFile(path)
		if err != nil {
			return "", err
		}
		if _, err := engine.RunCheckpointed(r, p, st, engine.CheckpointOptions{}); err != nil {
			_ = st.Close()
			return "", err
		}
		_ = st.Close()
		edited := r.Clone()
		_, _, _ = engine.ApplyEdit(edited, p, engine.Edit{Op: "amount", EventID: "e1", Amount: 12345})
		st2, err := store.OpenFile(path)
		if err != nil {
			return "", err
		}
		ok, err := st2.CheckCompatible(edited.ReplayID, model.HashReplay(edited), model.FormatVersion, engine.ParamsTag(p), model.AlgoVersion)
		if err != nil {
			return "", err
		}
		if ok {
			return "", errDetailf("content edit must invalidate hash")
		}
		recs, _, _ := st2.LoadSegments(edited.ReplayID)
		if len(recs) != 0 {
			return "", errDetailf("content mismatch must purge segments")
		}
		_ = st2.Close()
		return "edited content hash mismatch -> whole snapshot invalidated", nil
	}))
}

// precisionReplay builds two independent resource chains plus disjoint events.
func precisionReplay() *model.Replay {
	return &model.Replay{
		ReplayID: "precision",
		Entities: []model.EntitySpec{
			{ID: "p1", Side: "blue", MaxHP: 100, MaxMP: 100},
			{ID: "p2", Side: "blue", MaxHP: 100, MaxMP: 100},
			{ID: "x1", Side: "red", MaxHP: 100, MaxMP: 100},
			{ID: "x2", Side: "red", MaxHP: 100, MaxMP: 100},
		},
		Events: []*model.Event{
			{ID: "e1", Kind: "damage", At: 0, Casters: []string{"p1"}, Targets: []string{"x1"}, Amount: 10},
			{ID: "e2", Kind: "damage", At: 1000, Casters: []string{"x1"}, Targets: []string{"p1"}, Amount: 5},
			{ID: "e4", Kind: "gain", At: 1000, Casters: []string{"p2"}, Targets: []string{"p2"}, Amount: 7},
			{ID: "e3", Kind: "heal", At: 2000, Casters: []string{"p2"}, Targets: []string{"x1"}, Amount: 4},
			{ID: "e5", Kind: "spend", At: 2000, Casters: []string{"p2"}, Targets: []string{"p2"}, Amount: 3},
			{ID: "e6", Kind: "damage", At: 3000, Casters: []string{"p1"}, Targets: []string{"x2"}, Amount: 9},
		},
	}
}

func errWrongCode(err error, want string) error {
	return fmt.Errorf("want %s, got code=%s err=%v", want, errs.CodeOf(err), err)
}

func errDetailf(format string, args ...any) error { return fmt.Errorf(format, args...) }
