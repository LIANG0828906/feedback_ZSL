package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"quotaengine/gen"
	"quotaengine/quota"
)

type faultReport struct {
	Case                   string `json:"case"`
	Code                   string `json:"code"`
	Op                     string `json:"op,omitempty"`
	Path                   string `json:"path,omitempty"`
	TenantID               string `json:"tenant_id,omitempty"`
	NodeID                 string `json:"node_id,omitempty"`
	TimeMilli              int64  `json:"time_milli,omitempty"`
	Detail                 string `json:"detail,omitempty"`
	ResultDoesNotMutateSrc bool   `json:"source_project_unchanged"`
	CommittedLedgerUsable  bool   `json:"committed_ledger_usable"`
}

func main() {
	var (
		seed       int64
		workers    int
		queueLimit int
		dbPath     string
		artifacts  string
		inject     string
		commitStop int
	)
	flag.Int64Var(&seed, "seed", 42, "deterministic generator seed")
	flag.IntVar(&workers, "workers", 4, "worker budget (1..64)")
	flag.IntVar(&queueLimit, "queue", 64, "per-tenant waiting queue limit")
	flag.StringVar(&dbPath, "db", "", "checkpoint SQLite path (default temporary)")
	flag.StringVar(&artifacts, "artifacts", "", "optional directory for schema documents and truth")
	flag.StringVar(&inject, "inject", "", "fault: negative-quota|guarantee-over-capacity|quota-scale-overflow|unknown-schema|invalid-budget|queue-backpressure|truncated-checkpoint|version-mismatch")
	flag.IntVar(&commitStop, "commit-stop", 2, "stop checkpoint demo after N complete time commits")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, err := gen.Generate(seed)
	if err != nil {
		fail(err)
	}
	if inject != "" {
		runInjection(ctx, g, inject, workers, queueLimit, dbPath)
		return
	}
	runDemo(ctx, g, workers, queueLimit, dbPath, artifacts, commitStop)
}

