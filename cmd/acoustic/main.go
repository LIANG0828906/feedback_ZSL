package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"levelacoustic/internal/diag"
	"levelacoustic/internal/editing"
	"levelacoustic/internal/generator"
	"levelacoustic/internal/model"
	"levelacoustic/internal/solver"
	"levelacoustic/internal/store"
	"levelacoustic/internal/verify"
)

type engineFlags struct {
	workers       int
	maxBuffers    int
	bufferSamples int
	checkTruth    bool
	stopAfter     int
}

func (f *engineFlags) add(fs *flag.FlagSet) {
	fs.IntVar(&f.workers, "workers", 4, "worker count for listener synthesis")
	fs.IntVar(&f.maxBuffers, "buffers", 2, "maximum concurrent sample buffers")
	fs.IntVar(&f.bufferSamples, "buffer-samples", 64, "maximum samples per worker buffer")
	fs.BoolVar(&f.checkTruth, "check-truth", true, "compare solved room chains with generator truth")
	fs.IntVar(&f.stopAfter, "stop-after", 0, "simulate interruption after N room checkpoints")
}

func (f engineFlags) config(st *store.Store) solver.Config {
	return solver.Config{
		Store:            st,
		Workers:          f.workers,
		MaxBuffers:       f.maxBuffers,
		BufferSamples:    f.bufferSamples,
		CheckTruth:       f.checkTruth,
		StopAfterCommits: f.stopAfter,
	}
}

type solveEnvelope struct {
	OK         bool                 `json:"ok"`
	Result     *model.SolveResult   `json:"result,omitempty"`
	Restore    solver.RestoreReport `json:"restore"`
	StoreStats *store.StoreStats    `json:"store_stats,omitempty"`
	Error      *model.Error         `json:"error,omitempty"`
}

type editOpOutput struct {
	Index        int                       `json:"index"`
	Operation    string                    `json:"operation"`
	Invalidation *model.InvalidationReport `json:"invalidation"`
	Progress     string                    `json:"progress"`
	Committed    int                       `json:"committed_rooms"`
	Total        int                       `json:"total_rooms"`
	Recomputed   []string                  `json:"recomputed_rooms"`
	Reused       []string                  `json:"reused_rooms"`
	State        []model.RoomStateSnapshot `json:"room_states"`
}

type editEnvelope struct {
	OK           bool              `json:"ok"`
	Interrupted  bool              `json:"interrupted"`
	FinalProject string            `json:"final_project"`
	Operations   []editOpOutput    `json:"operations"`
	StoreStats   *store.StoreStats `json:"store_stats,omitempty"`
	Error        *model.Error      `json:"error,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "generate":
		return cmdGenerate(args[1:])
	case "validate":
		return cmdValidate(args[1:])
	case "solve":
		return cmdSolve(args[1:])
	case "edit":
		return cmdEdit(args[1:])
	case "resume":
		return cmdResume(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "version":
		fmt.Printf("levelacoustic algorithm=%s parameter=%s go=%s\n", model.AlgorithmVersion, model.ParameterVersion, runtime.Version())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: acoustic <command> [flags]

commands:
  generate   generate deterministic sample project and edit plan
  validate   validate schema and geometry
  solve      restore checkpoints and solve pending rooms
  edit       apply deterministic edits incrementally
  resume     restore committed rooms and continue after interruption
  verify     compare truth, full recompute and incremental cache
  serve      start loopback diagnostic page
  version    print algorithm and Go version`)
}

