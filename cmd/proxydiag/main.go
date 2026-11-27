package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"proxydiag/bizerr"
	"proxydiag/diag"
	"proxydiag/domain"
	"proxydiag/engine"
	"proxydiag/samplegen"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "incremental":
		err = cmdIncremental(os.Args[2:])
	case "checkpoint":
		err = cmdCheckpoint(os.Args[2:])
	case "inject":
		err = cmdInject(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "workbench":
		err = cmdWorkbench(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = bizerr.New(bizerr.InvalidArgument, "unknown command").With("command", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `proxydiag - deterministic proxy forwarding diagnostic workbench

Usage:
  proxydiag generate    [-seed N] [-out samples]
  proxydiag verify      [-samples samples] [-json]
  proxydiag run         [-samples samples] [-version v3] [-workers 8] [-buffer N] [-trace SEQ] [-json]
  proxydiag incremental [-samples samples] [-json]
  proxydiag checkpoint  [-samples samples] [-dir artifacts/checkpoint-demo] [-json]
  proxydiag inject      [-samples samples] [-json]
  proxydiag demo        [-samples samples]
  proxydiag workbench   [-samples samples]`)
}

func samplesFlag(fs *flag.FlagSet) *string {
	return fs.String("samples", "samples", "directory containing generated sample files")
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", samplegen.DefaultSeed, "deterministic generator seed")
	out := fs.String("out", "samples", "output directory")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := samplegen.Write(*out, *seed)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(m)
	} else {
		fmt.Printf("generated seed=%d version=%s requests=%d files=%d dir=%s\n",
			m.Seed, m.Version, m.Requests, len(m.Files), *out)
		for _, name := range sortedKeys(m.Files) {
			fmt.Printf("  %-18s sha256=%s\n", name, m.Files[name][:16])
		}
	}
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	samples := samplesFlag(fs)
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := diag.Verify(*samples)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(report)
	} else {
		fmt.Printf("samples: %s\n", report.Samples)
		for _, v := range report.Versions {
			fmt.Printf("  %s schema=%-17s requests=%d truth=%d/%d bounds=%d/%d max_delta=%d fingerprint=%s\n",
				v.Version, v.Schema, v.Requests, v.TruthMatched, v.Requests,
				v.WithinBounds, v.Requests, v.MaxAbsDelta, short(v.Fingerprint))
		}
		fmt.Printf("  equivalent_core=%t decision_parity=%t tick_ms_parity=%t\n",
			report.EquivalentCore, report.DecisionParity, report.TickMsParity)
		if report.AllTruthMatched && report.EquivalentCore && report.DecisionParity && report.TickMsParity {
			fmt.Println("VERIFY: PASS")
		} else {
			fmt.Println("VERIFY: FAIL")
			for _, f := range report.Failures {
				fmt.Println("  " + f)
			}
		}
	}
	if !report.AllTruthMatched || !report.EquivalentCore || !report.DecisionParity || !report.TickMsParity {
		return bizerr.New(bizerr.Internal, "verification failed")
	}
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	samples := samplesFlag(fs)
	version := fs.String("version", "v3", "schema version: v1, v2 or v3")
	workers := fs.Int("workers", 8, "declared worker budget")
	buffer := fs.Int("buffer", -1, "override every listener buffer; -1 keeps declared limits")
	trace := fs.Int("trace", 0, "print one detailed request result")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := diag.LoadProject(*samples, *version)
	if err != nil {
		return err
	}
	truth, err := diag.LoadTruth(*samples)
	if err != nil {
		return err
	}
	cfg := engine.ConcurrencyConfig{Workers: *workers}
	if *buffer >= 0 {
		cfg.ListenerBufferOverride = map[string]int{}
		for _, l := range p.Listeners {
			cfg.ListenerBufferOverride[l.ID] = *buffer
		}
	}
	report := engine.New(p, truth).RunConcurrent(cfg)
	if *trace > 0 {
		for _, r := range report.Results {
			if r.Seq == *trace {
				printJSON(r)
				return nil
			}
		}
		return bizerr.New(bizerr.InvalidArgument, "trace sequence not found").WithInt("seq", *trace)
	}
	if *jsonOut {
		printJSON(report)
		return nil
	}
	for _, r := range report.Results {
		status := "ok"
		if !r.TruthMatched || r.ErrorCode != "" {
			status = "fail"
		}
		fmt.Printf("seq=%02d %-5s listener=%-11s route=%-20s upstream=%-9s delta=%d truth=%t line=%q\n",
			r.Seq, status, dash(r.ListenerID), dash(r.RouteID), dash(r.SelectedUpstreamID),
			r.MaxAbsDelta, r.TruthMatched, r.RewrittenRequestLine)
		if r.ErrorCode != "" {
			fmt.Printf("      code=%s context=%v\n", r.ErrorCode, r.ErrorContext)
		}
	}
	fmt.Println(report.Summary())
	for _, t := range report.ListenerTimings {
		fmt.Printf("  timing listener=%-11s tasks=%d max_elapsed_ns=%d total_ns=%d\n",
			t.ListenerID, t.Tasks, t.ElapsedNS, t.TotalNS)
	}
	return nil
}

