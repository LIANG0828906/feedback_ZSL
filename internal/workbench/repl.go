package workbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/checkpoint"
	"sre-timeline/internal/concurrency"
	"sre-timeline/internal/engine"
	"sre-timeline/internal/model"
	"sre-timeline/internal/sample"
	"sre-timeline/internal/schemaio"
	"sre-timeline/internal/scoring"
	"sre-timeline/internal/topology"
)

type Console struct {
	ctx       context.Context
	out       io.Writer
	seed      int64
	workers   int
	buffer    int
	dbPath    string
	canonical *model.Project
	version   string
	eng       *engine.Engine
	analysis  *engine.Analysis
	store     *checkpoint.Store
	round     int64
}

func runReplCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", sample.DefaultSeed, "deterministic seed")
	workers := fs.Int("workers", 4, "worker budget")
	buffer := fs.Int("buffer", 4096, "per-service buffer limit")
	dbPath := fs.String("db", filepath.Join("artifacts", "checkpoints", "repl.db"), "SQLite checkpoint path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	console, err := NewConsole(ctx, stdout, *seed, *workers, *buffer, *dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "init failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "SRE diagnostic workbench. Type help for commands.")
	go func() {
		<-ctx.Done()
		fmt.Fprintln(stderr, "\ngraceful stop requested; last committed SQLite round remains complete")
		if console.store != nil {
			_ = console.store.Close()
		}
		os.Exit(0)
	}()
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	fmt.Fprint(stdout, "sre> ")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "exit" || line == "quit" {
			break
		}
		if line != "" {
			if quit := console.Exec(line); quit {
				break
			}
		}
		fmt.Fprint(stdout, "sre> ")
	}
	if console.store != nil {
		_ = console.store.Close()
	}
	return 0
}

func runDemo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	seed := fs.Int64("seed", sample.DefaultSeed, "seed")
	workers := fs.Int("workers", 4, "workers")
	buffer := fs.Int("buffer", 4096, "buffer")
	dbPath := fs.String("db", filepath.Join("artifacts", "checkpoints", "demo.db"), "checkpoint db")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	console, err := NewConsole(ctx, stdout, *seed, *workers, *buffer, *dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "demo init: %v\n", err)
		return 1
	}
	script := []string{
		"load v1",
		"intervals",
		"review 3",
		"candidates",
		"propagate payment",
		"diagnostics",
		"checkpoint save 1",
		"edit threshold payment 900",
		"diff",
		"checkpoint load",
		"checkpoint recover",
		"checkpoint mismatch",
		"load v2",
		"intervals",
		"load v3",
		"candidates",
		"propagate payment",
		"inject time-reversal",
		"inject negative-threshold",
		"inject cycle",
		"inject scale",
		"inject unknown-field",
		"inject backpressure-buffer",
		"inject backpressure-workers",
		"inject truncated-checkpoint",
	}
	for _, line := range script {
		fmt.Fprintf(stdout, "\nsre> %s\n", line)
		console.Exec(line)
	}
	if console.store != nil {
		_ = console.store.Close()
	}
	return 0
}

func NewConsole(ctx context.Context, out io.Writer, seed int64, workers, buffer int, dbPath string) (*Console, error) {
	p := sample.Generate(seed)
	if _, err := sample.BuildTruth(p); err != nil {
		return nil, err
	}
	c := &Console{ctx: ctx, out: out, seed: seed, workers: workers, buffer: buffer, dbPath: dbPath, canonical: p}
	if _, err := c.loadVersion("v1", false); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Console) Exec(line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	var err error
	switch fields[0] {
	case "help":
		printHelp(c.out)
	case "exit", "quit":
		return true
	case "load":
		if len(fields) < 2 {
			err = bizerr.New(bizerr.CodeInvalidInput, "load", "usage: load v1|v2|v3")
		} else {
			err = c.load(fields[1])
		}
	case "timeline":
		c.printTimeline()
	case "diagnostics":
		c.printDiagnostics()
	case "intervals":
		c.printIntervals(optionalArg(fields, 1))
	case "review":
		topN := 5
		if len(fields) > 1 {
			if v, err := strconv.Atoi(fields[1]); err == nil {
				topN = v
			}
		}
		c.printReview(topN)
	case "candidates":
		err = c.printCandidates(optionalArg(fields, 1))
	case "propagate":
		err = c.printPropagate(fields[1:])
	case "edit":
		err = c.edit(fields[1:])
	case "edge":
		err = c.edge(fields[1:])
	case "diff":
		err = c.diff()
	case "checkpoint":
		err = c.checkpoint(fields[1:])
	case "inject":
		err = c.inject(optionalArg(fields, 1))
	default:
		err = bizerr.New(bizerr.CodeInvalidInput, "command", "unknown command %q; type help", fields[0])
	}
	if err != nil {
		c.printError(err)
	}
	return false
}

