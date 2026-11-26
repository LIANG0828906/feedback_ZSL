package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"serviceregistry/anomaly"
	"serviceregistry/checkpoint"
	"serviceregistry/registry"
	"serviceregistry/samplegen"
)

type verifyCheck struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Detail  string `json:"detail,omitempty"`
	Metrics any    `json:"metrics,omitempty"`
}

func commandVerify(args []string) int {
	flags := newFlagSet("verify")
	seed := flags.Int64("seed", samplegen.DefaultSeed, "sample seed")
	if err := parseFlags(flags, args); err != nil {
		return fail(err, exitUsage)
	}
	checks := make([]verifyCheck, 0, 6)
	bundle, err := samplegen.BuildBundle(*seed)
	if err != nil {
		return fail(err, exitBusiness)
	}
	loaded := make(map[registry.SchemaVersion]*registry.Document)
	baselineHash := ""
	baselineTruth := []byte(nil)
	versionMatches := true
	for _, schema := range []registry.SchemaVersion{registry.SchemaV1, registry.SchemaV2, registry.SchemaV3} {
		document, err := registry.LoadDocument(bundle.Files[schema])
		if err != nil {
			return fail(err, exitBusiness)
		}
		loaded[schema] = document
		hash, err := document.Project.ModelHash()
		if err != nil {
			return fail(err, exitBusiness)
		}
		if baselineHash == "" {
			baselineHash = hash
		}
		run, err := registry.RunProject(document.Project, registry.RunOptions{})
		if err != nil {
			return fail(err, exitBusiness)
		}
		actual, _ := json.Marshal(run.Truth())
		expected, _ := json.Marshal(document.Truth)
		if baselineTruth == nil {
			baselineTruth = actual
		}
		if hash != baselineHash || !bytes.Equal(actual, expected) || !bytes.Equal(actual, baselineTruth) {
			versionMatches = false
		}
	}
	checks = append(checks, verifyCheck{
		Name:   "three_schema_normalization_and_truth",
		Passed: versionMatches,
		Detail: "v1 flat/ticks/absolute, v2 nested/ms/remaining, v3 composite units/optional watermark",
		Metrics: map[string]any{
			"model_hash": baselineHash,
			"steps":      len(bundle.Truth.Steps),
		},
	})

	project := loaded[registry.SchemaV1].Project
	full, snapshots, err := registry.ComputeProject(project, nil, true)
	if err != nil {
		return fail(err, exitBusiness)
	}
	incrementalEdit := registry.Edit{
		Kind:          registry.EditProbePeriod,
		InstanceID:    "api-a",
		EffectiveTick: 10,
		Value:         200,
	}
	incremental, err := registry.IncrementalRecompute(full, snapshots, project, incrementalEdit, 9)
	if err != nil {
		return fail(err, exitBusiness)
	}
	fullEdited, err := registry.RunProject(incremental.Project, registry.RunOptions{})
	if err != nil {
		return fail(err, exitBusiness)
	}
	incrementalJSON, _ := json.Marshal(stripInvalidation(incremental.Run.Steps))
	fullEditedJSON, _ := json.Marshal(fullEdited.Steps)
	checks = append(checks, verifyCheck{
		Name:   "incremental_edit_equivalence",
		Passed: bytes.Equal(incrementalJSON, fullEditedJSON),
		Detail: "probe period edit at tick 10; prefix through tick 9 reused",
		Metrics: map[string]any{
			"reused_steps":     incremental.Invalidation.Reused,
			"recomputed_steps": incremental.Invalidation.Recomputed,
			"closure":          incremental.Invalidation.Closure,
		},
	})

	tempDir, err := os.MkdirTemp("", "registry-verify-*")
	if err != nil {
		return fail(err, exitBusiness)
	}
	defer os.RemoveAll(tempDir)
	checkpointPath := filepath.Join(tempDir, "resume.db")
	commit := 7
	if err := (&checkpoint.Store{Path: checkpointPath}).Save(&checkpoint.Checkpoint{
		Snapshot: snapshots[commit],
		Steps:    append([]registry.StepResult(nil), full.Steps[:commit+1]...),
	}); err != nil {
		return fail(err, exitBusiness)
	}
	loadedCheckpoint, status, err := (&checkpoint.Store{Path: checkpointPath}).LoadCompatible(checkpoint.Identity{
		EngineVersion:  registry.EngineVersion,
		ProjectVersion: project.ProjectVersion,
		ModelHash:      baselineHash,
	})
	if err != nil {
		return fail(err, exitBusiness)
	}
	resumePass := status.Mode == checkpoint.ResumeModeFromCheckpoint
	resumedSteps := []registry.StepResult{}
	if resumePass {
		resumeEngine, err := registry.RestoreEngine(project, &loadedCheckpoint.Snapshot)
		if err != nil {
			return fail(err, exitBusiness)
		}
		remaining, err := resumeEngine.RunAll()
		if err != nil {
			return fail(err, exitBusiness)
		}
		resumedSteps = append(append([]registry.StepResult(nil), loadedCheckpoint.Steps...), remaining.Steps...)
	}
	resumedJSON, _ := json.Marshal(resumedSteps)
	fullJSON, _ := json.Marshal(full.Steps)
	resumePass = resumePass && bytes.Equal(resumedJSON, fullJSON)
	checks = append(checks, verifyCheck{
		Name:   "checkpoint_resume",
		Passed: resumePass,
		Detail: status.Reason,
		Metrics: map[string]any{
			"commit_tick": commit,
			"steps":       len(resumedSteps),
		},
	})

	crashPass, crashDetail := verifyForcedKill(tempDir, *seed, baselineHash, full)
	checks = append(checks, verifyCheck{
		Name:   "forced_kill_recovery",
		Passed: crashPass,
		Detail: crashDetail,
	})

	anomalyResults, err := anomaly.RunSuite(anomaly.Input{
		Project:        project,
		Documents:      bundle.Files,
		CheckpointPath: checkpointPath,
	})
	if err != nil {
		return fail(err, exitBusiness)
	}
	anomaliesPass := true
	for _, result := range anomalyResults {
		if !result.Passed {
			anomaliesPass = false
		}
	}
	checks = append(checks, verifyCheck{
		Name:    "anomaly_injection",
		Passed:  anomaliesPass,
		Detail:  "stable E_xxx codes, contextual paths, immutable source, usable catalog, primary checkpoint intact",
		Metrics: anomalyResults,
	})

	allPassed := true
	for _, check := range checks {
		if !check.Passed {
			allPassed = false
		}
	}
	report := map[string]any{
		"ok":              allPassed,
		"command":         "verify",
		"seed":            *seed,
		"project_version": project.ProjectVersion,
		"model_hash":      baselineHash,
		"checks":          checks,
	}
	if !allPassed {
		report["code"] = registry.CodeVerificationFailed
	}
	if err := jsonEncodeStdout(report); err != nil {
		return fail(err, exitInternal)
	}
	if !allPassed {
		return exitBusiness
	}
	return exitOK
}

