package workbench

import (
	"encoding/json"
	"os"

	"meshpolicy/internal/engine"
	"meshpolicy/internal/model"
	"meshpolicy/internal/sched"
	"meshpolicy/internal/schema"
	"meshpolicy/internal/store"
	"meshpolicy/internal/xerr"
)

func (s *Service) Inject(kind string, payload map[string]any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch kind {
	case "overlapping_selector":
		return engine.AnalyzeOverlaps(s.project), engine.OverlapError(s.project)

	case "missing_workload":
		p := model.CloneProject(s.project)
		for i := range p.Policies {
			p.Policies[i].Source.WorkloadRefs = append(p.Policies[i].Source.WorkloadRefs, "ghost-workload")
			break
		}
		return nil, schema.ValidateProject(p)

	case "exited_workload":
		p := model.CloneProject(s.project)
		for i := range p.Workloads {
			if p.Workloads[i].ID == "ledger" {
				p.Workloads[i].Exited = true
			}
		}
		return nil, schema.ValidateProject(p)

	case "selector_cycle":
		p := model.CloneProject(s.project)
		for i := range p.Workloads {
			if p.Workloads[i].ID == "pay" {
				p.Workloads[i].DependsOn = append(p.Workloads[i].DependsOn, "gateway")
			}
		}
		return nil, schema.ValidateProject(p)

	case "invalid_port_range":
		doc := rawDocument(s.sample.Documents["v1"])
		policies := doc["policies"].([]any)
		first := policies[0].(map[string]any)
		first["target_port"] = float64(70000)
		delete(first, "port_range")
		data, _ := json.Marshal(doc)
		_, err := schema.Load(data)
		return nil, err

	case "action_conflict":
		doc := rawDocument(s.sample.Documents["v1"])
		policies := doc["policies"].([]any)
		first := policies[0].(map[string]any)
		first["action"] = "allow"
		first["action_code"] = float64(1) // v1: 1 means deny, deliberately conflicts with action=allow
		data, _ := json.Marshal(doc)
		_, err := schema.Load(data)
		return nil, err

	case "policy_limit_zero", "policy_limit_invalid", "policy_limit_exceeded", "priority_out_of_range", "replace_active":
		return s.injectAdmissionLocked(kind)

	case "invalid_worker_budget":
		_, err := sched.NewPipeline[struct{}](sched.Config{Workers: 999, BufferPerNS: 64})
		return nil, err

	case "invalid_buffer_budget":
		_, err := sched.NewPipeline[struct{}](sched.Config{Workers: 4, BufferPerNS: 999999})
		return nil, err

	case "worker_exhausted":
		adm, err := sched.NewAdmission(sched.Config{Workers: 1, BufferPerNS: 1})
		if err != nil {
			return nil, err
		}
		release, err := adm.TryAdmit("pay")
		if err != nil {
			return nil, err
		}
		defer release()
		_, err = adm.TryAdmit("pay")
		return map[string]any{"observed": "second TryAdmit with one worker"}, err

	case "buffer_full":
		adm, err := sched.NewAdmission(sched.Config{Workers: 2, BufferPerNS: 1})
		if err != nil {
			return nil, err
		}
		release, err := adm.TryAdmit("pay")
		if err != nil {
			return nil, err
		}
		defer release()
		_, err = adm.TryAdmit("pay")
		return map[string]any{"observed": "second in-flight token exceeds per-namespace buffer"}, err

	case "truncated_checkpoint":
		return s.injectTruncatedCheckpointLocked()

	case "version_mismatch":
		return s.injectVersionMismatchLocked()

	default:
		return nil, xerr.New(xerr.EInjectionUnknown, "unknown anomaly injection", map[string]any{
			"injection": kind,
			"supported": []string{
				"overlapping_selector", "missing_workload", "exited_workload", "selector_cycle",
				"invalid_port_range", "action_conflict", "policy_limit_zero", "policy_limit_invalid",
				"policy_limit_exceeded", "priority_out_of_range", "replace_active",
				"invalid_worker_budget", "invalid_buffer_budget", "worker_exhausted", "buffer_full",
				"truncated_checkpoint", "version_mismatch",
			},
		})
	}
}

func rawDocument(data []byte) map[string]any {
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	return doc
}

func (s *Service) injectAdmissionLocked(kind string) (any, error) {
	if len(s.sample.Truth.Replacements) == 0 {
		return nil, xerr.New(xerr.EInjectionUnknown, "no replacement truth scenario available")
	}
	truthReq := s.sample.Truth.Replacements[0]
	p := model.CloneProject(s.project)
	req := engine.ReplaceRequest{
		Namespace: truthReq.Namespace, TargetID: truthReq.TargetID, AtSeq: truthReq.AtSeq,
		NewPolicy: truthReq.NewPolicy,
	}

	switch kind {
	case "policy_limit_zero":
		p.PolicyLimits[req.Namespace] = 0
	case "policy_limit_invalid":
		p.PolicyLimits[req.Namespace] = -7
	case "policy_limit_exceeded":
		p.PolicyLimits[req.Namespace] = 1
	case "priority_out_of_range":
		req.NewPolicy.Priority = 4096
	case "replace_active":
		req.TargetID = "pol-pay-order-allow"
	}
	result, err := engine.ReplacePolicy(p, req)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) injectTruncatedCheckpointLocked() (any, error) {
	path := s.dbPath + ".truncated"
	_ = os.Remove(path)
	cp := store.BuildCheckpoint(s.project, s.report, 4)
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := st.SaveCheckpoint(cp); err != nil {
		_ = st.Close()
		return nil, err
	}
	_ = st.Close()
	if err := os.Truncate(path, 64); err != nil {
		return nil, xerr.Wrap(xerr.ECheckpointTruncated, "failed to truncate test checkpoint", err, map[string]any{"path": path})
	}
	_, err = store.Open(path)
	return map[string]any{"path": path, "truncated_bytes": 64}, err
}

func (s *Service) injectVersionMismatchLocked() (any, error) {
	path := s.dbPath + ".version-mismatch"
	_ = os.Remove(path)
	cp := store.BuildCheckpoint(s.project, s.report, 4)
	cp.ProjectVersion = "pv-injected-snapshot-from-other-seed"
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	if err := st.SaveCheckpoint(cp); err != nil {
		_ = st.Close()
		return nil, err
	}
	_ = st.Close()

	readStore, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	loaded, err := readStore.LoadCheckpoint()
	_ = readStore.Close()
	if err != nil {
		return nil, err
	}
	return loaded, store.CheckVersion(loaded, store.CanonicalVersion(s.project))
}
