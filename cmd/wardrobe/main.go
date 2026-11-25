package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"wardrobe/diagnostics"
	"wardrobe/domain"
	"wardrobe/engine"
	"wardrobe/generator"
	"wardrobe/httpui"
	"wardrobe/scheduler"
	"wardrobe/schema"
	"wardrobe/store"
)

const defaultSamplePath = "artifacts/sample_bundle.json"
const defaultDBPath = "artifacts/wardrobe.db"
const defaultVerificationPath = "artifacts/verification.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "generate":
		code = runGenerate(os.Args[2:])
	case "verify":
		code = runVerify(os.Args[2:])
	case "serve":
		code = runServe(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, `wardrobe - deterministic wardrobe batching kernel

Commands:
  generate  -seed N -out artifacts/sample_bundle.json
  verify    -sample artifacts/sample_bundle.json -db artifacts/wardrobe.db
  serve     -addr 127.0.0.1:8787 -sample artifacts/sample_bundle.json -db artifacts/wardrobe.db

Environment:
  Linux/WSL Ubuntu, Go >= 1.22, no external service required.`)
}

func runGenerate(args []string) int {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := flags.Int64("seed", generator.DefaultSeed, "deterministic sample seed")
	out := flags.String("out", defaultSamplePath, "output bundle path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	bundle := generator.Generate(*seed)
	if err := schema.Validate(&bundle); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := writeJSONFile(*out, bundle); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("generated %s: seed=%d projects=%d cases=%d parts=%d\n",
		*out, *seed, len(bundle.Projects), countCases(bundle.Projects), countParts(bundle.Projects))
	return 0
}

type VerificationReport struct {
	Seed             int64          `json:"seed"`
	SamplePath       string         `json:"samplePath"`
	DatabasePath     string         `json:"databasePath"`
	AlgorithmVersion string         `json:"algorithmVersion"`
	Passed           bool           `json:"passed"`
	Projects         int            `json:"projects"`
	Cases            int            `json:"cases"`
	Batches          int            `json:"batches"`
	ConflictCounts   map[string]int `json:"conflictCounts"`
	DecisionReasons  map[string]int `json:"decisionReasons"`
	TruthMatched     int            `json:"truthMatched"`
	FullMatched      int            `json:"fullMatched"`
	RecoveryMatched  int            `json:"recoveryMatched"`
	TriMatched       int            `json:"triMatched"`
	CaseReports      []CaseReport   `json:"caseReports"`
	Errors           []string       `json:"errors,omitempty"`
}

type CaseReport struct {
	ProjectID    string `json:"projectId"`
	CaseID       string `json:"caseId"`
	TruthMatched bool   `json:"truthMatched"`
	FullMatched  bool   `json:"fullMatched"`
	Recovery     bool   `json:"recoveryMatched"`
	TriMatched   bool   `json:"triMatched"`
	FirstError   string `json:"firstError,omitempty"`
}

func runVerify(args []string) int {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := flags.Int64("seed", generator.DefaultSeed, "seed used only when the sample is generated")
	samplePath := flags.String("sample", defaultSamplePath, "sample bundle path")
	dbPath := flags.String("db", defaultDBPath, "SQLite checkpoint path")
	reportPath := flags.String("report", defaultVerificationPath, "verification JSON output")
	workers := flags.Int("workers", engine.DefaultSchedulerConfig().MaxWorkers, "maximum parallel workers")
	buffer := flags.Int("buffer", engine.DefaultSchedulerConfig().CandidateBufferPerWorker, "candidate parts buffered per worker")
	queue := flags.Int("queue", engine.DefaultSchedulerConfig().PendingQueueBudget, "pending part queue budget")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	bundle, generated, err := loadOrGenerate(*samplePath, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if generated {
		fmt.Printf("sample was missing; generated %s\n", *samplePath)
	}
	cfg := engine.Config{Scheduler: scheduler.Config{
		MaxWorkers: *workers, CandidateBufferPerWorker: *buffer, PendingQueueBudget: *queue,
	}}
	report := VerificationReport{
		Seed: bundle.Seed, SamplePath: *samplePath, DatabasePath: *dbPath,
		AlgorithmVersion: bundle.AlgorithmVersion, Projects: len(bundle.Projects),
		ConflictCounts: map[string]int{}, DecisionReasons: map[string]int{},
	}
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			report.Cases++
			report.Batches += len(truthCase.Batches)
			for _, conflict := range truthCase.Conflicts {
				report.ConflictCounts[conflict.Category]++
			}
			for _, decision := range truthCase.Decisions {
				report.DecisionReasons[decision.Reason]++
			}
		}
	}
	fullResults := make(map[string]domain.CaseResult)
	fullEngine, err := engine.New(bundle.Projects, engine.Config{Scheduler: cfg.Scheduler})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			key := project.ProjectID + "\x00" + truthCase.CaseID
			caseReport := CaseReport{ProjectID: project.ProjectID, CaseID: truthCase.CaseID}
			full, err := fullEngine.FullCase(project.ProjectID, truthCase.CaseID)
			if err != nil {
				caseReport.FirstError = err.Error()
				report.Errors = append(report.Errors, key+": "+err.Error())
			} else {
				fullResults[key] = full
				if mismatch := diagnostics.CompareExpected(truthCase, full); mismatch != nil {
					caseReport.FirstError = mismatch.Error()
					report.Errors = append(report.Errors, key+": "+mismatch.Error())
				} else {
					caseReport.TruthMatched = true
					report.TruthMatched++
				}
			}
			report.CaseReports = append(report.CaseReports, caseReport)
		}
	}

	sqliteStore, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	buildEngine, err := engine.New(bundle.Projects, engine.Config{Scheduler: cfg.Scheduler, Store: sqliteStore})
	if err != nil {
		_ = sqliteStore.Close()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			key := project.ProjectID + "\x00" + truthCase.CaseID
			built, err := buildEngine.BuildCase(project.ProjectID, truthCase.CaseID)
			reportIndex := findCaseReport(report.CaseReports, project.ProjectID, truthCase.CaseID)
			if err != nil {
				appendCaseError(&report, reportIndex, key+": build: "+err.Error())
				continue
			}
			if full, ok := fullResults[key]; ok {
				if mismatch := diagnostics.CompareResults(full, built); mismatch != nil {
					appendCaseError(&report, reportIndex, key+": build vs full: "+mismatch.Error())
				} else {
					report.CaseReports[reportIndex].FullMatched = true
					report.FullMatched++
				}
			}
		}
	}
	if err := sqliteStore.Close(); err != nil {
		report.Errors = append(report.Errors, "close sqlite: "+err.Error())
	}

	reopened, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	recoveryEngine, err := engine.New(bundle.Projects, engine.Config{Scheduler: cfg.Scheduler, Store: reopened})
	if err != nil {
		_ = reopened.Close()
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			key := project.ProjectID + "\x00" + truthCase.CaseID
			recovered, err := recoveryEngine.RecoverCase(project.ProjectID, truthCase.CaseID)
			reportIndex := findCaseReport(report.CaseReports, project.ProjectID, truthCase.CaseID)
			if err != nil {
				appendCaseError(&report, reportIndex, key+": recover: "+err.Error())
				continue
			}
			if !recovered.Recovered {
				appendCaseError(&report, reportIndex, key+": recovery did not reuse checkpoint")
				continue
			}
			if full, ok := fullResults[key]; ok {
				if mismatch := diagnostics.CompareResults(full, recovered.Result); mismatch != nil {
					appendCaseError(&report, reportIndex, key+": recovery vs full: "+mismatch.Error())
				} else {
					report.CaseReports[reportIndex].Recovery = true
					report.RecoveryMatched++
				}
			}
		}
	}
	_ = reopened.Close()

	triEngine, err := engine.New(bundle.Projects, engine.Config{Scheduler: cfg.Scheduler})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			tri, err := triEngine.TriCompare(project.ProjectID, truthCase.CaseID, engine.EditRequest{})
			reportIndex := findCaseReport(report.CaseReports, project.ProjectID, truthCase.CaseID)
			if err != nil {
				appendCaseError(&report, reportIndex, project.ProjectID+"/"+truthCase.CaseID+": tri: "+err.Error())
				continue
			}
			if !tri.Equal {
				message := "tri mismatch without diagnostic detail"
				if tri.FirstMismatch != nil {
					message = tri.FirstMismatch.Error()
				}
				appendCaseError(&report, reportIndex, project.ProjectID+"/"+truthCase.CaseID+": tri mismatch: "+message)
			} else {
				report.CaseReports[reportIndex].TriMatched = true
				report.TriMatched++
			}
		}
	}
	report.Passed = len(report.Errors) == 0 &&
		report.TruthMatched == report.Cases &&
		report.FullMatched == report.Cases &&
		report.RecoveryMatched == report.Cases &&
		report.TriMatched == report.Cases
	sort.Slice(report.CaseReports, func(i, j int) bool {
		if report.CaseReports[i].ProjectID != report.CaseReports[j].ProjectID {
			return report.CaseReports[i].ProjectID < report.CaseReports[j].ProjectID
		}
		return report.CaseReports[i].CaseID < report.CaseReports[j].CaseID
	})
	if err := writeJSONFile(*reportPath, report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	printVerification(report, *reportPath)
	if !report.Passed {
		return 1
	}
	return 0
}

