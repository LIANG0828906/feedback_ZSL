package budget

import (
	"encoding/json"
	"math"
)

type FaultProbe struct {
	Name     string    `json:"name"`
	Code     ErrorCode `json:"code"`
	Expected ErrorCode `json:"expected"`
	OK       bool      `json:"ok"`
	Location string    `json:"location,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

func RunFaultProbes(project *Project, v1Doc, v3Doc []byte) []FaultProbe {
	probes := []FaultProbe{
		probeDecode(),
		probeOrder(v3Doc),
		probeScale(v3Doc),
		probeScalePolicy(v1Doc),
		probeStage(project),
		probeArbitration(project),
		probeDiff(project),
		probeOverflow(),
		probeEval(),
	}
	return probes
}

func probeDecode() FaultProbe {
	_, err := LoadDocument([]byte(`{"apiVersion":"combat-budget/v3","project":`))
	return summarizeProbe("corrupt_project_json", ECodeDecode, err)
}

func probeOrder(v3Doc []byte) FaultProbe {
	var root map[string]any
	if err := json.Unmarshal(v3Doc, &root); err != nil {
		return FaultProbe{Name: "intent_order_inversion", Expected: ECodeOrder, Code: ECodeDecode, Detail: err.Error()}
	}
	intents, ok := root["player-intents"].([]any)
	if !ok || len(intents) < 2 {
		return FaultProbe{Name: "intent_order_inversion", Expected: ECodeOrder, Code: "E_STAGE", Detail: "fixture missing intents"}
	}
	intents[0], intents[1] = intents[1], intents[0]
	root["player-intents"] = intents
	data, _ := json.Marshal(root)
	_, err := LoadDocument(data)
	return summarizeProbe("intent_order_inversion", ECodeOrder, err)
}

func probeScale(v3Doc []byte) FaultProbe {
	var root map[string]any
	if err := json.Unmarshal(v3Doc, &root); err != nil {
		return FaultProbe{Name: "skill_scale_contradiction", Expected: ECodeScale, Code: ECodeDecode, Detail: err.Error()}
	}
	skills, _ := root["skills"].([]any)
	for _, raw := range skills {
		skill, _ := raw.(map[string]any)
		if stringValue(skill, "skill-id") == "fireball" {
			skill["cooldown-millis"] = 450
		}
	}
	policy, _ := root["scale-policy"].(map[string]any)
	if policy == nil {
		policy = map[string]any{}
		root["scale-policy"] = policy
	}
	policy["cooldown-rounding"] = string(RoundExact)
	data, _ := json.Marshal(root)
	_, err := LoadDocument(data)
	return summarizeProbe("skill_scale_contradiction", ECodeScale, err)
}

func probeScalePolicy(v1Doc []byte) FaultProbe {
	var root map[string]any
	if err := json.Unmarshal(v1Doc, &root); err != nil {
		return FaultProbe{Name: "scale_policy_contradiction", Expected: ECodeScale, Code: ECodeDecode, Detail: err.Error()}
	}
	policy, _ := root["scale_policy"].(map[string]any)
	if policy == nil {
		policy = map[string]any{}
		root["scale_policy"] = policy
	}
	policy["resource_fixed_point"] = "myriad_10000"
	data, _ := json.Marshal(root)
	_, err := LoadDocument(data)
	return summarizeProbe("scale_policy_contradiction", ECodeScale, err)
}

func probeStage(project *Project) FaultProbe {
	params := DefaultParams()
	params.FailTick = 1
	params.FailSkillID = "fireball"
	_, err := ReferenceSchedule(project, params)
	return summarizeProbe("single_window_stage_failure", ECodeStage, err)
}

func probeArbitration(project *Project) FaultProbe {
	clone, err := CloneProject(project)
	if err != nil {
		return FaultProbe{Name: "arbitration_ambiguity", Expected: ECodeARB, Code: ECodeIO, Detail: err.Error()}
	}
	for i := range clone.Skills {
		if clone.Skills[i].ID == "frost_nova" {
			clone.Skills[i].Priority = 10
		}
	}
	params := DefaultParams()
	params.StrictPriorityTies = true
	_, err = ReferenceSchedule(clone, params)
	return summarizeProbe("arbitration_ambiguity", ECodeARB, err)
}

func probeDiff(project *Project) FaultProbe {
	want, err := ReferenceSchedule(project, DefaultParams())
	if err != nil {
		return FaultProbe{Name: "report_diff", Expected: ECodeDiff, Code: ECodeStage, Detail: err.Error()}
	}
	got := *want
	got.Ticks = append([]TickState(nil), want.Ticks...)
	if len(got.Ticks) > 2 {
		got.Ticks[2].ResourceCurrent += 1
	}
	err = CompareReports(want, &got)
	return summarizeProbe("report_diff_location", ECodeDiff, err)
}

func probeOverflow() FaultProbe {
	_, err := ConvertScaled(math.MaxInt64, ScalePermille, ScalePermyriad, RoundExact, "probe.overflow")
	return summarizeProbe("integer_overflow_boundary", ECodeOVF, err)
}

func probeEval() FaultProbe {
	_, err := Eval(&Expr{Kind: "div", Node: "div-zero", Args: []*Expr{{Kind: "lit", Value: 1}, {Kind: "lit", Value: 0}}}, nil, "probe.div")
	return summarizeProbe("expression_div_zero", ECodeEval, err)
}

func summarizeProbe(name string, expected ErrorCode, err error) FaultProbe {
	probe := FaultProbe{Name: name, Expected: expected}
	if err == nil {
		probe.OK = false
		probe.Code = ""
		probe.Detail = "expected error but succeeded"
		return probe
	}
	be, ok := AsError(err)
	if !ok {
		probe.OK = false
		probe.Detail = err.Error()
		return probe
	}
	probe.Code = be.Code
	probe.OK = be.Code == expected
	probe.Location = be.Field
	if be.Tick != nil {
		probe.Location += "#tick=" + itoa(*be.Tick)
	}
	if be.SkillID != "" {
		probe.Location += "#skill=" + be.SkillID
	}
	probe.Detail = be.Message
	return probe
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
