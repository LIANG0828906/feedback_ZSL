package app_test

import (
	"path/filepath"
	"testing"
	"time"

	"ocrworkbench/internal/app"
)

func TestAppRunsThreePathsAndPublishesVerification(t *testing.T) {
	dir := t.TempDir()
	application, err := app.New(app.NewOptions{
		DataDir: filepath.Join(dir, "data"),
		DBPath:  filepath.Join(dir, "workbench.db"),
		Seed:    42,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if err := application.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		status := application.Status()
		if !status.Running {
			if status.LastError != "" {
				t.Fatalf("run failed: %s", status.LastError)
			}
			if !status.VerificationOK || len(status.Comparison) != 0 {
				t.Fatalf("verification failed: %+v", status.Comparison)
			}
			if len(status.Runs) != 3 {
				t.Fatalf("published %d runs", len(status.Runs))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for workbench run")
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestAppSurfacesUnitFailureAndRetryCompletesIt(t *testing.T) {
	dir := t.TempDir()
	application, err := app.New(app.NewOptions{
		DataDir: filepath.Join(dir, "data"),
		DBPath:  filepath.Join(dir, "workbench.db"),
		Seed:    42,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if err := application.InjectFault("line", "dense_1280", 1); err != nil {
		t.Fatal(err)
	}
	if err := application.Start(); err != nil {
		t.Fatal(err)
	}
	status := waitForRun(t, application)
	if status.VerificationOK {
		t.Fatal("faulted run unexpectedly verified clean")
	}
	realtime, ok := status.Runs["realtime"]
	if !ok {
		t.Fatal("partial realtime result was not published")
	}
	dense, ok := realtime.Image("dense_1280")
	if !ok || dense.Lines[1].State != "failed" {
		t.Fatalf("failed line not exposed: %+v", dense)
	}
	if err := application.Retry(); err != nil {
		t.Fatal(err)
	}
	status = waitForRun(t, application)
	if !status.VerificationOK || len(status.Comparison) != 0 || status.LastError != "" {
		t.Fatalf("retry did not recover: ok=%v issues=%+v err=%s", status.VerificationOK, status.Comparison, status.LastError)
	}
}

func waitForRun(t *testing.T, application *app.App) app.Status {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status := application.Status()
		if !status.Running {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for workbench run")
		}
		time.Sleep(30 * time.Millisecond)
	}
}