func runServe(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	seed := flags.Int64("seed", generator.DefaultSeed, "seed used only when the sample is generated")
	samplePath := flags.String("sample", defaultSamplePath, "sample bundle path")
	dbPath := flags.String("db", defaultDBPath, "SQLite checkpoint path")
	addr := flags.String("addr", "127.0.0.1:8787", "HTTP listen address")
	workers := flags.Int("workers", engine.DefaultSchedulerConfig().MaxWorkers, "maximum parallel workers")
	buffer := flags.Int("buffer", engine.DefaultSchedulerConfig().CandidateBufferPerWorker, "candidate parts buffered per worker")
	queue := flags.Int("queue", engine.DefaultSchedulerConfig().PendingQueueBudget, "pending part queue budget")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	bundle, generated, err := loadOrGenerate(*samplePath, *seed)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if generated {
		fmt.Printf("sample was missing; generated %s\n", *samplePath)
	}
	sqliteStore, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer sqliteStore.Close()
	engineUnderTest, err := engine.New(bundle.Projects, engine.Config{
		Scheduler: scheduler.Config{
			MaxWorkers: *workers, CandidateBufferPerWorker: *buffer, PendingQueueBudget: *queue,
		},
		Store: sqliteStore,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, project := range bundle.Projects {
		for _, truthCase := range project.Truth.Cases {
			full, err := engineUnderTest.FullCase(project.ProjectID, truthCase.CaseID)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			if mismatch := diagnostics.CompareExpected(truthCase, full); mismatch != nil {
				fmt.Fprintf(os.Stderr, "truth mismatch before serve: %s/%s: %v\n", project.ProjectID, truthCase.CaseID, mismatch)
				return 1
			}
		}
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           httpui.New(engineUnderTest).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Printf("wardrobe diagnostics: http://%s\n", *addr)
	fmt.Printf("sample=%s db=%s workers=%d buffer=%d queueParts=%d\n", *samplePath, *dbPath, *workers, *buffer, *queue)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func loadOrGenerate(path string, seed int64) (domain.Bundle, bool, error) {
	if _, err := os.Stat(path); err == nil {
		bundle, err := schema.LoadFile(path)
		return bundle, false, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Bundle{}, false, err
	}
	bundle := generator.Generate(seed)
	if err := schema.Validate(&bundle); err != nil {
		return domain.Bundle{}, false, err
	}
	if err := writeJSONFile(path, bundle); err != nil {
		return domain.Bundle{}, false, err
	}
	return bundle, true, nil
}

func writeJSONFile(path string, value any) error {
	if path == "" {
		return errors.New("output path is empty")
	}
	if directory := filepath.Dir(path); directory != "." {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func countCases(projects []domain.Project) int {
	total := 0
	for _, project := range projects {
		total += len(project.Truth.Cases)
	}
	return total
}

func countParts(projects []domain.Project) int {
	total := 0
	for _, project := range projects {
		total += len(project.Parts)
	}
	return total
}

func findCaseReport(reports []CaseReport, projectID, caseID string) int {
	for index := range reports {
		if reports[index].ProjectID == projectID && reports[index].CaseID == caseID {
			return index
		}
	}
	return -1
}

func appendCaseError(report *VerificationReport, index int, message string) {
	report.Errors = append(report.Errors, message)
	if index >= 0 {
		report.CaseReports[index].FirstError = message
	}
}

func printVerification(report VerificationReport, reportPath string) {
	fmt.Printf("seed=%d projects=%d cases=%d batches=%d\n",
		report.Seed, report.Projects, report.Cases, report.Batches)
	fmt.Printf("truth=%d/%d full-path=%d/%d sqlite-recovery=%d/%d three-path=%d/%d\n",
		report.TruthMatched, report.Cases, report.FullMatched, report.Cases,
		report.RecoveryMatched, report.Cases, report.TriMatched, report.Cases)
	conflictKeys := sortedKeys(report.ConflictCounts)
	reasonKeys := sortedKeys(report.DecisionReasons)
	fmt.Printf("conflicts: %s\n", formatCounts(conflictKeys, report.ConflictCounts))
	fmt.Printf("decisions: %s\n", formatCounts(reasonKeys, report.DecisionReasons))
	if report.Passed {
		fmt.Printf("PASS - verification report: %s\n", reportPath)
		return
	}
	fmt.Printf("FAIL - verification report: %s\n", reportPath)
	for _, err := range report.Errors {
		fmt.Printf("  %s\n", err)
	}
}

func sortedKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func formatCounts(keys []string, values map[string]int) string {
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ", ")
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}
