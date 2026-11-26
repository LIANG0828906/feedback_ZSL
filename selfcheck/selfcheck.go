// Package selfcheck runs the local deterministic acceptance path without HTTP.
package selfcheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dnslab/codes"
	"dnslab/editor"
	"dnslab/service"
	"dnslab/spec"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	OK             bool    `json:"ok"`
	Seed           int64   `json:"seed"`
	WallDurationMS float64 `json:"wall_duration_ms"`
	Checks         []Check `json:"checks"`
	TruthTotal     int     `json:"truth_total"`
	TruthMatched   int     `json:"truth_matched"`
}

func Run(seed int64) (*Report, error) {
	start := time.Now()
	report := &Report{OK: true, Seed: seed}
	tmp, err := os.MkdirTemp("", "dnslab-selfcheck-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	svc, err := service.New(seed, filepath.Join(tmp, "checkpoint.sqlite"))
	if err != nil {
		return nil, err
	}
	defer svc.Close()
	ctx := context.Background()

	schemas := []string{spec.SchemaV1, spec.SchemaV2, spec.SchemaV3}
	for _, schema := range schemas {
		if err := svc.LoadSchema(schema); err != nil {
			report.add(false, "load "+schema, codes.CodeOf(err), err.Error())
			continue
		}
		run, err := svc.RunAll(ctx, 4)
		if err != nil {
			report.add(false, "run "+schema, codes.CodeOf(err), err.Error())
			continue
		}
		report.TruthTotal += len(run.Truth)
		report.TruthMatched += run.Matches
		report.add(run.AllMatch, "truth "+schema, codes.OK,
			fmt.Sprintf("%d/%d semantic matches", run.Matches, len(run.Truth)))
	}

	editResp, err := svc.ApplyEdit(editor.Edit{Kind: "rule_upstream", RuleNumber: "R-002", UpstreamIndex: 1})
	if err != nil {
		report.add(false, "incremental edit", codes.CodeOf(err), err.Error())
	} else {
		report.add(editResp.AllMatch, "incremental equals full recompute", codes.OK,
			fmt.Sprintf("rebuilt=%v reused=%v", editResp.Report.RebuiltRegions, editResp.Report.ReusedRegions))
	}

	if _, err := svc.RunAll(ctx, 4); err != nil {
		report.add(false, "checkpoint baseline run", codes.CodeOf(err), err.Error())
	}
	if _, err := svc.SaveCheckpoint(ctx); err != nil {
		report.add(false, "save checkpoint", codes.CodeOf(err), err.Error())
	} else if _, err := svc.RestoreCheckpoint(ctx); err != nil {
		report.add(false, "restore matching checkpoint", codes.CodeOf(err), err.Error())
	} else {
		report.add(true, "save/restore matching checkpoint", codes.OK, "version matched")
	}

	injections := []struct {
		name string
		req  editor.InjectionRequest
		want string
	}{
		{"overlap", editor.InjectionRequest{Kind: "overlap"}, codes.ERuleOverlap},
		{"invalid upstream", editor.InjectionRequest{Kind: "invalid_upstream_ref"}, codes.EUpstreamReference},
		{"ttl", editor.InjectionRequest{Kind: "ttl_out_of_range"}, codes.ETTLRange},
		{"timeout", editor.InjectionRequest{Kind: "timeout_out_of_range"}, codes.ETimeoutRange},
		{"workers", editor.InjectionRequest{Kind: "worker_budget_out_of_range"}, codes.EBudgetRange},
		{"hops", editor.InjectionRequest{Kind: "hop_budget_out_of_range"}, codes.EHopsRange},
		{"backpressure", editor.InjectionRequest{Kind: "backpressure", Workers: 2}, codes.EBackpressureWorkers},
		{"truncated checkpoint", editor.InjectionRequest{Kind: "truncated_checkpoint"}, codes.ECheckpointTruncated},
		{"version mismatch", editor.InjectionRequest{Kind: "version_mismatch_checkpoint"}, codes.ECheckpointVersion},
	}
	for _, tc := range injections {
		err := svc.Inject(tc.req)
		code := codes.CodeOf(err)
		report.add(code == tc.want, "inject "+tc.name, code, "want "+tc.want)
	}

	report.WallDurationMS = float64(time.Since(start).Microseconds()) / 1000.0
	for _, c := range report.Checks {
		if !c.OK {
			report.OK = false
			return report, fmt.Errorf("selfcheck failed at %s (%s)", c.Name, c.Code)
		}
	}
	return report, nil
}

func (r *Report) add(ok bool, name, code, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Code: code, Detail: detail})
}
