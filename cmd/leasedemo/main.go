package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	leasekit "leasekit"
)

type summary struct {
	OutputDir             string   `json:"outputDir"`
	Seed                  int64    `json:"seed"`
	SchemaParity          bool     `json:"schemaParity"`
	TruthParity           bool     `json:"truthParity"`
	IncrementalParity     bool     `json:"incrementalParity"`
	ParallelParity        bool     `json:"parallelParity"`
	SQLiteHeader          bool     `json:"sqliteHeader"`
	CrashDetectionCode    string   `json:"crashDetectionCode"`
	RecoveryCode          string   `json:"recoveryCode"`
	ExceptionObservations int      `json:"exceptionObservations"`
	AllExceptionCodes     bool     `json:"allExceptionCodes"`
	SourceUnmodified      bool     `json:"sourceUnmodified"`
	KeyDecisions          []string `json:"keyDecisions"`
}

func main() {
	out := "demo-output"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := run(out); err != nil {
		fmt.Fprintf(os.Stderr, "leasedemo: %v\n", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if err := os.MkdirAll(filepath.Join(out, "checkpoint"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "exceptions"), 0o755); err != nil {
		return err
	}

	p := leasekit.GenerateProject(leasekit.FixedSeed)
	sourceBefore := mustJSON(p)
	truth, err := leasekit.GenerateTruth(p)
	if err != nil {
		return err
	}

	exports := map[string][]byte{}
	if exports["v1"], err = leasekit.ExportV1(p); err != nil {
		return err
	}
	if exports["v2"], err = leasekit.ExportV2(p); err != nil {
		return err
	}
	if exports["v3"], err = leasekit.ExportV3(p); err != nil {
		return err
	}
	schemaParity := true
	truthParity := true
	for name, body := range exports {
		loaded, err := leasekit.Load(body)
		if err != nil {
			return fmt.Errorf("load %s: %w", name, err)
		}
		if !leasekit.CoreEqual(p, loaded) {
			schemaParity = false
		}
		report, err := leasekit.Run(loaded)
		if err != nil {
			return err
		}
		if string(report.CanonicalJSON()) != string(truth.Report.CanonicalJSON()) {
			truthParity = false
		}
		if err := writeJSON(filepath.Join(out, "report-"+name+".json"), report); err != nil {
			return err
		}
	}

	parallel, err := leasekit.RunParallel(p, leasekit.ParallelOptions{
		Workers: 4, AttemptBudgetPerResource: 8, BatchSize: 2, ReverseSubmission: true,
	})
	if err != nil {
		return err
	}
	parallelParity := string(parallel.Report.CanonicalJSON()) == string(truth.Report.CanonicalJSON())
	if err := writeJSON(filepath.Join(out, "parallel-report.json"), parallel); err != nil {
		return err
	}

	session, err := leasekit.NewIncrementalSession(p)
	if err != nil {
		return err
	}
	inc, err := session.EditRenewWindow("lock-order", 1600)
	if err != nil {
		return err
	}
	fullEdited, err := session.FullRecompute()
	if err != nil {
		return err
	}
	incrementalParity := string(inc.Report.CanonicalJSON()) == string(fullEdited.CanonicalJSON())
	if err := writeJSON(filepath.Join(out, "incremental-window.json"), inc); err != nil {
		return err
	}

	dbPath := filepath.Join(out, "checkpoint", "lease.db")
	store := leasekit.NewSQLiteCheckpointStore(dbPath)
	cp12, err := leasekit.BuildCheckpoint(p, 12)
	if err != nil {
		return err
	}
	if err := leasekit.CommitCheckpoint(store, cp12); err != nil {
		return err
	}
	loadedCP, err := store.LoadCheckpoint()
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "sqlite-checkpoint-seq12.json"), loadedCP); err != nil {
		return err
	}

	cp20, err := leasekit.BuildCheckpoint(p, 20)
	if err != nil {
		return err
	}
	var crashCode string
	if err := store.WriteHolderStates(*cp20); err != nil {
		return err
	}
	if _, err := store.LoadCheckpoint(); err != nil {
		if biz, ok := leasekit.AsError(err); ok {
			crashCode = biz.Code
		}
	}
	recovery, err := leasekit.RecoverOrRecompute(p, store)
	if err != nil {
		return err
	}
	if err := recovery.Repair(p, store); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "recovery.json"), recovery); err != nil {
		return err
	}
	finalCP, err := store.LoadCheckpoint()
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "sqlite-checkpoint-final.json"), finalCP); err != nil {
		return err
	}
	header, err := os.ReadFile(dbPath)
	if err != nil {
		return err
	}
	sqliteHeader := len(header) >= 15 && string(header[:15]) == "SQLite format 3"

	exceptions, err := leasekit.DrillExceptions(p, filepath.Join(out, "exceptions"))
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "exceptions.json"), exceptions); err != nil {
		return err
	}
	allCodes := true
	for _, obs := range exceptions {
		if !obs.Stable || len(obs.Code) < 3 || obs.Code[:2] != "E_" {
			allCodes = false
		}
	}
	sourceAfter := mustJSON(p)

	sum := summary{
		OutputDir: out, Seed: p.Seed, SchemaParity: schemaParity, TruthParity: truthParity,
		IncrementalParity: incrementalParity, ParallelParity: parallelParity,
		SQLiteHeader: sqliteHeader, CrashDetectionCode: crashCode,
		RecoveryCode: recovery.ReasonCode, ExceptionObservations: len(exceptions),
		AllExceptionCodes: allCodes, SourceUnmodified: string(sourceBefore) == string(sourceAfter),
		KeyDecisions: []string{
			"seq=12 lower boundary at 2000ms accepted",
			"seq=14 priority loses to seq=17 despite earlier operation sequence",
			"seq=16 tie loses to seq=13 by fixed sequence order",
			"holder-order-a deadline 7000ms releases; seq=28 at 7100ms acquires",
		},
	}
	if err := writeJSON(filepath.Join(out, "summary.json"), sum); err != nil {
		return err
	}

	fmt.Printf("lease demo output: %s\n", out)
	fmt.Printf("schema parity=%v truth parity=%v incremental parity=%v parallel parity=%v\n", schemaParity, truthParity, incrementalParity, parallelParity)
	fmt.Printf("sqlite=%v crash=%s recovery=%s exceptions=%d stable=%v sourceUnmodified=%v\n", sqliteHeader, crashCode, recovery.ReasonCode, len(exceptions), allCodes, sum.SourceUnmodified)
	if !schemaParity || !truthParity || !incrementalParity || !parallelParity || !sqliteHeader || crashCode == "" || !allCodes || !sum.SourceUnmodified {
		return fmt.Errorf("one or more demo verifications failed: %+v", sum)
	}
	return nil
}

func writeJSON(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
