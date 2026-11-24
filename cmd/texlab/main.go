package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"texlab/internal/apperr"
	"texlab/internal/checkpoint"
	"texlab/internal/engine"
	"texlab/internal/gtex"
	"texlab/internal/model"
	"texlab/internal/plan"
	"texlab/internal/runrecord"
	"texlab/internal/sample"
	"texlab/internal/state"
	"texlab/internal/verify"
	"texlab/internal/webui"
	"texlab/internal/workbench"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "acceptance":
		err = cmdAcceptance(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `TexLab deterministic bit-depth/channel workbench

  texlab generate   [-samples DIR]
  texlab list       [-samples DIR]
  texlab run        [-samples DIR] [-data DIR] [-name NAME]
  texlab verify     [-samples DIR] [-data DIR] [-name NAME] [-report FILE]
  texlab acceptance [-samples DIR] [-out DIR]
  texlab serve      [-samples DIR] [-data DIR] [-addr HOST:PORT]
  texlab doctor`)
	os.Exit(2)
}

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	root := fs.String("samples", "samples", "sample output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := sample.GenerateAll(*root); err != nil {
		return err
	}
	for _, spec := range sample.Specs() {
		p, err := sample.Load(*root, spec.Name)
		if err != nil {
			return err
		}
		fmt.Printf("%-16s %4d×%-4d %2d-bit %-4s plan=%s truth=%s\n",
			spec.Name, p.Source.Width, p.Source.Height, p.Source.Depth, p.Source.Layout,
			short(model.PlanFingerprint(p.Plan)), short(p.Truth.Fingerprint))
	}
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	root := fs.String("samples", "samples", "sample directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	names, err := sample.Names(*root)
	if err != nil {
		return err
	}
	for _, name := range names {
		p, err := sample.Load(*root, name)
		if err != nil {
			return err
		}
		fmt.Printf("%s\t%d×%d\t%d-bit\t%s\t%d entries\tseed %d\n",
			name, p.Source.Width, p.Source.Height, p.Source.Depth, p.Source.Layout, len(p.Plan.Entries), p.Manifest.Seed)
	}
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := fs.String("samples", "samples", "sample directory")
	data := fs.String("data", filepath.Join(".texlab", "data"), "data directory")
	name := fs.String("name", "", "sample name")
	reportPath := fs.String("report", "", "report path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	p, err := sample.Load(*root, *name)
	if err != nil {
		return err
	}
	if *reportPath == "" {
		*reportPath = filepath.Join(*data, "reports", *name+".json")
	}
	report, err := verify.Run(verify.Options{
		PlanPath: filepath.Join(p.Dir, "plan.json"), TruthPath: filepath.Join(p.Dir, "truth.json"),
		CheckpointDir: filepath.Join(*data, "checkpoints", *name),
		RecordPath:    filepath.Join(*data, "runs", *name+".json"),
		ReportPath:    *reportPath, SourceImage: p.Source, SourceFingerprint: p.SourceFP,
	})
	if report != nil {
		fmt.Printf("verification=%s report=%s final=%s\n", report.Status, *reportPath, short(report.FinalDigest))
	}
	return err
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	root := fs.String("samples", "samples", "sample directory")
	data := fs.String("data", filepath.Join(".texlab", "data"), "data directory")
	name := fs.String("name", "", "sample name")
	workers := fs.Int("workers", 4, "worker count")
	pixels := fs.Int("pixels", 262144, "pixel scratch limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	svc, err := workbench.NewService(*root, *data, *workers, *pixels)
	if err != nil {
		return err
	}
	st, err := svc.LoadSample(*name)
	if err != nil {
		return err
	}
	fmt.Printf("sample=%s plan=%s truth=%s committed=%d/%d cache_hits=%d reused=%d invalidated=%d verification=%s\n",
		st.Sample.Name, short(st.PlanFingerprint), short(st.Verification.TruthHash),
		st.CommittedSegments, st.TotalSegments, st.CacheHits, st.ReusedCells, st.InvalidatedCells, st.Verification.Status)
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	root := fs.String("samples", "samples", "sample directory")
	data := fs.String("data", filepath.Join(".texlab", "data"), "data directory")
	addr := fs.String("addr", "127.0.0.1:34115", "listen address")
	workers := fs.Int("workers", 4, "worker count")
	pixels := fs.Int("pixels", 262144, "pixel scratch limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := workbench.NewService(*root, *data, *workers, *pixels)
	if err != nil {
		return err
	}
	server := &webui.Server{Service: svc, AssetDir: filepath.Join("frontend", "dist")}
	fmt.Printf("TexLab headless workbench: http://%s\n", *addr)
	return http.ListenAndServe(*addr, server.Handler())
}

func cmdDoctor(args []string) error {
	fmt.Printf("go=%s os=%s arch=%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("algorithm=%s\n", model.AlgorithmVersion)
	cwd, _ := os.Getwd()
	data := filepath.Join(cwd, ".texlab", "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(data, ".write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return err
	}
	_ = os.Remove(probe)
	fmt.Printf("data_dir=%s writable=true\n", data)
	return nil
}

type acceptanceReport struct {
	Schema     string             `json:"schema"`
	Algorithm  string             `json:"algorithm_version"`
	Samples    []sampleAcceptance `json:"samples"`
	ErrorCodes map[string]string  `json:"error_codes"`
	Status     string             `json:"status"`
}

type sampleAcceptance struct {
	Name                  string   `json:"name"`
	Width                 int      `json:"width"`
	Height                int      `json:"height"`
	Depth                 int      `json:"depth"`
	Layout                string   `json:"layout"`
	Seed                  int64    `json:"seed"`
	PlanFingerprint       string   `json:"plan_fingerprint"`
	TruthFingerprint      string   `json:"truth_fingerprint"`
	VerificationStatus    string   `json:"verification_status"`
	VerificationReportSHA string   `json:"verification_report_sha256"`
	ReportByteIdentical   bool     `json:"report_byte_identical"`
	IncrementalEqualFull  bool     `json:"incremental_equal_full"`
	ReorderEqualFull      bool     `json:"reorder_equal_full"`
	ResumeEqualFull       bool     `json:"resume_equal_full"`
	SerialEqualParallel   bool     `json:"serial_equal_parallel"`
	InvalidatedCells      int      `json:"invalidated_cells"`
	ReusedCells           int      `json:"reused_cells"`
	AffectedEntrySegments []string `json:"affected_entry_segments"`
	CommitSequenceHash    string   `json:"commit_sequence_hash"`
	FinalDigest           string   `json:"final_digest"`
}

func cmdAcceptance(args []string) error {
	fs := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	root := fs.String("samples", "samples", "sample directory")
	out := fs.String("out", filepath.Join("artifacts", "acceptance"), "acceptance output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := sample.GenerateAll(*root); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	report := acceptanceReport{
		Schema: "texlab-acceptance-v1", Algorithm: model.AlgorithmVersion,
		ErrorCodes: map[string]string{},
	}
	for _, spec := range sample.Specs() {
		p, err := sample.Load(*root, spec.Name)
		if err != nil {
			return err
		}
		fmt.Printf("[%s] initial full run + checkpoint…\n", spec.Name)
		work := filepath.Join(*out, ".work", spec.Name)
		_ = os.RemoveAll(work)
		cp1 := filepath.Join(work, "cp-initial")
		store1, err := checkpoint.NewStore(cp1, p.Plan, p.SourceFP)
		if err != nil {
			return err
		}
		initial, err := engine.Run(p.Plan, p.Source, p.SourceFP, engine.Options{Workers: 4, PixelBufferPerWorker: 262144, Sink: store1})
		if err != nil {
			return err
		}
		recPath := filepath.Join(work, "run.json")
		if err := runrecord.Write(recPath, runrecord.Build(initial)); err != nil {
			return err
		}
		reportA := filepath.Join(work, "verify-a.json")
		reportB := filepath.Join(work, "verify-b.json")
		verifyOpts := verify.Options{
			PlanPath: filepath.Join(p.Dir, "plan.json"), TruthPath: filepath.Join(p.Dir, "truth.json"),
			CheckpointDir: cp1, RecordPath: recPath, ReportPath: reportA,
			SourceImage: p.Source, SourceFingerprint: p.SourceFP,
		}
		vr, err := verify.Run(verifyOpts)
		if err != nil {
			return err
		}
		verifyOpts.ReportPath = reportB
		if _, err := verify.Run(verifyOpts); err != nil {
			return err
		}
		rawA, _ := os.ReadFile(reportA)
		rawB, _ := os.ReadFile(reportB)
		identical := bytes.Equal(rawA, rawB)
		if !identical {
			return apperr.New(apperr.EDiff, "verification reports are not byte-identical for %s", spec.Name)
		}
		finalReport := filepath.Join(*out, spec.Name+"-verification-report.json")
		if err := os.WriteFile(finalReport, rawA, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(rawA)

		edited := p.Plan.Clone()
		edited.Revision++
		edited.Entries[3].Permutation = []int{0, 1, 2, 3}
		if err := plan.Validate(edited, p.Source); err != nil {
			return err
		}
		fmt.Printf("[%s] incremental edit + full comparison…\n", spec.Name)
		cp2 := filepath.Join(work, "cp-incremental")
		store2, err := checkpoint.NewStore(cp2, edited, p.SourceFP)
		if err != nil {
			return err
		}
		incremental, err := engine.Run(edited, p.Source, p.SourceFP, engine.Options{
			Workers: 4, PixelBufferPerWorker: 262144, Previous: initial, Sink: store2,
		})
		if err != nil {
			return err
		}
		full, err := engine.Run(edited, p.Source, p.SourceFP, engine.Options{Workers: 1, PixelBufferPerWorker: 131072})
		if err != nil {
			return err
		}
		if err := engine.CompareRuns(full, incremental); err != nil {
			return err
		}
		parallel, err := engine.Run(edited, p.Source, p.SourceFP, engine.Options{Workers: 8, PixelBufferPerWorker: 524288})
		if err != nil {
			return err
		}
		if err := engine.CompareRuns(full, parallel); err != nil {
			return err
		}

		fmt.Printf("[%s] stage-order edit + full comparison…\n", spec.Name)
		reordered := p.Plan.Clone()
		reordered.Entries[0], reordered.Entries[1] = reordered.Entries[1], reordered.Entries[0]
		plan.Reindex(&reordered)
		reordered.Revision++
		if err := plan.Validate(reordered, p.Source); err != nil {
			return err
		}
		orderIncremental, err := engine.Run(reordered, p.Source, p.SourceFP, engine.Options{
			Workers: 4, PixelBufferPerWorker: 262144, Previous: initial,
		})
		if err != nil {
			return err
		}
		orderFull, err := engine.Run(reordered, p.Source, p.SourceFP, engine.Options{
			Workers: 2, PixelBufferPerWorker: 131072,
		})
		if err != nil {
			return err
		}
		if err := engine.CompareRuns(orderFull, orderIncremental); err != nil {
			return err
		}

		fmt.Printf("[%s] checkpoint interrupt/resume…\n", spec.Name)
		cp3 := filepath.Join(work, "cp-resume")
		store3, err := checkpoint.NewStore(cp3, edited, p.SourceFP)
		if err != nil {
			return err
		}
		partial, err := engine.Run(edited, p.Source, p.SourceFP, engine.Options{
			Workers: 3, PixelBufferPerWorker: 262144, MaxCommits: 4, Sink: store3,
		})
		if err != nil {
			return err
		}
		if len(partial.Segments) != 4 {
			return apperr.New(apperr.ECheckpoint, "partial run committed %d/4 segments", len(partial.Segments))
		}
		loaded, status, err := checkpoint.LoadAll(cp3, edited, p.SourceFP)
		if err != nil {
			return err
		}
		if !status.Valid || len(loaded) != 4 {
			return apperr.New(apperr.ECheckpoint, "resume snapshot invalid: %+v", status)
		}
		store4, err := checkpoint.NewStore(cp3, edited, p.SourceFP)
		if err != nil {
			return err
		}
		resumed, err := engine.Run(edited, p.Source, p.SourceFP, engine.Options{
			Workers: 5, PixelBufferPerWorker: 262144, Resume: loaded, Sink: store4,
		})
		if err != nil {
			return err
		}
		if err := engine.CompareRuns(full, resumed); err != nil {
			return err
		}

		var affected []string
		for seg := 0; seg < len(model.BuildSegments(p.Source.Width, p.Source.Height, p.Plan.Segments)); seg++ {
			for _, id := range []string{"regional_swap", "drop_alpha"} {
				st := incremental.StageIndex[id][seg]
				if st != nil && st.Invalidated {
					affected = append(affected, fmt.Sprintf("%s/S%02d", id, seg))
				}
			}
		}
		sort.Strings(affected)
		rec := runrecord.Build(incremental)
		report.Samples = append(report.Samples, sampleAcceptance{
			Name: spec.Name, Width: p.Source.Width, Height: p.Source.Height,
			Depth: p.Source.Depth, Layout: p.Source.Layout, Seed: int64(p.Manifest.Seed),
			PlanFingerprint: model.PlanFingerprint(p.Plan), TruthFingerprint: p.Truth.Fingerprint,
			VerificationStatus: vr.Status, VerificationReportSHA: hex.EncodeToString(sum[:]),
			ReportByteIdentical: identical, IncrementalEqualFull: true, ResumeEqualFull: true,
			ReorderEqualFull: true, SerialEqualParallel: true, InvalidatedCells: incremental.InvalidatedCells,
			ReusedCells: incremental.ReusedCells, AffectedEntrySegments: affected,
			CommitSequenceHash: hashInts(rec.CommitSequence), FinalDigest: rec.FinalDigest,
		})
		fmt.Printf("[%s] PASS · invalidated=%d reused=%d final=%s\n", spec.Name, incremental.InvalidatedCells, incremental.ReusedCells, short(rec.FinalDigest))
		_ = os.RemoveAll(filepath.Join(work, "cp-initial"))
		_ = os.RemoveAll(filepath.Join(work, "cp-incremental"))
		_ = os.RemoveAll(filepath.Join(work, "cp-resume"))
	}
	report.ErrorCodes = exerciseErrorCodes()
	report.Status = "PASS"
	raw, _ := json.MarshalIndent(report, "", "  ")
	reportPath := filepath.Join(*out, "acceptance-report.json")
	if err := os.WriteFile(reportPath, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(*out, ".work"))
	fmt.Printf("acceptance PASS · report=%s · samples=%d\n", reportPath, len(report.Samples))
	return nil
}

func exerciseErrorCodes() map[string]string {
	out := map[string]string{}
	// E_DECODE
	bad := make([]byte, 60)
	copy(bad, "NOTGTEX!")
	if _, _, err := gtex.Decode(bad); apperr.IsCode(err, apperr.EDecode) {
		out[apperr.EDecode] = "GTEX corruption rejected"
	}
	// E_PLAN
	im := model.NewImage(8, 8, 8, 4, "RGBA", "straight")
	badPlan := model.Plan{
		Schema: "texlab-plan-v1", Revision: 1, Segments: model.SegmentSpec{Cols: 2, Rows: 2},
		Regions: []model.Rect{{ID: "full", W: 8, H: 8}},
		Entries: []model.Entry{{
			Index: 1, ID: "bad", TargetDepth: 12, TargetChannels: 4,
			Permutation: []int{0, 1, 2, 3}, Alpha: "straight", Quantize: "round", Regions: []string{"full"},
		}},
	}
	if err := plan.Validate(badPlan, im); apperr.IsCode(err, apperr.EPlan) {
		out[apperr.EPlan] = "illegal target_depth rejected with entry/field context"
	}
	// E_STATE and E_VERSION
	m := state.NewMatrix(1, 1, 1, 1)
	e := model.Entry{Index: 1, ID: "e"}
	_ = m.Transition(1, 0, state.Processing, 1, "", nil)
	_ = m.Transition(1, 0, state.Committed, 1, "ok", &e)
	if err := m.Transition(1, 0, state.Processing, 1, "", nil); apperr.IsCode(err, apperr.EState) {
		out[apperr.EState] = "committed -> processing rejected without mutation"
	}
	if err := m.Transition(1, 0, state.Processing, 2, "", nil); apperr.IsCode(err, apperr.EVersion) {
		out[apperr.EVersion] = "version mix rejected with segment and both versions"
	}
	// E_DIFF
	want := &engine.RunResult{
		PlanFingerprint: "x", CommitSequence: []int{0},
		Plan:       model.Plan{Entries: []model.Entry{{Index: 1, ID: "e"}}},
		Segments:   []*engine.SegmentCommit{{Segment: model.Segment{Index: 0}, Final: imagePixel(1), FinalHash: "a"}},
		StageIndex: map[string]map[int]*engine.StageRecord{},
	}
	got := *want
	got.Segments = []*engine.SegmentCommit{{Segment: model.Segment{Index: 0}, Final: imagePixel(2), FinalHash: "b"}}
	if err := engine.CompareRuns(want, &got); apperr.IsCode(err, apperr.EDiff) {
		ae := apperr.As(err)
		if ae.Coordinate != nil {
			out[apperr.EDiff] = fmt.Sprintf("first pixel reported at (%d,%d) for entry %d segment %d", ae.Coordinate.X, ae.Coordinate.Y, ae.EntryIndex, ae.Segment)
		}
	}
	return out
}

func imagePixel(v uint16) model.Image {
	im := model.NewImage(1, 1, 8, 4, "RGBA", "straight")
	im.Pixels[0] = v
	return im
}

func hashInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return model.HashParts(parts...)
}

func short(v string) string {
	if len(v) > 16 {
		return v[:16]
	}
	return v
}
