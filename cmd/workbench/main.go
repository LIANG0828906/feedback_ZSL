package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"spatialworkbench/internal/audio"
	"spatialworkbench/internal/checkpoint"
	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/loader"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/session"
	"spatialworkbench/internal/verify"
	"spatialworkbench/internal/werr"
)

const defaultSamplePath = "samples/workbench_samples.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err *werr.Error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "inspect":
		err = runInspect(os.Args[2:])
	case "diagnostics":
		err = runDiagnostics(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "render":
		err = runRender(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		err = werr.New(werr.CodeState, "unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `spatial workbench

usage:
  workbench generate  [-out samples/workbench_samples.json] [-seed 20261124] [-samples 3]
  workbench inspect   [-sample FILE] [-id ID] [-index N] [-json]
  workbench diagnostics [-sample FILE] [-id ID] [-db FILE] [-frames N] [-batch] [-interactive]
  workbench verify    [-sample FILE] [-id ID] [-workdir DIR] [-resume-at N]
  workbench render    [-sample FILE] [-id ID] [-db FILE] [-out output.wav]

common parameter flags:
  -frame 512 -hop 256 -window 192 -quant 8 -interp 3 -gain 0.85
  -tolerance 4096 -workers 4 -buffer 65536 -route FL:0,FR:1,...`)
}

type paramFlags struct {
	frame     int
	hop       int
	window    int
	quant     int
	interp    int
	gain      float64
	tolerance float64
	workers   int
	buffer    int
	route     string
}

func (p *paramFlags) add(fs *flag.FlagSet) {
	fs.IntVar(&p.frame, "frame", 0, "analysis frame length")
	fs.IntVar(&p.hop, "hop", 0, "frame hop")
	fs.IntVar(&p.window, "window", 0, "energy integration window length")
	fs.IntVar(&p.quant, "quant", 0, "direct-path quantization step in samples")
	fs.IntVar(&p.interp, "interp", 0, "IR interpolation order 1..5")
	fs.Float64Var(&p.gain, "gain", -1, "render gain 0..1")
	fs.Float64Var(&p.tolerance, "tolerance", -1, "numeric error tolerance")
	fs.IntVar(&p.workers, "workers", 0, "maximum worker threads")
	fs.IntVar(&p.buffer, "buffer", 0, "sample-buffer limit per worker")
	fs.StringVar(&p.route, "route", "", "channel routing, e.g. FL:0,FR:1")
}

func (p paramFlags) apply(base model.Params, sample model.Sample) (model.Params, *werr.Error) {
	out := base.Clone()
	if p.frame != 0 {
		out.FrameLength = p.frame
	}
	if p.hop != 0 {
		out.Hop = p.hop
	}
	if p.window != 0 {
		out.WindowLength = p.window
	}
	if p.quant != 0 {
		out.QuantStep = p.quant
	}
	if p.interp != 0 {
		out.InterpOrder = p.interp
	}
	if p.gain >= 0 {
		out.RenderGain = p.gain
	}
	if p.tolerance >= 0 {
		out.ErrorTolerance = p.tolerance
	}
	if p.workers != 0 {
		out.MaxWorkers = p.workers
	}
	if p.buffer != 0 {
		out.BufferSamplesPerWorker = p.buffer
	}
	if strings.TrimSpace(p.route) != "" {
		routing := map[string]int{}
		for _, pair := range strings.Split(p.route, ",") {
			parts := strings.Split(strings.TrimSpace(pair), ":")
			if len(parts) != 2 {
				return out, parameterError("routing", "invalid route %q", pair)
			}
			index, err := strconv.Atoi(parts[1])
			if err != nil {
				return out, parameterError("routing", "invalid route index %q", parts[1])
			}
			routing[parts[0]] = index
		}
		out.Routing = routing
	}
	if err := out.Validate(sample); err != nil {
		return out, err
	}
	return out, nil
}

func parameterError(field, format string, args ...any) *werr.Error {
	e := werr.Para(format, args...)
	e.Field = field
	return e
}

type injectionFlags struct {
	missing string
	order   string
	clip    string
	bad     int
	orderAt int
	clipAt  int
}

func (i *injectionFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&i.missing, "inject-missing", "", "simulate a missing channel (E_CHAN)")
	fs.StringVar(&i.order, "inject-order", "", "simulate reversed arrival order on a channel (E_ORDER)")
	fs.IntVar(&i.orderAt, "inject-order-frame", 0, "frame for -inject-order")
	fs.StringVar(&i.clip, "inject-clip", "", "simulate gain overflow on a channel (E_CLIP)")
	fs.IntVar(&i.clipAt, "inject-clip-frame", 0, "frame for -inject-clip")
	fs.IntVar(&i.bad, "inject-bad-frame", -1, "simulate a failed frame (E_FRAME)")
}

func (i injectionFlags) value() session.Injection {
	injection := session.Injection{MissingChannel: i.missing, InvertArrivalChannel: i.order, GainOverflowChannel: i.clip}
	if i.order != "" {
		injection.InvertArrivalFrame = &i.orderAt
	}
	if i.clip != "" {
		injection.GainOverflowFrame = &i.clipAt
	}
	if i.bad >= 0 {
		injection.BadFrame = &i.bad
	}
	return injection
}

func runGenerate(args []string) *werr.Error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	out := fs.String("out", defaultSamplePath, "output manifest")
	seed := fs.Int64("seed", 20261124, "deterministic seed")
	count := fs.Int("samples", 3, "number of samples")
	prefix := fs.String("prefix", "local", "sample id prefix")
	if err := fs.Parse(args); err != nil {
		return werr.New(werr.CodeState, "%v", err)
	}
	manifest := generator.Generate(generator.Options{Seed: *seed, Samples: *count, Prefix: *prefix})
	if err := generator.WriteFile(*out, manifest); err != nil {
		return werr.New(werr.CodeState, "write manifest: %v", err)
	}
	fmt.Printf("generated %d deterministic sample(s) at %s (seed=%d)\n", len(manifest.Samples), *out, *seed)
	return nil
}

func runInspect(args []string) *werr.Error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	path := fs.String("sample", defaultSamplePath, "sample manifest")
	id := fs.String("id", "", "sample id")
	index := fs.Int("index", 0, "sample index")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return werr.New(werr.CodeState, "%v", err)
	}
	sample, err := loadSample(*path, *id, *index)
	if err != nil {
		return err
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(sample, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("sample: %s\nrate: %d Hz\nlayout: %s (%d channels)\nirSamples: %d\ndrySamples: %d\n", sample.ID, sample.SampleRate, sample.Layout.Name, len(sample.Layout.Channels), sample.IRSamples, len(sample.Dry.Samples))
	for _, ch := range sample.Channels {
		fmt.Printf("  %-4s direct=%d directEnergy=%.6f drr=%.3f dB itd=%d reflections=", ch.ID, ch.Truth.DirectArrival, ch.Truth.DirectEnergy, ch.Truth.DRRDB, ch.Truth.ITDSamples)
		for _, refl := range ch.Truth.Reflections {
			fmt.Printf("%d/%.4f ", refl.Arrival, refl.Energy)
		}
		fmt.Println()
	}
	return nil
}

func runDiagnostics(args []string) *werr.Error {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	path := fs.String("sample", defaultSamplePath, "sample manifest")
	id := fs.String("id", "", "sample id")
	index := fs.Int("index", 0, "sample index")
	db := fs.String("db", filepath.Join(".workbench", "checkpoint.sqlite"), "checkpoint database")
	frames := fs.Int("frames", 0, "number of frames to process; 0 means all")
	batch := fs.Bool("batch", false, "use bounded parallel estimation batches")
	interactive := fs.Bool("interactive", false, "pause and advance one action at a time")
	var paramsFlag paramFlags
	paramsFlag.add(fs)
	var inject injectionFlags
	inject.add(fs)
	if err := fs.Parse(args); err != nil {
		return werr.New(werr.CodeState, "%v", err)
	}
	sample, err := loadOrGenerateSample(*path, *id, *index)
	if err != nil {
		return err
	}
	params, err := paramsFlag.apply(model.DefaultParams(sample), sample)
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*db)
	if err != nil {
		return err
	}
	s, err := session.Open(sample, params, store, inject.value())
	if err != nil {
		store.Close()
		return err
	}
	defer s.Close()
	printSessionHeader(sample, params, s)
	if *interactive {
		if err := interactiveLoop(s); err != nil {
			return err
		}
	} else {
		target := *frames
		if target <= 0 {
			target = params.TotalFrames(len(sample.Dry.Samples))
		}
		for s.Progress().Committed < target {
			if *batch {
				n, err := s.ProcessAndCommitBatch(context.Background(), params.MaxWorkers*2)
				if err != nil {
					printFrames(s)
					return err
				}
				if n == 0 {
					break
				}
			} else {
				if _, err := s.ProcessAndCommitNext(context.Background()); err != nil {
					printFrames(s)
					printErrorContext(err)
					return err
				}
			}
		}
	}
	printFrames(s)
	printBufferEvents(s)
	if events := s.BufferUsage(); events != "" {
		fmt.Printf("buffer: %s\n", events)
	}
	return nil
}

func interactiveLoop(s *session.Session) *werr.Error {
	reader := bufio.NewReader(os.Stdin)
	for {
		progress := s.Progress()
		fmt.Printf("\nprogress %d/%d\n", progress.Committed, progress.Total)
		if progress.Committed == progress.Total {
			fmt.Println("complete")
			return nil
		}
		fmt.Print("[n]ext  [c]ommit  [p]rocess+commit  [b]atch  [r]etry  [q]uit > ")
		line, _ := reader.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "n":
			snapshot, err := s.ProcessNext(context.Background())
			if err != nil {
				printErrorContext(err)
			}
			printFrame(snapshot)
		case "c":
			snapshot, err := s.CommitNext(context.Background())
			if err != nil {
				printErrorContext(err)
			}
			printFrame(snapshot)
		case "p", "":
			snapshot, err := s.ProcessAndCommitNext(context.Background())
			if err != nil {
				printErrorContext(err)
			}
			printFrame(snapshot)
		case "b":
			if _, err := s.ProcessAndCommitBatch(context.Background(), 8); err != nil {
				printErrorContext(err)
			}
			printFrames(s)
		case "r":
			failed := firstState(s, session.StateFailed)
			if failed < 0 {
				fmt.Println("no failed frame")
				continue
			}
			s.SetInjection(session.Injection{})
			if err := s.RetryFrame(failed); err != nil {
				printErrorContext(err)
				continue
			}
			if snapshot, err := s.ProcessAndCommitNext(context.Background()); err != nil {
				printErrorContext(err)
			} else {
				printFrame(snapshot)
			}
		case "q":
			return nil
		default:
			fmt.Println("unknown action")
		}
	}
}

func runVerify(args []string) *werr.Error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	path := fs.String("sample", defaultSamplePath, "sample manifest")
	id := fs.String("id", "", "sample id")
	index := fs.Int("index", 0, "sample index")
	workdir := fs.String("workdir", "", "checkpoint directory (temporary if empty)")
	resumeAt := fs.Int("resume-at", 0, "commit count before restart")
	var paramsFlag paramFlags
	paramsFlag.add(fs)
	if err := fs.Parse(args); err != nil {
		return werr.New(werr.CodeState, "%v", err)
	}
	sample, err := loadOrGenerateSample(*path, *id, *index)
	if err != nil {
		return err
	}
	params, err := paramsFlag.apply(model.DefaultParams(sample), sample)
	if err != nil {
		return err
	}
	dir := *workdir
	if dir == "" {
		tempDir, tempErr := os.MkdirTemp("", "spatial-workbench-verify-*")
		if tempErr != nil {
			return werr.New(werr.CodeState, "create temp dir: %v", tempErr)
		}
		dir = tempDir
		defer os.RemoveAll(dir)
	}
	report, err := verify.RunThreePaths(context.Background(), sample, params, dir, *resumeAt)
	if err != nil {
		return err
	}
	fmt.Printf("sample=%s frames=%d resumeAt=%d\n", sample.ID, params.TotalFrames(len(sample.Dry.Samples)), report.ResumeAt)
	for _, comparison := range report.Compared {
		status := "PASS"
		if !comparison.Equal {
			status = "FAIL"
		}
		fmt.Printf("%s %s == %s", status, comparison.Left, comparison.Right)
		if comparison.Error != "" {
			fmt.Printf(": %s", comparison.Error)
		}
		fmt.Println()
	}
	if !report.Equal {
		return werr.New(werr.CodeDiff, "three-path comparison failed")
	}
	fmt.Println("three paths are sample-for-sample identical")
	return nil
}

func runRender(args []string) *werr.Error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	path := fs.String("sample", defaultSamplePath, "sample manifest")
	id := fs.String("id", "", "sample id")
	index := fs.Int("index", 0, "sample index")
	db := fs.String("db", filepath.Join(".workbench", "render.sqlite"), "checkpoint database")
	out := fs.String("out", filepath.Join("artifacts", "render.wav"), "float32 WAV output")
	var paramsFlag paramFlags
	paramsFlag.add(fs)
	if err := fs.Parse(args); err != nil {
		return werr.New(werr.CodeState, "%v", err)
	}
	sample, err := loadOrGenerateSample(*path, *id, *index)
	if err != nil {
		return err
	}
	params, err := paramsFlag.apply(model.DefaultParams(sample), sample)
	if err != nil {
		return err
	}
	store, err := checkpoint.Open(*db)
	if err != nil {
		return err
	}
	s, err := session.Open(sample, params, store, session.Injection{})
	if err != nil {
		store.Close()
		return err
	}
	defer s.Close()
	if _, err := s.ProcessAndCommitAll(context.Background()); err != nil {
		return err
	}
	output, err := s.Output()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return werr.New(werr.CodeState, "create output directory: %v", err)
	}
	file, openErr := os.Create(*out)
	if openErr != nil {
		return werr.New(werr.CodeState, "create WAV: %v", openErr)
	}
	writeErr := audio.WriteFloat32WAV(file, sample.SampleRate, output.Output)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return werr.New(werr.CodeState, "write WAV: %v %v", writeErr, closeErr)
	}
	samples := 0
	if len(output.Output) > 0 {
		samples = len(output.Output[0])
	}
	fmt.Printf("rendered %s: %d channels x %d samples at %d Hz -> %s\n", sample.ID, len(output.Output), samples, sample.SampleRate, *out)
	return nil
}

func loadSample(path, id string, index int) (model.Sample, *werr.Error) {
	manifest, err := loader.LoadFile(path)
	if err != nil {
		return model.Sample{}, err
	}
	if id != "" {
		for _, sample := range manifest.Samples {
			if sample.ID == id {
				return sample, nil
			}
		}
		return model.Sample{}, werr.New(werr.CodeState, "sample id %q not found", id)
	}
	if index < 0 || index >= len(manifest.Samples) {
		return model.Sample{}, werr.New(werr.CodeState, "sample index %d outside [0,%d)", index, len(manifest.Samples))
	}
	return manifest.Samples[index], nil
}

func loadOrGenerateSample(path, id string, index int) (model.Sample, *werr.Error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		manifest := generator.Generate(generator.Options{Seed: 20261124, Samples: 3, Prefix: "local"})
		if err := generator.WriteFile(path, manifest); err != nil {
			return model.Sample{}, werr.New(werr.CodeState, "generate default samples: %v", err)
		}
	}
	return loadSample(path, id, index)
}

func printSessionHeader(sample model.Sample, params model.Params, s *session.Session) {
	info := s.ResumeInfo()
	fmt.Printf("sample=%s rate=%d layout=%s channels=%d frames=%d\n", sample.ID, sample.SampleRate, sample.Layout.Name, len(sample.Layout.Channels), params.TotalFrames(len(sample.Dry.Samples)))
	fmt.Printf("params frame=%d hop=%d window=%d quant=%d interp=%d tolerance=%g workers=%d buffer=%d\n",
		params.FrameLength, params.Hop, params.WindowLength, params.QuantStep, params.InterpOrder, params.ErrorTolerance, params.MaxWorkers, params.BufferSamplesPerWorker)
	if info.Loaded > 0 || info.Invalidated {
		fmt.Printf("resume loaded=%d invalidated=%v reason=%s\n", info.Loaded, info.Invalidated, info.Reason)
	}
}

func printFrames(s *session.Session) {
	fmt.Printf("\n%-5s %-10s %-6s %-8s %-10s %-10s %-10s\n", "frame", "state", "seq", "delays", "early", "itd", "err")
	for _, frame := range s.Frames() {
		delays, early, itd := "-", "-", "-"
		if frame.Result != nil {
			var d, e, t []string
			for _, ch := range frame.Result.Reconstruction.Channels {
				d = append(d, fmt.Sprintf("%s:%d", ch.ID, ch.DirectDelay))
				e = append(e, fmt.Sprintf("%s:%.4g", ch.ID, ch.EarlyEnergy))
				t = append(t, fmt.Sprintf("%s:%d", ch.ID, ch.ITDSamples))
			}
			delays, early, itd = strings.Join(d, ","), strings.Join(e, ","), strings.Join(t, ",")
		}
		errText := ""
		if frame.Error != nil {
			errText = frame.Error.Code
		}
		fmt.Printf("%-5d %-10s %-6d %-8s %-10s %-10s %-10s\n", frame.Index, frame.State, frame.Seq, delays, early, itd, errText)
	}
}

func printFrame(frame session.FrameSnapshot) {
	if frame.Result == nil {
		fmt.Printf("frame %d state=%s\n", frame.Index, frame.State)
		return
	}
	fmt.Printf("frame %d state=%s direct/early/itd:", frame.Index, frame.State)
	for _, ch := range frame.Result.Reconstruction.Channels {
		fmt.Printf(" %s=%d/%.5g/%d", ch.ID, ch.DirectDelay, ch.EarlyEnergy, ch.ITDSamples)
	}
	fmt.Println()
}

func printBufferEvents(s *session.Session) {
	for _, event := range s.BufferEvents() {
		fmt.Printf("buffer-event: %s samples=%d used=%d capacity=%d\n", event.Reason, event.Samples, event.Used, event.Capacity)
	}
}

func printErrorContext(err *werr.Error) {
	if err == nil {
		return
	}
	fmt.Printf("error %s field=%s frame=%d channel=%s actual=%v expected=%v\n", err.Code, err.Field, err.Frame, err.Channel, err.Actual, err.Expected)
}

func firstState(s *session.Session, state session.State) int {
	for _, frame := range s.Frames() {
		if frame.State == state {
			return frame.Index
		}
	}
	return -1
}
