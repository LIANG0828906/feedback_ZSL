package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"tunnelworkbench/internal/checkpoint"
	"tunnelworkbench/internal/forward"
	"tunnelworkbench/internal/workbench"
)

type Check struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Code     string `json:"code,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

func RunAll(ctx context.Context, workDir string) *Report {
	report := &Report{OK: true, Checks: []Check{}}
	add := func(category, name string, ok bool, detail string) {
		report.Checks = append(report.Checks, Check{Category: category, Name: name, OK: ok, Detail: detail})
		if !ok {
			report.OK = false
		}
	}

	svc, err := workbench.NewService(workDir)
	if err != nil {
		add("setup", "workbench service", false, err.Error())
		return report
	}
	sample := forward.GenerateSample(forward.DefaultSeed)

	eq, err := svc.Equivalence()
	if err != nil {
		add("schema", "three schema equivalence", false, err.Error())
	} else {
		for _, c := range eq.Checks {
			add("schema", c.Name, c.OK, c.Detail)
		}
	}

	for _, schema := range []string{forward.SchemaV1, forward.SchemaV2, forward.SchemaV3} {
		eng, loadErr := forward.LoadEngineering(schema, sample.Documents[schema])
		if loadErr != nil {
			biz, _ := forward.AsBizError(loadErr)
			add("schema", "load "+schema, false, biz.Code+": "+biz.Message)
			continue
		}
		rep, runErr := forward.RunReport(eng.Project, eng.Events)
		if runErr != nil {
			add("state", "run "+schema, false, runErr.Error())
			continue
		}
		checkTruth(add, schema, sample, eng, rep)
		checkParallel(add, schema, eng, rep)
	}

	checkIncremental(add, sample)
	checkCheckpoint(ctx, add, svc)
	checkMismatchReplay(ctx, add, workDir, sample)
	checkWriteOrder(ctx, add, workDir)
	checkAnomalies(add, svc)
	return report
}

func checkTruth(add func(string, string, bool, string), schema string, sample *forward.Sample, eng *forward.Engineering, rep *forward.Report) {
	final := map[string]string{}
	for _, st := range rep.FinalStates {
		final[st.TunnelID] = st.State
	}
	add("state", schema+" final states match generator truth", reflect.DeepEqual(final, sample.Truth.ExpectedFinalStates), fmt.Sprintf("%v", final))

	transitionByEvent := map[string]forward.Transition{}
	changedCount := 0
	for _, ev := range rep.Events {
		for _, tr := range ev.Transitions {
			if tr.Changed {
				changedCount++
			}
			transitionByEvent[tr.EventID] = tr
		}
	}
	transitionOK := changedCount == len(sample.Truth.Transitions)
	transitionDetail := fmt.Sprintf("changed=%d truth=%d", changedCount, len(sample.Truth.Transitions))
	for _, expected := range sample.Truth.Transitions {
		actual, ok := transitionByEvent[expected.EventID]
		if !ok {
			transitionOK = false
			transitionDetail = "missing transition " + expected.EventID
			break
		}
		if actual.TunnelID != expected.TunnelID || actual.FromState != expected.FromState || actual.ToState != expected.ToState ||
			actual.EffectiveAtMs != expected.EffectiveAtMs || actual.SourceEventID != expected.SourceEventID || !actual.Changed {
			transitionOK = false
			transitionDetail = fmt.Sprintf("%s expected %s->%s@%d source=%s got %s->%s@%d source=%s",
				expected.EventID, expected.FromState, expected.ToState, expected.EffectiveAtMs, expected.SourceEventID,
				actual.FromState, actual.ToState, actual.EffectiveAtMs, actual.SourceEventID)
			break
		}
	}
	add("state", schema+" every transition matches event truth", transitionOK, transitionDetail)

	byPacket := map[string]forward.PacketEvaluation{}
	for _, ev := range rep.Events {
		for _, pe := range ev.Packets {
			byPacket[pe.Packet.ID] = pe
		}
	}
	allOK := true
	detail := ""
	for packetID, expected := range sample.Truth.FragmentPlans {
		pe, ok := byPacket[packetID]
		if !ok {
			allOK = false
			detail = "missing packet " + packetID
			break
		}
		actual := pe.Plan
		if actual.RejectCode != expected.RejectCode || actual.PieceCount != expected.PieceCount {
			allOK = false
			detail = fmt.Sprintf("%s expected code=%s count=%d got code=%s count=%d", packetID, expected.RejectCode, expected.PieceCount, actual.RejectCode, actual.PieceCount)
			break
		}
		if actual.RejectCode != "" {
			continue
		}
		if len(actual.Pieces) != len(expected.Pieces) {
			allOK = false
			detail = packetID + " piece length mismatch"
			break
		}
		for i := range expected.Pieces {
			if actual.Pieces[i].PayloadBytes != expected.Pieces[i].PayloadBytes {
				allOK = false
				detail = fmt.Sprintf("%s piece %d expected %d got %d", packetID, i, expected.Pieces[i].PayloadBytes, actual.Pieces[i].PayloadBytes)
			}
		}
		var totalDiff int64 = actual.TotalOuterBytes - expected.TotalOuterBytes
		if totalDiff < 0 {
			totalDiff = -totalDiff
		}
		if totalDiff > eng.Project.ToleranceBytes {
			allOK = false
			detail = fmt.Sprintf("%s total diff %d > tolerance %d", packetID, totalDiff, eng.Project.ToleranceBytes)
		}
	}
	add("fragment", schema+" fragment truth within tolerance", allOK, detail)
}

func checkParallel(add func(string, string, bool, string), schema string, eng *forward.Engineering, sequential *forward.Report) {
	budgets := []forward.Budget{{Workers: 1, PerTunnelBuffer: 1}, {Workers: 2, PerTunnelBuffer: 2}, {Workers: 4, PerTunnelBuffer: 8}}
	for i, budget := range budgets {
		parallel, err := forward.RunParallelReport(eng.Project, eng.Events, budget)
		if err != nil {
			add("concurrency", fmt.Sprintf("%s budget-%d", schema, i), false, err.Error())
			return
		}
		ok := forward.SignaturesEqual(forward.ReportSignature(sequential), forward.ReportSignature(parallel))
		add("concurrency", fmt.Sprintf("%s parallel workers=%d buffer=%d matches sequential", schema, budget.Workers, budget.PerTunnelBuffer), ok,
			"state transitions, verdict paths, fragments and reject set signature identical")
	}
}

func checkIncremental(add func(string, string, bool, string), sample *forward.Sample) {
	cases := []struct {
		name          string
		req           forward.EditRequest
		expectKey     string
		expectClosure string
		expectHitKey  string
	}{
		{
			name:          "mtu invalidates dependent fragments only",
			req:           forward.EditRequest{Kind: "tunnel_mtu", TunnelID: "T2", Value: 1360, EffectiveFromSeq: 9},
			expectKey:     "fragment:P-APP-LARGE",
			expectClosure: "tunnel:T2.mtu",
			expectHitKey:  "fragment:P-APP-SMALL",
		},
		{
			name:          "cipher edit invalidates only cipher-dependent fragments",
			req:           forward.EditRequest{Kind: "tunnel_cipher", TunnelID: "T1", Value: "chacha20-poly1305", EffectiveFromSeq: 1},
			expectKey:     "fragment:P-FALLBACK",
			expectClosure: "tunnel:T1.cipher",
			expectHitKey:  "state:E02",
		},
		{
			name:          "rule edit propagates verdict closure to fragment",
			req:           forward.EditRequest{Kind: "rule_action_tunnel", RuleID: "R-NET-BRANCH-16", Value: "T2", EffectiveFromSeq: 1},
			expectKey:     "fragment:P-BRANCH-WEB",
			expectClosure: "result:verdict:P-BRANCH-WEB",
		},
		{
			name:          "peer attribution rebuilds state and tunnel outputs",
			req:           forward.EditRequest{Kind: "tunnel_peer", TunnelID: "T1", Value: "P2", EffectiveFromSeq: 1},
			expectKey:     "state:E02",
			expectClosure: "tunnel:T1.peer_id",
		},
	}
	for _, tc := range cases {
		svc := forward.NewIncrementalService(sample.Project, sample.Events)
		if _, err := svc.Build(); err != nil {
			add("incremental", tc.name, false, err.Error())
			continue
		}
		result, err := svc.ApplyAndReplay(tc.req)
		if err != nil {
			add("incremental", tc.name, false, err.Error())
			continue
		}
		hasKey := containsString(result.InvalidatedKeys, tc.expectKey)
		hasClosure := containsString(result.Closure, tc.expectClosure)
		rebuiltNonEmpty := len(result.RebuiltKeys) > 0
		reusedNonEmpty := len(result.ReusedKeys) > 0
		ok := result.Consistent && hasKey && hasClosure && rebuiltNonEmpty && reusedNonEmpty
		detail := fmt.Sprintf("consistent=%v invalidated=%v closure=%v rebuilt=%d reused=%d intervals=%v",
			result.Consistent, tc.expectKey, tc.expectClosure, len(result.RebuiltKeys), len(result.ReusedKeys), result.RebuiltIntervals)
		if tc.expectHitKey != "" {
			hit := false
			for _, trace := range result.Traces {
				if trace.Key == tc.expectHitKey && trace.Status == "hit" {
					hit = true
				}
			}
			ok = ok && hit
			detail += " expected_hit=" + tc.expectHitKey + ":" + fmt.Sprint(hit)
		}
		add("incremental", tc.name, ok, detail)
	}
}

func checkCheckpoint(ctx context.Context, add func(string, string, bool, string), svc *workbench.Service) {
	demo, err := svc.CheckpointDemo(ctx, forward.SchemaV2, 7)
	if err != nil {
		biz, ok := forward.AsBizError(err)
		if ok {
			add("checkpoint", "prefix checkpoint and recovery", false, biz.Code+": "+biz.Message)
		} else {
			add("checkpoint", "prefix checkpoint and recovery", false, err.Error())
		}
		return
	}
	add("checkpoint", "prefix checkpoint written", demo.FirstCommittedSeq == 7, fmt.Sprintf("committed=%d db=%s", demo.FirstCommittedSeq, demo.DatabasePath))
	add("checkpoint", "matching checkpoint recovery final state", demo.MatchingFinalState && !demo.FullReplay, fmt.Sprintf("recovery_seq=%d replay_last=%d", demo.Recovery.Meta.LastEventSeq, demo.ReplayLastSeq))
}

func checkWriteOrder(ctx context.Context, add func(string, string, bool, string), workDir string) {
	path := filepath.Join(workDir, "write-order.db")
	store, err := checkpoint.Open(ctx, path)
	if err != nil {
		add("checkpoint", "state-before-seq ordering", false, err.Error())
		return
	}
	meta := checkpoint.Meta{ProjectID: "p", SchemaVersion: forward.SchemaV2, ProjectHash: "hash"}
	if err := store.InitializeProject(ctx, meta); err != nil {
		add("checkpoint", "state-before-seq ordering", false, err.Error())
		_ = store.Close()
		return
	}
	states := []forward.TunnelStatus{{TunnelID: "T1", State: forward.StateEstablished, PeerID: "P1"}}
	if err := store.SaveStateBundle(ctx, 99, states, nil); err != nil {
		add("checkpoint", "state-before-seq ordering", false, err.Error())
		_ = store.Close()
		return
	}
	// Deliberately do not call CommitSequence; close as if killed between the two commits.
	_ = store.Close()

	reopened, err := checkpoint.Open(ctx, path)
	if err != nil {
		add("checkpoint", "state-before-seq ordering", false, err.Error())
		return
	}
	defer reopened.Close()
	rec, biz := reopened.LoadCommitted(ctx, forward.SchemaV2, "hash")
	if biz != nil {
		add("checkpoint", "state-before-seq ordering", false, biz.Error())
		return
	}
	add("checkpoint", "uncommitted seq marker hides future state", rec.Meta.LastEventSeq == 0 && len(rec.States) == 0,
		fmt.Sprintf("last_seq=%d visible_states=%d", rec.Meta.LastEventSeq, len(rec.States)))
}

func checkMismatchReplay(ctx context.Context, add func(string, string, bool, string), workDir string, sample *forward.Sample) {
	path := filepath.Join(workDir, "mismatch-replay.db")
	_ = os.Remove(path)
	v1, err := forward.LoadEngineering(forward.SchemaV1, sample.Documents[forward.SchemaV1])
	if err != nil {
		add("checkpoint", "version mismatch full replay", false, err.Error())
		return
	}
	store, err := checkpoint.Open(ctx, path)
	if err != nil {
		add("checkpoint", "version mismatch full replay", false, err.Error())
		return
	}
	if _, err := checkpoint.RunAndCheckpointPrefix(ctx, store, v1.Project, v1.Events, 3); err != nil {
		add("checkpoint", "version mismatch full replay", false, err.Error())
		_ = store.Close()
		return
	}
	_ = store.Close()

	v2, err := forward.LoadEngineering(forward.SchemaV2, sample.Documents[forward.SchemaV2])
	if err != nil {
		add("checkpoint", "version mismatch full replay", false, err.Error())
		return
	}
	reopened, err := checkpoint.Open(ctx, path)
	if err != nil {
		add("checkpoint", "version mismatch full replay", false, err.Error())
		return
	}
	defer reopened.Close()
	result, _, fullReplay, err := checkpoint.ReplayFromRecovery(ctx, reopened, v2.Project, v2.Events)
	if err != nil {
		add("checkpoint", "version mismatch full replay", false, err.Error())
		return
	}
	last := 0
	if result != nil {
		last = result.LastCommitted
	}
	add("checkpoint", "version mismatch forces complete replay", fullReplay && last == len(v2.Events),
		fmt.Sprintf("full_replay=%v last_committed=%d events=%d", fullReplay, last, len(v2.Events)))
}

func checkAnomalies(add func(string, string, bool, string), svc *workbench.Service) {
	expected := map[string]string{
		"overlap_rule":                forward.CodeRuleOverlapConflict,
		"missing_peer":                forward.CodePeerNotFound,
		"mtu_too_small":               forward.CodeMTUTooSmall,
		"negative_keepalive":          forward.CodeKeepaliveNegative,
		"field_conflict":              forward.CodeFieldConflict,
		"unknown_schema":              forward.CodeSchemaUnknown,
		"budget_workers":              forward.CodeBudgetWorkers,
		"budget_buffer":               forward.CodeBudgetBuffer,
		"backpressure_workers":        forward.CodeBackpressureWorkers,
		"backpressure_buffer":         forward.CodeBackpressureBufferFull,
		"checkpoint_truncated":        forward.CodeCheckpointTruncated,
		"checkpoint_version_mismatch": forward.CodeCheckpointVersionMismatch,
	}
	for kind, code := range expected {
		res := svc.Inject(kind, nil)
		ok := res.Code == code
		detail := ""
		if res.Error != nil {
			detail = res.Error.Path
			if b, _ := json.Marshal(res.Error.Context); len(b) > 0 {
				detail += " " + string(b)
			}
		}
		add("anomaly", kind, ok, "expected="+code+" actual="+res.Code+" "+detail)
	}
}

func containsString(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}
