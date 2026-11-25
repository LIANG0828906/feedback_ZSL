package forward

import "encoding/json"

func canonicalJSON(v any) ([]byte, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(buf, &normalized); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

// ReportsEquivalent compares all correctness-bearing fields while ignoring diagnostic budget metadata.
func ReportsEquivalent(a, b *Report) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac := *a
	bc := *b
	ac.Budget = nil
	bc.Budget = nil
	ab, err := canonicalJSON(ac)
	if err != nil {
		return false
	}
	bb, err := canonicalJSON(bc)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}

type DecisionSignature struct {
	Seq         int                `json:"seq"`
	Transitions []Transition       `json:"transitions"`
	Packets     []PacketEvaluation `json:"packets"`
}

func ReportSignature(r *Report) []DecisionSignature {
	out := make([]DecisionSignature, 0, len(r.Events))
	for _, ev := range r.Events {
		out = append(out, DecisionSignature{Seq: ev.Seq, Transitions: ev.Transitions, Packets: ev.Packets})
	}
	return out
}

func SignaturesEqual(a, b []DecisionSignature) bool {
	ab, _ := canonicalJSON(a)
	bb, _ := canonicalJSON(b)
	return string(ab) == string(bb)
}
