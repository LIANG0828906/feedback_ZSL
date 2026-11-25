package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"quotaflow/internal/domain"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "load":
		err = runLoad(os.Args[2:])
	case "run":
		err = runLedger(os.Args[2:])
	case "demo":
		err = runDemo(os.Args[2:])
	case "interactive":
		err = runInteractive(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "inject":
		err = runInject(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		err = domain.NewBizError(domain.ESchemaInvalid, "$.command", "unknown command %q", os.Args[1])
	}
	if err != nil {
		writeError(err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `quotaflow - deterministic quota ledger and migration workbench

Commands:
  generate    -seed 42 -out samples
  load        -dir samples
  run         -dir samples -schema v2 -threads 4 -queue 64 [-db checkpoint.db]
  demo        [-seed 42] [-out artifacts/demo]
  interactive [-dir samples]
  verify      -dir samples
  inject      -dir samples -kind negative-quota [-schema v2]

Injection kinds: negative-quota, reserve-over-quota, fault-domain-cycle,
                 snapshot-version, unknown-unit-field, thread-budget,
                 queue-budget, checkpoint-truncated, checkpoint-version`)
}

func writeError(err error) {
	var biz *domain.BizError
	if errors.As(err, &biz) {
		raw, _ := json.MarshalIndent(biz, "", "  ")
		fmt.Fprintln(os.Stderr, string(raw))
		return
	}
	raw, _ := json.MarshalIndent(map[string]any{
		"code": domain.EInternalInvariant, "message": err.Error(),
	}, "", "  ")
	fmt.Fprintln(os.Stderr, string(raw))
}

func loadSet(dir string) (map[string]*domain.Project, error) {
	out := map[string]*domain.Project{}
	for _, version := range []string{"v1", "v2", "v3"} {
		path := filepath.Join(dir, "project-"+version+".json")
		project, err := loadProject(path)
		if err != nil {
			return nil, err
		}
		out[version] = project
	}
	return out, nil
}

func loadProject(path string) (*domain.Project, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, domain.NewBizError(domain.ESchemaInvalid, path, "read project: %v", err)
	}
	return loadBytes(raw, path)
}