func verifyForcedKill(tempDir string, seed int64, expectedHash string, full *registry.RunResult) (bool, string) {
	sampleDir := filepath.Join(tempDir, "samples")
	if _, err := samplegen.WriteBundle(sampleDir, seed); err != nil {
		return false, "generate samples: " + err.Error()
	}
	projectPath := filepath.Join(sampleDir, "v1.json")
	checkpointPath := filepath.Join(tempDir, "forced-kill.db")
	executable, err := os.Executable()
	if err != nil {
		return false, "locate executable: " + err.Error()
	}
	first := exec.Command(executable,
		"run",
		"--project", projectPath,
		"--checkpoint", checkpointPath,
		"--crash-after-commit", "5",
	)
	firstOutput, firstErr := first.CombinedOutput()
	if firstErr == nil {
		return false, "first process was not forcibly terminated: " + strings.TrimSpace(string(firstOutput))
	}
	var exitError *exec.ExitError
	if !errors.As(firstErr, &exitError) {
		return false, "first process failed without signal: " + firstErr.Error()
	}
	afterKill, err := (&checkpoint.Store{Path: checkpointPath}).Load()
	if err != nil {
		return false, "checkpoint after kill: " + err.Error()
	}
	if afterKill.Meta.CommitSeq != 5 {
		return false, "checkpoint after kill commit = " + strconv.FormatInt(afterKill.Meta.CommitSeq, 10)
	}
	second := exec.Command(executable,
		"run",
		"--project", projectPath,
		"--checkpoint", checkpointPath,
	)
	secondOutput, secondErr := second.CombinedOutput()
	if secondErr != nil {
		return false, "resume process: " + secondErr.Error() + ": " + strings.TrimSpace(string(secondOutput))
	}
	finalCheckpoint, err := (&checkpoint.Store{Path: checkpointPath}).Load()
	if err != nil {
		return false, "final checkpoint: " + err.Error()
	}
	if finalCheckpoint.Meta.ModelHash != expectedHash {
		return false, "final checkpoint hash mismatch"
	}
	actual, _ := json.Marshal(finalCheckpoint.Steps)
	expected, _ := json.Marshal(full.Steps)
	if !bytes.Equal(actual, expected) {
		return false, "resumed steps differ from full run"
	}
	return true, "SIGKILL after commit tick 5; resume reconstructed the full committed timeline"
}