func optionalArg(fields []string, idx int) string {
	if idx < len(fields) {
		return fields[idx]
	}
	return ""
}

func (c *Console) load(version string) error {
	_, err := c.loadVersion(version, true)
	return err
}

func (c *Console) loadVersion(version string, announce bool) (*engine.Analysis, error) {
	data, err := schemaio.Encode(c.canonical, version)
	if err != nil {
		return nil, err
	}
	loaded, err := schemaio.Load(data)
	if err != nil {
		return nil, err
	}
	eng, err := engine.New(loaded, engine.Options{Concurrency: concurrency.Config{Workers: c.workers, BufferLimit: c.buffer}})
	if err != nil {
		return nil, err
	}
	analysis, err := eng.Analyze(c.ctx)
	if err != nil {
		return nil, err
	}
	c.version = version
	c.eng = eng
	c.analysis = analysis
	if announce {
		fmt.Fprintf(c.out, "loaded %s: hash=%s services=%d events=%d intervals=%d candidates=%d\n",
			version, shortHash(eng.Hash()), len(loaded.Services), len(loaded.Events), len(analysis.Targets), len(analysis.Scoring.Candidates))
	}
	return analysis, nil
}

func (c *Console) printError(err error) {
	var be *bizerr.Error
	if errors.As(err, &be) {
		data, _ := json.MarshalIndent(be, "", "  ")
		fmt.Fprintf(c.out, "%s\n", data)
		return
	}
	fmt.Fprintf(c.out, "ERROR: %v\n", err)
}

func (c *Console) rel(ms int64) int64 {
	return (ms - c.eng.Project().Window.StartMS) / 1000
}

func (c *Console) printTimeline() {
	report := c.analysis.Alignment
	fmt.Fprintf(c.out, "tolerance=%dms kept=%d raw=%d removed=%v\n", report.ToleranceMS, len(report.Kept), len(report.All), report.RemovedIDs)
	for _, ev := range report.Kept {
		fmt.Fprintf(c.out, "T+%03ds %-8s %-10s %-24s dup_removed=%d\n", c.rel(ev.EffectiveMS), ev.Source, ev.ServiceID, ev.EventID, ev.DuplicatesRemoved)
	}
}

func (c *Console) printDiagnostics() {
	data, _ := json.MarshalIndent(c.analysis.Diagnostics, "", "  ")
	fmt.Fprintln(c.out, string(data))
}

func (c *Console) printIntervals(serviceFilter string) {
	for _, target := range c.analysis.Targets {
		iv := target.Data
		if serviceFilter != "" && iv.ServiceID != serviceFilter {
			continue
		}
		fmt.Fprintf(c.out, "%s %-9s [T+%03ds,T+%03ds) %s threshold=%d peak=%d evidence=%d key=%s\n",
			target.ID, iv.ServiceID, c.rel(iv.StartMS), c.rel(iv.EndMS), iv.Metric, iv.ThresholdBP, iv.PeakBP, len(iv.EvidenceIDs), iv.Key)
	}
}