func cmdGenerate(args []string) int {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", "data/sample.json", "sample project output")
	editsOut := fs.String("edits", "data/edits.json", "edit plan output")
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sample, err := generator.Generate(*seed)
	if err != nil {
		return fail(err)
	}
	if err := writeJSON(*out, sample.Project); err != nil {
		return fail(err)
	}
	if err := writeJSON(*editsOut, sample.Edits); err != nil {
		return fail(err)
	}
	fmt.Printf("generated rooms=%d sources=%d listeners=%d truth_pairs=%d seed=%d\n",
		len(sample.Project.Rooms), len(sample.Project.Sources), len(sample.Project.Listeners),
		len(sample.Project.Truth.Entries), *seed)
	fmt.Printf("project=%s edits=%s\n", *out, *editsOut)
	return 0
}

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	input := fs.String("input", "data/sample.json", "project input")
	requireTruth := fs.Bool("require-truth", true, "require truth checklist")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	project, err := loadProject(*input, *requireTruth)
	if err != nil {
		return fail(err)
	}
	if err := model.ValidateProject(project, model.ValidateOptions{RequireTruth: *requireTruth}); err != nil {
		return fail(err)
	}
	fmt.Printf("valid schema=%s rooms=%d walls=%d doors=%d sources=%d listeners=%d\n",
		project.SchemaVersion, len(project.Rooms), len(project.Walls), len(project.Doors),
		len(project.Sources), len(project.Listeners))
	return 0
}

