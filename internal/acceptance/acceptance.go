package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"roomreverb/internal/config"
	"roomreverb/internal/diff"
	"roomreverb/internal/engine"
	"roomreverb/internal/levelio"
	"roomreverb/internal/model"
	"roomreverb/internal/sample"
	"roomreverb/internal/sched"
	"roomreverb/internal/store"
)

type PathAccuracy struct {
	Key           string   `json:"key"`
	RoomChain     []string `json:"room_chain"`
	TruthChain    []string `json:"truth_chain"`
	ChainOK       bool     `json:"chain_ok"`
	Length        float64  `json:"length"`
	TruthLength   float64  `json:"truth_length"`
	LengthError   float64  `json:"length_error"`
	MaxCurveError float64  `json:"max_curve_error"`
	MaxSPLError   float64  `json:"max_spl_error"`
	WallClear     bool     `json:"wall_clear"`
}

type PrecisionReport struct {
	Profile       string  `json:"profile"`
	Decimals      int     `json:"decimals"`
	MaxCurveError float64 `json:"max_curve_error"`
	MaxSPLError   float64 `json:"max_spl_error"`
}

type Report struct {
	WorkDir               string            `json:"work_dir"`
	DatabasePath          string            `json:"database_path"`
	LevelPath             string            `json:"level_path"`
	PathChecks            []PathAccuracy    `json:"path_checks"`
	Precision             []PrecisionReport `json:"precision"`
	ThreeListenerL1Paths  []string          `json:"three_listener_l1_paths"`
	IncrementalCompareOK  bool              `json:"incremental_compare_ok"`
	RecoveryCompareOK     bool              `json:"recovery_compare_ok"`
	RecoveredRooms        int               `json:"recovered_rooms"`
	ScopeErrorCode        string            `json:"scope_error_code,omitempty"`
	ScopeFirstRoom        string            `json:"scope_first_room,omitempty"`
	SchedulerWaitRecorded bool              `json:"scheduler_wait_recorded"`
	SchedulerWaitedMS     int64             `json:"scheduler_waited_ms,omitempty"`
}

