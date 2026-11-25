package runner_test

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"obsproj/internal/checkpoint"
	"obsproj/internal/model"
	"obsproj/internal/runner"
	"obsproj/internal/scenario"
)

func TestThreePathsAgree(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	full, err := runner.RunFullReplay(bundle.Session, runner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	realtimeDir, err := runner.NewTempStoreDir("realtime-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(realtimeDir)
	realtimeOptions := runner.DefaultOptions()
	realtimeOptions.StoreDir = realtimeDir
	realtime, err := runner.RunRealtime(bundle.Session, realtimeOptions)
	if err != nil {
		t.Fatal(err)
	}
	resumeDir, err := runner.NewTempStoreDir("resume-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(resumeDir)
	resumeOptions := runner.DefaultOptions()
	resumeOptions.StoreDir = resumeDir
	resumeOptions.SplitIndex = 3
	resumed, err := runner.RunResume(bundle.Session, resumeOptions)
	if err != nil {
		t.Fatal(err)
	}
	if diff := runner.CompareResults(full, realtime); diff != nil {
		t.Fatalf("full/realtime: %v", diff)
	}
	if diff := runner.CompareResults(full, resumed); diff != nil {
		t.Fatalf("full/resume: %v", diff)
	}
	if len(full.Commits) != 8 || len(realtime.Commits) != 8 || len(resumed.Commits) != 8 {
		t.Fatalf("window counts: full=%d realtime=%d resume=%d", len(full.Commits), len(realtime.Commits), len(resumed.Commits))
	}
}

func TestCheckpointCorruptionKeepsValidPrefixAndResumes(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	full, err := runner.RunFullReplay(bundle.Session, runner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name            string
		mutate          func(records []any)
		expectedValid   int
		expectedInvalid int
	}{
		{
			name: "content hash",
			mutate: func(records []any) {
				commit := records[1].(map[string]any)["commit"].(map[string]any)
				commit["content_hash"] = "tampered"
			},
			expectedValid:   1,
			expectedInvalid: 2,
		},
		{
			name: "algorithm version",
			mutate: func(records []any) {
				records[2].(map[string]any)["algorithm_version"] = "tampered"
			},
			expectedValid:   2,
			expectedInvalid: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, err := runner.NewTempStoreDir("corrupt-test-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			options := runner.DefaultOptions()
			options.StoreDir = dir
			options.SplitIndex = 4
			if _, err := runner.RunFirstPhase(bundle.Session, options); err != nil {
				t.Fatal(err)
			}
			path := dir + "/" + checkpoint.FileName
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			records := doc["records"].([]any)
			test.mutate(records)
			tampered, err := json.MarshalIndent(doc, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(tampered, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}

			store, err := checkpoint.Open(dir, bundle.Session.Fingerprint())
			if err != nil {
				t.Fatal(err)
			}
			commits, report, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(commits) != test.expectedValid {
				t.Fatalf("valid commits=%d want=%d report=%+v", len(commits), test.expectedValid, report)
			}
			if report.InvalidatedFrom != test.expectedInvalid {
				t.Fatalf("invalidated_from=%d want=%d report=%+v", report.InvalidatedFrom, test.expectedInvalid, report)
			}
			resumed, resumeReport, err := runner.ResumeFromCheckpoint(bundle.Session, options)
			if err != nil {
				t.Fatal(err)
			}
			if resumeReport.InvalidatedFrom != test.expectedInvalid {
				t.Fatalf("resume report=%+v", resumeReport)
			}
			if diff := runner.CompareResults(full, resumed); diff != nil {
				t.Fatalf("resumed from valid prefix differs: %v", diff)
			}
		})
	}
}

func TestCheckpointMalformedDocumentIsDecodeError(t *testing.T) {
	dir, err := runner.NewTempStoreDir("malformed-ckpt-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store, err := checkpoint.Open(dir, "session-hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, loadErr := store.Load()
	assertCode(t, loadErr, model.CodeDecode)
}

func TestConcurrentStoreInstancesDoNotOverwrite(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	full, err := runner.RunFullReplay(bundle.Session, runner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := runner.NewTempStoreDir("concurrent-ckpt-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	options := runner.DefaultOptions()
	options.StoreDir = dir
	options.SplitIndex = 1
	if _, err := runner.RunFirstPhase(bundle.Session, options); err != nil {
		t.Fatal(err)
	}
	storeA, err := checkpoint.Open(dir, bundle.Session.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := checkpoint.Open(dir, bundle.Session.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*checkpoint.Store{storeA, storeB} {
		wg.Add(1)
		go func(store *checkpoint.Store) {
			defer wg.Done()
			errs <- store.Commit(full.Commits[1])
		}(store)
	}
	wg.Wait()
	close(errs)
	successes := 0
	for commitErr := range errs {
		if commitErr == nil {
			successes++
			continue
		}
		assertCode(t, commitErr, model.CodeState)
	}
	if successes != 1 {
		t.Fatalf("successful concurrent commits=%d want=1", successes)
	}
	store, err := checkpoint.Open(dir, bundle.Session.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	commits, report, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || report.InvalidatedFrom != 0 {
		t.Fatalf("checkpoint records=%d report=%+v", len(commits), report)
	}
}

func TestCompareReportsFirstMismatch(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	left, err := runner.RunFullReplay(bundle.Session, runner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	right, err := runner.RunFullReplay(bundle.Session, runner.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if diff := runner.CompareResults(left, right); diff != nil {
		t.Fatalf("identical runs differ: %v", diff)
	}
	right.Commits[4].Screens[0].Mapped.Pixels.X++
	diff := runner.CompareResults(left, right)
	assertCode(t, diff, model.CodeDiff)
	if diff.Window != 5 || diff.ScreenID != "A" || diff.Field != "mapped.pixels" {
		t.Fatalf("wrong first mismatch: %+v", diff)
	}
	if diff.Diff == nil || diff.Diff.Window != 5 || diff.Diff.ScreenID != "A" || diff.Diff.Field != "mapped.pixels" {
		t.Fatalf("missing structured diff: %+v", diff)
	}
}

func assertCode(t *testing.T, err error, code model.Code) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) || kernelErr == nil {
		t.Fatalf("expected %s, got %T: %v", code, err, err)
	}
	if kernelErr.Code != code {
		t.Fatalf("code=%s want=%s (%v)", kernelErr.Code, code, err)
	}
}