func runDemo(ctx context.Context, g gen.Generated, workers, queueLimit int, dbPath, artifacts string, commitStop int) {
	opts := quota.Options{Workers: workers, QueuePerTenant: queueLimit}
	fmt.Println("=== schema load ===")
	loaded := make([]*quota.Project, 3)
	for i, item := range []struct {
		name string
		raw  []byte
	}{{"v1", g.Documents.V1}, {"v2", g.Documents.V2}, {"v3", g.Documents.V3}} {
		p, err := quota.LoadWithOptions(item.raw, quota.LoadOptions{ExpectedVersion: item.name})
		if err != nil {
			fail(err)
		}
		loaded[i] = p
		fmt.Printf("%s tenants=%d nodes=%d racks=%d events=%d\n", item.name, len(p.Tenants), len(p.Nodes), len(p.Topology()), len(p.Events))
	}
	allEqual := true
	for i := 1; i < len(loaded); i++ {
		equal, err := quota.Equivalent(loaded[0], loaded[i])
		if err != nil {
			fail(err)
		}
		allEqual = allEqual && equal
	}
	fmt.Printf("semantic_equal=%t topology=%+v\n", allEqual, loaded[0].Topology())

	timeline, err := quota.Run(ctx, loaded[1], opts)
	if err != nil {
		fail(err)
	}
	fmt.Println("=== ledger / admission truth ===")
	for _, step := range timeline.Steps {
		admitted, rejected := []string{}, []string{}
		for _, d := range step.Decisions {
			if d.Kind != "request" {
				continue
			}
			label := fmt.Sprintf("%s@%s:%d(%s)", d.TenantID, d.NodeID, d.AmountMilli, d.OrderBasis)
			if d.Action == "admitted" || d.Action == "retried_admitted" {
				admitted = append(admitted, label)
			} else {
				rejected = append(rejected, label+"!"+d.Constraint)
			}
		}
		fmt.Printf("t=%dms admitted=%v rejected=%v queue=%d backpressure=%v\n", step.TimeMilli, admitted, rejected, len(step.Queue), step.Backpressure)
		for _, ledger := range step.Tenants {
			fmt.Printf("  %-8s available=%5d used=%5d guarantee_remaining=%5d quota=%5d burst=%4d weight=%d\n",
				ledger.TenantID, ledger.AvailableMilli, ledger.UsedMilli, ledger.GuaranteeRemainingMilli,
				ledger.QuotaMilli, ledger.BurstMilli, ledger.WeightPermille)
		}
	}
	if err := g.Truth.Check(timeline); err != nil {
		fail(err)
	}
	fmt.Println("truth_check=OK tolerance_millicores=0")

	fmt.Println("=== fixed-point critical conversions ===")
	for _, tc := range gen.CriticalConversions() {
		conv, convErr := quota.QuotaForNode(tc.CapacityMilli, tc.RatioBP, tc.WeightPermille, tc.CaseID)
		report := quota.CheckPrecision(conv, tc.Truth, quota.DefaultToleranceMilli, tc.CaseID)
		raw, _ := json.Marshal(map[string]any{
			"case": tc.CaseID, "path": conv.Path, "formula": conv.Formula,
			"intermediate_values": conv.Intermediates,
			"numerator":           conv.Numerator, "denominator": conv.Denominator,
			"result": conv.Result, "truth": report.Truth, "delta": report.Delta,
			"tolerance": report.Tolerance, "exceeds_tolerance": report.ExceedsTolerance,
			"overflow": conv.Overflow, "saturated": conv.Saturated,
			"accepted": report.Accepted, "note": report.Note, "error": errorText(convErr),
		})
		fmt.Println(string(raw))
	}

	fmt.Println("=== incremental invalidation ===")
	base := timeline
	edits := []quota.Edit{
		{Kind: quota.EditNodeCapacity, NodeID: "n-core-1", TimeMilli: 0, Value: 5000},
		{Kind: quota.EditTenantQuota, TenantID: "t-alpha", TimeMilli: 1000, Value: 3500},
		{Kind: quota.EditWeightEvent, TenantID: "t-gamma", EventSeq: 12, TimeMilli: 3000, Value: 2000},
	}
	for _, edit := range edits {
		result, err := quota.ApplyEdit(ctx, loaded[1], opts, base, edit)
		if err != nil {
			fail(err)
		}
		diff := quota.DiffTimelines(base, result.Timeline)
		fmt.Printf("edit=%s target=%s/%s at=%dms invalidated_from=%d reused=%d recomputed=%d full_equivalent=%t diffs=%d\n",
			edit.Kind, edit.TenantID, edit.NodeID, edit.TimeMilli, result.InvalidatedFrom,
			result.ReusedSteps, result.RecomputedSteps, result.FullEquivalent, len(diff))
		fmt.Printf("  closure=%q\n", result.Closure)
		if len(diff) > 0 {
			fmt.Printf("  first_diff=%+v\n", diff[0])
		}
	}

	fmt.Println("=== checkpoint interruption / resume ===")
	ownDB := false
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "quota-demo-*")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(dir)
		dbPath = filepath.Join(dir, "checkpoint.db")
		ownDB = true
	} else if err := os.Remove(dbPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fail(err)
	}
	store, err := quota.OpenCheckpoint(dbPath)
	if err != nil {
		fail(err)
	}
	first, err := quota.RunWithCheckpointLimit(ctx, loaded[1], opts, store, commitStop)
	if err != nil {
		fail(err)
	}
	fmt.Printf("first_run interrupted=%t committed_steps=%d resumed=false db=%s\n", first.Interrupted, len(first.Timeline.Steps), dbPath)
	if err := store.Close(); err != nil {
		fail(err)
	}
	store, err = quota.OpenCheckpoint(dbPath)
	if err != nil {
		fail(err)
	}
	rest, err := quota.RunWithCheckpoint(ctx, loaded[1], opts, store)
	if err != nil {
		fail(err)
	}
	combined := &quota.Timeline{
		ProjectFingerprint: rest.Timeline.ProjectFingerprint,
		Steps:              append(append([]quota.TimeStep(nil), first.Timeline.Steps...), rest.Timeline.Steps...),
		FinalState:         rest.Timeline.FinalState,
	}
	if err := quota.CompareTimelines(combined, base); err != nil {
		fail(err)
	}
	snap, err := store.Load(ctx, loaded[1])
	if err != nil {
		fail(err)
	}
	fmt.Printf("resumed=%t from=%dms suffix_steps=%d checkpoint_time=%dms reconciled_ledgers=%d final_equals_full=true\n",
		rest.Resumed, rest.ResumedFromMilli, len(rest.Timeline.Steps), snap.Meta.CommittedTimeMilli, len(snap.Ledger))
	v1, err := quota.Load(g.Documents.V1)
	if err != nil {
		fail(err)
	}
	_, mismatch := store.Load(ctx, v1)
	fmt.Printf("checkpoint_version_mismatch_code=%s committed_v2_ledger_still_usable=%t\n",
		quota.ErrorCode(mismatch), checkpointUsable(ctx, store, loaded[1]))
	if !ownDB {
		fmt.Printf("checkpoint_db_preserved=%s\n", dbPath)
	}
	if err := store.Close(); err != nil {
		fail(err)
	}

	if artifacts != "" {
		if err := writeArtifacts(artifacts, g); err != nil {
			fail(err)
		}
		fmt.Printf("artifacts_written=%s\n", artifacts)
	}
}