func (c *Console) printReview(topN int) {
	if topN <= 0 {
		topN = 5
	}
	for _, target := range c.analysis.Targets {
		iv := target.Data
		fmt.Fprintf(c.out, "\n%s service=%s [%s,%s) metric=%s threshold=%d\n",
			target.ID, iv.ServiceID, fmt.Sprintf("T+%03ds", c.rel(iv.StartMS)), fmt.Sprintf("T+%03ds", c.rel(iv.EndMS)), iv.Metric, iv.ThresholdBP)
		contribs := []scoring.Contribution{}
		for _, ct := range c.analysis.Scoring.Contributions {
			if ct.IntervalID == target.ID && ct.Participated {
				contribs = append(contribs, ct)
			}
		}
		sortContributions(contribs)
		for i, ct := range contribs {
			if i >= topN {
				break
			}
			fmt.Fprintf(c.out, "  #%d %-24s score=%d timing=%d reach=%d type=%d gen=%d\n",
				i+1, ct.EventID, ct.ScoreRaw, ct.TimingRaw, ct.ReachRaw, ct.TypeRaw, ct.TopologyGeneration)
		}
	}
}

func (c *Console) printCandidates(intervalID string) error {
	if intervalID != "" && strings.HasPrefix(intervalID, "I-") {
		contribs := []scoring.Contribution{}
		found := false
		for _, ct := range c.analysis.Scoring.Contributions {
			if ct.IntervalID == intervalID {
				found = true
				if ct.Participated {
					contribs = append(contribs, ct)
				}
			}
		}
		if !found {
			return bizerr.New(bizerr.CodeNotFound, "interval_id", "interval %q not found", intervalID)
		}
		sortContributions(contribs)
		fmt.Fprintf(c.out, "contributions for %s\n", intervalID)
		for _, ct := range contribs {
			fmt.Fprintf(c.out, "  %-24s score=%-8d timing=%.3f reach=%.3f type=%.3f gen=%d\n",
				ct.EventID, ct.ScoreRaw, float64(ct.TimingRaw)/1e6, float64(ct.ReachRaw)/1e6, float64(ct.TypeRaw)/1e6, ct.TopologyGeneration)
		}
		return nil
	}
	fmt.Fprintln(c.out, "rank event                    source    service    total_raw  intervals")
	for _, cand := range c.analysis.Scoring.Candidates {
		fmt.Fprintf(c.out, "#%-3d %-25s %-9s %-10s %-10d %v\n", cand.Rank, cand.EventID, cand.Source, cand.ServiceID, cand.TotalRaw, cand.IntervalIDs)
	}
	return nil
}

func sortContributions(in []scoring.Contribution) {
	for i := 0; i < len(in); i++ {
		for j := i + 1; j < len(in); j++ {
			if in[j].ScoreRaw > in[i].ScoreRaw || (in[j].ScoreRaw == in[i].ScoreRaw && in[j].EventSeq < in[i].EventSeq) {
				in[i], in[j] = in[j], in[i]
			}
		}
	}
}

func (c *Console) printPropagate(args []string) error {
	if len(args) == 0 {
		return bizerr.New(bizerr.CodeInvalidInput, "propagate", "usage: propagate <service> [generation]")
	}
	gen := c.eng.Project().Policy.PropagationGen
	if len(args) > 1 {
		v, err := strconv.Atoi(args[1])
		if err != nil {
			return bizerr.Wrap(bizerr.CodeInvalidInput, "generation", err, "invalid generation")
		}
		gen = v
	}
	result, err := c.eng.Topology().Propagate(args[0], gen, topology.DefaultDecayBP)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "source=%s closure=%v cycle=%v capped=%v\n", result.SourceService, result.Closure, result.CycleDetected, result.GenerationCapped)
	for _, impact := range result.Impacts {
		fmt.Fprintf(c.out, "  target=%-10s gen=%d impact_raw=%d saturated=%v\n", impact.TargetService, impact.Generation, impact.ImpactRaw, impact.Saturated)
	}
	return nil
}

