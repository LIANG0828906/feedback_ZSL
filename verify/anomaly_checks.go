package verify

import (
	"context"
	"encoding/json"

	"example.com/multiscreen-cast/kernel"
	"example.com/multiscreen-cast/protocol"
)

type AnomalyReport struct {
	Decode  string `json:"decode"`
	Order   string `json:"order"`
	Cap     string `json:"cap"`
	Segment string `json:"segment"`
}

func CheckAnomalyChains(ctx context.Context, set *protocol.SampleSet) (*AnomalyReport, *protocol.KernelError) {
	report := &AnomalyReport{}
	if _, err := DecodeAnomalous([]byte(`{"sessions":[`)); err == nil || err.Code != protocol.CodeDecode {
		return nil, protocol.Errorf(protocol.CodeDiff, "anomaly", "$.decode", "expected E_DECODE, got %v", err)
	}
	report.Decode = string(protocol.CodeDecode)

	orderSet, err := cloneSet(set)
	if err != nil {
		return nil, protocol.AsKernelError(err)
	}
	if mutationErr := ApplyInputAnomaly(orderSet, AnomalyOrder); mutationErr != nil {
		return nil, mutationErr
	}
	if validationErr := protocol.ValidateSession(&orderSet.Sessions[0], "anomaly", "$.sessions[0]"); validationErr == nil ||
		validationErr.Code != protocol.CodeOrder {
		return nil, protocol.Errorf(protocol.CodeDiff, "anomaly", "$.order", "expected E_ORDER, got %v", validationErr)
	}
	report.Order = string(protocol.CodeOrder)

	capSet, err := cloneSet(set)
	if err != nil {
		return nil, protocol.AsKernelError(err)
	}
	if mutationErr := ApplyInputAnomaly(capSet, AnomalyCap); mutationErr != nil {
		return nil, mutationErr
	}
	if validationErr := protocol.ValidateSession(&capSet.Sessions[0], "anomaly", "$.sessions[0]"); validationErr == nil ||
		validationErr.Code != protocol.CodeCap {
		return nil, protocol.Errorf(protocol.CodeDiff, "anomaly", "$.cap", "expected E_CAP, got %v", validationErr)
	}
	report.Cap = string(protocol.CodeCap)

	session := &set.Sessions[0]
	fullEngine, engineErr := kernel.NewEngine(session, kernel.Config{Mapping: kernel.DefaultMappingConfig()})
	if engineErr != nil {
		return nil, engineErr
	}
	fullTrace, engineErr := fullEngine.Run(ctx)
	if engineErr != nil {
		return nil, engineErr
	}
	failedEngine, engineErr := kernel.NewEngine(session, kernel.Config{
		Mapping: kernel.DefaultMappingConfig(),
		FailureInjector: func(window int) (string, string, bool) {
			if window == 1 {
				return session.InitialDisplays[0], "deterministic segment fault", true
			}
			return "", "", false
		},
	})
	if engineErr != nil {
		return nil, engineErr
	}
	var segmentErr *protocol.KernelError
	for !failedEngine.Done() {
		_, segmentErr = failedEngine.Advance(ctx)
		if segmentErr != nil {
			break
		}
	}
	if segmentErr == nil || segmentErr.Code != protocol.CodeSeg || segmentErr.Window != 1 || segmentErr.DisplayID == "" {
		return nil, protocol.Errorf(protocol.CodeDiff, "anomaly", "$.segment", "expected located E_SEG, got %v", segmentErr)
	}
	if failedEngine.Progress().Committed != 0 {
		return nil, protocol.Errorf(protocol.CodeDiff, "anomaly", "$.segment",
			"failed window polluted committed progress: %d", failedEngine.Progress().Committed)
	}
	failedEngine.SetFailureInjector(nil)
	retriedTrace, engineErr := failedEngine.Run(ctx)
	if engineErr != nil {
		return nil, engineErr
	}
	if diff := kernel.CompareTraces(*fullTrace, *retriedTrace); diff != nil {
		return nil, diff
	}
	report.Segment = string(protocol.CodeSeg)
	return report, nil
}

func cloneSet(set *protocol.SampleSet) (*protocol.SampleSet, error) {
	payload, err := json.Marshal(set)
	if err != nil {
		return nil, err
	}
	var copySet protocol.SampleSet
	if err := json.Unmarshal(payload, &copySet); err != nil {
		return nil, err
	}
	return &copySet, nil
}
