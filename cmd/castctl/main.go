package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"example.com/multiscreen-cast/kernel"
	"example.com/multiscreen-cast/protocol"
	"example.com/multiscreen-cast/store"
	"example.com/multiscreen-cast/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err *protocol.KernelError
	switch os.Args[1] {
	case "verify":
		err = verifyCommand(os.Args[2:])
	case "doctor":
		err = doctorCommand(os.Args[2:])
	case "inject":
		err = injectCommand(os.Args[2:])
	case "run":
		err = runCommand(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		payload, _ := json.MarshalIndent(struct {
			OK    bool                  `json:"ok"`
			Error *protocol.KernelError `json:"error"`
		}{OK: false, Error: err}, "", "  ")
		fmt.Fprintln(os.Stderr, string(payload))
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: castctl <verify|doctor|inject|run> [flags]")
}

func verifyCommand(args []string) *protocol.KernelError {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	samplePath := flags.String("sample", "samples/sample.json", "sample JSON path")
	workDir := flags.String("workdir", "", "temporary verification directory")
	checkAnomalies := flags.Bool("anomalies", true, "run four anomaly injection chains")
	if err := flags.Parse(args); err != nil {
		return protocol.Errorf(protocol.CodeSchema, "castctl", "$", "%v", err)
	}
	set, loadErr := protocol.LoadSampleSet(*samplePath)
	if loadErr != nil {
		return loadErr
	}
	report, verifyErr := verify.VerifySample(context.Background(), set, *workDir)
	if verifyErr != nil {
		return verifyErr
	}
	var anomalyReport *verify.AnomalyReport
	if *checkAnomalies {
		var anomalyErr *protocol.KernelError
		anomalyReport, anomalyErr = verify.CheckAnomalyChains(context.Background(), set)
		if anomalyErr != nil {
			return anomalyErr
		}
	}
	payload, _ := json.MarshalIndent(struct {
		OK        bool                  `json:"ok"`
		Report    *verify.Report        `json:"report"`
		Anomalies *verify.AnomalyReport `json:"anomalies,omitempty"`
	}{OK: true, Report: report, Anomalies: anomalyReport}, "", "  ")
	fmt.Println(string(payload))
	return nil
}

func doctorCommand(args []string) *protocol.KernelError {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return protocol.Errorf(protocol.CodeSchema, "castctl", "$", "%v", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return protocol.Errorf(protocol.CodeStore, "doctor", "$", "cannot get working directory: %v", err)
	}
	tempDir, err := os.MkdirTemp(workDir, ".castctl-doctor-*")
	if err != nil {
		return protocol.Errorf(protocol.CodeStore, workDir, "$", "working directory is not writable: %v", err)
	}
	defer os.RemoveAll(tempDir)
	dbPath := filepath.Join(tempDir, "probe.sqlite")
	checkpointStore, openErr := store.Open(dbPath)
	if openErr != nil {
		return protocol.AsKernelError(openErr)
	}
	if closeErr := checkpointStore.Close(); closeErr != nil {
		return protocol.Errorf(protocol.CodeStore, dbPath, "$", "cannot close probe database: %v", closeErr)
	}
	listener, listenErr := net.Listen("tcp", "127.0.0.1:0")
	loopback := listenErr == nil
	loopbackAddress := ""
	if loopback {
		loopbackAddress = listener.Addr().String()
		_ = listener.Close()
	}
	if !loopback {
		return protocol.Errorf(protocol.CodeStore, "doctor", "$.loopback", "127.0.0.1 loopback is unavailable: %v", listenErr)
	}
	payload, _ := json.MarshalIndent(struct {
		OK              bool   `json:"ok"`
		GoVersion       string `json:"go_version"`
		WorkingDir      string `json:"working_dir"`
		SQLiteWritable  bool   `json:"sqlite_writable"`
		Loopback        bool   `json:"loopback"`
		LoopbackAddress string `json:"loopback_address"`
	}{
		OK: true, GoVersion: runtime.Version(), WorkingDir: workDir, SQLiteWritable: true,
		Loopback: loopback, LoopbackAddress: loopbackAddress,
	}, "", "  ")
	fmt.Println(string(payload))
	return nil
}

func injectCommand(args []string) *protocol.KernelError {
	flags := flag.NewFlagSet("inject", flag.ContinueOnError)
	samplePath := flags.String("sample", "samples/sample.json", "input sample path")
	outputPath := flags.String("out", "", "output path (defaults to <sample>.inject.json)")
	kind := flags.String("kind", string(verify.AnomalyOrder), "decode|order|cap|segment")
	if err := flags.Parse(args); err != nil {
		return protocol.Errorf(protocol.CodeSchema, "castctl", "$", "%v", err)
	}
	if *outputPath == "" {
		*outputPath = *samplePath + ".inject.json"
	}
	if verify.AnomalyKind(*kind) == verify.AnomalyDecode {
		if err := os.WriteFile(*outputPath, []byte(`{"sessions":[`), 0o644); err != nil {
			return protocol.Errorf(protocol.CodeStore, *outputPath, "$", "cannot write malformed sample: %v", err)
		}
		fmt.Printf("wrote E_DECODE sample to %s\n", *outputPath)
		return nil
	}
	set, loadErr := protocol.LoadSampleSet(*samplePath)
	if loadErr != nil {
		return loadErr
	}
	mutationErr := verify.ApplyInputAnomaly(set, verify.AnomalyKind(*kind))
	if mutationErr != nil {
		return mutationErr
	}
	payload, err := verify.MarshalSampleSet(set)
	if err != nil {
		return protocol.Errorf(protocol.CodeDecode, *outputPath, "$", "cannot marshal anomaly sample: %v", err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(*outputPath, payload, 0o644); err != nil {
		return protocol.Errorf(protocol.CodeStore, *outputPath, "$", "cannot write anomaly sample: %v", err)
	}
	fmt.Printf("wrote %s anomaly sample to %s\n", *kind, *outputPath)
	return nil
}

func runCommand(args []string) *protocol.KernelError {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	samplePath := flags.String("sample", "samples/sample.json", "sample JSON path")
	mode := flags.String("mode", "oneshot", "oneshot|realtime|recovery")
	dbPath := flags.String("db", "checkpoints.sqlite", "SQLite checkpoint path")
	sessionID := flags.String("session", "", "session id (defaults to first)")
	if err := flags.Parse(args); err != nil {
		return protocol.Errorf(protocol.CodeSchema, "castctl", "$", "%v", err)
	}
	set, loadErr := protocol.LoadSampleSet(*samplePath)
	if loadErr != nil {
		return loadErr
	}
	var session *protocol.Session
	for i := range set.Sessions {
		if *sessionID == "" || set.Sessions[i].ID == *sessionID {
			session = &set.Sessions[i]
			break
		}
	}
	if session == nil {
		return protocol.Errorf(protocol.CodeRef, "castctl", "$.session", "session %q not found", *sessionID)
	}
	cfg := kernel.Config{Mapping: kernel.DefaultMappingConfig()}
	var checkpointStore *store.Store
	switch *mode {
	case "oneshot", "realtime", "recovery":
	default:
		return protocol.Errorf(protocol.CodeSchema, "castctl", "$.mode", "unknown mode %q", *mode)
	}
	if *mode != "oneshot" {
		opened, openErr := store.Open(*dbPath)
		if openErr != nil {
			return protocol.AsKernelError(openErr)
		}
		checkpointStore = opened
		defer checkpointStore.Close()
		cfg.Checkpoint = checkpointStore
	}
	var engine *kernel.Engine
	var engineErr *protocol.KernelError
	restored := false
	if *mode == "recovery" {
		snapshot, status, loadErr := checkpointStore.LoadLatest(session, protocol.CurrentAlgorithmVersion)
		if loadErr != nil {
			return protocol.AsKernelError(loadErr)
		}
		if status.Invalidated {
			fmt.Fprintf(os.Stderr, "checkpoint invalidated, full replay required: %s\n", status.Reason)
		}
		if snapshot != nil {
			engine, engineErr = kernel.RestoreEngine(session, cfg, *snapshot)
			restored = true
		} else {
			engine, engineErr = kernel.NewEngine(session, cfg)
		}
	} else {
		engine, engineErr = kernel.NewEngine(session, cfg)
	}
	if engineErr != nil {
		return engineErr
	}
	trace, runErr := engine.Run(context.Background())
	if runErr != nil {
		return runErr
	}
	payload, _ := json.MarshalIndent(struct {
		OK         bool                  `json:"ok"`
		Mode       string                `json:"mode"`
		Restored   bool                  `json:"restored"`
		SessionID  string                `json:"session_id"`
		Windows    int                   `json:"windows"`
		FinalState protocol.SessionState `json:"final_state"`
	}{
		OK: true, Mode: *mode, Restored: restored, SessionID: session.ID,
		Windows: len(trace.Windows), FinalState: trace.FinalState,
	}, "", "  ")
	fmt.Println(string(payload))
	return nil
}