func (c *Console) edit(args []string) error {
	if len(args) == 0 {
		return bizerr.New(bizerr.CodeInvalidInput, "edit", "usage: threshold|event-time|add-config ...")
	}
	switch args[0] {
	case "threshold":
		if len(args) < 3 {
			return bizerr.New(bizerr.CodeInvalidInput, "edit threshold", "usage: edit threshold <service> <bp>")
		}
		bp, err := strconv.Atoi(args[2])
		if err != nil {
			return bizerr.Wrap(bizerr.CodeInvalidInput, "bp", err, "invalid threshold")
		}
		plan, err := c.eng.EditThreshold(args[1], bp)
		if err != nil {
			return err
		}
		return c.reanalyze(plan)
	case "event-time":
		if len(args) < 3 {
			return bizerr.New(bizerr.CodeInvalidInput, "edit event-time", "usage: edit event-time <event-id> <relative-seconds>")
		}
		sec, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return bizerr.Wrap(bizerr.CodeInvalidInput, "relative-seconds", err, "invalid seconds")
		}
		newMS := c.eng.Project().Window.StartMS + sec*1000
		plan, err := c.eng.CorrectEventTime(args[1], newMS)
		if err != nil {
			return err
		}
		return c.reanalyze(plan)
	case "add-config":
		if len(args) < 4 {
			return bizerr.New(bizerr.CodeInvalidInput, "edit add-config", "usage: edit add-config <service> <event-id> <relative-seconds> [weight-bp]")
		}
		sec, err := strconv.ParseInt(args[3], 10, 64)
		if err != nil {
			return bizerr.Wrap(bizerr.CodeInvalidInput, "relative-seconds", err, "invalid seconds")
		}
		weight := 8500
		if len(args) > 4 {
			weight, err = strconv.Atoi(args[4])
			if err != nil {
				return bizerr.Wrap(bizerr.CodeInvalidInput, "weight-bp", err, "invalid weight")
			}
		}
		event := model.Event{
			ID: args[2], Source: model.SourceChange, ServiceID: args[1], Kind: "config_change",
			Fingerprint: args[2], OccurredMS: c.eng.Project().Window.StartMS + sec*1000,
			ChangeType: "config_change", TypeWeightBP: weight, AliveMS: 60_000,
		}
		plan, err := c.eng.AddEvent(event)
		if err != nil {
			return err
		}
		return c.reanalyze(plan)
	default:
		return bizerr.New(bizerr.CodeInvalidInput, "edit", "unknown edit %q", args[0])
	}
}

func (c *Console) edge(args []string) error {
	if len(args) < 3 {
		return bizerr.New(bizerr.CodeInvalidInput, "edge", "usage: edge add|delete <caller> <callee>")
	}
	var plan *engine.InvalidationPlan
	var err error
	switch args[0] {
	case "add":
		plan, err = c.eng.AddEdge(args[1], args[2])
	case "delete":
		plan, err = c.eng.DeleteEdge(args[1], args[2])
	default:
		return bizerr.New(bizerr.CodeInvalidInput, "edge", "usage: edge add|delete")
	}
	if err != nil {
		return err
	}
	return c.reanalyze(plan)
}

func (c *Console) reanalyze(plan *engine.InvalidationPlan) error {
	analysis, err := c.eng.Analyze(c.ctx)
	if err != nil {
		return err
	}
	c.analysis = analysis
	data, _ := json.MarshalIndent(plan, "", "  ")
	fmt.Fprintln(c.out, string(data))
	fmt.Fprintf(c.out, "recompute services=%v reused=%v score_recomputed=%d score_reused=%d\n",
		analysis.Diagnostics.RecomputedServices, analysis.Diagnostics.ReusedServices,
		len(analysis.Diagnostics.RecomputedScoreKeys), len(analysis.Diagnostics.ReusedScoreKeys))
	return nil
}

func (c *Console) diff() error {
	fresh, err := engine.New(c.eng.Project(), engine.Options{Concurrency: concurrency.Config{Workers: 1, BufferLimit: c.buffer}})
	if err != nil {
		return err
	}
	freshAnalysis, err := fresh.Analyze(c.ctx)
	if err != nil {
		return err
	}
	incCandidates := compactCandidates(c.analysis)
	fullCandidates := compactCandidates(freshAnalysis)
	incIntervals := compactIntervals(c.analysis)
	fullIntervals := compactIntervals(freshAnalysis)
	if reflect.DeepEqual(incCandidates, fullCandidates) && reflect.DeepEqual(incIntervals, fullIntervals) {
		fmt.Fprintln(c.out, "DIFF MATCH: incremental result equals one-shot full recompute")
		return nil
	}
	fmt.Fprintf(c.out, "DIFF MISMATCH:\nincremental=%+v\nfull=%+v\n", incCandidates, fullCandidates)
	return bizerr.New("E_INCREMENTAL_DIFF", "diff", "incremental result differs from full recompute")
}

