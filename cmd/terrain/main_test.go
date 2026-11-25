package main

import "testing"

func TestEndToEndWithPauseResume(t *testing.T) {
	if err := run(t.TempDir(), 2, 32, 2, 20261125); err != nil {
		t.Fatal(err)
	}
}

func TestEndToEndSingleWorkerAndNoInitialCommit(t *testing.T) {
	if err := run(t.TempDir(), 1, 16, 0, 20261125); err != nil {
		t.Fatal(err)
	}
}
