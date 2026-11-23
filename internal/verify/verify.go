package verify

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"roomreverb/internal/checkpoint"
	"roomreverb/internal/editing"
	"roomreverb/internal/engine"
	"roomreverb/internal/errs"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
)

type ThreeWayResult struct {
	Base    *engine.RunResult `json:"base"`
	Full    *engine.RunResult `json:"full"`
	Preview *engine.RunResult `json:"preview"`
	Resume  *engine.RunResult `json:"resume"`
	Edits   []model.Edit      `json:"edits"`
}

func ThreeWay(ctx context.Context, base model.Level, edits []model.Edit, cfg engine.Config, dbPath string) (ThreeWayResult, error) {
	baseCfg := cfg
	baseCfg.ParamsVersion = 0
	baseCfg.InterruptAfter = 0
	baseCfg.UseCache = false
	baseRun, err := engine.RunFull(ctx, base, baseCfg)
	if err != nil {
		return ThreeWayResult{Base: baseRun, Edits: edits}, err
	}
	edited := base.Clone()
	for _, edit := range edits {
		applied, err := editing.Apply(edited, edit)
		if err != nil {
			return ThreeWayResult{Base: baseRun, Edits: edits}, err
		}
		edited = applied.Level
	}
	fullCfg := cfg
	fullCfg.ParamsVersion = len(edits)
	fullCfg.InterruptAfter = 0
	full, err := engine.RunFull(ctx, edited, fullCfg)
	if err != nil {
		return ThreeWayResult{Base: baseRun, Full: full, Edits: edits}, err
	}

	previewCfg := cfg
	previewCfg.ParamsVersion = 0
	previewCfg.UseCache = false
	session, err := engine.NewPreview(ctx, base, nil, previewCfg)
	if err != nil {
		return ThreeWayResult{Base: baseRun, Full: full, Edits: edits}, err
	}
	for _, edit := range edits {
		if _, err := session.Apply(ctx, edit); err != nil {
			return ThreeWayResult{Base: baseRun, Full: full, Preview: session.RunResult(), Edits: edits}, err
		}
	}
	preview := session.RunResult()
	if err := engine.Compare(full, preview); err != nil {
		err.(*errs.Error).Message = "preview vs full: " + err.(*errs.Error).Message
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Edits: edits}, err
	}

	resumeCfg := cfg
	resumeCfg.ParamsVersion = len(edits)
	resumeCfg.RunID = "threeway-resume"
	resumeCfg.InterruptAfter = 2
	resumeCfg.UseCache = false
	if dbPath == "" {
		dbPath = filepath.Join(os.TempDir(), fmt.Sprintf("roomreverb-threeway-%d.sqlite", os.Getpid()))
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Edits: edits}, errs.Wrap(errs.ECheck, err)
	}
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Edits: edits}, err
	}
	partial, firstErr := engine.RunWithCheckpoint(ctx, edited, store, resumeCfg)
	if firstErr != nil && !errs.IsCode(firstErr, errs.ECanceled) {
		_ = store.Close()
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Resume: partial, Edits: edits}, firstErr
	}
	if err := store.Close(); err != nil {
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Resume: partial, Edits: edits}, errs.Wrap(errs.ECheck, err)
	}
	store, err = checkpoint.Open(dbPath)
	if err != nil {
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Edits: edits}, err
	}
	defer store.Close()
	resumeCfg.InterruptAfter = 0
	resumed, err := engine.RunWithCheckpoint(ctx, edited, store, resumeCfg)
	if err != nil {
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Resume: resumed, Edits: edits}, err
	}
	if err := engine.Compare(full, resumed); err != nil {
		if typed, ok := err.(*errs.Error); ok {
			typed.Message = "resume vs full: " + typed.Message
		}
		return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Resume: resumed, Edits: edits}, err
	}
	return ThreeWayResult{Base: baseRun, Full: full, Preview: preview, Resume: resumed, Edits: edits}, nil
}