func cmdSolve(args []string) int {
	fs := flag.NewFlagSet("solve", flag.ContinueOnError)
	input := fs.String("input", "data/sample.json", "project input")
	db := fs.String("db", "data/acoustic.db", "SQLite checkpoint path")
	out := fs.String("out", "artifacts/solve.json", "solve output")
	full := fs.Bool("full", false, "delete checkpoints and solve from empty cache")
	var ef engineFlags
	ef.add(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	project, err := loadProject(*input, true)
	if err != nil {
		return fail(err)
	}
	if *full {
		if err := removeSQLite(*db); err != nil {
			return fail(err)
		}
	}
	st, err := store.Open(*db)
	if err != nil {
		return fail(err)
	}
	eng, err := solver.NewEngine(context.Background(), project, ef.config(st))
	if err != nil {
		_ = st.Close()
		return fail(err)
	}
	result, solveErr := eng.Solve(context.Background())
	stats := st.Stats()
	envelope := solveEnvelope{
		OK:         solveErr == nil,
		Result:     result,
		Restore:    eng.Restore(),
		StoreStats: &stats,
	}
	if solveErr != nil {
		envelope.Error = asModelError(solveErr)
	}
	_ = st.Close()
	if err := writeJSON(*out, envelope); err != nil {
		return fail(err)
	}
	if result != nil {
		fmt.Printf("solve progress=%s recomputed=%v reused=%v\n", result.Progress, result.RecomputedRooms, result.ReusedRooms)
		fmt.Printf("resources max_parallel=%d buffers=%d buffer_samples=%d jobs=%d queued=%d wait_us=%d\n",
			result.Resources.MaxParallel, result.Resources.BufferCount, result.Resources.MaxBufferSamples,
			result.Resources.Jobs, result.Resources.QueuedJobs, result.Resources.TotalWaitMicros)
	}
	fmt.Printf("checkpoints writes=%d output=%s\n", stats.Writes, *out)
	if solveErr != nil && asModelError(solveErr).Code != model.ErrInterrupted {
		fmt.Fprintln(os.Stderr, errorJSON(envelope.Error))
		return 1
	}
	if solveErr != nil {
		fmt.Printf("interrupted=%v (resume from committed rooms)\n", true)
	}
	return 0
}

func cmdEdit(args []string) int {
	fs := flag.NewFlagSet("edit", flag.ContinueOnError)
	input := fs.String("input", "data/sample.json", "base project")
	editsPath := fs.String("edits", "data/edits.json", "edit plan")
	db := fs.String("db", "data/acoustic.db", "SQLite checkpoint path")
	projectOut := fs.String("project-out", "data/edited.json", "final edited project")
	out := fs.String("out", "artifacts/edit.json", "edit output")
	crashAfter := fs.Int("crash-after", 0, "simulate crash after N final checkpoints")
	var ef engineFlags
	ef.add(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	base, err := loadProject(*input, true)
	if err != nil {
		return fail(err)
	}
	plan, err := loadEditPlan(*editsPath)
	if err != nil {
		return fail(err)
	}
	st, err := store.Open(*db)
	if err != nil {
		return fail(err)
	}
	eng, err := solver.NewEngine(context.Background(), base, ef.config(st))
	if err != nil {
		_ = st.Close()
		return fail(err)
	}
	if _, err := eng.Solve(context.Background()); err != nil {
		_ = st.Close()
		return fail(err)
	}

	envelope := editEnvelope{OK: true, FinalProject: *projectOut}
	current := base
	interrupted := false
	for i, op := range plan.Ops {
		next, err := editing.ApplyOne(current, op)
		if err != nil {
			_ = st.Close()
			return fail(err)
		}
		report, err := eng.ApplyProject(context.Background(), next, op.Op)
		if err != nil {
			_ = st.Close()
			return fail(err)
		}
		current = next
		if err := writeJSON(*projectOut, current); err != nil {
			_ = st.Close()
			return fail(err)
		}
		limit := 0
		if i == len(plan.Ops)-1 && *crashAfter > 0 {
			limit = *crashAfter
		}
		var result *model.SolveResult
		if limit > 0 {
			result, err = eng.SolveWithLimit(context.Background(), limit)
		} else {
			result, err = eng.Solve(context.Background())
		}
		if result != nil {
			envelope.Operations = append(envelope.Operations, editOpOutput{
				Index:        i,
				Operation:    op.Op,
				Invalidation: report,
				Progress:     result.Progress,
				Committed:    result.CommittedRooms,
				Total:        result.TotalRooms,
				Recomputed:   result.RecomputedRooms,
				Reused:       result.ReusedRooms,
				State:        result.RoomStates,
			})
			fmt.Printf("edit[%d] %-18s invalidated=%v reused=%v progress=%s\n",
				i, op.Op, report.InvalidatedRooms, report.ReusedRooms, result.Progress)
		}
		if err != nil {
			me := asModelError(err)
			envelope.Error = me
			if me != nil && me.Code == model.ErrInterrupted {
				interrupted = true
				break
			}
			envelope.OK = false
			stats := st.Stats()
			envelope.StoreStats = &stats
			_ = st.Close()
			_ = writeJSON(*out, envelope)
			fmt.Fprintln(os.Stderr, errorJSON(me))
			return 1
		}
	}
	envelope.Interrupted = interrupted
	stats := st.Stats()
	envelope.StoreStats = &stats
	_ = st.Close()
	if err := writeJSON(*out, envelope); err != nil {
		return fail(err)
	}
	fmt.Printf("edited_project=%s output=%s interrupted=%v\n", *projectOut, *out, interrupted)
	return 0
}

func cmdResume(args []string) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	input := fs.String("input", "data/edited.json", "edited project input")
	db := fs.String("db", "data/acoustic.db", "SQLite checkpoint path")
	out := fs.String("out", "artifacts/resume.json", "resume output")
	var ef engineFlags
	ef.add(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	project, err := loadProject(*input, true)
	if err != nil {
		return fail(err)
	}
	st, err := store.Open(*db)
	if err != nil {
		return fail(err)
	}
	eng, err := solver.NewEngine(context.Background(), project, ef.config(st))
	if err != nil {
		_ = st.Close()
		return fail(err)
	}
	restore := eng.Restore()
	result, solveErr := eng.Solve(context.Background())
	stats := st.Stats()
	envelope := solveEnvelope{
		OK:         solveErr == nil,
		Result:     result,
		Restore:    restore,
		StoreStats: &stats,
	}
	if solveErr != nil {
		envelope.Error = asModelError(solveErr)
	}
	_ = st.Close()
	if err := writeJSON(*out, envelope); err != nil {
		return fail(err)
	}
	fmt.Printf("restore rows=%d reused=%v stale=%v discarded=%v\n",
		restore.HadRows, restore.ReusedRooms, restore.StaleRooms, restore.Discarded)
	if result != nil {
		fmt.Printf("resume progress=%s recomputed=%v reused=%v output=%s\n",
			result.Progress, result.RecomputedRooms, result.ReusedRooms, *out)
	}
	if solveErr != nil {
		fmt.Fprintln(os.Stderr, errorJSON(envelope.Error))
		return 1
	}
	return 0
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	basePath := fs.String("base", "data/sample.json", "base project")
	editsPath := fs.String("edits", "data/edits.json", "edit plan")
	db := fs.String("db", "data/acoustic.db", "incremental SQLite checkpoint path")
	out := fs.String("out", "artifacts/verify.json", "verification output")
	requireClean := fs.Bool("require-clean-restore", true, "require all persisted rooms to restore cleanly")
	var ef engineFlags
	ef.add(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	base, err := loadProject(*basePath, true)
	if err != nil {
		return fail(err)
	}
	plan, err := loadEditPlan(*editsPath)
	if err != nil {
		return fail(err)
	}
	cfg := ef.config(nil)
	cfg.StopAfterCommits = 0
	report, verifyErr := verify.Run(context.Background(), base, plan, *db, cfg, *requireClean)
	if report != nil {
		report.Seed = base.Seed
	}
	if err := writeJSON(*out, report); err != nil {
		return fail(err)
	}
	if report != nil {
		fmt.Println(report.Summary())
		fmt.Printf("truth=%s\nfull=%s\nincremental=%s\noutput=%s\n",
			report.TruthDigest, report.FullDigest, report.IncrementalDigest, *out)
	}
	if verifyErr != nil {
		fmt.Fprintln(os.Stderr, errorJSON(report.FirstError))
		return 1
	}
	return 0
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	input := fs.String("input", "data/edited.json", "project input")
	db := fs.String("db", "data/acoustic.db", "SQLite checkpoint path")
	projectOut := fs.String("project-out", "data/served.json", "project saved after edits")
	addr := fs.String("addr", "127.0.0.1:8089", "loopback diagnostic address")
	var ef engineFlags
	ef.add(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	project, err := loadProject(*input, true)
	if err != nil {
		return fail(err)
	}
	st, err := store.Open(*db)
	if err != nil {
		return fail(err)
	}
	eng, err := solver.NewEngine(context.Background(), project, ef.config(st))
	if err != nil {
		_ = st.Close()
		return fail(err)
	}
	if _, err := eng.Solve(context.Background()); err != nil {
		_ = st.Close()
		return fail(err)
	}
	srv := diag.New(eng, *projectOut)
	fmt.Printf("diagnostic page listening on http://%s/\n", *addr)
	err = srv.ListenAndServe(*addr)
	_ = st.Close()
	if err != nil {
		return fail(err)
	}
	return 0
}

func loadProject(path string, requireTruth bool) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project %s: %w", path, err)
	}
	return model.DecodeProject(data, requireTruth)
}

func loadEditPlan(path string) (model.EditPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.EditPlan{}, fmt.Errorf("read edit plan %s: %w", path, err)
	}
	var plan model.EditPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return model.EditPlan{}, model.SchemaError("edit_plan", "decode %s: %v", path, err)
	}
	if len(plan.Ops) == 0 {
		return model.EditPlan{}, model.SchemaError("edit_plan.ops", "edit plan must contain at least one operation")
	}
	return plan, nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write([]byte("\n")); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		_ = os.Remove(path)
	}
	return os.Rename(tmpName, path)
}

func removeSQLite(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		err := os.Remove(path + suffix)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func asModelError(err error) *model.Error {
	if err == nil {
		return nil
	}
	if me, ok := model.AsError(err); ok {
		return me
	}
	return model.NewError(model.ErrIO, "%v", err)
}

func fail(err error) int {
	me := asModelError(err)
	fmt.Fprintln(os.Stderr, errorJSON(me))
	return 1
}

func errorJSON(me *model.Error) string {
	if me == nil {
		return `{"code":"E_IO","message":"unknown error"}`
	}
	data, _ := json.Marshal(me)
	return string(data)
}
