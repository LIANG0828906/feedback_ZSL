package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/tabwriter"

	"quotaflow/internal/checkpoint"
	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
	"quotaflow/internal/generator"
	"quotaflow/internal/inject"
	"quotaflow/internal/schema"
)

func loadBytes(raw []byte, source string) (*domain.Project, error) {
	return schema.LoadBytes(raw, source)
}

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	seed := fs.Int64("seed", 42, "deterministic generator seed")
	out := fs.String("out", "samples", "output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return domain.NewBizError(domain.ESchemaInvalid, *out, "create output directory: %v", err)
	}
	project, truth, err := generator.Generate(*seed, *out)
	if err != nil {
		return err
	}
	fmt.Printf("generated %s in %s: tenants=%d nodes=%d events=%d ticks=%d migrations=%d pauses=%d\n",
		project.ID, *out, len(project.Tenants), len(project.Nodes), len(project.Events),
		project.MaxTick, len(truth.Migrations), len(truth.PauseHistory))
	return nil
}

func runLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := loadSet(*dir)
	if err != nil {
		return err
	}
	base := set["v1"]
	type summary struct {
		Schema      string `json:"schema"`
		Fingerprint string `json:"fingerprint"`
		Tenants     int    `json:"tenants"`
		Nodes       int    `json:"nodes"`
		Events      int    `json:"events"`
		Ticks       int64  `json:"ticks"`
	}
	out := []summary{}
	for _, version := range []string{"v1", "v2", "v3"} {
		project := set[version]
		if project.CanonicalFingerprint() != base.CanonicalFingerprint() {
			return domain.NewBizError(domain.ESchemaInvalid, "$.schema."+version,
				"normalized project differs from v1")
		}
		out = append(out, summary{
			Schema: version, Fingerprint: project.CanonicalFingerprint(),
			Tenants: len(project.Tenants), Nodes: len(project.Nodes),
			Events: len(project.Events), Ticks: project.MaxTick,
		})
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
	return nil
}

func runLedger(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample directory")
	schemaVersion := fs.String("schema", "v2", "schema version to load")
	threads := fs.Int("threads", 4, "tenant worker budget")
	queue := fs.Int("queue", 64, "per-tenant waiting queue limit")
	dbPath := fs.String("db", "", "optional checkpoint SQLite path")
	crashTick := fs.Int64("crash-tick", 0, "test hook: force process exit while committing this tick")
	crashPhase := fs.String("crash-phase", checkpoint.PhaseBeforeGlobal, "test hook: commit phase for forced exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := loadProject(filepath.Join(*dir, "project-"+*schemaVersion+".json"))
	if err != nil {
		return err
	}
	eng, err := engine.New(project, engine.Config{ThreadBudget: *threads, QueueLimit: *queue})
	if err != nil {
		return err
	}
	var store *checkpoint.Store
	if *dbPath != "" {
		if err := checkpoint.Remove(*dbPath); err != nil {
			return err
		}
		store, _, err = checkpoint.Open(*dbPath, "quotaflow-checkpoint-v1", project.CanonicalFingerprint())
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.EnsureInitial(eng.InitialState()); err != nil {
			return err
		}
	}
	printTickHeader()
	for tick := int64(1); tick <= project.MaxTick; tick++ {
		if err := eng.Step(); err != nil {
			return err
		}
		state := eng.LastState()
		if store != nil {
			var hook checkpoint.CommitHook
			if *crashTick > 0 {
				hook = func(phase string) error {
					if tick == *crashTick && phase == *crashPhase {
						os.Exit(137)
					}
					return nil
				}
			}
			if err := store.CommitWithHook(tick, state, hook); err != nil {
				return err
			}
		}
		obs, _ := eng.Tick(tick)
		printTickCompact(obs)
	}
	stats, _ := json.MarshalIndent(eng.Stats(), "", "  ")
	fmt.Println(string(stats))
	return nil
}

func printTickHeader() {
	fmt.Fprintf(os.Stdout, "%-4s %-10s %-8s %-8s %-8s %-8s %-8s %s\n",
		"tick", "tenant", "quota", "used", "avail", "reserve", "waiting", "pauses/migrations")
}

func printTickCompact(obs *domain.TickObservation) {
	for _, tenant := range obs.Tenants {
		pauses := []string{}
		for _, pause := range obs.Pauses {
			if pause.TenantID == tenant.TenantID {
				pauses = append(pauses, "pause:"+pause.FromNode+"->"+pause.ToNode)
			}
		}
		for _, migration := range obs.Migrations {
			if migration.TenantID == tenant.TenantID && migration.StartTick == obs.Tick {
				pauses = append(pauses, "mig:"+migration.FromNode+"->"+migration.ToNode+"@"+migration.Status)
			}
		}
		fmt.Fprintf(os.Stdout, "%-4d %-10s %-8d %-8d %-8d %-8d %-8d %s\n",
			obs.Tick, tenant.TenantID, tenant.QuotaMilli, tenant.UsedMilli,
			tenant.AvailableMilli, tenant.ReserveRemainingMilli, tenant.WaitingMilli,
			strings.Join(pauses, ","))
	}
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := loadSet(*dir)
	if err != nil {
		return err
	}
	project := set["v2"]
	rawTruth, err := os.ReadFile(filepath.Join(*dir, "truth.json"))
	if err != nil {
		return domain.NewBizError(domain.ESchemaInvalid, *dir+"/truth.json", "read truth: %v", err)
	}
	var truth domain.Truth
	if err := json.Unmarshal(rawTruth, &truth); err != nil {
		return domain.NewBizError(domain.ESchemaInvalid, *dir+"/truth.json", "decode truth: %v", err)
	}
	for _, version := range []string{"v1", "v2", "v3"} {
		if set[version].CanonicalFingerprint() != truth.ProjectFingerprint {
			return domain.NewBizError(domain.ESchemaInvalid, "$."+version, "fingerprint differs from truth")
		}
		versionEngine, err := engine.New(set[version], engine.Config{ThreadBudget: 4, QueueLimit: 64})
		if err != nil {
			return err
		}
		if err := versionEngine.RunAll(); err != nil {
			return err
		}
		if !reflect.DeepEqual(versionEngine.Observations(), truth.Ticks) {
			return domain.NewBizError(domain.ELedgerInvariant, "$."+version,
				"engine output differs from generator truth")
		}
	}
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64})
	if err != nil {
		return err
	}
	if err := eng.RunAll(); err != nil {
		return err
	}
	full, err := eng.FullRecompute()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(eng.Observations(), full.Observations()) {
		return domain.NewBizError(domain.ELedgerInvariant, "$.incremental", "full recompute mismatch")
	}
	report, err := eng.ApplyEdit(engine.Edit{
		ID: "verify-weight", Kind: engine.EditTenantWeight, Target: "t-alpha", FromTick: 4, Weight: 3,
	})
	if err != nil {
		return err
	}
	full, err = eng.FullRecompute()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(eng.Observations(), full.Observations()) {
		return domain.NewBizError(domain.ELedgerInvariant, "$.incremental", "incremental mismatch")
	}
	fmt.Printf("PASS schema-equivalence tenants/nodes/events; truth ticks=%d; migrations=%d; pauses=%d; reused=%d recomputed=%d\n",
		len(truth.Ticks), len(truth.Migrations), len(truth.PauseHistory),
		len(report.ReusedTicks), len(report.RecomputedTicks))
	return nil
}

func runInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample directory")
	kind := fs.String("kind", inject.NegativeQuota, "injection kind")
	schemaVersion := fs.String("schema", "v2", "schema version to inject into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := filepath.Join(*dir, "project-"+*schemaVersion+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		return domain.NewBizError(domain.ESchemaInvalid, path, "read source: %v", err)
	}
	switch *kind {
	case "thread-budget":
		_, err := engine.New(generator.Scenario(42), engine.Config{ThreadBudget: 0, QueueLimit: 64})
		return reportInjectionError(*kind, "$.thread_budget", domain.EBackpressureBudget, err)
	case "queue-budget":
		_, err := engine.New(generator.Scenario(42), engine.Config{ThreadBudget: 2, QueueLimit: 0})
		return reportInjectionError(*kind, "$.queue_limit", domain.EBackpressureQueue, err)
	case "checkpoint-truncated":
		path := filepath.Join(*dir, "truncated-checkpoint.db")
		if err := os.WriteFile(path, []byte("not a sqlite checkpoint"), 0o644); err != nil {
			return err
		}
		_, _, err := checkpoint.Open(path, "quotaflow-checkpoint-v1", "hash")
		return reportInjectionError(*kind, path, domain.ECheckpointTruncated, err)
	case "checkpoint-version":
		versionPath := filepath.Join(*dir, "version-checkpoint.db")
		if err := checkpoint.Remove(versionPath); err != nil {
			return err
		}
		first, _, err := checkpoint.Open(versionPath, "expected-v1", "hash-a")
		if err != nil {
			return err
		}
		first.Close()
		store, rebuilt, err := checkpoint.Open(versionPath, "expected-v2", "hash-b")
		if err != nil {
			return err
		}
		defer store.Close()
		if !rebuilt {
			return domain.NewBizError(domain.ECheckpointVersion, path, "expected version mismatch rebuild")
		}
		_ = rebuilt
		fmt.Printf("injected=checkpoint-version code=E_CHECKPOINT_VERSION rebuilt=true context=version-checkpoint.db\n")
		return nil
	}
	mutated, err := inject.MutateProject(*kind, before)
	if err != nil {
		return err
	}
	_, loadErr := schema.LoadBytes(mutated, "injected:"+path)
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		return readErr
	}
	if !bytes.Equal(before, after) {
		return domain.NewBizError(domain.EInjectionSource, path, "source project was modified")
	}
	if loadErr == nil {
		return domain.NewBizError(domain.EInjectionSource, path, "injection did not trigger an error")
	}
	biz := loadErr.(*domain.BizError)
	fmt.Printf("injected=%s code=%s path=%s source_unchanged=true\n", *kind, biz.Code, biz.Path)
	return nil
}

func reportInjectionError(kind, path, expected string, err error) error {
	if err == nil {
		return domain.NewBizError(expected, path, "injection did not trigger an error")
	}
	if domain.CodeOf(err) != expected {
		return err
	}
	biz := err.(*domain.BizError)
	fmt.Printf("injected=%s code=%s path=%s source_unchanged=true\n", kind, biz.Code, biz.Path)
	return nil
}

func printJSON(value any) {
	raw, _ := json.MarshalIndent(value, "", "  ")
	fmt.Println(string(raw))
}

func writeTabbed(rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 1, 2, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
}