func compactCandidates(a *engine.Analysis) []compactCandidate {
	out := make([]compactCandidate, 0, len(a.Scoring.Candidates))
	for _, cand := range a.Scoring.Candidates {
		out = append(out, compactCandidate{Rank: cand.Rank, Event: cand.EventID, Raw: cand.TotalRaw})
	}
	return out
}

func compactIntervals(a *engine.Analysis) []intervalSummaryCLI {
	out := make([]intervalSummaryCLI, 0, len(a.Targets))
	for _, target := range a.Targets {
		out = append(out, intervalSummaryCLI{Service: target.Data.ServiceID, Start: target.Data.StartMS, End: target.Data.EndMS})
	}
	return out
}

type intervalSummaryCLI struct {
	Service string `json:"service"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
}

func (c *Console) checkpoint(args []string) error {
	action := "save"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "save":
		roundNo := c.round + 1
		if len(args) > 1 {
			v, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return bizerr.Wrap(bizerr.CodeInvalidInput, "round", err, "invalid round")
			}
			roundNo = v
		}
		if err := c.openStore(); err != nil {
			return err
		}
		if err := c.persistCurrentRound(roundNo); err != nil {
			return err
		}
		return nil
	case "load":
		if err := c.openStore(); err != nil {
			return err
		}
		loaded, err := c.store.LoadLatest(c.ctx, c.eng.Hash())
		if err != nil {
			return err
		}
		fmt.Fprintf(c.out, "checkpoint loaded round=%d version=%s services=%d caches=%d\n", loaded.RoundNo, shortHash(loaded.ProjectVersion), len(loaded.ServiceStates), len(loaded.ContributionCaches))
		return nil
	case "recover":
		if err := c.openStore(); err != nil {
			return err
		}
		loaded, err := c.store.LoadLatest(c.ctx, c.eng.Hash())
		if err == nil {
			fmt.Fprintf(c.out, "checkpoint accepted: round=%d services=%d caches=%d\n", loaded.RoundNo, len(loaded.ServiceStates), len(loaded.ContributionCaches))
			return nil
		}
		if !bizerr.IsCode(err, bizerr.CodeCheckpointVersion) {
			return err
		}
		c.printError(err)
		fmt.Fprintln(c.out, "CHECKPOINT RECOVER: version mismatch discards partial reuse; running one-shot full recompute")
		fresh, freshErr := engine.New(c.eng.Project(), engine.Options{Concurrency: concurrency.Config{Workers: 1, BufferLimit: c.buffer}})
		if freshErr != nil {
			return freshErr
		}
		fullAnalysis, analyzeErr := fresh.Analyze(c.ctx)
		if analyzeErr != nil {
			return analyzeErr
		}
		c.analysis = fullAnalysis
		roundNo := c.round + 1
		if loaded != nil {
			roundNo = loaded.RoundNo + 1
		}
		if err := c.persistCurrentRound(roundNo); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "CHECKPOINT RECOVER: full recompute matched and materialized round=%d\n", roundNo)
		return nil
	case "mismatch":
		if err := c.openStore(); err != nil {
			return err
		}
		_, err := c.store.LoadLatest(c.ctx, "deliberately-stale-version")
		c.printError(err)
		return nil
	case "truncate":
		if c.store != nil {
			_ = c.store.Close()
			c.store = nil
		}
		if err := os.Truncate(c.dbPath, 32); err != nil {
			return err
		}
		_, err := checkpoint.Open(c.ctx, c.dbPath)
		c.printError(err)
		return nil
	default:
		return bizerr.New(bizerr.CodeInvalidInput, "checkpoint", "usage: checkpoint save|load|recover|mismatch|truncate")
	}
}

func (c *Console) persistCurrentRound(roundNo int64) error {
	round := checkpoint.Round{RoundNo: roundNo, ProjectID: c.eng.Project().ID, ProjectVersion: c.eng.Hash(), SchemaVariant: c.version}
	for serviceID, result := range c.analysis.Intervals {
		payload, _ := json.Marshal(result)
		round.ServiceStates = append(round.ServiceStates, checkpoint.ServiceState{ServiceID: serviceID, StateJSON: string(payload)})
	}
	contribsByKey := map[string][]scoring.Contribution{}
	for _, contribution := range c.analysis.Scoring.Contributions {
		contribsByKey[contribution.IntervalKey] = append(contribsByKey[contribution.IntervalKey], contribution)
	}
	for _, target := range c.analysis.Targets {
		payload, _ := json.Marshal(contribsByKey[target.Data.Key])
		round.ContributionCaches = append(round.ContributionCaches, checkpoint.ContributionCache{IntervalKey: target.Data.Key, IntervalID: target.ID, PayloadJSON: string(payload)})
	}
	if err := c.store.SaveRound(c.ctx, round); err != nil {
		return err
	}
	c.round = roundNo
	fmt.Fprintf(c.out, "checkpoint saved round=%d version=%s path=%s\n", roundNo, shortHash(c.eng.Hash()), c.dbPath)
	return nil
}

func (c *Console) openStore() error {
	if c.store != nil {
		return nil
	}
	if dir := filepath.Dir(c.dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	store, err := checkpoint.Open(c.ctx, c.dbPath)
	if err != nil {
		return err
	}
	c.store = store
	return nil
}

func (c *Console) inject(kind string) error {
	switch kind {
	case "time-reversal":
		bad, _ := engine.CloneProject(c.canonical)
		bad.Window.StartMS = bad.Window.EndMS + 1
		c.printError(schemaio.ValidateCanonical(bad))
		return nil
	case "negative-threshold":
		bad, _ := engine.CloneProject(c.canonical)
		bad.Services[0].ErrorThresholdBP = -10
		c.printError(schemaio.ValidateCanonical(bad))
		return nil
	case "cycle":
		bad, _ := engine.CloneProject(c.canonical)
		bad.Edges = append(bad.Edges, model.Edge{Caller: "inventory", Callee: "order"})
		topo, err := topology.New(bad.Services, bad.Edges)
		if err == nil {
			err = topo.ValidateAcyclic()
		}
		c.printError(err)
		return nil
	case "scale":
		data, _ := schemaio.Encode(c.canonical, model.SchemaV2)
		var doc map[string]any
		_ = json.Unmarshal(data, &doc)
		doc["policy"].(map[string]any)["scoreScale"] = float64(7)
		bad, _ := json.Marshal(doc)
		_, err := schemaio.Load(bad)
		c.printError(err)
		return nil
	case "unknown-field":
		data, _ := schemaio.Encode(c.canonical, model.SchemaV1)
		var doc map[string]any
		_ = json.Unmarshal(data, &doc)
		doc["unrecognized_legacy_column"] = "x"
		bad, _ := json.Marshal(doc)
		_, err := schemaio.Load(bad)
		c.printError(err)
		return nil
	case "backpressure-buffer":
		small, err := engine.New(c.canonical, engine.Options{Concurrency: concurrency.Config{Workers: 1, BufferLimit: 1}})
		if err != nil {
			return err
		}
		_, err = small.Analyze(c.ctx)
		c.printError(err)
		return nil
	case "backpressure-workers":
		c.printError(saturatedWorkerError())
		return nil
	case "truncated-checkpoint":
		path := filepath.Join(os.TempDir(), "sre-timeline-truncated.db")
		if err := os.WriteFile(path, make([]byte, 32), 0o644); err != nil {
			return err
		}
		_, err := checkpoint.Open(c.ctx, path)
		c.printError(err)
		return nil
	default:
		return bizerr.New(bizerr.CodeInvalidInput, "inject", "unknown injection %q", kind)
	}
}

func saturatedWorkerError() error {
	ctx := context.Background()
	pool, err := concurrency.NewPool(concurrency.Config{Workers: 1, BufferLimit: 1})
	if err != nil {
		return err
	}
	block := make(chan struct{})
	started := make(chan struct{})
	submitErr := pool.Submit(ctx, func() {
		close(started)
		<-block
	})
	if submitErr != nil {
		return submitErr
	}
	<-started
	if err := pool.TrySubmit(func() {}); err != nil {
		close(block)
		pool.Close()
		return err
	}
	err = pool.TrySubmit(func() {})
	close(block)
	pool.Close()
	return err
}

func shortHash(hash string) string {
	if len(hash) >= 12 {
		return hash[:12]
	}
	return hash
}