func cmdIncremental(args []string) error {
	fs := flag.NewFlagSet("incremental", flag.ContinueOnError)
	samples := samplesFlag(fs)
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := diag.Incremental(*samples)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(report)
	} else {
		fmt.Printf("source_unchanged=%t all_match_full=%t\n", report.SourceUnchanged, report.AllMatchFull)
		for _, step := range report.Steps {
			fmt.Printf("  %-15s target=%-18s at=%d source=%s recomputed=%v reused=%v match_full=%t\n",
				step.Kind, step.Target, step.AtSeq, step.Source, step.Recomputed, step.Reused, step.MatchFull)
			fmt.Printf("      closure=%v\n", step.Closure)
		}
	}
	if !report.AllMatchFull || !report.SourceUnchanged {
		return bizerr.New(bizerr.Internal, "incremental equivalence failed")
	}
	return nil
}

func cmdCheckpoint(args []string) error {
	fs := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	samples := samplesFlag(fs)
	dir := fs.String("dir", "artifacts/checkpoint-demo", "checkpoint journal directory")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := diag.CheckpointDemo(*samples, *dir)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(report)
	} else {
		fmt.Printf("checkpoint path=%s\n", report.Path)
		fmt.Printf("  committed=%d recovered=%d last_seq=%d listener_caches=%d health_entries=%d\n",
			report.Committed, report.Recovered, report.LastCommittedSeq,
			report.ListenerCaches, report.HealthEntries)
		fmt.Printf("  all_decisions_match=%t uncommitted_ignored=%t truncation_code=%s prior_usable=%t version_mismatch=%s recomputed=%d\n",
			report.AllDecisionsMatch, report.UncommittedIgnored, report.TruncationCode,
			report.PriorUsableAfterCut, report.VersionMismatchCode, report.VersionMismatchRecomputed)
		for _, f := range report.Failures {
			fmt.Println("  " + f)
		}
	}
	if !report.AllDecisionsMatch || !report.UncommittedIgnored ||
		report.TruncationCode != bizerr.CheckpointTruncated ||
		!report.PriorUsableAfterCut ||
		report.VersionMismatchCode != bizerr.CheckpointVersion ||
		report.VersionMismatchRecomputed != report.Committed {
		return bizerr.New(bizerr.Internal, "checkpoint acceptance checks failed")
	}
	return nil
}

func cmdInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	samples := samplesFlag(fs)
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	report, err := diag.InjectAnomalies(*samples)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(report)
	} else {
		for _, c := range report.Cases {
			fmt.Printf("%-31s code=%-32s source_unchanged=%t cache_usable=%t\n",
				c.Name, c.Code, c.SourceUnchanged, c.CacheUsable)
		}
		fmt.Printf("injections_stable=%t\n", report.AllStable)
	}
	if !report.AllStable {
		return bizerr.New(bizerr.Internal, "one or more anomaly injections were not stable")
	}
	return nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	samples := samplesFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Println("== three-schema load and hop precision ==")
	if err := cmdVerify([]string{"-samples", *samples}); err != nil {
		return err
	}
	fmt.Println("== incremental invalidation ==")
	if err := cmdIncremental([]string{"-samples", *samples}); err != nil {
		return err
	}
	fmt.Println("== checkpoint recovery ==")
	if err := cmdCheckpoint([]string{"-samples", *samples}); err != nil {
		return err
	}
	fmt.Println("== anomaly codes ==")
	if err := cmdInject([]string{"-samples", *samples}); err != nil {
		return err
	}
	fmt.Println("DEMO: PASS")
	return nil
}

