package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"tunnelworkbench/internal/forward"
	"tunnelworkbench/internal/workbench"
)

func runRepl(args []string) error {
	schema := forward.SchemaV2
	if len(args) > 0 {
		schema = args[0]
	}
	svc, err := workbench.NewService("artifacts/workbench")
	if err != nil {
		return err
	}
	fmt.Println("tunnelbench REPL. Commands: help, overview, equiv, load [v1|v2|v3], run [seq|parallel], checkpoint, inject <kind>, edit <json>, quit")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("tunnelbench> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		switch parts[0] {
		case "quit", "exit":
			return nil
		case "help":
			fmt.Println("overview | equiv | load v1|v2|v3 | run seq|parallel | checkpoint | inject KIND | edit JSON")
		case "overview":
			printJSON(svc.SampleOverview())
		case "equiv":
			rep, err := svc.Equivalence()
			if err != nil {
				return err
			}
			printJSON(rep)
		case "load":
			if len(parts) > 1 {
				schema = parts[1]
			}
			eng, err := svc.Load(schema)
			if err != nil {
				printBiz(err)
				continue
			}
			fmt.Printf("loaded %s: tunnels=%d peers=%d rules=%d events=%d hash=%s\n", schema, len(eng.Project.Tunnels), len(eng.Project.Peers), len(eng.Project.Rules), len(eng.Events), eng.Project.ContentHash[:12])
		case "run":
			parallel := len(parts) > 1 && parts[1] == "parallel"
			rep, err := svc.Simulate(schema, parallel, forward.Budget{Workers: 4, PerTunnelBuffer: 8})
			if err != nil {
				printBiz(err)
				continue
			}
			printJSON(map[string]any{"event_count": rep.EventCount, "final_states": rep.FinalStates, "reject_set": rep.RejectSet})
		case "checkpoint":
			demo, err := svc.CheckpointDemo(context.Background(), schema, 7)
			if err != nil {
				printBiz(err)
				continue
			}
			printJSON(demo)
		case "inject":
			if len(parts) < 2 {
				fmt.Println("usage: inject <kind>")
				continue
			}
			printJSON(svc.Inject(parts[1], nil))
		case "edit":
			raw := strings.TrimPrefix(line, "edit")
			raw = strings.TrimSpace(raw)
			var req forward.EditRequest
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				fmt.Println("invalid edit JSON:", err)
				continue
			}
			res, err := svc.ApplyEdit(req)
			if err != nil {
				printBiz(err)
				continue
			}
			printJSON(map[string]any{"consistent": res.Consistent, "change": res.ChangeSet, "diffs": res.Diffs})
		default:
			fmt.Println("unknown command; type help")
		}
	}
	return scanner.Err()
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}

func printBiz(err error) {
	if biz, ok := forward.AsBizError(err); ok {
		printJSON(biz)
		return
	}
	fmt.Println("error:", err)
}
