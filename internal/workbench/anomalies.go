package workbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"tunnelworkbench/internal/checkpoint"
	"tunnelworkbench/internal/forward"
)

func (s *Service) Inject(kind string, payload map[string]any) *AnomalyResult {
	res := &AnomalyResult{Kind: kind, HealthyAfter: true}
	var biz *forward.BizError

	switch kind {
	case "overlap_rule":
		biz = s.injectOverlap()
	case "missing_peer":
		biz = s.injectMissingPeer()
	case "mtu_too_small":
		biz = s.injectMTU()
	case "negative_keepalive":
		biz = s.injectNegativeKeepalive()
	case "field_conflict":
		biz = s.injectFieldConflict()
	case "unknown_schema":
		_, err := forward.LoadEngineering("v9", []byte(`{}`))
		biz, _ = forward.AsBizError(err)
	case "budget_workers":
		biz = forward.ValidateBudget(forward.Budget{Workers: -1, PerTunnelBuffer: 8})
	case "budget_buffer":
		biz = forward.ValidateBudget(forward.Budget{Workers: 2, PerTunnelBuffer: 1_000_000})
	case "backpressure_workers":
		biz = probeWorkerBackpressure()
	case "backpressure_buffer":
		biz = s.probeBufferBackpressure()
	case "checkpoint_truncated":
		biz = s.injectTruncatedCheckpoint()
	case "checkpoint_version_mismatch":
		biz = s.injectCheckpointVersionMismatch()
	default:
		biz = forward.NewBizError(forward.CodeEditInvalid, "inject.kind", "unknown anomaly kind %q", kind)
	}

	if biz != nil {
		res.Code = biz.Code
		res.Error = biz
		res.Context = biz.Context
		res.Note = "stable business code returned with locatable path/context; source sample and last good checkpoint were not modified"
	} else {
		res.Note = "anomaly probe completed without a business error"
		res.HealthyAfter = false
	}
	return res
}

func (s *Service) injectOverlap() *forward.BizError {
	bad := s.sample.Project.Clone()
	bad.Rules = append(bad.Rules, forward.Rule{
		ID: "R-INJECT-OVERLAP", DeclOrder: 99, Priority: 20, GroupPeerID: "P3",
		CIDRs:  []string{"10.20.8.128/25"},
		Action: forward.Action{Type: forward.ActionTunnel, TunnelID: "T3"},
	})
	return bizOrNil(forward.ValidateProjectWith(bad, forward.ValidationOptions{RejectEqualPriorityOverlap: true}))
}

func (s *Service) injectMissingPeer() *forward.BizError {
	bad := s.sample.Project.Clone()
	bad.Tunnels = append(bad.Tunnels, forward.Tunnel{
		ID: "T-BAD-PEER", DeclOrder: 99, PeerID: "P-DOES-NOT-EXIST",
		Local:         forward.Endpoint{IP: "198.51.100.99", Port: 50099},
		Remote:        forward.Endpoint{IP: "203.0.113.99", Port: 51899},
		OuterProtocol: "udp", CipherSuite: "aes-256-gcm",
		MTUBytes: 1420, PathMTUBytes: 1500, KeepaliveMS: 10000,
	})
	return bizOrNil(forward.ValidateProject(bad))
}

func (s *Service) injectMTU() *forward.BizError {
	bad := s.sample.Project.Clone()
	bad.Tunnels[0].MTUBytes = 8
	return bizOrNil(forward.ValidateProject(bad))
}

func (s *Service) injectNegativeKeepalive() *forward.BizError {
	bad := s.sample.Project.Clone()
	bad.Tunnels[1].KeepaliveMS = -1
	return bizOrNil(forward.ValidateProject(bad))
}

func (s *Service) injectFieldConflict() *forward.BizError {
	var doc map[string]any
	if err := json.Unmarshal(s.sample.Documents[forward.SchemaV1], &doc); err != nil {
		return forward.WrapBiz(forward.CodeSchemaParse, "anomaly.document", err, "cannot clone v1 document")
	}
	tunnels, ok := doc["tunnel_table"].([]any)
	if !ok || len(tunnels) == 0 {
		return forward.NewBizError(forward.CodeFieldRequired, "tunnel_table[0]", "generator document missing tunnel")
	}
	first, ok := tunnels[0].(map[string]any)
	if !ok {
		return forward.NewBizError(forward.CodeFieldType, "tunnel_table[0]", "tunnel must be object")
	}
	first["keepalive_ms"] = 999_999
	raw, err := json.Marshal(doc)
	if err != nil {
		return forward.WrapBiz(forward.CodeSchemaParse, "anomaly.document", err, "cannot marshal conflict document")
	}
	_, loadErr := forward.LoadEngineering(forward.SchemaV1, raw)
	biz, _ := forward.AsBizError(loadErr)
	return biz
}