func cmdWorkbench(args []string) error {
	fs := flag.NewFlagSet("workbench", flag.ContinueOnError)
	samples := samplesFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	truth, err := diag.LoadTruth(*samples)
	if err != nil {
		return err
	}
	p, err := diag.LoadProject(*samples, "v3")
	if err != nil {
		return err
	}
	eng := engine.New(p, truth)
	report := eng.RunConcurrent(engine.ConcurrencyConfig{Workers: 8})
	fmt.Printf("proxydiag workbench: version=%s requests=%d %s\n", p.Version, len(p.Requests), report.Summary())
	fmt.Println("type 'help' for commands")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("proxydiag> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "quit", "exit":
			return nil
		case "help":
			printWorkbenchHelp()
		case "load":
			if len(fields) != 2 {
				fmt.Println("usage: load v1|v2|v3")
				continue
			}
			np, err := diag.LoadProject(*samples, fields[1])
			if err != nil {
				printErr(err)
				continue
			}
			eng = engine.New(np, truth)
			rep := eng.RunConcurrent(engine.ConcurrencyConfig{Workers: 8})
			fmt.Printf("loaded %s: %s\n", np.SourceSchema, rep.Summary())
		case "run":
			workers := 8
			if len(fields) > 1 {
				workers, err = strconv.Atoi(fields[1])
				if err != nil {
					printErr(err)
					continue
				}
			}
			rep := eng.RunConcurrent(engine.ConcurrencyConfig{Workers: workers})
			fmt.Println(rep.Summary())
			for _, r := range rep.Results {
				fmt.Printf("  seq=%d terminal=%s listener=%s route=%s upstream=%s truth=%t\n",
					r.Seq, r.Terminal, dash(r.ListenerID), dash(r.RouteID),
					dash(r.SelectedUpstreamID), r.TruthMatched)
			}
		case "show":
			seq, ok := parseSeq(fields)
			if !ok {
				continue
			}
			r, found := eng.Result(seq)
			if !found {
				fmt.Printf("seq %d not in cache\n", seq)
				continue
			}
			printJSON(r)
		case "hops":
			seq, ok := parseSeq(fields)
			if !ok {
				continue
			}
			r, found := eng.Result(seq)
			if !found {
				fmt.Printf("seq %d not in cache\n", seq)
				continue
			}
			for _, h := range r.Trace {
				fmt.Printf("  #%02d %-30s value=%-10d truth=%-10d delta=%d within=%t\n",
					h.Seq, h.Name, h.Value, h.Truth, h.Delta, h.WithinTolerance)
			}
		case "health":
			if len(fields) != 3 {
				fmt.Println("usage: health <upstream> <true|false>")
				continue
			}
			healthy, err := strconv.ParseBool(fields[2])
			if err != nil {
				printErr(err)
				continue
			}
			applyAndPrint(eng, engine.Edit{Kind: engine.EditHealth, UpstreamID: fields[1], Healthy: healthy, AtSeq: 1})
		case "weight":
			if len(fields) != 3 {
				fmt.Println("usage: weight <upstream> <weight-ppm>")
				continue
			}
			w, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				printErr(err)
				continue
			}
			applyAndPrint(eng, engine.Edit{Kind: engine.EditWeight, UpstreamID: fields[1], WeightPPM: w, AtSeq: 1})
		case "rewrite":
			if len(fields) != 4 {
				fmt.Println("usage: rewrite <route> <strip-prefix> <add-prefix>")
				continue
			}
			applyAndPrint(eng, engine.Edit{
				Kind: engine.EditRewrite, RouteID: fields[1], StripPrefix: fields[2],
				AddPrefix: fields[3], AtSeq: 1,
			})
		case "delete":
			seq, ok := parseSeq(fields)
			if !ok {
				continue
			}
			applyAndPrint(eng, engine.Edit{Kind: engine.EditDelete, Seq: seq, AtSeq: seq})
		case "add":
			if len(fields) < 3 {
				fmt.Println("usage: add <seq> <path> [method]")
				continue
			}
			seq, err := strconv.Atoi(fields[1])
			if err != nil {
				printErr(err)
				continue
			}
			method := "GET"
			if len(fields) > 3 {
				method = strings.ToUpper(fields[3])
			}
			project := eng.Project()
			arrival := int64(0)
			if len(project.Requests) > 0 {
				arrival = project.Requests[len(project.Requests)-1].ArrivalMS + 10
			}
			req := &domain.Request{
				Seq: seq, ArrivalMS: arrival, Method: method, RawPath: fields[2],
				Headers: []domain.Header{{Name: "X-Request-ID", Value: fmt.Sprintf("req-%03d", seq)}},
			}
			applyAndPrint(eng, engine.Edit{Kind: engine.EditAdd, Request: req, AtSeq: seq})
		case "compare":
			got := eng.Results()
			full := engine.EvaluateAll(eng.Project(), nil)
			if equalDecisions(got, full) {
				fmt.Println("cache == full recompute")
			} else {
				fmt.Println("CACHE MISMATCH")
			}
		case "checkpoint":
			dir := "artifacts/checkpoint-demo"
			if len(fields) > 1 {
				dir = fields[1]
			}
			rep, err := diag.CheckpointDemo(*samples, dir)
			if err != nil {
				printErr(err)
				continue
			}
			fmt.Printf("committed=%d recovered=%d last=%d cut=%s prior_usable=%t version=%s\n",
				rep.Committed, rep.Recovered, rep.LastCommittedSeq, rep.TruncationCode,
				rep.PriorUsableAfterCut, rep.VersionMismatchCode)
		case "inject":
			rep, err := diag.InjectAnomalies(*samples)
			if err != nil {
				printErr(err)
				continue
			}
			for _, c := range rep.Cases {
				fmt.Printf("  %-31s %s\n", c.Name, c.Code)
			}
		case "verify":
			rep, err := diag.Verify(*samples)
			if err != nil {
				printErr(err)
				continue
			}
			fmt.Printf("all_truth=%t equivalent_core=%t decision_parity=%t\n",
				rep.AllTruthMatched, rep.EquivalentCore, rep.DecisionParity)
		default:
			fmt.Println("unknown command; type 'help'")
		}
	}
	return scanner.Err()
}

