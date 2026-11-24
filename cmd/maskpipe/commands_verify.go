package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"

	"maskpipe/internal/model"
	"maskpipe/internal/verify"
)

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dbPath := fs.String("db", "", "SQLite checkpoint database")
	manifestPath := fs.String("manifest", "samples/local-seed/manifest.json", "sample manifest JSON")
	sceneID := fs.String("scene", "blocks", "scene id")
	snapshotID := fs.String("snapshot", "", "snapshot id (default: only snapshot)")
	out := fs.String("out", "reports/verification.json", "JSON report output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dbPath == "" {
		return model.NewError(model.CodeMeta, "db", "flag is required")
	}

	var scene *model.SceneManifest
	if *manifestPath != "" {
		data, err := os.ReadFile(*manifestPath)
		if err != nil {
			return model.NewError(model.CodeMeta, "manifest", "%v", err)
		}
		var manifest model.SampleManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return model.NewError(model.CodeMeta, "manifest_json", "%v", err)
		}
		for i := range manifest.Samples {
			if manifest.Samples[i].ID == *sceneID || *sceneID == "" {
				scene = &manifest.Samples[i]
				break
			}
		}
		if scene == nil {
			return model.NewError(model.CodeMeta, "scene", "scene %q not found", *sceneID)
		}
	}

	report, err := verify.Run(ctx, *dbPath, *snapshotID, scene)
	if err != nil {
		return err
	}
	if err := writeJSON(*out, report); err != nil {
		return err
	}
	if *out != "" {
		println("verification report:", *out, "ok=", report.OK)
	}
	if !report.OK && report.FirstViolation != nil {
		b, _ := json.MarshalIndent(report.FirstViolation, "", "  ")
		println(string(b))
	}
	return nil
}