func probeWorkerBackpressure() *forward.BizError {
	sched, biz := forward.NewScheduler(forward.Budget{Workers: 1, PerTunnelBuffer: 1})
	if biz != nil {
		return biz
	}
	lease, biz := sched.TryAcquireWorker()
	if biz != nil {
		return biz
	}
	_, exhaustBiz := sched.TryAcquireWorker()
	lease.Release()
	sched.Close()
	if exhaustBiz == nil {
		return forward.NewBizError(forward.CodeInternal, "budget.workers", "second worker lease unexpectedly accepted")
	}
	return exhaustBiz
}

func (s *Service) probeBufferBackpressure() *forward.BizError {
	sched, biz := forward.NewScheduler(forward.Budget{Workers: 1, PerTunnelBuffer: 1})
	if biz != nil {
		return biz
	}
	defer sched.Close()
	lease, biz := sched.TryAcquireWorker()
	if biz != nil {
		return biz
	}
	defer lease.Release()

	var backpressure *forward.BizError
	for i := 0; i < 4; i++ {
		_, err := sched.TrySubmit("T-BACKPRESSURE", func() {})
		if err != nil {
			backpressure = err
			break
		}
	}
	if backpressure == nil {
		return forward.NewBizError(forward.CodeInternal, "budget.per_tunnel_buffer", "bounded queue did not exert backpressure")
	}
	return backpressure
}

func (s *Service) injectTruncatedCheckpoint() *forward.BizError {
	ctx := context.Background()
	goodPath := filepath.Join(s.workDir, "anomaly-good.db")
	badPath := filepath.Join(s.workDir, "anomaly-truncated.db")
	_ = os.Remove(goodPath)
	_ = os.Remove(badPath)

	eng := s.engineer[forward.SchemaV1]
	store, err := checkpoint.Open(ctx, goodPath)
	if err != nil {
		return forward.AsBizErrorOrInternal(err)
	}
	if _, err := checkpoint.RunAndCheckpointPrefix(ctx, store, eng.Project, eng.Events, 4); err != nil {
		_ = store.Close()
		return forward.AsBizErrorOrInternal(err)
	}
	if err := store.Close(); err != nil {
		return forward.AsBizErrorOrInternal(err)
	}
	if err := copyFile(goodPath, badPath); err != nil {
		return forward.WrapBiz(forward.CodeInternal, badPath, err, "cannot copy checkpoint for truncation injection")
	}
	if err := os.Truncate(badPath, 128); err != nil {
		return forward.WrapBiz(forward.CodeInternal, badPath, err, "cannot truncate checkpoint copy")
	}
	badStore, openErr := checkpoint.Open(ctx, badPath)
	if openErr != nil {
		return forward.AsBizErrorOrInternal(openErr)
	}
	_ = badStore.Close()
	return forward.NewBizError(forward.CodeInternal, "checkpoint", "truncated checkpoint was accepted")
}

func (s *Service) injectCheckpointVersionMismatch() *forward.BizError {
	ctx := context.Background()
	path := filepath.Join(s.workDir, "anomaly-version.db")
	_ = os.Remove(path)
	eng := s.engineer[forward.SchemaV1]
	store, err := checkpoint.Open(ctx, path)
	if err != nil {
		return forward.AsBizErrorOrInternal(err)
	}
	if _, err := checkpoint.RunAndCheckpointPrefix(ctx, store, eng.Project, eng.Events, 3); err != nil {
		_ = store.Close()
		return forward.AsBizErrorOrInternal(err)
	}

	v2 := s.engineer[forward.SchemaV2].Project
	_, biz := store.LoadCommitted(ctx, forward.SchemaV3, v2.CanonicalHash())
	_ = store.Close()
	if biz == nil {
		return forward.NewBizError(forward.CodeInternal, "checkpoint_meta", "version mismatch checkpoint unexpectedly accepted")
	}
	return biz
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o644)
}

func bizOrNil(err error) *forward.BizError {
	biz, _ := forward.AsBizError(err)
	return biz
}
