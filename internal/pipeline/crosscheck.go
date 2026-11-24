package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"integritykernel/internal/checkpoint"
	"integritykernel/internal/core"
	"integritykernel/internal/engine"
	"integritykernel/internal/loader"
	"integritykernel/internal/review"
	"integritykernel/internal/samplegen"
)

type State struct {
	Name   string
	Params core.Params
	Region core.Rect
}

type StateResult struct {
	Name                   string                    `json:"name"`
	Invalidations          []core.InvalidationRecord `json:"invalidations,omitempty"`
	IncrementalReport      *core.Report              `json:"-"`
	RecoveryReport         *core.Report              `json:"-"`
	FullReport             *core.Report              `json:"-"`
	IncrementalVsFullError *core.Error               `json:"incremental_vs_full_error,omitempty"`
	RecoveryVsFullError    *core.Error               `json:"recovery_vs_full_error,omitempty"`
}

type CrossCheckResult struct {
	OK                 bool                   `json:"ok"`
	SampleDir          string                 `json:"sample_dir"`
	RecoveryDatabase   string                 `json:"recovery_database"`
	States             []StateResult          `json:"states"`
	FirstMismatch      *core.Error            `json:"first_mismatch,omitempty"`
	TruthCheck         *core.Error            `json:"truth_check,omitempty"`
	InjectedBatchError *core.Error            `json:"injected_batch_error,omitempty"`
	RestartTransaction *core.Error            `json:"restart_transaction,omitempty"`
	InitialRecoveryLog []checkpoint.LogRecord `json:"initial_recovery_log,omitempty"`
	Review             *review.Result         `json:"review,omitempty"`
	TransactionCheckOK bool                   `json:"transaction_check_ok"`
}

func DefaultStates(manifest core.Manifest) []State {
	base := core.DefaultParams()
	base.BatchSize = 12
	base.MaxParallel = 2

	wrongRegion := manifest.WatermarkRegion
	wrongRegion.X -= 3

	blockParams := base
	blockParams.BlockSize = 12

	thresholdParams := blockParams
	thresholdParams.VerdictThreshold = 0.65

	return []State{
		{Name: "initial_offset_region", Params: base, Region: wrongRegion},
		{Name: "corrected_region", Params: base, Region: manifest.WatermarkRegion},
		{Name: "block_size_12", Params: blockParams, Region: manifest.WatermarkRegion},
		{Name: "verdict_threshold_65", Params: thresholdParams, Region: manifest.WatermarkRegion},
	}
}

