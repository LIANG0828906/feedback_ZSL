package verify

import (
	"bytes"
	"encoding/json"

	"example.com/multiscreen-cast/protocol"
)

type AnomalyKind string

const (
	AnomalyDecode  AnomalyKind = "decode"
	AnomalyOrder   AnomalyKind = "order"
	AnomalyCap     AnomalyKind = "cap"
	AnomalySegment AnomalyKind = "segment"
)

func ApplyInputAnomaly(set *protocol.SampleSet, kind AnomalyKind) *protocol.KernelError {
	if set == nil || len(set.Sessions) == 0 {
		return protocol.Errorf(protocol.CodeSchema, "anomaly", "$.sessions", "no session to mutate")
	}
	session := &set.Sessions[0]
	switch kind {
	case AnomalyOrder:
		if len(session.Windows) >= 2 {
			session.Windows[0].Time = session.Windows[1].Time + 1
			return nil
		}
		if len(session.Events) < 2 {
			return protocol.Errorf(protocol.CodeSchema, "anomaly", "$.events", "need at least two events or windows")
		}
		session.Events[1].Seq = session.Events[0].Seq
	case AnomalyCap:
		if len(session.Displays) == 0 {
			return protocol.Errorf(protocol.CodeSchema, "anomaly", "$.displays", "need at least one display")
		}
		session.Displays[0].Capability.Resolution.Width++
	case AnomalyDecode:
		return protocol.Errorf(protocol.CodeSchema, "anomaly", "$", "decode anomaly affects serialized input, not the in-memory document")
	case AnomalySegment:
		return protocol.Errorf(protocol.CodeSchema, "anomaly", "$", "segment anomaly is injected through kernel config")
	default:
		return protocol.Errorf(protocol.CodeSchema, "anomaly", "$.kind", "unknown anomaly kind %q", kind)
	}
	return nil
}

func DecodeAnomalous(payload []byte) (*protocol.SampleSet, *protocol.KernelError) {
	return protocol.DecodeSampleSet(bytes.NewReader(payload), "anomaly.json")
}

func MarshalSampleSet(set *protocol.SampleSet) ([]byte, error) {
	return json.MarshalIndent(set, "", "  ")
}
