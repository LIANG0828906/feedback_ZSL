package sampling

import (
	"hash/fnv"

	"samplingkernel/internal/model"
)

const ScaleFullPermyriad uint32 = 10000

func Bucket(projectID, requestID, edgeID string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(projectID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(requestID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(edgeID))
	return h.Sum32() % ScaleFullPermyriad
}

func DecideHead(projectID string, edge model.Edge, requestID, traceID string) model.HeadDecision {
	bucket := Bucket(projectID, requestID, edge.ID)
	decision := model.HeadDecision{
		RequestID:     requestID,
		TraceID:       traceID,
		EdgeID:        edge.ID,
		EdgeNo:        edge.No,
		Bucket:        bucket,
		RatePermyriad: edge.BaseRatePermyriad,
	}
	switch {
	case edge.BaseRatePermyriad == 0:
		decision.Keep = false
		decision.DecisionType = "ALWAYS_OFF"
		decision.Basis = "rate=0"
	case edge.BaseRatePermyriad >= ScaleFullPermyriad:
		decision.Keep = true
		decision.DecisionType = "ALWAYS_ON"
		decision.Basis = "rate=10000bp"
	case bucket < edge.BaseRatePermyriad:
		decision.Keep = true
		decision.DecisionType = "DETERMINISTIC_HASH_KEEP"
		decision.Basis = "fnv1a32_bucket<rate"
	default:
		decision.Keep = false
		decision.DecisionType = "DETERMINISTIC_HASH_DROP"
		decision.Basis = "fnv1a32_bucket>=rate"
	}
	return decision
}