func printWorkbenchHelp() {
	fmt.Print(`commands:
  load v1|v2|v3                 reload a schema version
  run [workers]                 run deterministic concurrent admission
  show <seq>                    show full cached result JSON
  hops <seq>                    show fixed-point hop comparison
  health <upstream> <bool>      incremental health invalidation
  weight <upstream> <ppm>       incremental weight invalidation
  rewrite <route> <strip> <add> incremental rewrite invalidation
  delete <seq>                  delete request and invalidate suffix
  add <seq> <path> [method]     add request and invalidate suffix
  compare                       compare cache with full recompute
  checkpoint [dir]              run checkpoint recovery demo
  inject                        show stable anomaly codes
  verify                        rerun three-version verification
  quit
`)
}

func applyAndPrint(eng *engine.Engine, edit engine.Edit) {
	report, err := eng.Apply(edit)
	if err != nil {
		printErr(err)
		return
	}
	fmt.Printf("applied %s target=%s at=%d source=%s recomputed=%d reused=%d\n",
		report.Kind, report.Target, report.AtSeq, report.Source,
		len(report.Recomputed), len(report.Reused))
}

func parseSeq(fields []string) (int, bool) {
	if len(fields) != 2 {
		fmt.Println("usage: <command> <seq>")
		return 0, false
	}
	seq, err := strconv.Atoi(fields[1])
	if err != nil {
		printErr(err)
		return 0, false
	}
	return seq, true
}

func printErr(err error) {
	fmt.Println("error:", err)
}

func equalDecisions(a, b []engine.Result) bool {
	aa := append([]engine.Result(nil), a...)
	bb := append([]engine.Result(nil), b...)
	for i := range aa {
		aa[i].Cache = engine.CacheInfo{}
		aa[i].TruthMatched = true
		aa[i].Mismatches = nil
		aa[i].MaxAbsDelta = 0
		aa[i].WithinTolerance = true
		for j := range aa[i].Trace {
			aa[i].Trace[j].Truth = 0
			aa[i].Trace[j].Delta = 0
			aa[i].Trace[j].Compared = false
			aa[i].Trace[j].WithinTolerance = true
		}
	}
	for i := range bb {
		bb[i].Cache = engine.CacheInfo{}
		bb[i].TruthMatched = true
		bb[i].Mismatches = nil
		bb[i].MaxAbsDelta = 0
		bb[i].WithinTolerance = true
		for j := range bb[i].Trace {
			bb[i].Trace[j].Truth = 0
			bb[i].Trace[j].Delta = 0
			bb[i].Trace[j].Compared = false
			bb[i].Trace[j].WithinTolerance = true
		}
	}
	return reflect.DeepEqual(aa, bb)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