func Run() (*Report, *model.Error) {
	cfg := config.Default()
	dir, err := os.MkdirTemp("", "roomreverb-acceptance-")
	if err != nil {
		return nil, model.NewError(model.CodeSchema, "tempdir", err.Error())
	}
	cfg.DatabasePath = filepath.Join(dir, "checkpoint.sqlite")
	cfg.LevelPath = filepath.Join(dir, "level.json")
	cfg.MaxParallelism = 4
	cfg.SampleBufferLimit = 64

	level := sample.Generate(cfg.Seed)
	data, _ := json.MarshalIndent(level, "", "  ")
	if err := os.WriteFile(cfg.LevelPath, data, 0o644); err != nil {
		return nil, model.NewError(model.CodeSchema, cfg.LevelPath, err.Error())
	}

	full, merr := engine.ComputeFull(level, cfg, nil)
	if merr != nil {
		return nil, asError(merr)
	}
	if merr := engine.VerifyTruths(level, full, cfg); merr != nil {
		return nil, merr
	}

	report := &Report{WorkDir: dir, DatabasePath: cfg.DatabasePath, LevelPath: cfg.LevelPath}
	truthByKey := map[string]model.PathTruth{}
	for _, truth := range level.Truth.Paths {
		truthByKey[truth.SourceID+"->"+truth.ListenerID] = truth
	}
	for _, path := range full.Paths {
		truth := truthByKey[path.Key]
		check := PathAccuracy{
			Key: path.Key, RoomChain: path.RoomChain, TruthChain: truth.RoomChain,
			ChainOK: equalStrings(path.RoomChain, truth.RoomChain), WallClear: path.WallClear,
			Length: path.Length, TruthLength: truth.Length, LengthError: abs(path.Length - truth.Length),
		}
		for i := range path.CurveDB {
			check.MaxCurveError = maxFloat(check.MaxCurveError, abs(path.CurveDB[i]-truth.CurveDB[i]))
		}
		check.MaxSPLError = abs(path.SPLDB - truth.SPLDB)
		report.PathChecks = append(report.PathChecks, check)
		if path.ListenerID == "L1" {
			report.ThreeListenerL1Paths = append(report.ThreeListenerL1Paths, path.Key)
		}
	}

	for _, profileName := range []string{"coarse", "standard", "tight"} {
		precisionCfg := cfg
		precisionCfg.PrecisionProfile = profileName
		precisionFull, err := engine.ComputeFull(level, precisionCfg, nil)
		if err != nil {
			return report, asError(err)
		}
		pr := PrecisionReport{Profile: profileName, Decimals: precisionCfg.Profiles[profileName].Decimals}
		for _, path := range precisionFull.Paths {
			truth := truthByKey[path.Key]
			for i := range path.CurveDB {
				pr.MaxCurveError = maxFloat(pr.MaxCurveError, abs(path.CurveDB[i]-truth.CurveDB[i]))
			}
			pr.MaxSPLError = maxFloat(pr.MaxSPLError, abs(path.SPLDB-truth.SPLDB))
		}
		report.Precision = append(report.Precision, pr)
	}
	if len(report.Precision) == 3 {
		if report.Precision[1].MaxCurveError > report.Precision[0].MaxCurveError ||
			report.Precision[2].MaxCurveError > report.Precision[1].MaxCurveError {
			return report, model.NewError(model.CodePrec, "precision_profiles", "curve maximum error is not monotonic")
		}
		if report.Precision[1].MaxSPLError > report.Precision[0].MaxSPLError ||
			report.Precision[2].MaxSPLError > report.Precision[1].MaxSPLError {
			return report, model.NewError(model.CodePrec, "precision_profiles", "SPL maximum error is not monotonic")
		}
	}

	st, merr := store.Open(cfg.DatabasePath)
	if merr != nil {
		return report, model.NewError(model.CodeSchema, "sqlite", merr.Error())
	}
	eng := engine.New(cfg, level, st)
	if _, merr := eng.RunBaseline(); merr != nil {
		_ = st.Close()
		return report, asError(merr)
	}
	rt60 := 1.37
	plan, updateErr := eng.UpdateRoomParams("R2", engine.RoomParamsEdit{RT60Seconds: &rt60})
	if updateErr != nil {
		_ = st.Close()
		return report, updateErr
	}
	incremental, recomputeErr := eng.RecomputeWithPlan(plan)
	if recomputeErr != nil {
		_ = st.Close()
		return report, recomputeErr
	}
	editedLevel := eng.Level()
	freshEdited, cmpErr := engine.CompareIncrementalAndFull(editedLevel, cfg, incremental)
	if cmpErr != nil {
		_ = st.Close()
		return report, cmpErr
	}
	report.IncrementalCompareOK = true

	scopeErr := eng.ScopeCheck([]string{"R1", "R2", "R3", "R4", "R5", "R6"})
	if scopeErr != nil {
		report.ScopeErrorCode = scopeErr.Code
		report.ScopeFirstRoom = scopeErr.Ref
	}
	if err := st.Close(); err != nil {
		return report, model.NewError(model.CodeSchema, "sqlite-close", err.Error())
	}

	reloaded, lerr := levelio.LoadFile(cfg.LevelPath)
	if lerr != nil {
		return report, asError(lerr)
	}
	st2, oerr := store.Open(cfg.DatabasePath)
	if oerr != nil {
		return report, model.NewError(model.CodeSchema, "sqlite-reopen", oerr.Error())
	}
	eng2 := engine.New(cfg, reloaded, st2)
	recovered, invalid, recoverErr := eng2.Recover()
	if recoverErr != nil {
		_ = st2.Close()
		return report, recoverErr
	}
	if invalid {
		_ = st2.Close()
		return report, model.NewError(model.CodeSchema, "recovery", "valid checkpoint was incorrectly invalidated")
	}
	report.RecoveredRooms = recovered
	recoveryResult := eng2.Snapshot().Result
	if derr := diff.CompareFullResults(freshEdited, recoveryResult); derr != nil {
		_ = st2.Close()
		return report, derr
	}
	report.RecoveryCompareOK = true
	_ = st2.Close()

	event, ok := checkSchedulerBlocking()
	report.SchedulerWaitRecorded = ok
	if ok {
		report.SchedulerWaitedMS = event.WaitedMS
	}
	return report, nil
}

func checkSchedulerBlocking() (sched.QueueEvent, bool) {
	pool := sched.NewPool(1, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = pool.Invoke(sched.NamedTask{ID: "blocker", Fn: func() error {
			close(entered)
			<-release
			return nil
		}})
	}()
	<-entered
	go func() { _ = pool.Invoke(sched.NamedTask{ID: "buffered", Fn: func() error { return nil }}) }()
	waitForBuffer()
	done3 := make(chan error, 1)
	go func() {
		done3 <- pool.Invoke(sched.NamedTask{ID: "blocked-sample-task", Fn: func() error { return nil }})
	}()
	waitForBuffer()
	close(release)
	<-done3
	events := pool.RecordedEvents()
	for _, event := range events {
		if event.TaskID == "blocked-sample-task" && event.Reason == "per_worker_sample_buffer_full" {
			return event, true
		}
	}
	return sched.QueueEvent{}, false
}

func waitForBuffer() {
	// A short deterministic yield is sufficient in this local acceptance check;
	// no result is discarded and the blocking event remains observable.
	time.Sleep(75 * time.Millisecond)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func asError(err error) *model.Error {
	if merr, ok := err.(*model.Error); ok {
		return merr
	}
	return model.NewError(model.CodeSchema, "compute", err.Error())
}