func RunCrossCheck(sampleDir, artifactDir string) (*CrossCheckResult, error) {
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		return nil, err
	}
	if _, err := samplegen.Generate(samplegen.DefaultOptions(sampleDir)); err != nil {
		return nil, err
	}
	seq, err := loader.Load(sampleDir)
	if err != nil {
		return nil, err
	}
	fingerprintRaw, err := os.ReadFile(filepath.Join(sampleDir, "truth.fingerprint.json"))
	if err != nil {
		return nil, core.WithField(core.NewError(core.CodeDecode, "read truth fingerprint: %v", err), "truth.fingerprint.json")
	}
	var fingerprint core.TruthFingerprint
	if err := json.Unmarshal(fingerprintRaw, &fingerprint); err != nil {
		return nil, core.WithField(core.NewError(core.CodeDecode, "decode truth fingerprint: %v", err), "truth.fingerprint.json")
	}

	states := DefaultStates(seq.Manifest)
	result := &CrossCheckResult{OK: true, SampleDir: sampleDir}

	incremental, err := engine.New(seq, engine.Options{Params: states[0].Params, Region: &states[0].Region})
	if err != nil {
		return nil, err
	}
	if err := incremental.Run(); err != nil {
		return nil, err
	}

	recoveryDB := filepath.Join(artifactDir, "recovery-checkpoint.db")
	result.RecoveryDatabase = recoveryDB
	_ = os.Remove(recoveryDB)
	_ = os.Remove(recoveryDB + "-wal")
	_ = os.Remove(recoveryDB + "-shm")

	store, err := checkpoint.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	recoveryFirst, err := engine.New(seq, engine.Options{
		Params: states[0].Params, Region: &states[0].Region, Store: store,
		CommitHook: func(seq int) error {
			if seq == 2 {
				return errors.New("injected durable commit interruption")
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	runErr := recoveryFirst.Run()
	if runErr != nil {
		if kernelErr, ok := core.AsError(runErr); ok {
			result.InjectedBatchError = kernelErr
		}
	}
	if err := store.Close(); err != nil {
		return nil, err
	}

	store, err = checkpoint.Open(recoveryDB)
	if err != nil {
		return nil, err
	}
	recovery, err := engine.New(seq, engine.Options{Params: states[0].Params, Region: &states[0].Region, Store: store})
	if err != nil {
		return nil, err
	}
	_, txnErr, err := recovery.LoadCheckpoint()
	if err != nil {
		return nil, err
	}
	result.RestartTransaction = txnErr
	if err := recovery.Run(); err != nil {
		return nil, err
	}
	initialLog, err := store.LogRecords()
	if err != nil {
		return nil, err
	}
	result.InitialRecoveryLog = initialLog

	for i, state := range states {
		stateResult := StateResult{Name: state.Name}

		if i == 1 {
			invalidations, err := incremental.CorrectRegion(state.Region)
			if err != nil {
				return nil, err
			}
			stateResult.Invalidations = invalidations
			if err := incremental.Run(); err != nil {
				return nil, err
			}
			recoveryInvalidations, err := recovery.CorrectRegion(state.Region)
			if err != nil {
				return nil, err
			}
			_ = recoveryInvalidations
			if err := recovery.Run(); err != nil {
				return nil, err
			}
		} else if i == 2 {
			invalidations, err := incremental.ApplyParams(state.Params)
			if err != nil {
				return nil, err
			}
			stateResult.Invalidations = invalidations
			if err := incremental.Run(); err != nil {
				return nil, err
			}
			if _, err := recovery.ApplyParams(state.Params); err != nil {
				return nil, err
			}
			if err := recovery.Run(); err != nil {
				return nil, err
			}
		} else if i == 3 {
			invalidations, err := incremental.ApplyParams(state.Params)
			if err != nil {
				return nil, err
			}
			stateResult.Invalidations = invalidations
			if err := incremental.Run(); err != nil {
				return nil, err
			}
			if _, err := recovery.ApplyParams(state.Params); err != nil {
				return nil, err
			}
			if err := recovery.Run(); err != nil {
				return nil, err
			}
		}

		fullReport, err := runFullReport(seq, state)
		if err != nil {
			return nil, err
		}
		incReport := incremental.BuildReport()
		recReport := recovery.BuildReport()
		stateResult.IncrementalReport = incReport
		stateResult.RecoveryReport = recReport
		stateResult.FullReport = fullReport

		if mismatch := CompareReports(fullReport, incReport); mismatch != nil {
			stateResult.IncrementalVsFullError = mismatch
			if result.FirstMismatch == nil {
				result.FirstMismatch = mismatch
			}
		}
		if mismatch := CompareReports(fullReport, recReport); mismatch != nil {
			stateResult.RecoveryVsFullError = mismatch
			if result.FirstMismatch == nil {
				result.FirstMismatch = mismatch
			}
		}
		result.States = append(result.States, stateResult)
	}

	finalReport := result.States[len(result.States)-1].IncrementalReport
	result.TruthCheck = CheckTruthInterval(finalReport, fingerprint, states[len(states)-1].Params.FrameTolerance)
	if result.TruthCheck != nil && result.FirstMismatch == nil {
		result.FirstMismatch = result.TruthCheck
	}

	result.TransactionCheckOK = result.InjectedBatchError != nil &&
		result.InjectedBatchError.Code == core.CodeTxn &&
		result.InjectedBatchError.Batch != nil && *result.InjectedBatchError.Batch == 2 &&
		result.RestartTransaction != nil && result.RestartTransaction.Code == core.CodeTxn &&
		result.RestartTransaction.Batch != nil && *result.RestartTransaction.Batch == 2 &&
		len(result.InitialRecoveryLog) == seq.Manifest.FrameCount*4

	if err := store.Close(); err != nil {
		return nil, err
	}
	reviewResult, err := review.Run(recoveryDB, filepath.Join(sampleDir, "truth.fingerprint.json"))
	if err != nil {
		return nil, err
	}
	result.Review = reviewResult
	if !reviewResult.OK && result.FirstMismatch == nil {
		result.FirstMismatch = reviewResult.FirstViolation
	}
	result.OK = result.FirstMismatch == nil && result.TruthCheck == nil &&
		result.TransactionCheckOK && reviewResult.OK
	return result, nil
}

func runFullReport(seq *loader.Sequence, state State) (*core.Report, error) {
	store, err := checkpoint.Open(":memory:")
	if err != nil {
		return nil, err
	}
	defer store.Close()
	eng, err := engine.New(seq, engine.Options{Params: state.Params, Region: &state.Region, Store: store})
	if err != nil {
		return nil, err
	}
	if _, _, err := eng.LoadCheckpoint(); err != nil {
		return nil, err
	}
	if err := eng.Run(); err != nil {
		return nil, err
	}
	return eng.BuildReport(), nil
}
