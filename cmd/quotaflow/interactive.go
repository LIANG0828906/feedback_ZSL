package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"quotaflow/internal/checkpoint"
	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
	"quotaflow/internal/inject"
	"quotaflow/internal/schema"
)

func runInteractive(args []string) error {
	fs := flag.NewFlagSet("interactive", flag.ContinueOnError)
	dir := fs.String("dir", "samples", "sample directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	set, err := loadSet(*dir)
	if err != nil {
		return err
	}
	project := set["v2"]
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64, CheckpointVersion: "quotaflow-checkpoint-v1"})
	if err != nil {
		return err
	}
	if err := eng.RunAll(); err != nil {
		return err
	}
	lastReport := ""
	editSequence := 0
	fmt.Printf("quotaflow interactive; samples=%s project=%s ticks=%d\nType 'help' for commands.\n", *dir, project.ID, project.MaxTick)
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("qflow> ")
		if !scanner.Scan() {
			break
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "help":
			fmt.Print(`commands:
  ticks                                   list committed ticks
  show <tick>                             print tick ledger/pauses/migrations JSON
  edit weight <tenant> <weight> <tick>    apply incremental tenant weight edit
  edit capacity <node> <milli> <tick>     apply incremental node capacity edit
  edit fdnode <node> <domain> <tick>      apply incremental node fault-domain edit
  edit fdparent <domain> <parent|-> <tick> edit fault-domain parent
  fullcheck                               compare incremental cache with full recompute
  inject <kind>                           run project injection against project-v2.json
  checkpoint <path>                       rerun from zero with SQLite commits
  resume <path>                           load latest committed tick and continue
  quit                                    exit
`)
		case "ticks":
			for _, tick := range eng.Observations() {
				fmt.Printf("tick=%d pauses=%d pending=%d migrations=%d\n",
					tick.Tick, len(tick.Pauses), len(tick.Pending), len(tick.Migrations))
			}
		case "show":
			if len(fields) != 2 {
				fmt.Println("usage: show <tick>")
				continue
			}
			tick, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				fmt.Println("invalid tick")
				continue
			}
			obs, ok := eng.Tick(tick)
			if !ok {
				fmt.Println("tick is not committed")
				continue
			}
			printJSON(obs)
		case "edit":
			editSequence++
			edit, err := parseEdit(fields, editSequence)
			if err != nil {
				fmt.Printf("%s\n", err)
				continue
			}
			report, err := eng.ApplyEdit(edit)
			if err != nil {
				writeError(err)
				continue
			}
			raw, _ := json.MarshalIndent(report, "", "  ")
			lastReport = string(raw)
			fmt.Println(lastReport)
		case "fullcheck":
			full, err := eng.FullRecompute()
			if err != nil {
				writeError(err)
				continue
			}
			fmt.Printf("incremental_equals_full=%v\n", observationsEqual(eng, full))
			if lastReport != "" {
				fmt.Println("last invalidation report:")
				fmt.Println(lastReport)
			}
		case "inject":
			if len(fields) != 2 {
				fmt.Println("usage: inject <kind>")
				continue
			}
			path := filepath.Join(*dir, "project-v2.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				writeError(err)
				continue
			}
			mutated, err := inject.MutateProject(fields[1], raw)
			if err != nil {
				writeError(err)
				continue
			}
			_, err = schema.LoadBytes(mutated, "injected")
			writeError(err)
		case "checkpoint":
			if len(fields) != 2 {
				fmt.Println("usage: checkpoint <path>")
				continue
			}
			if err := interactiveCheckpoint(fields[1], project); err != nil {
				writeError(err)
				continue
			}
			fmt.Println("checkpoint complete")
		case "resume":
			if len(fields) != 2 {
				fmt.Println("usage: resume <path>")
				continue
			}
			resumed, tick, err := interactiveResume(fields[1], project)
			if err != nil {
				writeError(err)
				continue
			}
			eng = resumed
			fmt.Printf("resumed after tick %d; next=%d\n", tick, eng.NextTick())
		case "quit", "exit":
			return nil
		default:
			fmt.Printf("unknown command %q; type help\n", fields[0])
		}
	}
	return scanner.Err()
}

func parseEdit(fields []string, sequence int) (engine.Edit, error) {
	if len(fields) != 5 {
		return engine.Edit{}, domain.NewBizError(domain.EEditInvalid, "$.edit", "usage: edit <weight|capacity|fdnode|fdparent> ...")
	}
	kind := fields[1]
	fromTick, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return engine.Edit{}, domain.NewBizError(domain.EEditInvalid, "$.edit.from_tick", "invalid tick")
	}
	edit := engine.Edit{ID: fmt.Sprintf("interactive-%03d", sequence), FromTick: fromTick}
	switch kind {
	case "weight":
		value, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return engine.Edit{}, domain.NewBizError(domain.EEditInvalid, "$.edit.weight", "invalid weight")
		}
		edit.Kind = engine.EditTenantWeight
		edit.Target = fields[2]
		edit.Weight = value
	case "capacity":
		value, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return engine.Edit{}, domain.NewBizError(domain.EEditInvalid, "$.edit.capacity_milli", "invalid capacity")
		}
		edit.Kind = engine.EditNodeCapacity
		edit.Target = fields[2]
		edit.CapacityMilli = value
	case "fdnode":
		edit.Kind = engine.EditNodeFaultDomain
		edit.Target = fields[2]
		edit.FaultDomain = fields[3]
	case "fdparent":
		edit.Kind = engine.EditDomainParent
		edit.Target = fields[2]
		if fields[3] != "-" {
			edit.Parent = fields[3]
		}
	default:
		return engine.Edit{}, domain.NewBizError(domain.EEditInvalid, "$.edit.kind", "unknown edit kind %q", kind)
	}
	return edit, nil
}

func observationsEqual(a, b *engine.Engine) bool {
	left := a.Observations()
	right := b.Observations()
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		leftJSON, _ := json.Marshal(left[i])
		rightJSON, _ := json.Marshal(right[i])
		if string(leftJSON) != string(rightJSON) {
			return false
		}
	}
	return true
}

func interactiveCheckpoint(path string, project *domain.Project) error {
	if err := checkpoint.Remove(path); err != nil {
		return err
	}
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64, CheckpointVersion: "quotaflow-checkpoint-v1"})
	if err != nil {
		return err
	}
	store, _, err := checkpoint.Open(path, "quotaflow-checkpoint-v1", project.CanonicalFingerprint())
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.EnsureInitial(eng.InitialState()); err != nil {
		return err
	}
	for tick := int64(1); tick <= project.MaxTick; tick++ {
		if err := eng.Step(); err != nil {
			return err
		}
		if err := store.Commit(tick, eng.LastState()); err != nil {
			return err
		}
	}
	return nil
}

func interactiveResume(path string, project *domain.Project) (*engine.Engine, int64, error) {
	store, _, err := checkpoint.Open(path, "quotaflow-checkpoint-v1", project.CanonicalFingerprint())
	if err != nil {
		return nil, 0, err
	}
	state, meta, err := store.Load()
	store.Close()
	if err != nil {
		return nil, 0, err
	}
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64, CheckpointVersion: "quotaflow-checkpoint-v1"})
	if err != nil {
		return nil, 0, err
	}
	if err := eng.ResumeFrom(state); err != nil {
		return nil, 0, err
	}
	if err := eng.RunAll(); err != nil {
		return nil, 0, err
	}
	return eng, meta.Tick, nil
}