// VerifyTruth compares a computed run against the generator's analytic truth.
func VerifyTruth(level model.Level, truth model.Truth, run *engine.RunResult) error {
	if truth.AlgorithmVersion != model.AlgorithmVersion || truth.Precision != model.Precision {
		return &errs.Error{Code: errs.EDiff, Field: "algorithm_version", Message: fmt.Sprintf("truth version %s/%s does not match %s/%s", truth.AlgorithmVersion, truth.Precision, model.AlgorithmVersion, model.Precision)}
	}
	if run.AlgorithmVersion != model.AlgorithmVersion || run.Precision != model.Precision {
		return &errs.Error{Code: errs.EDiff, Field: "algorithm_version", Message: fmt.Sprintf("run version %s/%s does not match %s/%s", run.AlgorithmVersion, run.Precision, model.AlgorithmVersion, model.Precision)}
	}
	for _, l := range level.Listeners {
		room, ok := truth.ListenerRooms[l.ID]
		if !ok {
			return &errs.Error{Code: errs.EDiff, Listener: l.ID, Field: "listener_rooms", Message: "truth is missing listener room"}
		}
		found := ""
		for _, r := range level.Rooms {
			if r.Rect.ContainsInclusive(l.Pos) {
				found = r.ID
				break
			}
		}
		if found != room {
			return &errs.Error{Code: errs.EDiff, Room: room, Listener: l.ID, Field: "room_id", Message: fmt.Sprintf("want %s got %s", room, found)}
		}
	}
	sort.Slice(truth.Pairs, func(i, j int) bool {
		if truth.Pairs[i].ListenerID != truth.Pairs[j].ListenerID {
			return truth.Pairs[i].ListenerID < truth.Pairs[j].ListenerID
		}
		return truth.Pairs[i].SourceID < truth.Pairs[j].SourceID
	})
	for _, tp := range truth.Pairs {
		resultRoom := ""
		var pair solver.PairResult
		var ok bool
		for roomID, rr := range run.Rooms {
			if p, found := rr.Pair(tp.SourceID, tp.ListenerID); found {
				resultRoom, pair, ok = roomID, p, true
				break
			}
		}
		if !ok {
			return &errs.Error{Code: errs.EDiff, Room: truth.ListenerRooms[tp.ListenerID], Listener: tp.ListenerID, Source: tp.SourceID, Field: "pair", Message: "run has no result for truth pair"}
		}
		if tp.Reachable != pair.Reachable {
			return &errs.Error{Code: errs.EDiff, Room: resultRoom, Listener: tp.ListenerID, Source: tp.SourceID, Field: "reachable", Message: fmt.Sprintf("want %v got %v", tp.Reachable, pair.Reachable)}
		}
		if !tp.Reachable {
			continue
		}
		if fmt.Sprint(tp.RoomPath) != fmt.Sprint(pair.RoomPath) {
			return &errs.Error{Code: errs.EDiff, Room: resultRoom, Listener: tp.ListenerID, Source: tp.SourceID, Field: "room_path", Message: fmt.Sprintf("want %v got %v", tp.RoomPath, pair.RoomPath)}
		}
		if fmt.Sprint(tp.DoorPath) != fmt.Sprint(pair.PortalPath) {
			return &errs.Error{Code: errs.EDiff, Room: resultRoom, Listener: tp.ListenerID, Source: tp.SourceID, Field: "portal_path", Message: fmt.Sprintf("want %v got %v", tp.DoorPath, pair.PortalPath)}
		}
		if math.Abs(tp.PathMM-pair.PathMM) > truth.Tolerance.PathMM {
			return &errs.Error{Code: errs.EDiff, Room: resultRoom, Listener: tp.ListenerID, Source: tp.SourceID, Field: "path_mm", Message: fmt.Sprintf("want %.6f got %.6f", tp.PathMM, pair.PathMM)}
		}
		if math.Abs(tp.DecayMS-pair.DecayMS) > truth.Tolerance.DecayMS {
			return &errs.Error{Code: errs.EDiff, Room: resultRoom, Listener: tp.ListenerID, Source: tp.SourceID, Field: "decay_ms", Message: fmt.Sprintf("want %.6f got %.6f", tp.DecayMS, pair.DecayMS)}
		}
		if math.Abs(tp.EarlyEnergy-pair.EarlyEnergy) > truth.Tolerance.EarlyEnergy {
			return &errs.Error{Code: errs.EDiff, Room: resultRoom, Listener: tp.ListenerID, Source: tp.SourceID, Field: "early_energy", Message: fmt.Sprintf("want %.9f got %.9f", tp.EarlyEnergy, pair.EarlyEnergy)}
		}
	}
	return nil
}
