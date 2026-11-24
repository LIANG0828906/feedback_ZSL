package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"maskpipe/internal/model"
	"maskpipe/internal/sample"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "threeway":
		err = cmdThreeWay(ctx, os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "version":
		fmt.Println("maskpipe", "maskpipe-algo-1.0.0")
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		if pe, ok := model.AsError(err); ok {
			b, _ := json.MarshalIndent(pe, "", "  ")
			fmt.Fprintln(os.Stderr, string(b))
		} else {
			fmt.Fprintln(os.Stderr, "ERROR:", err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `maskpipe commands:
  generate  [-root samples/local-seed] [-seed 42]
  validate  [-sample samples/local-seed] [-scene blocks]
  run       [-sample DIR] [-scene ID] [-db FILE] [-out report.json] [-params params.json] [-mode incremental|resume|full] [-preview]
  threeway  [-sample DIR] [-scene ID] [-out reports/threeway]
  verify    -db CHECKPOINT.db [-manifest samples/local-seed/manifest.json] [-scene ID] [-out verification.json]
  serve     [-sample DIR] [-scene ID] [-db FILE] [-addr 127.0.0.1:8088]
  version`)
}

type commonFlags struct {
	sampleDir string
	sceneID   string
}

func bindCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.sampleDir, "sample", "samples/local-seed", "local sample directory containing manifest.json")
	fs.StringVar(&c.sampleDir, "samples", "samples/local-seed", "alias for -sample")
	fs.StringVar(&c.sceneID, "scene", "blocks", "scene id: blocks, low-contrast, embedded-noise")
	return c
}

func loadScene(c *commonFlags) (*sample.LoadedScene, error) {
	return sample.LoadScene(c.sampleDir, c.sceneID)
}

func writeJSON(path string, value any) error {
	if path == "" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
