package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"qosshape.local/gateway/qos"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		if qe, ok := err.(*qos.Error); ok {
			raw, _ := json.Marshal(qe)
			fmt.Fprintln(os.Stderr, string(raw))
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "demo"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "demo":
		return runDemo(args)
	case "generate":
		return runGenerate(args)
	case "inject":
		return runInject(args)
	case "run", "resume":
		return runCheckpoint(args, command == "resume")
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", command, usage)
	}
}

const usage = `qosdemo commands:
  generate --seed 42 --out artifacts/demo
  demo     --seed 42 --out artifacts/demo
  inject   --seed 42 --out artifacts/demo/injections
  run      --schema 1 --seed 42 --checkpoint artifacts/demo/run.db [--crash-after 4]
  resume   --schema 1 --seed 42 --checkpoint artifacts/demo/run.db
`

func commonFlags(args []string) (*flag.FlagSet, *int64, *string, error) {
	fs := flag.NewFlagSet("qosdemo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	out := fs.String("out", "artifacts/demo", "artifact directory")
	if err := fs.Parse(args); err != nil {
		return nil, nil, nil, err
	}
	return fs, seed, out, nil
}

func runGenerate(args []string) error {
	_, seed, out, err := commonFlags(args)
	if err != nil {
		return err
	}
	manifest, err := qos.GenerateSample(context.Background(), *seed, *out)
	if err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Println(string(raw))
	return nil
}

func runInject(args []string) error {
	_, seed, out, err := commonFlags(args)
	if err != nil {
		return err
	}
	report, err := qos.RunInjectionCases(context.Background(), *seed, *out)
	if err != nil {
		return err
	}
	printInjections(report)
	if !report.AllPass {
		return fmt.Errorf("one or more injection cases did not return the expected stable code")
	}
	return nil
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("qosdemo-demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic sample seed")
	out := fs.String("out", "artifacts/demo", "artifact directory")
	checkpointPath := fs.String("checkpoint", "", "checkpoint path (default <out>/demo-checkpoint.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *checkpointPath == "" {
		*checkpointPath = filepath.Join(*out, "demo-checkpoint.db")
	}
	ctx := context.Background()
	manifest, err := qos.GenerateSample(ctx, *seed, *out)
	if err != nil {
		return err
	}
	fmt.Printf("sample seed=%d version=%s digest=%s steps=%d emitted=%d delayed=%d dropped=%d expired=%d\n",
		manifest.Seed, manifest.ProjectVersion, manifest.ProjectDigest[:16], manifest.Steps,
		manifest.Emitted, manifest.DelayedEvents, manifest.Dropped, manifest.Expired)

	projects := make([]*qos.Project, 0, 3)
	results := make([]*qos.RunResult, 0, 3)
	for version := 1; version <= 3; version++ {
		path := filepath.Join(*out, fmt.Sprintf("sample_v%d.json", version))
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		project, err := qos.LoadProject(raw)
		if err != nil {
			return err
		}
		projects = append(projects, project)
		eng, err := qos.NewEngine(project)
		if err != nil {
			return err
		}
		result, err := eng.Run(ctx)
		if err != nil {
			return err
		}
		results = append(results, result)
		truthRaw, err := os.ReadFile(filepath.Join(*out, fmt.Sprintf("truth_v%d.json", version)))
		if err != nil {
			return err
		}
		truth, err := qos.DecodeTruth(truthRaw, project)
		if err != nil {
			return err
		}
		if !stepsEquivalent(truth.Steps, result.Steps) {
			return fmt.Errorf("truth_v%d does not match engine output", version)
		}
		fmt.Printf("load v%d channels=%v digest=%s truth=OK\n", version, project.ChannelOrder(), shortDigest(project))
	}
	if err := compareProjects(projects); err != nil {
		return err
	}
	for i := 1; i < len(results); i++ {
		if !reflect.DeepEqual(results[0].Emissions, results[i].Emissions) ||
			!reflect.DeepEqual(results[0].Drops, results[i].Drops) ||
			!reflect.DeepEqual(results[0].FinalTokens, results[i].FinalTokens) {
			return fmt.Errorf("v1 and v%d execution differ", i+1)
		}
	}
	fmt.Println("cross-version normalized channels, emissions, drops and token balances: OK")
	fmt.Println("--- per-time trace (canonical v1 timing) ---")
	for _, st := range results[0].Steps {
		printStep(st)
	}

	checkpointReport, err := demoCheckpoint(ctx, projects[0], results[0], *checkpointPath)
	if err != nil {
		return err
	}
	incrementalReport, err := demoIncremental(ctx, projects[0])
	if err != nil {
		return err
	}
	injections, err := qos.RunInjectionCases(ctx, *seed, filepath.Join(*out, "injection-work"))
	if err != nil {
		return err
	}
	printInjections(injections)
	if !injections.AllPass {
		return fmt.Errorf("injection cases failed")
	}

	report := map[string]any{
		"sample":      manifest,
		"checkpoint":  checkpointReport,
		"incremental": incrementalReport,
		"injections":  injections,
	}
	raw, _ := json.MarshalIndent(report, "", "  ")
	reportPath := filepath.Join(*out, "demo_report.json")
	if err := os.WriteFile(reportPath, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("demo report: %s\n", reportPath)
	return nil
}

func demoCheckpoint(ctx context.Context, project *qos.Project, full *qos.RunResult, path string) (map[string]any, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	store, err := qos.OpenCheckpoint(path)
	if err != nil {
		return nil, err
	}
	eng, err := qos.NewEngine(project)
	if err != nil {
		store.Close()
		return nil, err
	}
	const commitSteps = 5
	prefix := make([]qos.StepResult, 0, commitSteps)
	for i := 0; i < commitSteps && !eng.Done(); i++ {
		st, err := eng.Step()
		if err != nil {
			store.Close()
			return nil, err
		}
		prefix = append(prefix, st)
		if err := store.Save(ctx, project, eng.Snapshot()); err != nil {
			store.Close()
			return nil, err
		}
	}
	if err := store.Close(); err != nil {
		return nil, err
	}
	store, err = qos.OpenCheckpoint(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	snap, err := store.Load(ctx, project)
	if err != nil {
		return nil, err
	}
	fmt.Printf("checkpoint committed next_step=%d next_ms=%d queue=%d channels=%d\n",
		snap.NextStep, snap.NextMS, len(snap.Delays), len(snap.Channels))
	resumed, status, err := qos.ResumeEngine(ctx, store, project)
	if err != nil {
		return nil, err
	}
	rest, err := resumed.Run(ctx)
	if err != nil {
		return nil, err
	}
	steps := append(append([]qos.StepResult(nil), prefix...), rest.Steps...)
	ok := reflect.DeepEqual(full.Emissions, rest.Emissions) &&
		reflect.DeepEqual(full.Drops, rest.Drops) &&
		reflect.DeepEqual(full.FinalTokens, rest.FinalTokens) &&
		stepsEquivalent(full.Steps, steps)
	fmt.Printf("checkpoint resume status=%s final_equal=%v\n", status, ok)
	return map[string]any{
		"path": path, "status": status, "committed_next_step": snap.NextStep,
		"delayed_in_checkpoint": len(snap.Delays), "final_equal": ok,
	}, nil
}

func demoIncremental(ctx context.Context, project *qos.Project) (map[string]any, error) {
	inc, err := qos.NewIncremental(ctx, project)
	if err != nil {
		return nil, err
	}
	prefixTokens := make([]string, 0, 4)
	for i := 0; i < 4 && i < len(inc.Trace.Steps); i++ {
		prefixTokens = append(prefixTokens, inc.Trace.Steps[i].CacheToken)
	}
	rate := int64(90000)
	_, report, err := inc.Apply(ctx, qos.Edit{
		SourceID: "demo-edit-rate-gold", AtMS: 40, ChannelID: "gold",
		RatePermyriad: &rate,
	})
	if err != nil {
		return nil, err
	}
	reusedPreserved := len(prefixTokens) <= len(inc.Trace.Steps)
	for i := 0; reusedPreserved && i < len(prefixTokens); i++ {
		reusedPreserved = inc.Trace.Steps[i].CacheToken == prefixTokens[i] && inc.Trace.Steps[i].Reused
	}
	raw, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println("--- incremental invalidation ---")
	fmt.Println(string(raw))
	fmt.Printf("incremental prefix cache preserved=%v\n", reusedPreserved)
	reportMap := map[string]any{}
	_ = json.Unmarshal(raw, &reportMap)
	reportMap["prefix_cache_preserved"] = reusedPreserved
	return reportMap, nil
}

func runCheckpoint(args []string, resume bool) error {
	fs := flag.NewFlagSet("qosdemo-run", flag.ContinueOnError)
	schema := fs.Int("schema", 1, "sample schema version (1..3)")
	seed := fs.Int64("seed", 42, "sample seed")
	out := fs.String("out", "artifacts/demo", "artifact directory")
	path := fs.String("checkpoint", "artifacts/demo/run.db", "SQLite checkpoint path")
	crashAfter := fs.Int("crash-after", 0, "SIGKILL the process after this many committed steps")
	resumeFlag := fs.Bool("resume", resume, "resume from checkpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resume = resume || *resumeFlag
	if *schema < 1 || *schema > 3 {
		return fmt.Errorf("schema must be 1..3")
	}
	project, err := loadOrGenerate(*seed, *schema, *out)
	if err != nil {
		return err
	}
	if !resume {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(*path + suffix)
		}
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o755); err != nil {
		return err
	}
	store, err := qos.OpenCheckpoint(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	var eng *qos.Engine
	if resume {
		eng, _, err = qos.ResumeEngine(context.Background(), store, project)
		if err != nil {
			if qos.CodeOf(err) == qos.ECheckpointVersion && eng != nil {
				fmt.Fprintf(os.Stderr, "checkpoint restart: %v\n", err)
			} else {
				return err
			}
		}
	} else {
		eng, err = qos.NewEngine(project)
		if err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	committed := 0
	for !eng.Done() {
		select {
		case <-ctx.Done():
			if err := store.Save(context.Background(), project, eng.Snapshot()); err != nil {
				return err
			}
			fmt.Printf("signal received; checkpoint saved at next_step=%d\n", eng.NextStep())
			return nil
		default:
		}
		st, err := eng.Step()
		if err != nil {
			return err
		}
		if err := store.Save(ctx, project, eng.Snapshot()); err != nil {
			return err
		}
		committed++
		if *crashAfter > 0 && committed >= *crashAfter {
			fmt.Printf("simulating hard kill after committed step=%d next_step=%d\n", st.Index, eng.NextStep())
			os.Stdout.Sync()
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	}
	fmt.Printf("run complete committed_steps=%d next_step=%d unresolved=%d\n", committed, eng.NextStep(), 0)
	return nil
}

func loadOrGenerate(seed int64, schema int, out string) (*qos.Project, error) {
	path := filepath.Join(out, fmt.Sprintf("sample_v%d.json", schema))
	if raw, err := os.ReadFile(path); err == nil {
		p, err := qos.LoadProject(raw)
		if err == nil && p.Version == fmt.Sprintf("gen-%d-r1", seed) {
			return p, nil
		}
	}
	p := qos.Scenario(seed)
	p.SchemaVersion = schema
	raw, err := qos.EncodeProject(p, schema)
	if err != nil {
		return nil, err
	}
	return qos.LoadProject(raw)
}

func compareProjects(projects []*qos.Project) error {
	if len(projects) == 0 {
		return nil
	}
	base := projects[0]
	baseDigest, _ := base.CanonicalDigest()
	for i := 1; i < len(projects); i++ {
		digest, _ := projects[i].CanonicalDigest()
		if digest != baseDigest {
			return fmt.Errorf("project digest mismatch for v%d", i+1)
		}
		if !reflect.DeepEqual(base.ChannelOrder(), projects[i].ChannelOrder()) {
			return fmt.Errorf("channel order mismatch for v%d", i+1)
		}
		for j := range base.Channels {
			a, b := base.Channels[j], projects[i].Channels[j]
			if a.ID != b.ID || a.Order != b.Order || a.RatePermyriad != b.RatePermyriad ||
				a.CapacityBytes != b.CapacityBytes || a.InitialTokens != b.InitialTokens ||
				a.WeightPermyriad != b.WeightPermyriad || a.QueueAgeMS != b.QueueAgeMS {
				return fmt.Errorf("normalized channel mismatch for v%d channel %s", i+1, a.ID)
			}
		}
	}
	return nil
}

func shortDigest(p *qos.Project) string {
	d, _ := p.CanonicalDigest()
	if len(d) > 16 {
		return d[:16]
	}
	return d
}

func stepsEquivalent(a, b []qos.StepResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if qos.TruthStepDigest(a[i]) != qos.TruthStepDigest(b[i]) {
			return false
		}
	}
	return true
}

func printStep(st qos.StepResult) {
	tokens := make([]string, 0, len(st.Tokens))
	for _, t := range st.Tokens {
		tokens = append(tokens, fmt.Sprintf("%s=%d", t.ChannelID, t.TokensBytes))
	}
	delayed := make([]string, 0, len(st.Delayed))
	for _, d := range st.Delayed {
		delayed = append(delayed, fmt.Sprintf("%s(+%dms)", d.PacketID, d.DelayMS))
	}
	dropped := make([]string, 0, len(st.Dropped))
	for _, d := range st.Dropped {
		dropped = append(dropped, fmt.Sprintf("%s(%s)", d.PacketID, d.Reason))
	}
	replayed := make([]string, 0, len(st.Replayed))
	for _, r := range st.Replayed {
		replayed = append(replayed, fmt.Sprintf("%s#%d", r.PacketID, r.ReplaySeq))
	}
	expired := make([]string, 0, len(st.Expired))
	for _, e := range st.Expired {
		expired = append(expired, fmt.Sprintf("%s@%dms", e.PacketID, e.DeadlineMS))
	}
	emitted := make([]string, 0, len(st.Emitted))
	for _, e := range st.Emitted {
		emitted = append(emitted, fmt.Sprintf("%s/%s/%dB/%s", e.PacketID, e.ChannelID, e.SizeBytes, e.WeightBasisID))
	}
	queue := make([]string, 0, len(st.Queue))
	for _, q := range st.Queue {
		queue = append(queue, fmt.Sprintf("%s@%dms(rem=%d,seq=%d)", q.PacketID, q.ExpireAtMS, q.RemainingMS, q.ReplaySeq))
	}
	suppressed := make([]string, 0, len(st.Suppressed))
	for _, s := range st.Suppressed {
		suppressed = append(suppressed, fmt.Sprintf("%s(%dB,%s)", s.ChannelID, s.PendingBytes, s.WeightBasisID))
	}
	fmt.Printf("t=%3dms tick=%3d tokens[%s] emit[%s] defer[%s] replay[%s] drop[%s] expire[%s] queue[%s] suppressed[%s]\n",
		st.AtMS, st.AtTick, strings.Join(tokens, ","), strings.Join(emitted, ","),
		strings.Join(delayed, ","), strings.Join(replayed, ","), strings.Join(dropped, ","),
		strings.Join(expired, ","),
		strings.Join(queue, ","), strings.Join(suppressed, ","))
}

func printInjections(report *qos.InjectionReport) {
	fmt.Println("--- anomaly injection ---")
	for _, c := range report.Cases {
		fmt.Printf("%-30s code=%-24s path=%-38s source_unchanged=%v\n", c.Name, c.Code, c.Path, c.SourceStable)
	}
	fmt.Printf("injection all_pass=%v\n", report.AllPass)
}
