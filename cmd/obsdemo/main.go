package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"obsproj/internal/kernel"
	"obsproj/internal/model"
	"obsproj/internal/runner"
	"obsproj/internal/scenario"
	"obsproj/internal/schema"
)

type summary struct {
	Seed          int64             `json:"seed"`
	SessionID     string            `json:"session_id"`
	SemanticHash  string            `json:"semantic_hash"`
	FinalStatus   string            `json:"final_status"`
	Windows       int               `json:"windows"`
	Decisions     int               `json:"decisions"`
	Packets       int               `json:"packets"`
	SchemaHashes  map[string]string `json:"schema_hashes"`
	LoadedHashes  map[string]string `json:"loaded_hashes"`
	PathAgreement bool              `json:"path_agreement"`
	QueueEvents   int               `json:"queue_events"`
	LossyCode     string            `json:"lossy_code,omitempty"`
	LossyEntries  int               `json:"lossy_entries,omitempty"`
	Injected      map[string]string `json:"injected_diagnostics"`
}

func main() {
	outDir := flag.String("out", "artifacts/generated", "artifact output directory")
	seed := flag.Int64("seed", scenario.DefaultSeed, "deterministic generator seed")
	workers := flag.Int("workers", 4, "maximum mapping worker count")
	buffer := flag.Int64("buffer", 65536, "per-worker pixel-buffer capacity")
	split := flag.Int("split", 3, "number of windows committed before resume")
	flag.Parse()

	bundle, err := scenario.Generate(*seed)
	must(err)
	must(scenario.WriteBundle(*outDir, bundle))

	loadedHashes := map[string]string{}
	for version, data := range map[string][]byte{
		schema.Version1: bundle.V1,
		schema.Version2: bundle.V2,
		schema.Version3: bundle.V3,
	} {
		session, decodeErr := schema.Decode(data)
		must(decodeErr)
		loadedHashes[version] = session.Fingerprint()
		if loadedHashes[version] != bundle.Session.Fingerprint() {
			panic(fmt.Sprintf("%s loaded to a different internal model", version))
		}
	}

	options := runner.DefaultOptions()
	options.Engine.MapPool.Workers = *workers
	options.Engine.MapPool.BufferPixels = *buffer
	options.SplitIndex = *split

	realtimeDir, err := runner.NewTempStoreDir("obs-realtime-")
	must(err)
	defer os.RemoveAll(realtimeDir)
	options.StoreDir = realtimeDir
	realtime, err := runner.RunRealtime(bundle.Session, options)
	must(err)

	full, err := runner.RunFullReplay(bundle.Session, options)
	must(err)

	resumeDir, err := runner.NewTempStoreDir("obs-resume-")
	must(err)
	defer os.RemoveAll(resumeDir)
	resumeOptions := options
	resumeOptions.StoreDir = resumeDir
	resumed, err := runner.RunResume(bundle.Session, resumeOptions)
	must(err)

	must(runner.CompareResults(full, realtime))
	must(runner.CompareResults(full, resumed))

	lossy, err := scenario.GenerateLossy(*seed)
	must(err)
	lossyV1, decodeErr := schema.Decode(lossy.V1.Data)
	must(decodeErr)
	lossyCode := ""
	lossyEntries := 0
	if lossy.V1Err != nil {
		lossyCode = string(lossy.V1Err.Code)
		lossyEntries = len(lossy.V1Err.Losses)
	}

	packets := 0
	decisions := 0
	for _, commit := range full.Commits {
		decisions += len(commit.Decisions)
		for _, screen := range commit.Screens {
			packets += len(screen.Packets)
		}
	}
	hash := bundle.Session.Fingerprint()
	loadedHashes[schema.Version1+"_lossy_degraded"] = lossyV1.Fingerprint()
	report := summary{
		Seed:          bundle.Session.Seed,
		SessionID:     bundle.Session.ID,
		SemanticHash:  hash,
		FinalStatus:   string(full.Final.SessionState),
		Windows:       len(full.Commits),
		Decisions:     decisions,
		Packets:       packets,
		SchemaHashes:  bundle.Truth.SchemaHashes,
		LoadedHashes:  loadedHashes,
		PathAgreement: true,
		QueueEvents:   len(realtime.Queue),
		LossyCode:     lossyCode,
		LossyEntries:  lossyEntries,
		Injected:      injectedDiagnostics(bundle),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(report))
}

func injectedDiagnostics(bundle *scenario.Bundle) map[string]string {
	out := map[string]string{}
	_, decodeErr := schema.Decode([]byte("{broken"))
	out["E_DECODE"] = errorCode(decodeErr)

	orderSession := bundle.Session.Clone()
	for i := range orderSession.Commands {
		if orderSession.Commands[i].Window == 2 {
			orderSession.Commands[i].At = -1
		}
	}
	_, orderErr := kernel.NewEngine(orderSession, kernel.DefaultEngineOptions())
	out["E_ORDER"] = errorCode(orderErr)

	capSession := bundle.Session.Clone()
	badScale := model.Scale100
	capSession.Events = append(capSession.Events, model.Event{
		ID: "demo-cap", Window: 3, At: 1490, Kind: model.EventTopology, ScreenID: "C",
		Topology: &model.TopologyPatch{Scale: &badScale},
	})
	out["E_CAP"] = errorCode(firstProcessError(capSession, kernel.DefaultEngineOptions()))

	segOptions := kernel.DefaultEngineOptions()
	segOptions.InjectWindowFailure = 5
	segOptions.InjectScreenFailure = "B"
	out["E_SEG"] = errorCode(firstProcessError(bundle.Session, segOptions))
	return out
}

func firstProcessError(session *model.Session, options kernel.EngineOptions) error {
	engine, err := kernel.NewEngine(session, options)
	if err != nil {
		return err
	}
	for _, window := range engine.WindowNumbers() {
		if _, _, processErr := engine.ProcessWindow(window); processErr != nil {
			return processErr
		}
	}
	return nil
}

func errorCode(err error) string {
	if err == nil {
		return "<none>"
	}
	if kernelErr, ok := model.AsKernelError(err); ok && kernelErr != nil {
		return string(kernelErr.Code)
	}
	return fmt.Sprintf("%T", err)
}

func must(err error) {
	if err == nil {
		return
	}
	if kernelErr, ok := model.AsKernelError(err); ok {
		if kernelErr == nil {
			return
		}
		data, _ := json.MarshalIndent(kernelErr, "", "  ")
		fmt.Fprintln(os.Stderr, string(data))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "%T: %v\n", err, err)
	os.Exit(1)
}