func runInjection(ctx context.Context, g gen.Generated, name string, workers, queueLimit int, dbPath string) {
	originalFingerprint := projectFP(g.Project)
	report := faultReport{Case: name, ResultDoesNotMutateSrc: true, CommittedLedgerUsable: true}
	var err error
	switch name {
	case "negative-quota", "quota-scale-overflow", "guarantee-over-capacity", "unknown-schema":
		var raw []byte
		switch name {
		case "negative-quota":
			raw, _, err = gen.InjectNegativeQuota(g.Documents.V2)
		case "quota-scale-overflow":
			raw, _, err = gen.InjectQuotaScaleOverflow(g.Documents.V2)
		case "guarantee-over-capacity":
			raw, _, err = gen.InjectGuaranteeOverCapacity(g.Documents.V2)
		case "unknown-schema":
			raw, _, err = gen.InjectUnknownSchema(g.Documents.V2)
		}
		if err == nil {
			_, err = quota.Load(raw)
		}
	case "invalid-budget":
		var p *quota.Project
		p, err = quota.Load(g.Documents.V2)
		if err == nil {
			_, err = quota.Run(ctx, p, quota.Options{Workers: 65, QueuePerTenant: queueLimit})
		}
	case "queue-backpressure":
		p := &quota.Project{
			SchemaVersion: "v2",
			Tenants:       []quota.Tenant{{ID: "t-zero", QuotaBP: 0, GuaranteeBP: 0, Priority: 0}},
			Nodes:         []quota.Node{{ID: "n1", CapacityMilli: 1000, RackID: "r1", AllowedTenants: []string{"t-zero"}}},
			Events: []quota.Event{
				{Seq: 1, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-zero", NodeID: "n1", AmountMilli: 1000},
				{Seq: 2, TimeMilli: 0, Type: quota.EventRequest, TenantID: "t-zero", NodeID: "n1", AmountMilli: 1000},
			},
		}
		_, err = quota.Run(ctx, p, quota.Options{Workers: workers, QueuePerTenant: 1})
	case "truncated-checkpoint":
		dir, mkErr := os.MkdirTemp("", "quota-truncate-*")
		if mkErr != nil {
			err = mkErr
			break
		}
		defer os.RemoveAll(dir)
		good := filepath.Join(dir, "good.db")
		bad := filepath.Join(dir, "bad.db")
		var store *quota.CheckpointStore
		store, err = quota.OpenCheckpoint(good)
		if err != nil {
			break
		}
		_, err = quota.RunWithCheckpointLimit(ctx, g.Project, quota.Options{Workers: workers, QueuePerTenant: queueLimit}, store, 2)
		if closeErr := store.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			break
		}
		if err = gen.TruncateCopy(good, bad); err == nil {
			_, err = quota.OpenCheckpoint(bad)
		}
		report.CommittedLedgerUsable = checkpointPathUsable(ctx, good, g.Project)
	case "version-mismatch":
		path := dbPath
		if path == "" {
			dir, mkErr := os.MkdirTemp("", "quota-version-*")
			if mkErr != nil {
				err = mkErr
				break
			}
			defer os.RemoveAll(dir)
			path = filepath.Join(dir, "checkpoint.db")
		}
		var store *quota.CheckpointStore
		store, err = quota.OpenCheckpoint(path)
		if err != nil {
			break
		}
		if err = store.Save(ctx, g.Project, quota.NewState(g.Project)); err != nil {
			store.Close()
			break
		}
		var v1 *quota.Project
		v1, err = quota.Load(g.Documents.V1)
		if err == nil {
			_, err = store.Load(ctx, v1)
		}
		report.CommittedLedgerUsable = checkpointUsable(ctx, store, g.Project)
		store.Close()
	default:
		err = fmt.Errorf("unknown injection %q", name)
	}
	fillFaultContext(&report, err)
	report.ResultDoesNotMutateSrc = projectFP(g.Project) == originalFingerprint
	raw, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(raw))
	if report.Code == "" {
		fail(err)
	}
}

func fillFaultContext(report *faultReport, err error) {
	if err == nil {
		return
	}
	report.Code = quota.ErrorCode(err)
	if report.Code == "" {
		report.Code = "E_UNCLASSIFIED"
	}
	var be *quota.BusinessError
	if errors.As(err, &be) {
		report.Op = be.Op
		report.Path = be.Path
		report.TenantID = be.TenantID
		report.NodeID = be.NodeID
		report.TimeMilli = be.TimeMilli
		report.Detail = be.Detail
	} else {
		report.Detail = err.Error()
	}
}

func checkpointUsable(ctx context.Context, store *quota.CheckpointStore, p *quota.Project) bool {
	_, err := store.Load(ctx, p)
	return err == nil
}

func checkpointPathUsable(ctx context.Context, path string, p *quota.Project) bool {
	store, err := quota.OpenCheckpoint(path)
	if err != nil {
		return false
	}
	defer store.Close()
	_, err = store.Load(ctx, p)
	return err == nil
}

func projectFP(p *quota.Project) string {
	fp, err := p.Fingerprint()
	if err != nil {
		return ""
	}
	return fp
}

func writeArtifacts(dir string, g gen.Generated) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{"v1.json": g.Documents.V1, "v2.json": g.Documents.V2, "v3.json": g.Documents.V3}
	truthRaw, err := json.MarshalIndent(g.Truth, "", "  ")
	if err != nil {
		return err
	}
	files["truth.json"] = truthRaw
	for name, raw := range files {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func fail(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "demo_failed: %v\n", err)
	os.Exit(1)
}
